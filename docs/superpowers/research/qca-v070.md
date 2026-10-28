# Qoder Cloud Agents（QCA）代码调研 · v0.7.0

权威版本：tag `v0.7.0`（dbdbb2991）。除另注外，行号均对应 `git show v0.7.0:<path>` 的内容。
main（HEAD 7fe2b849d）与 tag 的 qoder 差异见文末「main 相对 v0.7.0」。

## TL;DR

1. QCA 不是新执行引擎，而是把**既有 daemon 任务协议**托管给一个「Qoder 托管会话 API 客户端」：桥不跑本地 CLI、不 checkout 仓库、不执行 shell（`server/cmd/qoder-runtime/README.md:5-8`）。
2. 两种形态共用同一份 `internal/qoderruntime`：standalone `cmd/qoder-runtime`（`state.json` + `flock`）与 managed（server 内 goroutine + 加密 journal 存 PG）。差别只在 `Config.LoadState/SaveState/BeforeStep/OnReady` 注入（`qoderruntime/bridge.go:23-44,115-203`）。
3. 注册协议复用 `/api/daemon/register`，桥自报 `type=qoder_cloud` + `runtime_mode=cloud`；`runtime_mode` 字段是既有的（`handler/daemon.go:318-329`），本地 daemon 从不发送，缺省归一为 `local`（`daemon.go:574-583`）。
4. 服务端所有特判都锚定 `daemon_id` 前缀 `qoder-managed-`（`daemon.go:135-139`）与 provider 字符串 `qoder_cloud`（`qoderruntime/bridge.go:21`）。
5. managed 用 `pg_try_advisory_lock(71243,8191)` 单主（`qoder_connection.go:387-393`），每连接一个 bridge goroutine，`revision` 变即重启（`:433-523`）。
6. 凭证与 journal 同一把 AES-256-GCM 密钥（`MULTICA_VCS_SECRET_KEY`）加密落 `cloud_agent_connection`（`util/secretbox/secretbox.go:6-14`、`handler/vcs.go:83-104`）。
7. 能力面很窄：仅 issue 执行；无 chat/附件/shell/文件传输/任意 Git host/标签或 commit checkout/计费（`README.md:137-154`）。
8. 可复用层（连接表、选主、租约、凭证链、幂等、事件轮询、issue 状态机）远大于专有层（URL、session 语义、event schema、`end_turn`）。

## 架构图

```
                        ┌──────────────────────────── Multica server ─────────────────────────────┐
 web / desktop ──HTTP──▶│ /api/workspaces/{id}/qoder[...]  (admin)  ──▶ handler/qoder_connection.go│
 (packages/views)       │                                              ──▶ cloud_agent_connection │
                        │                                                     (AES-256-GCM)        │
                        │ RunQoderConnections ──advisory lock(71243,8191)──▶ supervisor loop      │
                        │        └─ 每连接: qoderruntime.Bridge.Run(goroutine)                       │
                        │              Config{LoadState/SaveState/BeforeStep/OnReady} = PG          │
                        └───────────────┬───────────────────────────────▲──────────────────────────┘
                       daemon API (mdt_)│                               │ 任务/结果
                                        ▼                               │
                        ┌────────── qoderruntime.Bridge ──────────┐     │
 standalone 进程亦可 ──▶│ register→claim→session→events→complete    │─────┘
 (cmd/qoder-runtime)    │ journal: PG(加密) 或 state.json+flock     │
                        └───────────────┬──────────────────────────┘
                                        │ Bearer Qoder PAT
                                        ▼
                        Qoder Cloud API  /sessions /environments /agents
                        (https://api.qoder.com[.cn]/api/v1/cloud)
```

进程/组件：① Qoder 云端（会话执行方）；② 桥进程（standalone 或 server 内 goroutine）；③ Multica server（任务队列、daemon API、连接管理、supervisor）；④ Web/Desktop（只经 `packages/core/api` 调 workspace 端点，无 qoder 专有接线）。

## 数据流（逐跳）

1. **建任务**：agent 绑定 `runtime_id`（provider `qoder_cloud`）后，issue 指派/评论触发创建 `agent_task_queue` 行；server 侧无 cloud 专有派发分支，等桥来 claim。
2. **claim**：桥 `POST /api/daemon/runtimes/{runtimeID}/tasks/claim`（`bridge.go:288`）。`DaemonAuth` 认 `mdt_`，命中 `qoder-managed-` 时额外过 `qoderCredentialAllowed`（`daemon.go:67-77`）。响应含 `auth_token`（`mat_`，24h，`daemon.go:4301-4326`）、issue 上下文、agent 指令、`repos`、`prior_session_id`（来自通用 `GetLastTaskSession`，同 runtime 才给，`daemon.go:3434-3447`）。
3. **prepare**：校验身份/仅 issue/无 MCP 与 custom 覆盖/必须绑 `qoder_agent_id`（`prompt.go:13-37`）；用任务级 `mat_` 读 `GET /api/issues/{id}`（`prompt.go:38-47`，`bridge.go` 的 `Check` 亦跳过 mdt_ 的 workspace 探测 `:619`）；组 prompt、解析仓库资源、做 `resourceFingerprint`；journal 写 `prepared`（`bridge.go:284-304`）。
4. **start**：`POST /api/daemon/tasks/{id}/start` → server 事务内 `syncQoderIssue` 把 `todo/backlog` 推到 `in_progress` 并记 revision 标记（`service/qoder_issue_lifecycle.go:23-76`，`service/task.go:4367`）。
5. **建会话**：`POST {qoder}/sessions`，body `{agent, environment_id, title, metadata{multica_task_id,multica_workspace_id,multica_resources=指纹}, resources[]}`（`bridge.go:438-464`）。先置 `creating` 再发；响应丢失**不重试**，直接失败（`:358-362`）。
6. **续跑判定**：若 `PriorSessionID` 非空且 session `status=idle`、无其他 run 占用、agent/environment 一致、资源指纹相同，则复用，否则新建（`bridge.go:502-527`）。
7. **pin session**：`POST /api/daemon/tasks/{id}/session`（`bridge.go:398`）。
8. **发消息**：先把 `phase=sending` + `submission_deadline` 落盘，再 `POST {qoder}/sessions/{sid}/events` 一条 `user.message`（`bridge.go:401-427`）。
9. **轮询事件**：每 3s（`bridge.go:73-75`）`GET {qoder}/sessions/{sid}/events?limit=100&order=asc&after_id=<cursor>`（或 `page=`）（`events.go:42-60`）；事件 ID 与不透明 `page` 游标分开跟踪（`README.md:108-111`）。
10. **回写**：`agent.message`→text、`agent.tool_use/result`→tool_use/result、`session.error`→error；每条 `POST /api/daemon/tasks/{id}/messages`，带 `idempotency_key="qoder:"+eventID`（`events.go:78-123`）。服务端以 `uuid.NewSHA1(taskID, key)` 定 UUID 去重（`daemon.go:5618-5629`）。
11. **结束**：仅 `session.status_idle` 且 `stop_reason.type=="end_turn"` 且无未恢复 error 才算 completed（`events.go:101-110`）；`status_terminated/deleted` 或其它 stop reason → failed。交付 `POST /tasks/{id}/complete`（`session_id`,`output`）或 `/fail`（`error`,`failure_reason="qoder_cloud_execution"`）（`bridge.go:578-593`）。
12. **取消/回收**：轮询 `GET /tasks/{id}/status`；变 `cancelled/failed/completed` → 入清理队列 → `POST {qoder}/sessions/{sid}/cancel` → 轮询 session 直到 `idle/terminated`（`bridge.go:306-351,543-577`）；cancelled 再回 `/cancel-ack`。
13. **issue 落幕**：complete 时 `completing=true`，仅当 `context.qoder_issue_revision` 等于当前 issue revision 且状态仍 `in_progress` 才推进 `in_review`（`qoder_issue_lifecycle.go:35-45`；SQL `pkg/db/queries/qoder_issue_lifecycle.sql:1-12`），随后广播状态与 activity（`:78-84`）。
14. **连接退役**（stop/管理员降级/删除 workspace）：`cleanupInactiveQoder` → `FailTask("Qoder connection stopped")` + `CancelRemoteSession`（只需 Qoder 凭证，不需前 owner PAT）→ 清 `journal_encrypted`（`handler/qoder_cleanup.go:16-90`、`bridge.go:684-691`）。

**端点清单 — Multica 侧**（`cmd/server/router.go:1562-1617`）：`POST /api/daemon/register`、`/heartbeat`、`/runtimes/{id}/tasks/claim`、`/tasks/{id}/{status,start,session,messages,cancel-ack,complete,fail}`。
**端点清单 — workspace 侧**（`router.go:1722-1723` member、`:1809-1814` owner/admin）：`GET /qoder`、`GET /qoder/agents`、`PUT /qoder`、`POST /qoder/{check,environments,stop,resume}`、`DELETE /qoder`。
**端点清单 — Qoder 侧**：`POST /sessions`、`GET /sessions/{id}`、`POST|GET /sessions/{id}/events`、`POST /sessions/{id}/cancel`、`GET /environments/{id}`、`GET /{agents|environments}?limit&after_id`（`bridge.go:447,514,556,566,610-632`；`events.go:54`；`qoder_catalog.go:54`）。
**持久化**：表 `cloud_agent_connection`（config/journal 加密、enabled、revision、status、last_error、busy_until）、`daemon_token`、`agent_runtime`（provider/daemon_id/runtime_mode）、`agent_task_queue`（`session_id`、`context.qoder_issue_revision`）、task messages；standalone 另有 `state.json`+`bridge.lock`。

## 文件清单

| 文件 | 行 | 职责 |
| --- | --- | --- |
| `server/internal/qoderruntime/bridge.go` | 691 | **桥核心**：Run/注册/step/claim/session 创建与续跑/取消/交付/Check/Binding |
| `server/internal/qoderruntime/state.go` | 151 | journal 结构与原子写、binding 校验、phase 合法性、LoadState/SaveState 抽象 |
| `server/internal/qoderruntime/events.go` | 147 | Qoder event schema、游标分页、事件→Multica message 映射与 `end_turn` 判定 |
| `server/internal/qoderruntime/prompt.go` | 195 | claim→prompt、仓库资源白名单、`agent.runtime_config.qoder_agent_id` 解析、分支名 |
| `server/internal/qoderruntime/credentials.go` | 89 | GitHub repo key 规范化、0600 token 文件读取、临时请求副本注入（不入 journal） |
| `server/internal/qoderruntime/http.go` | 82 | 双 base URL 校验（https，loopback 例外）、Bearer、禁跟随重定向、错误脱敏、8MiB 上限 |
| `server/internal/qoderruntime/claim.go` | 59 | claim HTTP 载荷的只读结构，刻意不依赖本地 daemon 实现 |
| `server/internal/qoderruntime/lock_{unix,windows}.go` | 30/22 | 目录互斥（flock）/目录 fsync |
| `server/cmd/qoder-runtime/main.go` | 82 | standalone 入口：config 文件+env 覆盖、`--check`、信号处理 |
| `server/cmd/qoder-runtime/README.md` | 285 | 部署/边界/迁移/运维的权威文档 |
| `server/internal/handler/qoder_connection.go` | 639 | **managed 主体**：settings 加解密、save/resume/stop、**supervisor**、step 租约、凭证校验 |
| `server/internal/handler/qoder_catalog.go` | 253 | 官方区域白名单（防 SSRF）、environments/agents 目录、per-agent 覆盖设置拒绝 |
| `server/internal/handler/qoder_disconnect.go` | 240 | 断开：先停桥→取消远端→删 runtime/连接→解绑 agent |
| `server/internal/handler/qoder_cleanup.go` | 116 | 退役连接的远端取消与 journal 清理、workspace 删除前置 |
| `server/internal/service/qoder_issue_lifecycle.go` | 84 | issue 状态机（in_progress/in_review + revision 防护） |
| `server/pkg/db/queries/qoder_issue_lifecycle.sql` | 17 | `LockQoderTaskIssue`（provider=’qoder_cloud’ 且无更新任务）、revision 写入 |
| `server/internal/handler/daemon.go`（改动点） | 6601 | 注册接受 `runtime_mode=cloud`、`qoder-managed-` 命名空间门禁、owner 绑定豁免 |
| `server/migrations/586_cloud_agent_connection.*` | 16 | 连接表（无 FK，按仓库规则） |
| `server/migrations/587_..._provider_index.*` | 1 | `(workspace_id, provider)` 唯一索引（CONCURRENTLY） |
| `server/migrations/588_daemon_token_owner.up.sql` | 14 | 非 QCA daemon token 绑 owner；QCA token 有意保持 NULL |
| `server/migrations/134/242_...qoder*` | 23/27 | **本地 Qoder CLI** 的 `protocol_family` 白名单，与 QCA 无关 |
| `server/pkg/agent/qoder.go` | 455 | **本地 Qoder CLI（ACP）agent**，与 QCA 无关（同名前缀易混） |
| `packages/core/runtimes/qoder.ts` | 104 | query/mutation 封装（5s 轮询连接状态） |
| `packages/core/runtimes/qoder-schema.ts` | 97 | zod 解析与默认值（available/configured 等）、区域与上限常量 |
| `packages/core/agents/qoder-runtime-config.ts` | 20 | 从 `runtime_config` 读 `qoder_agent_id` / 默认仓库 |
| `packages/views/runtimes/components/qoder-detail-page.tsx` | 579 | 运行时详情：Pause/Resume/Disconnect、agent 列表 |
| `packages/views/runtimes/components/qoder-connection-cards.tsx` | 1028 | 连接/仓库/限额三张卡，掩码与「空值保留旧值」交互 |
| `packages/views/runtimes/components/qoder-connect-form.tsx` | 488 | 区域切换、PAT 校验、环境加载 |
| `packages/views/runtimes/components/qoder-configuration.tsx` | 52 | 对话框壳；`available=false` 时显示服务端配置提示 |
| `packages/views/runtimes/components/qoder-status-badge.tsx` | 36 | 状态徽章 |
| `packages/views/agents/components/qoder-agent-select.tsx` | 89 | agent 表单里的 QCA Agent 选择 |
| `packages/views/agents/components/tabs/qoder-agent-tab.tsx` | 110 | agent 详情里的绑定/默认仓库页签 |
| `packages/views/runtimes/components/runtimes-page.tsx`（改动点） | 1290 | 云服务行、从机器列表剔除 qoder runtime（`:241-306,927-1044`） |
| `packages/core/api/client.ts`（改动点） | 5433 | 6 个 workspace qoder 端点封装（`:2021-2050`） |

**通用基础设施（provider 无关）**：daemon 注册/心跳/claim/status/complete/fail/messages/cancel-ack 全套 handler；`daemonAuth`→`daemonRuntimeBindingAllowed`；`agent_task_queue`+`prior_session_id` 续跑；`task messages` 幂等去重；`secretbox`；`DaemonTokenCache`；`TeardownRuntime`；advisory lock 惯例。
**Qoder 专有**：`qoderruntime/*` 全部、`handler/qoder_*.go`、`service/qoder_issue_lifecycle.go`、`packages/*/qoder*`、`586/587/588` 中的 provider 取值。

## 凭证与安全

- **加密**：AES-256-GCM，12B 随机 nonce 前置，无 key-id/AAD，**不支持轮换**（`util/secretbox/secretbox.go:6-14,67-88`）。密钥 `MULTICA_VCS_SECRET_KEY`（base64 32B）在 router 装配；缺失则 `VCSSecretBox=nil`，QCA 端点返回 503 / `available=false`（`router.go:1303-1317`、`qoder_connection.go:92-96,131-134`）。
- **落库内容**：`config_encrypted` = 名称/区域/环境/Qoder PAT/GitHub map/桥用 `mdt_`；`journal_encrypted` = 同一把 key（`qoder_connection.go:26-35,347-356`、`:489-507`）。响应只回 `has_token`、仓库名与计数（`:54-69,112-125`）；空 token/空 GitHub token 保留旧值，删仓库即删凭证（`:182-211`）。
- **桥内部 token**：`auth.GenerateDaemonToken()` = `mdt_`+40hex（`auth/jwt.go:69-76`），1h 过期；supervisor 仅在 `enabled && owner 仍为 owner/admin` 且剩余 <30min 时续期（`qoder_connection.go:421`）；save/stop 先删 token 行再 `DaemonTokenCache.Invalidate`（`:354,362,571-581`）；降权/停用时无条件删除并失效（`:422-432`）。注意：`auth/cloud_pat.go` 的 `mcn_` 是 Cloud Fleet 节点 PAT，**与 QCA 无关**。
- **任务级 token**：claim 发 `mat_`（24h，绑 task/agent），桥用它读 issue（`daemon.go:4301-4326`、`prompt.go:38-39`）。
- **授权门禁**：`qoderCredentialAllowed` 要求 连接启用 + `daemon_token.daemon_id/hash/未过期` + owner 为 owner/admin（`qoder_connection.go:593-608`）；注册额外要求「单个 `qoder_cloud` runtime、无 profile」（`daemon.go:600-620`），并禁止普通成员占用 `qoder-managed-` 命名空间（`:681-687`）。
- **选主/租约**：supervisor 用会话级 advisory lock `(71243,8191)`（`qoder_connection.go:387-393`）；配置写/停用/断开用 `pg_advisory_xact_lock(hashtextextended(ws,19))` 串行化（`:285,545`、`qoder_cleanup.go:98`）；远端步骤用 `busy_until=now()+45s` 租约，远端 HTTP 在数据库事务之外执行（`:610-638`、`README.md:213-214`）。
- **journal**：standalone 明文 JSON，0700 目录、原子 rename+fsync、`flock` 独占、binding 不匹配即拒（`state.go:12-22,46-71,90-92`、`lock_unix.go:11-20`）；managed 加密在 PG，因此 API 实例无需共享盘（`README.md:209-214`）。journal **不含 API 凭证**（`state.go:12-14`）。
- **撤销/停用语义**：Pause(stop)=保留 QCA/GitHub 凭证、删桥 token、`enabled=false,status=stopped`、拒绝 active 任务（`qoder_connection.go:532-583`）；Disconnect=删连接+runtime，agent 解绑保留历史（`qoder_disconnect.go:25-118`）；workspace 删除先停用并清远端（`qoder_cleanup.go:92-116`）。
- **SSRF/输入**：托管只允许两个官方区域 URL（`qoder_catalog.go:20-34`）；禁重定向、非 loopback 必须 https（`qoderruntime/http.go:24-35`）；错误不回显 response body/URL（`:38,63`）；目录 6 次/分/workspace、最多 10 页（`router.go:520`、`qoder_catalog.go:47-49`）；请求体 64KiB 且拒绝未知字段（`qoder_connection.go:144-154`）。

## 能力边界（出处）

| 不支持 | 出处 |
| --- | --- |
| shell/命令执行、本地 CLI、仓库 checkout | `README.md:5-8` |
| chat、squad、本地目录 | `README.md:137-145`；`prompt.go:18-19`（chat session/leader task 直接拒）；`prompt.go:74-79`（`local_directory`） |
| 附件 | `prompt.go:48-50` |
| 任意 Git host / 非 HTTPS / 带凭据 URL | `prompt.go:145-149`；`credentials.go:11-23` |
| tag/commit checkout（只支持分支） | `prompt.go:151-153` |
| 一个 run 多仓库 | `prompt.go:90-92` |
| Multica 自定义 skills/custom_env/custom_args/MCP/model/thinking/service_tier | `prompt.go:24-32`；保存时即拒（`qoder_catalog.go:199-214`、`agent.go:1474-1485`） |
| 连接应用/Remote MCP/plugin hook 工具 | `prompt.go:24-26` |
| 交互式工具审批（`requires_action` → 直接 failed） | `README.md:142-145`；`events.go:101-110` |
| 文件传输、产物回传（只回文本与 PR 链接） | `README.md:147-150` |
| 计费/用量换算（桥从不上报 usage） | `README.md:142`；`qoderruntime` 中无 usage 代码 |
| chat / quick-create（「先建 issue 再指派」） | `handler/issue.go:2990-2995` |
| 无 cloud-node 生命周期/更新 API | `README.md:152-154` |
| 完整评论历史 hydration、结构化 PR 字段 | `README.md:137-150` |

## 测试与可验证性

- **桥单测（不接真实 Qoder）**：`bridge_test.go:34-149` 用两个 `httptest` server 分别模拟 Multica 与 Qoder，断言 `X-Workspace-ID`、Bearer 作用域（issue 读用 `mat_`）、不发送 `X-Client-Capabilities`；覆盖 issue run、提交结果不明重放、取消、`requires_action`、目录锁、重定向拒绝、游标、重启后终态上报、限额/超时、managed 无本地盘 journal（`bridge_test.go` 23 个 Test）。
- **并发/续跑**：`concurrency_test.go:17-156` 的 `multiHarness`，覆盖并发上限、idle 续跑、换仓库/换凭证不续、指纹缺失不续、分支名（11 个 Test）。
- **凭证**：`credentials_test.go` 断言 token 只出现在「请求副本」、journal 不含 token、文件 0600 校验。
- **CLI 配置**：`cmd/qoder-runtime/main_test.go` 覆盖 env/文件优先级与未知字段拒绝。
- **可选真机集成**：`local_integration_test.go:1` `//go:build integration` + `MULTICA_QODER_INTEGRATION_CONFIG`；要求 loopback Multica 与测试 workspace，Qoder 仍是 in-process fixture，覆盖「丢失 transcript 响应后只落一条输出」并清理现场（`README.md:167-178`）。
- **handler 侧（DB 支撑）**：`qoder_connection_test.go`（密钥掩码、限额、paused 保持、managed 注册后撤销、退役取消并清 prompt、租约不占连接、quick-create 拒绝）；`qoder_disconnect_test.go`；`qoder_catalog_test.go`（分页/脱敏/错误脱敏/覆盖拒绝）；`qoder_agent_settings_test.go`；`qoder_issue_lifecycle_test.go`。
- **前端**：`packages/core/runtimes/qoder-schema.test.ts`、`views/runtimes/components/qoder-{configuration,detail-page}.test.tsx`、`views/agents/components/{agent-detail-inspector, tabs/qoder-agent-tab}.test.*`、`runtimes-page.test.tsx`（云服务行、不从机器列表出现）。
- **注意**：`server/pkg/agent/qoder_test.go`、`server/internal/daemon/agents_probe_qoder_test.go` 测的是**本地 Qoder CLI**，不是 QCA。

## provider-agnostic 边界清单（再做一个非 Qoder 云 provider 时）

**可直接复用（无需改代码，或仅加 provider 常量）**

1. `cloud_agent_connection` 表结构与 `(workspace_id, provider)` 唯一索引（586/587）——provider 列已经是泛化设计。
2. `secretbox` + `sealVCSSecret/openVCSSecret`（加密、掩码、空值保留语义）。
3. supervisor 骨架：advisory lock 选主、`revision` 驱动重启、失败指数退避、`busy_until` 步租约、5s 循环、owner/role 复核（`qoder_connection.go:366-530,610-638`）。
4. daemon token 全链：`mdt_` 铸造/续期/撤销/缓存失效、`<provider-ns>-` 命名空间豁免惯例、`qoderCredentialAllowed` 的等价校验、`daemon.go` 注册门禁结构。
5. daemon 协议：claim→status→start→session→messages→complete/fail→cancel-ack；`prior_session_id` 续跑；`mat_` 任务 token；messages 幂等（`uuid.NewSHA1`）。
6. journal 抽象：`LoadState/SaveState/BeforeStep/OnReady` + 磁盘/DB 两种后端、原子写、binding、phase 校验、cleanup 队列。
7. issue 状态机骨架与 revision 防护（`syncQoderIssue` 只差 SQL 里的 provider 字面量，`qoder_issue_lifecycle.sql:6`）。
8. workspace 删除/停止/断开的编排顺序（先停→取消→删→解绑）。
9. 确认性/幂等原则：发送意图先落盘、模糊响应不重发、创建响应丢失不重建。

**必须重写（Qoder 专有）**

1. Base URL 白名单与 region 语义（`qoder_catalog.go:20-34`、`qoder-schema.ts:11-14`）。
2. `{qoder_agent_id, repository}` 的 `runtime_config` 结构与 QCA Agent 目录 API（`prompt.go:123-141`）。
3. 会话语义：`POST /sessions` 的资源/`metadata.multica_resources` 指纹、idle 续跑条件、cancel 后等 `idle|terminated`（`bridge.go:438-527,555-577`）。
4. 事件 schema 与推断：`agent.message/tool_use/mcp_tool_*`、`session.error.retry_status`、`session.status_idle.stop_reason=end_turn`（`events.go:12-31,78-115`）；分页字段 `data/has_more/next_page/after_id`。
5. 目录分页字段与 ID 前缀校验（`env_`/`agent_`，`last_id`，`qoder_catalog.go:67-107`）。
6. prompt 文本、「请推 `multica/<id>-<slug>` 分支并前缀 PR 标题」的交付约定（`prompt.go:52-60,164-179`）。
7. 凭据注入形态（`authorization_token` 字段 + GitHub 仓库绑定）与「仅 github.com」限制（`credentials.go`、`prompt.go:145-149`）。
8. 前端 Qoder 文案/交互与 `provider="qoder_cloud"` 的 UI 分支（`runtimes-page.tsx`、`runtime-settings-page.tsx:63`、`provider-logo.tsx:427-430`）。

## 未解问题 / 需要 Lead 注意

1. **README 迁移号与 tag 不符**：README 说连接迁移是 `573/574`、生命周期是 `575_qoder_lifecycle`（`README.md:253-266,282-285`），但 tag 上实际是 `586/587/588`，且**不存在** `575_qoder_lifecycle` 文件；`cmd/migrate/main.go:190` 只登记了 587 索引。升级旧 PoC 库的说明在当前 tag 上无法照做。
2. **README 自相矛盾**：`:103-106` 说 follow-up 会续用 idle Qoder session，`:220` 却写「does not add … session continuation」；代码实现的是前者（`bridge.go:502-527`）。`:220` 应视为过期表述。
3. **Qoder 侧 API 契约无仓内依据**：字段与语义全从代码反推（`events.go`、`bridge.go`），无 vendored 文档/契约测试；真实 provider 变更无法在 CI 中提前发现。
4. **`cloud_pat` 与 QCA 无关**：若上下文把 `auth/cloud_pat.go`（`mcn_`，Cloud Fleet 节点 PAT）当成 QCA 凭证链，属于误判；QCA 用的是 `mdt_` + `mat_`。
5. **managed 的启动校验较弱**：`Check()` 对 `mdt_` token 跳过 Multica workspace 探测（`bridge.go:619`），managed 首次启动只靠 `register` + `OnReady` 证明连通；是否有意为之未在代码注释说明。
6. **密钥不可轮换**：secretbox 无 key-id/AAD（`secretbox.go:11-14`）；换 key 等于同时丢失连接凭证与未完成 journal。文档只说「保持稳定」。
7. **managed 自连 loopback**：supervisor 传 `"http://localhost:"+port`（`main.go:754`），依赖同进程端口可被自身访问；在仅 TLS/反代部署下是否成立未验证。
8. **未覆盖**：provider 侧限流退避细节（仅 429→30s）、`fail` 回调长期失败时 journal 的膨胀上限，代码中无界。

## main 相对 v0.7.0 的差异（`git diff --stat v0.7.0..HEAD`，9 文件 +94/−9）

- 无 `server/internal/qoderruntime`、`handler/qoder_*`、`service/qoder_issue_lifecycle.go`、迁移的改动——后端 QCA 实现与 tag 一致。
- 语义修正：`packages/views/agents/create/runtime-choice-list.tsx:44` 的 `qoderChoiceState` 增加 `!connection.available → "not_configured"`，`choose-runtime-page.tsx:91` 不再因 `available=false` 隐藏 Qoder；`qoder-configuration.test.tsx` 断言缺 `MULTICA_VCS_SECRET_KEY` 时显示配置提示而非凭据表单；`README.md:192` 同步为「菜单仍可见，打开后提示服务端配置」。
- 其余 diff（agents 列表搜索/model 列、OpenClaw `modelOverride`、zh-Hant locale）与 QCA 无关。
