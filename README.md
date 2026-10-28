# Short Links Backend

独立短链接服务后端，使用 Go、Gin、PostgreSQL 和 Redis。PostgreSQL 是身份、权限、短链规则与访问日志的权威存储；Redis 仅承载可过期缓存和短时限流数据；MinIO 仅保存素材对象。

## 当前范围

- 认证、邮箱验证、邀请码、会话、用户权限与额度管理。
- 通用 HTTPS 短链、访问规则、规则模板、访问日志、审计日志、未知短码安全事件与访问统计。
- 管理员登记业务桶并授予用户业务权限；MinIO 素材上传、预览、软删除、恢复和到期物理清理。
- PostgreSQL 为业务权威数据源，Redis 仅用于短期邮件限流；MinIO 凭据只保留在服务端。
- 接口和业务规则以产品规格 `SHORT_LINK_CONFIRMED_FEATURES.md` 为准。

服务按 `transport/httpapi`、`modules/*`、`platform/*` 和 `app` 分层。HTTP Handler 负责 DTO/状态码，领域服务负责业务事务，平台包封装数据库、Redis 和 MinIO；权限由 Gin 中间件在进入业务 Handler 前实时检查。详见 [架构文档](docs/ARCHITECTURE.md)、[API 文档](docs/API.md) 与[大文件上传方案](docs/LARGE_FILE_UPLOAD_DESIGN.md)。

## 本地运行

1. 复制 `.env.example` 为 `.env`，调整数据库、Redis、首位管理员、SMTP 和 MinIO 配置。
2. 启动 PostgreSQL 与 Redis。
3. 执行 `go run ./cmd/api`。
4. 检查 `GET /healthz` 与 `GET /readyz`。

服务启动时自动应用嵌入式数据库迁移。如果库内没有管理员，会使用 `INITIAL_ADMIN_EMAIL` 与 `INITIAL_ADMIN_PASSWORD` 初始化；库内已有管理员时不会覆盖账号。

## Docker Compose

将 `.env.example` 复制为 `.env` 并设置强密码、两个 HMAC 密钥、邮件及 MinIO 参数。Compose 会启动 PostgreSQL、Redis、MinIO 和后端。PostgreSQL、Redis、MinIO 数据分别绑定到 Compose 文件目录下的 `data/postgres`、`data/redis`、`data/minio`；例如部署目录为 `/application/stub` 时，数据位于 `/application/stub/data/`，迁移时需连同该目录一起备份。MinIO 控制台只绑定宿主机 `127.0.0.1`，建议通过 SSH 隧道管理。MinIO root 凭据只供 MinIO 容器使用；启动后需创建业务桶和独立的后端应用用户，并将该用户凭据填入 `MINIO_ACCESS_KEY`、`MINIO_SECRET_KEY`，不要让后端使用 root 凭据。反向代理需加入外部 `web_net` 和 `minio_net`，部署前须确认两个网络已存在。

```sh
docker compose --env-file .env up -d --build
```

后端不发布宿主机端口，由 Nginx 通过 `web_net` 反向代理到 `short-links-back:8080`。MinIO S3 API 通过 `minio_net` 提供给后端及 Nginx；控制台端口默认仅监听宿主机 `127.0.0.1:9001`。普通上传及分片上传都经过后端，分片请求由后端流式写入 MinIO 内网；后端 `/tmp` 已映射到宿主机专用 `./data/upload-tmp` 目录，部署前先执行 `mkdir -p data/upload-tmp && chown 10001:10001 data/upload-tmp`，并按并发量调整容量。分片接口不应把整个大文件一次性读入内存。

## GitHub Actions 与镜像部署

仓库根目录的 [`.github/workflows/backend.yml`](.github/workflows/backend.yml) 会在 PR/推送上执行格式检查、`go vet`、单元测试和容器构建。默认分支推送及 `v*` 标签通过检查后，会发布 `linux/amd64`、`linux/arm64` 镜像到 `ghcr.io/<GitHub组织或用户名>/short-links-back`；PR 只构建、不推送镜像。手动运行工作流时，需显式勾选 `publish_image` 才会发布。

生产部署时，在服务器的 `.env` 中将 `SHORT_LINKS_IMAGE` 设置为 `ghcr.io/<GitHub组织或用户名>/short-links-back:latest`，然后拉取并启动已发布镜像：

```sh
docker compose --env-file .env pull
docker compose --env-file .env up -d --no-build
```

首次发布后，需在 GitHub 的 Container package 设置中将镜像设为公开；若保持私有，部署服务器必须先使用具有 `read:packages` 权限的凭据登录 GHCR。其他服务仍需按前文加入 `web_net` 和 `minio_net` 网络。

首期无需 Kafka：数据库事务承载需要强一致的账号、短链、额度和访问记录；Redis 已覆盖当前短期限流场景；MinIO 负责大对象。只有后续出现独立异步消费、削峰或跨服务事件订阅需求并达到实际吞吐量时，再引入消息队列。

## 主要接口域

详见 [`docs/API.md`](docs/API.md)，包括各接口的请求与响应 DTO、权限和状态变化。

- `/api/v1/auth/*`、`/api/v1/me/*`、`/api/v1/invites`：账号与会话。
- `/api/v1/links`、`/api/v1/rule-templates`：短链与规则模板。
- `/api/v1/assets`：MinIO 素材。
- `/api/v1/logs/*`、`/api/v1/analytics/*`：日志与访问统计。
- `/api/v1/admin/*`：管理员设置、用户、额度、权限、业务和安全事件。
- `/r/{code}`：匿名浏览器短链入口；规则通过后返回 HTTP 302。

Compose 使用三个独立网络：数据库网络仅限后端、PostgreSQL 与 Redis；`web_net` 供反向代理访问后端；`minio_net` 供后端、Nginx 与 MinIO 通信。部署前应确保 `web_net`、`minio_net` 两个外部网络存在。
