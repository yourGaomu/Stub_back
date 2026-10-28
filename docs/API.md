# HTTP API

本文描述当前 Go 服务已注册的 HTTP 接口。所有管理 API 使用 `/api/v1` 前缀；除邮件验证码、注册、登录、密码重置与公开短链入口外，均使用人类 `Authorization: Bearer <accessToken>` 或机器 `X-API-Key: stub_sk_...`。机器 Key 也可放在 `Authorization: Bearer stub_sk_...` 中。人类 Token 最长 7 天，API Key 的状态、有效期、IP、频率、scope、业务授权和累计额度由服务端实时检查。

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

普通用户必须具有相应 `links.*` 权限；资源范围始终从登录用户工作空间确定。短链目标创建后不可更改。第一阶段暂不实现批量创建；提醒功能安排在后续开发。

| 方法与路径 | 作用 | 请求 | 成功响应 |
| --- | --- | --- | --- |
| `POST /links` | 创建 HTTPS URL 或素材短链 | 可选 `title`, `tags`, `code`；`target`, `expiresAt`, `accessPassword`, `ruleSource` | `201`: 稳定 `linkId`、`title`、`tags`、`code`、`url`、目标、规则及模板版本 |
| `GET /links` | 查询可见短链 | `workspaceId`, `status`, `availability`, `targetType`, `code`, `keyword`, `assetId`, 创建时间筛选、分页 | 分页短链摘要，含标题和标签 |
| `GET /links/{linkId}` | 查询详情 | 路径 `linkId` | `200`: 短链 DTO |
| `PATCH /links/{linkId}/metadata` | 修改标题和标签 | `title`, `tags` | `200`: 更新后的短链 DTO |
| `PATCH /links/{linkId}/rules` | 修改到期时间、次数、IP 范围和口令规则 | 任一规则字段；`expiresAt:null` 表示清除到期时间 | `200`: 更新后的短链 DTO |
| `POST /links/{linkId}/renew` | 续期并可选择保留或轮换短码 | `expiresAt`, `codeStrategy` | `200`: 当前 `linkId`, `code`, URL, `codeChanged` |
| `POST /links/{linkId}/rotate-code` | 仅轮换短码 | `strategy`, 自定义策略时传 `code` | `200`: 新码、新 URL |
| `POST /links/{linkId}/reset-visits` | 重置访问次数 | 无 | `200`: `visitsUsed=0`, `maxVisits`, `resetAt` |
| `POST /links/{linkId}/disable` | 停用短链 | 无 | `200`: 状态与可用性 |
| `POST /links/{linkId}/enable` | 启用短链 | 无 | `200`: 状态与可用性 |
| `GET /r/{code}` | 匿名解析短链，规则通过后 302 | 路径短码 | HTML 挑战/错误页或 `302 Location` |
| `POST /r/{code}/password` | 提交访问口令；口令错误不消耗访问次数 | 表单 `password` | HTML 错误页或 `302 Location` |

标题为可选字段，最多 120 个字符；标签可选，最多 20 个，每个最多 32 个字符。服务端会去除标签首尾空格，并按不区分大小写的方式去重。`keyword` 对标题、标签、短码和 HTTPS 目标 URL 执行不区分大小写的包含查询；`assetId` 为精确筛选。

两个 `PATCH` 接口都要求 `links.manage`，并且只能操作本人工作空间内的短链。元数据请求必须同时提供 `title` 和 `tags`，允许传空字符串或空数组清空；规则请求可部分更新。规则修改保留 `LinkId`、目标、短码、累计访问次数及创建时记录的模板 ID/版本，规则仍以短链自身快照为准；每次修改写入审计日志，不记录口令明文。启用口令时可不传 `accessPassword` 以保留现有口令；从未启用状态改为启用时必须提供新口令。访问次数规则修改不会重置 `visitsUsed`。

创建接口 `ruleSource` 只能选择 `{ "type": "inline", "rules": ... }` 或 `{ "type": "template", "template": { "templateId": "...", "version": 1 } }`。`maxVisits` 缺省为 `-1`，`0` 禁止访问。访问日志以短链服务实际收到的放行尝试为准，不推断 302 后的下载结果。

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
| `GET /assets/uploads` | 查询本人仍在进行的分片上传会话，供刷新页面后恢复 | 无；API Key 自动限定到绑定业务 | `200`: `{items: [{uploadSessionId, idempotencyKey, fileName, sizeBytes, partSizeBytes, partCount, receivedParts, receivedBytes, status, expiresAt}]}` |
| `POST /assets/uploads` | 创建分片上传会话并预留额度；`assets.upload` | `businessId`, `path`, `fileName`, `contentType`, `sizeBytes`, 可选 `idempotencyKey` | `201`: `uploadSessionId`, `idempotencyKey`, 分片大小/数量、会话状态和过期时间 |
| `PUT /assets/uploads/{uploadSessionId}/parts/{partNumber}` | 流式接收一个 32 MiB 以内的分片；`assets.upload` | `application/octet-stream` 原始分片内容，必须带 `Content-Length` | `200`: 更新后的会话进度 |
| `GET /assets/uploads/{uploadSessionId}` | 查询上传会话和已收分片；`assets.upload` | 路径 ID | `200`: 分片号、已收字节、会话状态 |
| `POST /assets/uploads/{uploadSessionId}/complete` | 校验并合并分片、登记素材、结算额度；`assets.upload` | 无 | `200`: 完成状态和 `asset` DTO |
| `DELETE /assets/uploads/{uploadSessionId}` | 中止分片并释放预留额度；`assets.upload` | 路径 ID | `200`: cancelled 状态 |
| `GET /assets` | 查询素材；`assets.read` | 人类会话可传 `businessId`；API Key 自动限定为绑定业务；另传 `includeDeleted`, `keyword`, 管理员可传 `workspaceId` / `ownerUserId`, 分页 | 分页素材 DTO |
| `GET /assets/{assetId}` | 查询素材元数据；`assets.read` | 路径 ID | `200`: 素材 DTO，不含桶名/对象 key |
| `GET /assets/{assetId}/content` | 签发 15 分钟预签名访问 URL；`assets.read` | 路径 ID | `302 Location` 指向对象公网域名 |
| `DELETE /assets/{assetId}` | 软删除并进入 7 天恢复期；`assets.delete` | 路径 ID | `200`: `deletedAt`, `purgeAfter`, `status` |
| `POST /assets/{assetId}/restore` | 恢复未物理删除的素材；`assets.restore` | 路径 ID | `200`: `restoredAt`, `status` |
| `GET /logs/access` | 查询短链访问日志；`logs.read_own` | `linkId`, `from`, `to`, `result`, `reason`, 管理员可跨空间/用户/业务筛选，分页 | 分页访问事件 |
| `GET /logs/audit` | 查询操作审计；`logs.read_own` | `workspaceId`, `actorUserId`, `businessId`, `resourceType`, `resourceId`, `action`, `result`, `from`, `to`, 分页 | 分页审计事件 |
| `GET /analytics/summary` | 查询用户或工作空间统计；`links.read` | `range=24h\|7d\|30d`, `timeZone`, 管理员可传 `workspaceId` | 汇总、失败原因、时间序列 |
| `GET /analytics/links/{linkId}` | 查询单条短链统计；`links.read` | `range`, `timeZone` | 同上 |

素材删除任务每分钟后台扫描到期记录；只有 MinIO 确认对象删除后才释放额度。`/healthz` 表示进程存活，`/readyz` 同时检查已配置的 PostgreSQL、Redis 与 MinIO 依赖。
