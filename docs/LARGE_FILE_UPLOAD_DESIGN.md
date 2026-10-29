# 大文件素材上传方案

本文记录素材上传的两种方式、分片续传流程、额度预留、用户界面和 MCP 内容传输。普通上传和分片接口均已落地，部署后需执行 Goose 迁移 `00004_multipart_uploads.sql`。

## 1. 目标与方案决定

保留两种上传方式，调用方按文件大小和网络情况选择：

1. 普通单文件上传：一个 HTTP multipart/form-data 请求，把整个文件作为一个上传任务提交给后端。适合小文件，失败后需要从头重传。
2. 分片续传：先向后端申请上传会话，再由浏览器、客户端、SDK 或 MCP 将分片内容发送到后端；后端流式写入 MinIO 内网；失败时只重传缺失分片。

两种方式统一执行用户身份、assets.upload 权限、业务桶授权、单文件上限、存储总额度和 API Key 约束。分片续传是传输方式，不应绕过已有权限或额度。

分片上传时，2 GB 文件不会一次性装入 Go 内存，但文件分片会经过 Go 后端。Go 服务负责验证、维护上传会话和额度，并将每个分片的请求体流式写入 MinIO 内网 `minio:9000`。网页和 MCP 都不需要拿到 MinIO 上传地址或凭据。MinIO 公网域名只用于素材下载时的短期预签名 302；MinIO Console 不对普通用户开放。

## 2. 文件大小与额度规则

现有用户字段 maxSingleFileBytes 表示一个素材允许的最大文件大小，继续沿用该含义，并同时应用于普通上传和分片上传。它是每个文件的上限，不是单次 HTTP 请求体上限。

- 普通上传另有服务端单请求上限，首期固定为 100 MiB。即使管理员提高了某个用户的 maxSingleFileBytes，超过普通请求上限的文件也必须改走分片接口。
- maxSingleFileBytes 新用户默认仍为 50 MB；管理员可单独提高。用户要上传约 2 GB 文件，管理员必须先把该用户的单文件上限提高到至少该文件大小。
- 用户存储总额度默认仍为 500 MB。用户要上传约 2 GB 文件，还必须把总存储额度提高到至少“当前已使用空间 + 其他上传预留空间 + 本文件大小”。任一上限不足都在创建上传会话时拒绝。
- 所有比较使用数据库中的精确字节数；页面沿用当前十进制 MB 显示方式。分片尺寸使用 MiB 表示，避免把 MB 和 MiB 混为一谈。
- 上传开始时客户端提供的 sizeBytes 只用于预留和初步校验，不能作为已存文件大小的权威值。文件完成后必须由服务端通过 MinIO HeadObject 取得实际对象大小，并要求它与预期大小完全一致。

### 2.1 额度预留

用户表增加 storage_reserved_bytes，或采用具有同等并发安全性的会话汇总方案。推荐在用户行上维护此字段，便于原子判断和展示。

可用空间计算：

    可用空间 = storage_quota_bytes - storage_used_bytes - storage_reserved_bytes

申请上传会话时，在一个 PostgreSQL 事务中锁定用户额度记录，依次检查：

1. 用户和 API Key 的素材上传权限、业务桶授权均有效。
2. 文件大小大于 0，且不超过该用户 maxSingleFileBytes 和系统允许的最大文件大小。
3. storage_used_bytes + storage_reserved_bytes + 本文件 sizeBytes 不超过 storage_quota_bytes。
4. 用户尚未超过同时进行的上传会话数量限制。
5. 对该文件大小增加 storage_reserved_bytes，并建立待创建的上传会话记录。

MinIO 的 multipart 创建操作不能和数据库事务组成一个原子事务。后端把会话先记为 initializing 并完成额度预留，再调用 MinIO 创建 multipart upload；成功后保存 MinIO upload ID 并转为 uploading。MinIO 创建失败时，把会话标记为 failed 并释放预留；后台 worker 负责回收遗留会话。

完成上传后，先验证 MinIO 对象已合并、实际大小等于预期值，再在同一个数据库事务中创建素材记录、增加 storage_used_bytes、减少 storage_reserved_bytes，并把会话标记为 completed。不得先释放预留再增加已用额度。

取消、过期、合并失败或对象校验失败时，先调用 MinIO AbortMultipartUpload；如果对象已经合并则删除对象。只有 MinIO 确认清理成功后才能释放 storage_reserved_bytes。MinIO 暂时不可用时，会话进入 cleanup_pending，预留保留并由后台任务重试。后台任务也应处理进程崩溃后遗留的 creating、completing 和 cleanup_pending 会话。

API Key 若配置了独立累计上传额度，也应在创建会话时预留，完成时结算，取消时释放；不能只在最终完成时才检查，否则多个并发会话可能共同超额。

## 3. 分片参数与调用限制

以下默认参数已经确认，作为首期后端策略：

| 配置项 | 已确认值 | 用途 |
| --- | --- | --- |
| 默认分片大小 | 32 MiB | 约 2 GiB 文件约 64 个分片；小于 32 MiB 的文件使用较小的单个末片 |
| S3 最小非末片大小 | 5 MiB | 遵循 S3 Multipart Upload 约束；末片可小于该值 |
| 最大分片数 | 10,000 | 超出时后端按文件大小选择更大的分片尺寸或拒绝 |
| 单会话并发分片 | 3 个 | 控制浏览器并发、内存和 MinIO 压力 |
| 每用户活动会话数 | 3 个 | 防止大量未完成会话长期占用预留额度 |
| 上传会话有效期 | 24 小时 | 到期后终止并清理未完成的 MinIO multipart upload |
| 客户端自动重试 | 同一分片最多 3 次 | 指数退避；耗尽后显示失败并允许用户手动重试 |

分片尺寸和并发数由后端策略返回，客户端不能自行把分片放大、跳过分片或为其他对象上传。每个分片请求只允许访问指定会话、指定分片号和指定长度；后端按流读取请求体并调用 MinIO `UploadPart`。上传接口不返回 MinIO URL，服务端日志也不得记录文件内容、凭据或请求体。

为了阻止客户端声明小文件、实际上传超大分片，分片接口要求 `Content-Length`；后端按分片号计算准确长度，限制请求体，完成时由 MinIO `HeadObject` 校验实际对象大小。Nginx 关闭请求缓冲，避免先把整片写入 Nginx 临时目录。

## 4. 完整上传流程

### 4.1 创建会话

1. 调用方提交业务标识、文件名、内容类型、总字节数和可选逻辑目录。浏览器还可提交幂等键，避免双击重复创建。
2. 后端验证登录 Token 或 API Key、assets.upload 权限、API Key 绑定业务、业务授权、文件上限、存储总额度和活动会话数。
3. 后端原子预留文件预计占用的额度，并创建数据库会话。
4. 后端向 MinIO 创建 Multipart Upload，保存 MinIO upload ID，返回 uploadSessionId、分片大小、分片数和会话过期时间。
5. 如果同一幂等键重试，返回原有活动会话，而不是重复预留额度或创建第二个 MinIO 对象。

### 4.2 上传与续传

1. 客户端按后端返回的 partSizeBytes 切片。浏览器每次最多并发上传 3 个分片。
2. 客户端以原始二进制请求体调用后端分片接口，并带上分片号和 `Content-Length`。
3. 后端验证会话和分片参数，边读取请求体边写入 MinIO 内网；分片成功后返回已接收的分片号和字节数。
4. 网络失败时只重试失败的 part；不需要重新申请或暴露 MinIO 上传 URL。
5. 客户端查询会话状态时，后端以 MinIO ListParts 的结果为准，返回已收到的分片号和字节数。数据库会话保存业务状态，MinIO 是分片是否已到达的权威来源。
6. 页面刷新或应用重启后，用户可以重新查询活动会话并重新选择同一个本地文件继续上传。浏览器通常不能在没有用户操作的情况下重新读取本地文件，因此不能承诺仅靠服务端恢复本地文件读取权限。

### 4.3 完成与取消

1. 所有分片上传完成后，客户端调用完成接口。
2. 后端锁定会话并检查状态；从 MinIO 查询已收分片，确认分片号连续、分片数符合预期、总长度等于预期文件大小。
3. 后端调用 MinIO CompleteMultipartUpload，并用 HeadObject 再次核验实际对象大小。
4. 核验成功后，事务性地登记素材、把预留额度转为已用额度，并返回素材 DTO。重复完成请求返回同一个素材结果。
5. 任一步校验失败时，后端清理 MinIO 分片或已合并对象；清理确认后释放预留额度。
6. 用户取消时，后端调用 MinIO AbortMultipartUpload，成功后释放预留额度并标记 cancelled。
7. 到期清理任务周期性扫描过期会话，执行同样的 MinIO 清理和额度释放流程。

## 5. 前端页面需要的功能

需要修改前端上传页面。建议把上传任务展示成一个列表，每个文件单独一行或卡片，显示：

- 文件名、文件大小、所属业务和上传状态。
- 实时进度：已完成字节数 / 文件总字节数、百分比和进度条。正在传输的分片使用浏览器上传进度，已完成分片从 MinIO 状态查询结果计算。
- 当前上传速度和预计剩余时间。速度按近期样本计算，剩余时间标注为估算值；暂停、等待网络或重试期间不显示过度精确的时间。
- 状态包括准备中、上传中、暂停、重试中、等待恢复、合并中、完成、失败、取消和过期。
- 对失败分片提供重试；提供暂停/继续和取消操作。暂停可中止当前客户端 PUT，已成功分片保留在 MinIO，继续时只补传缺少的分片。
- 页面刷新后从后端读取本人未完成的会话，并允许用户重新选择本地文件后续传。页面可以用 IndexedDB 保存非敏感的会话 ID 和分片进度提示，但服务端状态始终优先；不要把预签名 URL 持久化。

前端应使用 XMLHttpRequest 或其他能报告上传进度的实现，不应仅依靠普通 fetch 的响应完成事件来显示实时字节进度。官方 SDK 或命令行工具以后可以复用相同的后端分片内容接口，为非浏览器调用方封装切片、重试和续传；普通 POST 接口仍然保留。

## 6. 已实现接口

所有接口都要求用户 Bearer Token 或 API Key。必须先验证 assets.upload 权限和业务授权。人类 Token 上传时需要提交 businessId；API Key 从 Key 记录反查绑定业务，调用方不提交或不能覆盖它。

### 6.1 创建分片上传会话

POST /api/v1/assets/uploads

作用：检查权限和额度，预留存储空间，建立 MinIO Multipart Upload 会话。

请求字段：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| businessId | string | 人类 Token 必传；API Key 调用由服务端从 Key 获取 |
| path | string | 可选，经过校验的逻辑目录 |
| fileName | string | 必传，原始文件名 |
| contentType | string | 可选，文件 MIME 类型 |
| sizeBytes | integer | 必传，预计总文件字节数，必须大于 0 |
| idempotencyKey | string | 可选，客户端生成；用于创建会话重试去重 |

成功响应 201：

    {
      "uploadSessionId": "upl_<uuid>",
      "idempotencyKey": "web:v1:<sha256>",
      "fileName": "video.mp4",
      "sizeBytes": 2147483648,
      "partSizeBytes": 33554432,
      "partCount": 64,
      "expiresAt": "RFC3339 UTC",
      "status": "uploading",
      "receivedParts": [],
      "receivedBytes": 0
    }

### 6.2 查询上传会话和已收分片

GET /api/v1/assets/uploads/{uploadSessionId}

作用：查询会话状态，并由后端向 MinIO 查询实际已收到的分片；只能查询会话所有者或有管理权限的管理员。

成功响应 200：

    {
      "uploadSessionId": "upl_<uuid>",
      "status": "uploading",
      "sizeBytes": 2147483648,
      "partSizeBytes": 33554432,
      "partCount": 64,
      "receivedBytes": 67108864,
      "receivedParts": [1, 2],
      "expiresAt": "RFC3339 UTC"
    }

### 6.3 上传某个分片的内容

PUT /api/v1/assets/uploads/{uploadSessionId}/parts/{partNumber}

作用：验证会话仍有效、分片号和分片长度符合服务端策略，并将请求体流式写入 MinIO 内网。浏览器、SDK 和 MCP 都只请求此后端接口，不直接访问 MinIO 上传端点。

请求头：

    Content-Type: application/octet-stream
    Content-Length: 33554432
请求体：浏览器和 SDK 发送当前分片的原始二进制内容，不使用 multipart，不使用 Base64。MCP JSON-RPC 工具由于参数协议限制，使用 Base64 表达单个分片，不能一次传输整个大文件。

成功响应 200：

成功响应 200：返回更新后的上传会话 DTO。

### 6.4 完成上传

POST /api/v1/assets/uploads/{uploadSessionId}/complete

作用：后端读取 MinIO 分片清单、校验并合并对象、校验实际大小、登记素材并结算配额。客户端不传入可信的总大小或可任意拼装的 ETag 清单。

请求：无请求体。

成功响应 201：

    {
      "asset": {
        "assetId": "ast_<uuid>",
        "businessId": "business-id",
        "path": "可校验的逻辑目录",
        "fileName": "video.mp4",
        "contentType": "video/mp4",
        "sizeBytes": 2147483648,
        "checksumSha256": "",
        "createdAt": "RFC3339 UTC"
      },
      "status": "completed"
    }

重复完成时返回同一素材 DTO，不应再次增加 storage_used_bytes。

### 6.5 取消上传

DELETE /api/v1/assets/uploads/{uploadSessionId}

作用：取消本人尚未完成的会话，清理 MinIO 分片并释放额度；只有清理成功才返回 cancelled。

成功响应 200：

    {
      "uploadSessionId": "upl_<uuid>",
      "status": "cancelled",
      "releasedBytes": 2147483648
    }

错误建议使用稳定错误码，例如 file_too_large、insufficient_storage_quota、upload_session_limit_exceeded、upload_session_expired、upload_session_not_found、upload_part_invalid、upload_parts_incomplete 和 upload_checksum_mismatch。HTTP 错误响应沿用当前统一 error/requestId 结构。

## 7. MCP 调用方式

MCP 复用第 4 节的上传会话、分片、续传、额度预留和完成校验规则。MCP 文件输入只接受实际文件内容，不接受本地路径、公网 URL 或文件引用。用户在 Agent 对话中附加/提供文件并说“帮我把这个视频上传到短链接”后，Agent 宿主把附件内容以分片字节流交给 MCP；用户不需要输入路径。

### 7.1 MCP 内容上传流程

1. Agent 宿主把实际文件内容按分片传给 MCP，并提供文件名、类型和总大小等元数据；这些信息由附件内容产生，不要求用户手动填写。
2. MCP 使用当前用户本人持有的 API Key 请求后端创建上传会话。后端检查 assets.upload 权限、业务授权、单文件上限和存储额度，并预留预计文件大小。
3. MCP 接收每个分片的实际字节内容，校验分片序号和长度，再使用服务端 MinIO 客户端通过内网把该分片写入对应 Multipart Upload。当前分片模式校验最终对象总大小，不计算完整文件 SHA-256；MCP 服务只缓冲有限数量的分片，不把整个 2 GB 文件装入内存。
4. 失败的分片由 Agent 宿主重新发送；MCP 查询服务端会话状态，只补传 MinIO 未收到的分片。
5. 所有分片到齐后，MCP 调用完成接口。后端合并对象、检查实际大小、登记素材，并把预留额度转为已用额度。
6. MCP 返回素材 ID、文件名、大小和状态。取消、超时和清理失败仍按第 4 节规则释放或保留额度。

因此，MCP 路径的数据流是 Agent 宿主 -> MCP 服务 -> 短链接后端 -> MinIO 内网；网页路径是浏览器 -> 短链接后端 -> MinIO 内网。两条入口都只传分片内容，2 GB 文件不会一次性载入 Go 内存，并共用上传会话状态、配额、权限和素材登记规则。

### 7.2 MCP 工具接口

MCP 工具通过当前 Streamable HTTP JSON-RPC 端点实现。每次 `upload_asset_part` 调用的 `contentBase64` 只能表示一个不超过 32 MiB 的分片；整个 2 GB 文件仍由多个独立工具调用传输，服务端将 Base64 解码流式写入 MinIO。

| 工具 | 入参 | 作用 | 返回 |
| --- | --- | --- | --- |
| begin_asset_upload | fileName, contentType, sizeBytes | 创建上传会话并预留额度；元数据来自待上传内容 | uploadSessionId、partSizeBytes、partCount、expiresAt |
| upload_asset_part | uploadSessionId, partNumber, contentBase64 | 接收单个分片的内容并流式写入 MinIO；只接受会话指定的分片号和长度 | 分片号、已收字节、状态 |
| get_asset_upload_status | uploadSessionId | 查询已收到的分片和会话状态 | completedParts、receivedBytes、status、expiresAt |
| complete_asset_upload | uploadSessionId | 校验并合并分片、登记素材、结算额度 | 素材 DTO、completed 状态 |
| cancel_asset_upload | uploadSessionId | 清理未完成分片并释放预留额度 | cancelled 状态、releasedBytes |

### 7.3 MCP 内容传输要求

普通 MCP 工具调用通过 JSON-RPC 传参，不是通用的数 GB 二进制流协议。当前实现将每个分片限制为 32 MiB，单个 JSON-RPC 请求限制为 48 MiB，Agent 宿主必须能把附件内容分片传给 MCP。不能因此改成让用户输入路径或 URL。

不要让完整大文件进入模型文本上下文或经模型逐字生成 Base64。由 Agent 宿主的附件处理层生成并传递每个分片的 Base64 内容；失败时只重传对应分片。2 GB 文件约需 64 次 MCP 分片调用，实际工具宿主可能对单次参数大小另有限制。

### 7.4 权限、凭据与额度

MCP 使用当前用户本人持有、绑定单一业务的 API Key，并沿用 assets.upload 权限、业务授权、单文件上限、存储总额度和 API Key 累计额度。不得使用全局共享 API Key 代表所有用户。API Key 保存在 Agent 宿主的安全凭据存储中，不作为文件内容或普通工具参数；MinIO 凭据不下发给 Agent。

上传会话绑定创建者；查询、续传、完成和取消都需检查当前用户及权限。审计记录文件元数据、分片结果、来源和请求 ID，不记录 API Key、文件内容或 MinIO 凭据。

## 8. 数据与状态

`asset_upload_sessions` 表保存会话 ID、用户/工作空间/业务 ID、API Key ID、MinIO upload ID、对象 key、原始文件名、MIME 类型、预期总字节数、分片大小、分片数、预留字节、状态、幂等键、过期时间、创建/完成/清理时间和失败摘要；`asset_upload_parts` 保存 MinIO 分片回执副本，查询进度仍以 MinIO ListParts 为权威。

状态定义为 initializing、uploading、completing、completed、cleanup_pending、cancelled、expired、failed。状态更新和素材登记应有事务边界；MinIO 与 PostgreSQL 之间的失败通过幂等操作和后台补偿任务收敛。

MinIO 的 ListParts 是分片接收状态权威，不需要把每次临时进度写入 PostgreSQL。会话状态、预留额度和素材元数据由 PostgreSQL 保存。日志记录操作者、会话、大小、结果、来源 IP、User-Agent 和 requestId；不得记录 Token、MinIO 凭据或预签名 URL。

## 9. 部署要求

- 浏览器和 MCP 的控制、分片上传请求都使用 `stubapi.mikasa.wiki`；分片请求体由后端流式转发到 MinIO 内网 `minio:9000`。
- 普通 `POST /api/v1/assets` 也需要在 Nginx 设置略高于 `100 MiB` 的 `client_max_body_size`（例如 `102m`），否则请求会在到达 Go 服务前被 Nginx 拒绝；普通接口超过 100 MiB 仍由后端返回 413 并要求改用分片接口。
- Nginx 对 `/api/v1/assets/uploads/` 关闭 `proxy_request_buffering`，设置 34 MiB 请求体上限；`/api/mcp` 设置 50 MiB 上限以容纳 32 MiB 分片的 Base64 JSON。示例：

```nginx
location ^~ /api/v1/assets/uploads {
    client_max_body_size 34m;
    proxy_request_buffering off;
    proxy_read_timeout 600s;
    proxy_send_timeout 600s;
    proxy_pass http://short-links-back:8080;
}

location = /api/mcp {
    client_max_body_size 50m;
    proxy_read_timeout 600s;
    proxy_send_timeout 600s;
    proxy_pass http://short-links-back:8080;
}

location ^~ /api/ {
    client_max_body_size 102m;
    proxy_pass http://short-links-back:8080;
}
```

`/api/v1/assets/uploads` 和 `/api/mcp` 的专用规则应放在通用 `/api/` 规则之前；普通上传接口的 102m 只是给 100 MiB 文件及 multipart 表单留少量边界空间。
- 后端容器挂载专用上传临时目录，例如 `./data/upload-tmp:/tmp`，宿主机需提前创建目录并赋予容器用户 `10001:10001` 写权限。分片接口按流处理，临时目录只用于普通 multipart 暂存、失败重试或异常恢复，不按 2 GB 文件大小一次性占用内存。
- `thing.mikasa.wiki` 仍可反代 MinIO:9000，用于下载预签名 URL 的 302；不向浏览器提供上传预签名 URL，MinIO Console 端口不向普通用户开放。

## 10. 已确认的首期规则

- 普通单请求上传上限：100 MiB。
- 分片大小：32 MiB；每会话最多并发 3 个分片；每用户最多 3 个活动上传会话。
- 上传分片不使用预签名 URL，浏览器和 MCP 只调用后端分片内容接口；上传会话有效期 24 小时，单个分片自动重试最多 3 次。素材下载仍可按现有规则返回 15 分钟预签名 URL 302。
- maxSingleFileBytes 同时限制普通上传和分片上传。新用户单文件上限默认 50 MB，总存储额度默认 500 MB；上传约 2 GB 文件前，管理员需要提高该用户的单文件上限和总存储额度。
- MCP 使用每个用户本人持有、绑定单一业务的 API Key，不使用全局共享 Key。MCP 文件输入只接受 Agent 宿主传入的实际内容分片，不接受路径、URL 或文件引用；若宿主暂不支持内容流传输，则使用已登录网页选择/附加文件并上传的备用流程。
- 浏览器刷新后可恢复服务端上传会话，但需要用户重新选择同一个本地文件；服务端状态以 MinIO 已收分片为准。

以上规则已写入后端迁移、HTTP 路由、前端上传任务和 MCP 工具实现。

## 11. 完成阶段修复与待验证

分片全部接收后仍需调用 `complete`，成功登记素材和结算额度才算上传完成。此次修复补齐完成事务锁定会话时遗漏的 SQL 参数；分片素材的 `checksum_sha256` 允许为 NULL，列表与详情统一返回空字符串，不将分片 ETag 当作整文件 SHA-256。并发完成请求发现会话已完成时，先释放会话行锁再查询结果。

内部异常在服务端日志中记录 `request_id` 和错误详情，完成事务错误同时包含会话 ID 及失败阶段；HTTP 响应仍保留通用错误信息。日志不额外记录请求头、Token、文件内容或 MinIO 凭据。

### 11.1 已卡住会话的恢复

1. 先部署包含修复的后端版本，再查询原上传会话的状态和 `expiresAt`。
2. 会话仍为 `uploading` 或 `completing`、未过期且对象未清理时，对同一会话再次调用 `POST /api/v1/assets/uploads/{uploadSessionId}/complete`。后端会先检查完整对象大小；对象已合并时跳过合并，只重试登记和结算，不必重新传文件。
3. 成功后确认 `status=completed`、`asset_id` 非空，用户的预留额度转成已用额度。会话行的 `reserved_bytes` 保留原始预留金额，不作为当前剩余预留额度。
4. 不手动修改会话状态、素材记录或用户额度。会话过期后按原清理机制处理，不能保证原会话仍可续传。

### 11.2 待用户验证

- 原有 `completing` 会话在有效期内可完成，素材列表、详情和下载可用。
- 对同一已完成会话重复调用完成接口，返回同一素材，用户额度只结算一次。
- 两个完成请求并发到达时均能结束，不因重复查询等待本请求持有的会话行锁。
- 后续内部错误日志包含失败阶段与真实错误，客户端不收到内部错误详情。

本次仅修改代码与方案，未执行编译、构建、测试或线上恢复；以上事项需部署后验证。
