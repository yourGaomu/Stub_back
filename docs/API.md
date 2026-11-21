# HTTP API

本文描述当前 Go 服务已注册的 HTTP 接口。所有管理 API 使用 `/api/v1` 前缀；除邮件验证码、注册、登录、密码重置与公开短链入口外，均使用人类 `Authorization: Bearer <accessToken>` 或机器 `X-API-Key: stub_sk_...`。机器 Key 也可放在 `Authorization: Bearer stub_sk_...` 中。人类 Token 最长 7 天，API Key 的状态、有效期、IP、频率、scope、业务授权和累计额度由服务端实时检查。

MCP 使用 `POST /api/mcp`，凭据必须放在 `X-API-Key` 或 `Authorization` 请求头中，不支持 URL 查询参数传递凭据。

成功列表统一返回 `{ "items": [], "pagination": { "page": 1, "pageSize": 20, "total": 0 } }`。错误统一返回 `{ "error": { "code": "...", "message": "...", "requestId": "..." } }`。时间为 RFC 3339 UTC；公开资源 ID 使用 `usr_`、`link_`、`tpl_`、`ast_` 等前缀。

## 认证与账户

| 方法与路径 | 作用与权限 | 请求 | 成功响应 |
| --- | --- | --- | --- |
| `POST /auth/email-codes` | 申请注册或重置密码验证码；匿名 | `email`, `purpose`, 可选 `inviteCode` | `202`: `accepted`, `expiresInSeconds`, `resendAvailableAt` |
| `POST /auth/register` | 邮箱验证后创建普通用户；匿名 | `email`, `password`, `verificationCode`, 可选 `inviteCode` | `201`: `user`, `workspace` |
| `POST /auth/login` | 创建独立设备会话；需 `X-Client-App` | `email`, `password`, 可选设备信息 | `200`: `accessToken`, `expiresAt`, `user`, `session` |
| `POST /auth/password-resets` | 重置密码并撤销旧会话；匿名 | `email`, `verificationCode`, `newPassword` | `204` |
| `GET /me` | 查询当前用户 | 无 | `200`: 当前 `user` |
| `GET /me/permissions` | 查询当前权限 | 无 | `200`: `isAdmin`, `items` |
| `GET /me/usage` | 查询本人素材/短链用量与额度 | 无 | `200`: `storage`, `urlLinks`, `minioAssetLinks` |
| `POST /auth/logout` | 撤销当前会话 | 无 | `204` |
| `GET /me/sessions` | 分页查询本人会话 | `page`, `pageSize` | `200`: `items`, `pagination` |
| `POST /me/sessions/heartbeat` | 更新当前会话活跃时间，不延长 Token | 无 | `200`: 当前 `session` |
| `DELETE /me/sessions/{sessionId}` | 撤销本人指定会话 | 路径 `sessionId` | `204` |
| `GET /invites` | 查询本人邀请码；管理员可用 `createdByUserId` 跨用户筛选 | `status`, `createdByUserId`, 分页 | 分页 `items` |
| `POST /invites` | 创建一次性邀请码；受注册开关和 `invites.create_revoke` 约束 | `assignedRole`, 可选 `boundEmail`, `expiresAt` | `201`: `inviteId`, 一次性明文 `code`, `inviteURL`, `expiresAt` |
| `POST /invites/{inviteId}/revoke` | 撤销未使用邀请码 | 路径 `inviteId` | `200`: 撤销状态与时间 |

验证码、邀请码明文和访问 Token 不会写入日志或数据库明文字段。登录失败、注册、邀请码核销、密码重置、会话创建与撤销均写入长期审计；审计保留来源 IP、客户端标识或必要的业务结果字段。密码重置成功后所有旧会话立即失效。

验证码比较失败时，在同一事务内递增 `attempt_count` 并提交后返回 `422 invalid_or_expired_verification_code`；每条验证码最多五次错误比较。无可用验证码或次数已达上限同样返回 422。正确码的消费与注册账号/空间/权限/邀请码核销，或密码更新/撤销会话/审计写入共同提交；业务失败时全部回滚。

## 管理员

管理员接口需有效 Bearer Token 且 `role=admin`；管理员拥有所有普通用户功能权限。

| 方法与路径 | 作用 | 请求 | 成功响应 |
| --- | --- | --- | --- |
| `GET /admin/settings/registration` | 查询开放注册/邀请码注册开关 | 无 | `200`: 两个布尔开关和更新时间 |
| `PATCH /admin/settings/registration` | 修改注册开关 | 至少一个 `allowPublicRegistration` / `allowInviteRegistration` | `200`: 最新设置 |
| `GET /admin/users` | 搜索用户及用量 | `query`, `role`, 分页 | 分页用户 DTO |
| `GET /admin/users/{userId}` | 查询用户详情 | 路径 `userId` | `200`: 用户 DTO |
| `GET /admin/users/{userId}/quotas` | 查询用户额度 | 路径 `userId` | `200`: 文件、存储及短链额度 |
| `PATCH /admin/users/{userId}/quotas` | 调整用户额度 | 可选 `maxSingleFileBytes`, `storageQuotaBytes`, `apiKeyQuota`, `urlLinkQuota`, `minioAssetLinkQuota` | `200`: 最新额度与用量 |
| `GET /admin/permissions/catalog` | 查询服务端权限目录 | 无 | `200`: `{items}` |
| `GET /admin/users/{userId}/permissions` | 查询用户当前权限 | 路径 `userId` | `200`: 权限状态 DTO |
| `PUT /admin/users/{userId}/permissions/{permissionCode}` | 授予权限 | 路径参数 | `200`: 新权限状态 |
| `DELETE /admin/users/{userId}/permissions/{permissionCode}` | 撤销权限；撤销邀请码权限时一并撤销未使用邀请码 | 路径参数 | `200`: 新权限状态 |
| `POST /admin/invites/revoke-unused` | 批量撤销用户或全站未使用邀请码 | 可选 `userId`，全站操作必须 `confirmAll=true` | `200`: `revokedCount`, `revokedAt` |
| `GET /admin/businesses` | 分页查询登记业务 | `status`, 分页 | 分页业务 DTO |
| `POST /admin/businesses` | 登记已存在的 MinIO 桶 | `businessId`, `displayName`, `bucketName` | `201`: 业务 DTO |
| `PATCH /admin/businesses/{businessId}` | 修改展示名或启停业务 | `displayName`, `status` | `200`: 业务 DTO |
| `GET /admin/users/{userId}/businesses` | 查询用户业务授权 | 路径 `userId` | `200`: `{items}` |
| `PUT /admin/users/{userId}/businesses/{businessId}` | 授予业务访问权 | 路径参数 | `200`: 授权 DTO |
| `DELETE /admin/users/{userId}/businesses/{businessId}` | 撤销业务访问权 | 路径参数 | `204` |
| `GET /admin/security-events/unknown-short-codes` | 查看未知短码聚合事件 | `from`, `to`, `sourceIp`, `rateLimited`, 分页 | 分页安全事件 DTO |

## API Key（机器调用）

每把 API Key 必须绑定一个 `businessId`，客户端调用业务接口时不再传业务标识，服务端从 Key 反查并注入。普通用户默认最多 20 把未吊销 Key，实际上限由管理员配置 `apiKeyQuota`。创建、编辑、启停和吊销本人 Key 需要 `api_keys.manage` 和人类 Session Token；明文 `secretKey` 只在创建响应中出现一次。

| 方法与路径 | 作用 | 请求/权限 | 成功响应 |
| --- | --- | --- | --- |
| `POST /api-keys` | 创建机器调用 Key | 人类 Bearer；`name`, `businessId`, `scopes`, 可选 IP、频率、累计额度、`allowAccountAccess` | `201`: Key DTO 与一次性 `secretKey` |
| `GET /api-keys` | 查询本人 Key | 人类 Bearer；可选 `status`, 分页 | `200`: 分页 Key DTO，不含明文 |
| `PATCH /api-keys/{apiKeyId}` | 修改规则、scope、业务、额度和账号只读开关 | 人类 Bearer | `200`: 更新后的 Key DTO |
| `POST /api-keys/{apiKeyId}/disable` | 临时停用 | 人类 Bearer | `200`: Key DTO |
| `POST /api-keys/{apiKeyId}/enable` | 恢复启用 | 人类 Bearer | `200`: Key DTO |
| `POST /api-keys/{apiKeyId}/revoke` | 永久吊销，不可恢复 | 人类 Bearer | `200`: Key DTO |
| `GET /admin/users/{userId}/api-keys` | 管理员查询用户 Key | 管理员人类 Bearer | `200`: 分页 Key DTO |
| `POST /admin/api-keys/{apiKeyId}/revoke` | 管理员强制吊销 | 管理员人类 Bearer | `204` |

API Key 只能授予短链、素材和模板的 11 项业务 scope；禁止调用认证、会话、邀请码、管理员、日志和 API Key 管理接口。`/me`、`/me/permissions`、`/me/usage`、`/me/businesses` 仅在 `allowAccountAccess=true` 时可调用。Key 的有效权限是 Key scope 与用户当前权限的实时交集。

API Key 相关错误还包括：`api_key_owner_permission_denied`（所属用户的 API Key 管理权限已被撤销）、`api_key_account_access_denied`（未开启账号只读接口）、`api_key_business_denied`（请求业务与 Key 绑定业务不一致）、`api_key_limit_below_usage`（新额度低于已用累计值）、`invalid_api_key_scope`（scope 不在白名单或重复）、`invalid_api_key_ip`（来源 IP/CIDR 无效）、`invalid_api_key_expiration`（到期时间无效）和 `api_key_already_revoked`（已吊销且不可恢复）。上传和建链额度分别超限时返回 `429 api_key_upload_quota_exceeded`、`429 api_key_link_quota_exceeded`。

## 短链

普通用户必须具有相应 `links.*` 权限。普通用户资源范围由登录用户工作空间确定。短链目标创建后固定。

| 方法与路径 | 作用 | 请求 | 成功响应 |
| --- | --- | --- | --- |
| `POST /links` | 创建 HTTPS URL 或素材短链 | 可选 `title`, `tags`, `code`, `expiresAt`, `activeFrom`, `activeUntil`, `accessPassword`；必传 `target`, `ruleSource` | `201`，包括稳定 `linkId`、短码、规则及开放窗口 |
| `GET /links` | 查询可见短链 | `workspaceId`, `status`, `availability`, `targetType`, `code`, `keyword`, `assetId`, `maxVisits`, 创建时间筛选、分页 | 分页短链摘要，包括开放窗口 |
| `GET /links/{linkId}` | 查询详情 | 路径 `linkId` | `200` 短链 DTO，包括开放窗口 |
| `PATCH /links/{linkId}/metadata` | 修改标题、标签或分组 | `title`, `tags`, `groupId` | `200` 更新后的短链 DTO |
| `PATCH /links/{linkId}/rules` | 修改有效期、开放窗口、次数、IP 范围和口令规则 | 部分更新，时间字段可传 `null` 清除 | `200` 更新后的短链 DTO |
| `POST /links/{linkId}/renew` | 续期并可选择保留或轮换短码 | `expiresAt`, `codeStrategy` | `200`: 当前 `linkId`, `code`, URL, `codeChanged` |
| `POST /links/{linkId}/rotate-code` | 仅轮换短码 | `strategy`, 自定义策略时传 `code` | `200`: 新码、新 URL |
| `POST /links/{linkId}/reset-visits` | 重置访问次数 | 无 | `200`: `visitsUsed=0`, `maxVisits`, `resetAt` |
| `POST /links/{linkId}/disable` | 停用短链 | 无 | `200`: 状态与可用性 |
| `POST /links/{linkId}/enable` | 启用短链 | 无 | `200`: 状态与可用性 |
| `GET /r/{code}` | 匿名解析短链，规则通过后 302 | 路径短码 | HTML 挑战/错误页或 `302 Location` |
| `POST /r/{code}/password` | 提交访问口令；口令错误不消耗访问次数 | 表单 `password` | HTML 错误页或 `302 Location` |

标题为可选字段，最多 120 个字符；标签可选，最多 20 个，每个最多 32 个字符。服务端会去除标签首尾空格，并按不区分大小写的方式去重。`keyword` 对标题、标签、短码和 HTTPS 目标 URL 执行不区分大小写的包含查询；`assetId` 为精确筛选。`maxVisits` 精确匹配规则上限，接受 `-1`、`0` 或 int64 范围内的正整数；省略时不筛选，空值、非整数、低于 -1 或溢出返回 `422 invalid_filter`。列表固定按 `created_at DESC, link_id DESC` 排序。

两个 `PATCH` 接口都要求 `links.manage`。普通用户只能操作本人工作空间内的短链。元数据中的 `title` 和 `tags` 必须成对提供，允许用空字符串和空数组清空。规则 PATCH 支持部分更新，保留目标、短码、访问计数及原模板 ID 和版本。审计记录规则变化及窗口前后值，口令不写入审计。从关闭口令改为开启时必须提供新口令，已开启时省略 `accessPassword` 会保留旧口令。修改最大次数会开启新的提醒周期，访问计数仍保留。

创建接口 `ruleSource` 只能选择 `{ "type": "inline", "rules": ... }` 或 `{ "type": "template", "template": { "templateId": "...", "version": 1 } }`。`maxVisits` 缺省为 `-1`，`0` 禁止访问。创建或更新访问口令时长度须为 1 至 1024 字节，且不能全为空白。

`activeFrom`、`activeUntil` 和 `expiresAt` 接受带时区的 RFC3339 字符串，按 PostgreSQL 微秒精度保存，返回 UTC 时间或 `null`。开放区间含起点、不含终点。实际截止取 `expiresAt` 与 `activeUntil` 中较早的非空值。开始必须严格早于实际截止。创建或主动填写新的结束、到期时间时，值必须晚于当前时间。开始可以是过去时间。

规则 PATCH 对时间字段有三种操作。省略保持原值，`null` 清除边界，传时间字符串替换。未修改的历史结束时间不会阻止其它规则编辑。续期只改变 `expiresAt`，保留 `activeUntil`。例如 `{"activeFrom":"2026-12-12T09:00:00+08:00","activeUntil":"2026-12-12T18:00:00+08:00"}` 表示在同一天北京时间 09:00 起开放，到 18:00 停止。若 17:00 已到期，则在 17:00 停止。

列表、详情和 CSV 中的可用性按 `disabled` → `expired` → `window_ended` → `not_started` → `visit_limit_reached` → `available` 判定。`GET /links` 和短链 CSV 均接受这六种 `availability` 值。业务到期和窗口结束都已过去时，返回 `expired`。CSV 列结构保持兼容，开放起止时间从列表和详情 JSON 读取。

`visitsUsed` 仅在有效 `302 Location` 准备成功后递增，并与 `result=allowed`、`consumesVisit=true` 的日志同事务提交。短链行锁与锁后短码校验保护访问额度。公开入口先检查短码撤销，再检查可用性和 IP，最后才显示或验证口令。`not_started`、次数耗尽、IP 拒绝返回 403。短码撤销、停用、到期、`window_ended` 返回 410。窗口拒绝不写口令尝试状态，访问日志记录对应原因且不扣次。素材不可用时记录 `asset_unavailable` 并返回 503。签发的素材地址按独立期限有效。计数记录服务端放行，客户端最终打开或下载结果不在这一口径内。

此计数口径从升级后的新请求生效；已有 `visits_used`、历史日志的 `consumes_visit` 不回填、不重算，也不会自动重置。需要重置时仍由具备权限的用户显式调用 `reset-visits`，并保留审计。

## 短链分组

分组归工作空间所有，不改变短链的访问规则或 API Key 业务范围。集合与维护接口仅支持人类会话；普通用户固定默认空间，管理员默认也使用默认空间，只有显式 `workspaceId` 才跨空间读取/创建。每空间最多 50 个分组，名称去首尾空格、1–120 字符、大小写不敏感唯一，描述最多 1000 字符。

| 方法与路径 | 权限与请求 | 成功响应 |
| --- | --- | --- |
| `GET /link-groups` | `links.read`；分页，管理员可传 `workspaceId` | 分页 GroupDTO：`groupId`, `workspaceId`, `name`, `description`, `linkCount`, `createdAt`, `updatedAt` |
| `POST /link-groups` | `links.manage`；`name`，可选 `description`, 管理员 `workspaceId` | `201` GroupDTO |
| `PATCH /link-groups/{groupId}` | `links.manage`；至少一个 `name`/`description` | `200` GroupDTO |
| `DELETE /link-groups/{groupId}` | `links.manage` | `200`：`groupId`, `unlinkedCount`, `deleted=true`，仅解除关联 |

短链创建可传 `groupId`，列表可传 `groupId` 或 `ungrouped=true`（互斥）；省略两者为全部。详情/列表返回 `groupId`、`groupName`，未分组时为 null。元数据 PATCH 的 `title` 和 `tags` 仍须成对，另允许独立 `groupId`：省略保持、null 清空、字符串移入。服务端按短链最终工作空间校验分组，跨空间返回 404；数据库复合外键兜底。删除分组、移组及组信息变化均记录审计，短链/素材/访问日志保留。

## CSV 导出

只接受人类会话，不支持 API Key。`GET /links/export.csv` 需要 `links.read`，复用短链列表筛选（忽略分页）；`GET /logs/access/export.csv` 与 `GET /logs/audit/export.csv` 需要 `logs.read_own`，复用相应日志筛选。访问日志额外支持 `groupId`/`ungrouped=true`，按短链当前分组查询历史事件，不是历史分组快照。

单次最多 10000 条、20 MiB，日志范围最多 90 天（缺省最近 30 天）；超过返回 422，不静默截断。每用户每小时最多 10 次生成、同时 1 个，超限返回 429，`Retry-After` 提示重试。生成超时为 60 秒，失败/取消清理临时文件。临时 CSV 完整生成且审计成功后才发送 attachment；生成失败仍为统一 JSON 错误，传输中断不能保证客户端收到完整文件。

格式 UTF-8 BOM、CRLF、标准 CSV 转义、公式前缀保护，时间 UTC RFC3339；响应 `Cache-Control: no-store`、`X-Content-Type-Options: nosniff`。短链 CSV 列：`linkId,code,url,title,tags,targetType,assetId,assetSizeBytes,groupId,groupName,status,availability,visitsUsed,maxVisits,expiresAt,createdAt`。不导出原始目标 URL、密码/哈希、Token、验证码或预签名地址。访问日志列为 `event_id,link_id,accessed_at_utc,source_ip,user_agent,result,reason,consumes_visit,request_id`；审计列为 `event_id,actor_type,actor_id,action,resource_type,resource_id,occurred_at_utc,source_ip,result,details_json,request_id`，details 仍经过白名单。

导出开始/生成成功或失败记录审计，只记录类型、筛选键、行数/大小和耗时，不记录 CSV 内容。日志范围沿用现有访问创建者、审计本人/空间/邮箱范围，不因导出放宽。

## 站内通知与提醒

以下接口仅接受人类 Bearer 会话。通知按当前登录用户隔离，管理员也只读取自己的收件箱。API Key 返回 `403 api_key_forbidden_endpoint`。响应使用 `Cache-Control: no-store`。

| 方法与路径 | 请求 | 成功响应 |
| --- | --- | --- |
| `GET /notifications` | 可选 `unread=true` 只列未读。`page` 默认 1，`pageSize` 默认 20、最大 100 | `200`，`{items,pagination}`，按创建时间与通知 ID 倒序 |
| `GET /notifications/unread-count` | 无 | `200`，`{unreadCount}` |
| `PATCH /notifications/{notificationId}/read` | 稳定 `ntf_UUID`，无请求体 | `204`，重复请求保留首次已读时间 |
| `POST /notifications/read-all` | 无 | `200`，`{markedCount}` 为本次标记数量 |
| `GET /me/notification-settings` | 无 | `200`，`{expiryEnabled,visitsEnabled}`，默认两项均开启 |
| `PATCH /me/notification-settings` | `expiryEnabled` 或 `visitsEnabled` 布尔值，至少提供一项 | `200`，更新后的两项偏好 |

通知包含 `notificationId`、`workspaceId`、`linkId`、`kind`、`title`、`summary`、UTC `createdAt` 和 `readAt`。已删除短链的 `linkId` 可为 `null`，未读通知的 `readAt` 为 `null`。通知不存在或不属于本人时均返回 `404 notification_not_found`。

- `expiry_24h` 针对启用且尚未到达实际截止的短链。距离实际截止大于 0 且不超过 24 小时时可生成。同一短链在同一实际截止时刻最多生成一次。
- `visits_80` 针对启用且最大次数大于 0 的短链，阈值为最大次数的 80% 向上取整。达到或超过阈值均符合条件。每个计数周期最多生成一次，清零或修改最大次数会开启新周期。
- 后台启动时执行一轮，随后每分钟扫描，优先处理近期截止的短链。单轮最多检查 200 个候选、生成 100 条，超时 30 秒。生成前复核时间、短链和偏好。竞争中的候选可跳过，下一轮仍符合条件时再尝试；积压时不保证在截止前送达。关闭偏好保留历史，重新开启可补发当前仍满足条件且从未生成的提醒。首版只向站内收件箱写入，接收人为短链创建者。

## 规则模板

普通用户需 `templates.read_use` 查询/引用，需 `templates.manage` 修改。模板版本不可变；短链保存解析后的规则快照和使用的模板版本。

| 方法与路径 | 作用 | 请求 | 成功响应 |
| --- | --- | --- | --- |
| `POST /rule-templates` | 创建模板和首个版本 | `name`, `description`, `rules`, `changeNote` | `201`: 模板 DTO |
| `GET /rule-templates` | 查询模板 | `workspaceId`, `status`, `includeDeleted`, `keyword`, 分页 | 分页模板 DTO |
| `GET /rule-templates/{templateId}` | 查询模板详情 | 路径 ID | `200`: 模板 DTO |
| `PATCH /rule-templates/{templateId}` | 修改名称/描述 | `name`, `description` | `200`: 模板 DTO |
| `POST /rule-templates/{templateId}/versions` | 创建不可变新版本 | `rules`, `changeNote` | `201`: 新版本 DTO |
| `GET /rule-templates/{templateId}/versions` | 查询版本列表 | 分页 | 分页版本 DTO |
| `GET /rule-templates/{templateId}/versions/{version}` | 查询指定历史版本 | 路径 ID 与版本 | `200`: 版本 DTO |
| `POST /rule-templates/{templateId}/disable` | 停用模板 | 无 | `200`: 模板状态 |
| `POST /rule-templates/{templateId}/enable` | 启用模板 | 无 | `200`: 模板状态 |
| `DELETE /rule-templates/{templateId}` | 软删除模板 | 无 | `200`: 模板状态 |
| `POST /rule-templates/{templateId}/restore` | 恢复软删除模板 | 无 | `200`: 模板状态 |

> 当前接口清单对应已注册路由。分片接口使用后端代理内容传输，不返回 MinIO 上传地址。

## 业务、素材、日志与统计

| 方法与路径 | 作用与权限 | 请求 | 成功响应 |
| --- | --- | --- | --- |
| `GET /me/businesses` | 查询当前可访问业务 | 无 | `200`: `{items}` |
| `POST /assets` | 上传单个素材；`assets.upload` 且必须获准访问业务 | 人类会话 multipart 必传 `businessId`；API Key 不传该字段，由服务端注入 Key 绑定业务；另传 `path`, `file` | `201`: `assetId`, 文件元数据、校验值、创建时间 |
| `GET /assets/uploads` | 查询本人仍在进行或等待清理的分片上传会话，供刷新页面后恢复/查看清理状态 | 无；API Key 自动限定到绑定业务 | `200`: `{items: [{uploadSessionId, idempotencyKey, fileName, businessId, path, sizeBytes, partSizeBytes, partCount, receivedParts, receivedBytes, status, expiresAt}]}`；`cleanup_pending` 表示存储清理后会自动释放预留额度 |
| `POST /assets/uploads` | 创建分片上传会话并预留额度；`assets.upload`；JSON 请求体最大 1 MiB | `businessId`, `path`, `fileName`, `contentType`, `sizeBytes`, 可选 `idempotencyKey` | `201`: `uploadSessionId`, `idempotencyKey`, 分片大小/数量、会话状态和过期时间；请求体过大返回 `413 upload_request_too_large` |
| `PUT /assets/uploads/{uploadSessionId}/parts/{partNumber}` | 流式接收一个 32 MiB 以内的分片；`assets.upload` | `application/octet-stream` 原始分片内容，必须带 `Content-Length` | `200`: 更新后的会话进度 |
| `GET /assets/uploads/{uploadSessionId}` | 查询上传会话和已收分片；`assets.upload` | 路径 ID | `200`: 分片号、已收字节、会话状态 |
| `POST /assets/uploads/{uploadSessionId}/complete` | 校验并合并分片、登记素材、结算额度；`assets.upload` | 无 | `200`: 完成状态和 `asset` DTO |
| `DELETE /assets/uploads/{uploadSessionId}` | 中止分片并释放预留额度；`assets.upload` | 路径 ID | `200`: cancelled 状态；`503 storage_unavailable`: 清理未完成，后台会重试且会话保留在上传任务中 |
| `GET /assets` | 查询素材；`assets.read` | 人类会话可传 `businessId`；API Key 自动限定为绑定业务；另传 `includeDeleted`, `keyword`, 管理员可传 `workspaceId` / `ownerUserId`, 分页 | 分页素材 DTO |
| `GET /assets/{assetId}` | 查询素材元数据；`assets.read` | 路径 ID | `200`: 素材 DTO，不含桶名/对象 key |
| `GET /assets/{assetId}/content` | 签发 15 分钟预签名访问 URL；`assets.read` | 路径 ID | `302 Location` 指向对象公网域名 |
| `DELETE /assets/{assetId}` | 软删除并进入 7 天恢复期；`assets.delete` | 路径 ID | `200`: `deletedAt`, `purgeAfter`, `status` |
| `POST /assets/{assetId}/restore` | 恢复未物理删除的素材；`assets.restore` | 路径 ID | `200`: `restoredAt`, `status` |
| `GET /logs/access` | 查询短链访问日志；`logs.read_own` | `linkId`, `from`, `to`, `result`, `reason`, 管理员可跨空间/用户/业务筛选，分页 | 分页访问事件 |
| `GET /logs/audit` | 查询操作审计；`logs.read_own` | `workspaceId`, `actorUserId`, `businessId`, `resourceType`, `resourceId`, `action`, `result`, `from`, `to`, 分页 | 分页审计事件 |
| `GET /analytics/summary` | 查询用户或工作空间统计；`links.read` | `range=24h\|7d\|30d`, `timeZone`, 管理员可传 `workspaceId` | 汇总、失败原因、时间序列 |
| `GET /analytics/links/{linkId}` | 查询单条短链统计；`links.read` | `range`, `timeZone` | 同上 |

统计的 `totals` 与每个时间桶使用相同口径：

- `requestCount`：短链访问日志总数，含挑战与口令尝试。
- `visitAttemptCount`：`event_type='link_access' AND result!='challenge'`，含所有主链路放行及规则/目标不可用拒绝；不依赖 `consumes_visit`，不含口令挑战与 `password_attempt`。
- `redirectCount`：`event_type='link_access' AND result='allowed' AND reason IS NULL`，表示服务端放行，不表示后续下载完成。
- `failureCount`：仅 `result='denied'`，包括口令错误/冷却；挑战不计为失败。失败原因分布也只汇总 denied。

CORS 来源通过 `CORS_ALLOWED_ORIGINS` 显式配置，具体默认值与原生应用来源示例见 README。非 development 环境为空时拒绝跨域请求（403），无 Origin 和同源请求仍正常处理。

素材删除任务每分钟后台扫描到期记录；只有 MinIO 确认对象删除后才释放额度。`/healthz` 表示进程存活，`/readyz` 同时检查已配置的 PostgreSQL、Redis 与 MinIO 依赖。
