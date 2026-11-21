# 第二阶段 2.2 交付与验收

验收日期：2026-11-21。范围为 Web 和 App 站内通知、到期与次数提醒、短链定时开放窗口。验收未部署或执行生产迁移。功能分支推送不代表上线。

## 已实现

- **开放窗口**：创建、复制和规则编辑支持 `activeFrom`、`activeUntil`。开始时刻包含，结束时刻不包含；实际截止取开放结束和业务到期的较早值。规则 PATCH 省略保持、`null` 清除、时间值替换。两端按本地时区输入，后端按 UTC 和微秒精度返回。续期不修改窗口，清除窗口不移除其它访问限制。
- **公开访问**：GET 及口令 POST 在处理口令前检查短码和可用性。在准备好跳转地址以后，扣次前再检查窗口；拒绝不扣次。可用性统一用于列表、详情和短链 CSV 筛选，CSV 列结构未变。已签发的素材下载地址仍按其独立的签名期限有效。
- **个人收件箱**：两端显示未读数，支持分页、仅未读、单条或全部已读和个人提醒偏好。通知按稳定 `linkId` 打开详情，不依赖当前分页或现有短码。仅人类会话可调用；管理员仍只访问自己的通知。前台才轮询未读数；隐藏取消轮询请求，恢复立即刷新，切换账号隔离旧响应。
- **提醒生成**：只为启用短链的创建者生成。到期提醒在实际截止前 24 小时内可生成，按实际截止时间去重；有限次数达到向上取整的 80% 时按计数周期提醒，`-1` 和 `0` 不提醒。重置次数或改变上限开启新周期。关闭偏好保留历史，重新开启只补当前仍符合条件的事件。提醒不保存目标地址、口令或签名链接。
- **扫描边界**：启动补扫，随后每分钟扫描，单轮超时 30 秒、最多检查 200 个候选并写入 100 条。优先看较近的到期事件，跳过竞争中的候选后在预算内继续。锁后取当前时间并重读短链和偏好，唯一约束防重复。积压、停机或锁竞争时不承诺在截止前送达；短链解析不等待扫描结果。

接口见 [后端 API](API.md#站内通知与提醒)。不包含邮件、系统推送、异常访问告警、Webhook 或多人工作空间。

## 验证结果

| 项目 | 结果 |
| --- | --- |
| Go 全仓 `test` 与 `vet` | 通过。包含精确起止、时区偏移、口令前置检查、签名跨过截止、PATCH 三态、次数周期和 CSV 状态筛选；收件箱 HTTP 测试覆盖本人隔离、有效 API Key 拒绝、已读幂等和偏好部分更新。扫描测试验证锁后复核、跳过被锁候选后继续，以及启动扫描和取消退出。 |
| Web/App TypeScript 与生产模式构建 | 均通过。仅 Vite 大 JS chunk 提示，Web 约 1138 kB，App 约 671 kB。构建配置使用本地预览地址，没有部署。 |
| Node 请求边界 | 59 项通过（33 项新增、26 项已有）。覆盖时间窗本地/UTC 转换、夏令时跳时、微秒顺序/截止、过往窗口复制、显式清空、旧会话 401、前后台轮询及迟到响应。轮询测试使用受控时间与请求替身，不等同原生生命周期测试。 |
| 三仓 `git diff --check` | 通过，仅 Git LF/CRLF 提示；Web 素材页原有修改保持，文件 hash 为 `ed1d8728440cb8fa012e5cbd58a8d30b2c790efe`。 |
| 真实 PostgreSQL、Redis、原生 App | 无专用 `TEST_DATABASE_URL`，真实迁移与并发用例跳过。未运行真实 Redis、Android/iOS 真机或原生同步。 |

### 浏览器与请求日志

使用两个独立的 127.0.0.1 内存 API。使用本地合成账号，未连接业务服务。GUI 使用稳定构建，核对页面显示和脱敏请求日志：

| 场景 | 实际观察 |
| --- | --- |
| Web 通知 | 铃铛初始 2 条未读。查看通知请求返回 204，随后按稳定 ID 请求列表第 21 条短链，详情显示正确；列表第一页 20 条不含该链。全部已读标记余下 1 条，未读数归零。 |
| Web 提醒设置 | 只关闭到期提醒，PATCH 仅发送 `{"expiryEnabled":false}`，响应中 `visitsEnabled` 保持为 `true`。 |
| Web 窗口 | 列表显示「尚未开放」和「开放结束」。清空原定时短链的起止后保存，PATCH 只传两个 `null`，原到期时间保留，当前状态更新为可用。 |
| App 通知（390 CSS px） | 在统计页打开通知底部面板，跳回短链 Tab 并打开第 21 条短链详情，未读数由 2 变 1。全部已读标记余下 1 条，切换仅未读后显示「暂无未读通知」。 |
| App 提醒设置（390 CSS px） | 只关闭次数提醒，PATCH 仅发送 `{"visitsEnabled":false}`，响应中 `expiryEnabled` 保持为 `true`。 |
| App 窗口（390 CSS px） | 「开放结束」筛选发送 `availability=window_ended`，返回 1 条。保存清除窗口的 PATCH 只传两个 `null`，原到期不变，短链退出筛选结果。 |

未穷举两端创建、复制、分页、重试和窄屏组合。创建和复制的边界有自动回归，浏览器验收的范围以上表为准。

## 复验与发布顺序

本项目由三个独立仓库组成。将本仓库、[Web](https://github.com/yourGaomu/Stub_front/tree/feat/short-links-management) 和 [App](https://github.com/yourGaomu/Stub_app/tree/feat/short-links-management) 的同名功能分支检出为同级 `back/`、`front/`、`app/`，分别安装两端现有 npm 依赖。Node 回归需要 Node 24。从三个仓库的共同父目录运行：

```sh
TEST_DATABASE_URL= go -C back test -count=1 ./...
go -C back vet ./...
node --test back/docs/tests/*.test.mjs
npm --prefix front run typecheck
VITE_API_BASE_URL=http://127.0.0.1:43122 VITE_PUBLIC_BASE_URL=http://127.0.0.1:43122 VITE_API_PROXY_TARGET=http://127.0.0.1:43122 npm --prefix front run build
VITE_PUBLIC_BASE_URL=http://127.0.0.1:43123 VITE_API_PROXY_TARGET=http://127.0.0.1:43123 npm --prefix app run build
node back/docs/tests/phase2-2-preview.mjs
```

最后三个命令中的地址只用于本地复验，不能用于发布包。预览脚本注释列出本地账号、测试数据和预期请求；关闭进程后所有测试写入消失。真实 PostgreSQL 用例只接受显式专用 `TEST_DATABASE_URL`，在专用测试库内使用独立 schema，勿指向生产库。

发布前先在专用 PostgreSQL 验证 `00006_link_windows_notifications.sql` 和 `00007_notification_tables.sql` 的迁移、窗口约束及双实例提醒生成，再判定是否发布后端，随后更新 Web 和 App。生产回退优先回退应用版本并保留表数据；直接执行 Down 会删除窗口字段或收件箱数据。原生 App 仍需确认前后台切换、面板手势和系统时区。
