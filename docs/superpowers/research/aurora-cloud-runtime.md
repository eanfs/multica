# Aurora cloud runtime 调研（`origin/feat/aurora-38-app`）

> 权威版本：`origin/feat/aurora-38-app` @ `91e8cee`（2026-09-23）。除另注外，行号均指 `git show origin/feat/aurora-38-app:<path>`。QCA 侧对应 `main`/`v0.7.0`，其细读见 `docs/superpowers/research/qca-v070.md`。仅源码阅读，未切分支、未跑测试。

## TL;DR

1. `apps/aurora` 完全不接触 cloud runtime：它只是 Next.js 壳 + 三个页面，请求经同源代理打到主 server 的 `/api/aurora/*`（`apps/aurora/proxy.ts:60-72`、`apps/aurora/config/runtime-urls.ts:141-151`）。
2. 所谓 “aurora cloud runtime” 是后端执行基础设施，应拆成四层：(a) `internal/cloudruntime` 通用 Multica-Cloud HTTP 传输；(b) `aurorafleet` Docker 节点控制器；(c) daemon managed 注册 + Aurora 工具面收窄；(d) 通用 `agent_task_queue` 任务链路 + 积分结算。
3. `aurorafleet` 不是新执行引擎，而是 multica-cloud node API 的 self-host 替身（`server/internal/aurorafleet/doc.go:5-14`）：同样的 `/api/v1/nodes` 路径，Docker 容器代替 EC2 实例，容器里跑完整 Multica daemon。
4. 38 上它只是**骨架**：fleet 只注入 `MULTICA_SERVER_URL`/`AURORA_SANDBOX_TOKEN`（`controller.go:14-21,246-253`），fleet API 自身无入站鉴权（`controller.go:58-80`）；自动 provision、沙箱镜像、managed daemon 模式都不在本分支。
5. managed runtime 由 server 预 seed（`provider=aurora_managed, runtime_mode=cloud, daemon_id NULL`，`server/pkg/db/queries/aurora_agents.sql:7-28`），沙箱 daemon 用共享 token 调 `POST /api/daemon/managed/register` 把它翻 online（`server/internal/handler/aurora_runtime.go:31-79`）；claim 还要求 runtime online 且 `last_seen_at` 在 150s 内（`server/pkg/db/queries/agent.sql:777-779`、`server/internal/service/task.go:195`）。
6. 但 38 上**闭环缺失**：daemon 只 claim 自己注册返回的 runtime（`server/internal/daemon/daemon.go:2223-2231`），managed register 返回的 runtime id 没有代码路径进入 claim 集；e2e 测试手工拼接两半（`server/internal/handler/aurora_fleet_e2e_test.go:66-92`）。
7. 执行侧是 claude CLI + 收窄工具面：`MaxTurns=30`、`permission-mode=default`、deny `Bash/WebFetch/WebSearch`（`daemon.go:7673-7678,8688-8699`、`server/internal/daemon/aurora_tool_surface.go:50-56`），非 claude provider fail closed。
8. 产物通道（`aurora_asset` + `POST /api/daemon/tasks/{id}/artifacts`）已接好，但 38 上**没有 producer**，`TaskResult.Artifacts` 永远为空（`server/internal/daemon/types.go:320-326`）。
9. QCA 是 provider 会话桥（远端 session + 事件游标 + journal），不实现节点生命周期，且明确不做 shell/exec/文件传输（`server/cmd/qoder-runtime/README.md:7,137-142`）；它与 aurorafleet 只在「`agent_runtime(runtime_mode=cloud)` + daemon 任务协议」这一层重叠。
10. 结论：**QCA 不能当 Aurora 执行后端**；可复用的是它的 runtime 注册/claim/凭据抽象（`runtime_mode=cloud` 已在 main 的 `/api/daemon/register` 落地，`server/internal/handler/daemon.go:576-580`），不是 QCA 桥本身。

## 1. cloud runtime 到底指哪一层

- **(a) `server/internal/cloudruntime` = 传输/代理层，不是运行时。** 它是一个带超时、指标、`X-User-ID`/`X-Request-ID` 标注的通用 HTTP 客户端（`client.go:32-45,111-177`），主 server 用它代理 `/api/cloud-runtime/*`（`handler/cloud_runtime.go:103-145`）和 `/api/cloud-billing/*`（`handler/cloud_billing.go:117-134`）。SaaS 下 baseURL 指向 multica-cloud，self-host Aurora 下可指向 aurorafleet。它既不属于 Aurora，也不实现节点或任务。
- **(b) `aurorafleet` = self-host 节点宿主控制器。** 暴露 `/api/v1/nodes` 的 create/list/delete/start/stop/reboot/status/exec + `/healthz`/`/readyz`（`controller.go:58-80`），`Backend` 接口抽象 Docker/内存两种实现（`backend.go:38-72`）。它替代的是 multica-cloud 的 Fleet，不是 daemon。
- **(c) managed 注册 + 工具面 = 执行身份与策略层。** 服务端 seed 一行 `aurora_managed` cloud runtime（`aurora/agents.go:49-77`），沙箱 daemon 用 `AURORA_SANDBOX_TOKEN` 激活它（`handler/aurora_runtime.go:31-79`）；daemon 侧识别 `aurora:*` system key，对 claude 施加窄工具面（`daemon/aurora_tool_surface.go:8-57`）。
- **(d) 任务/生成链 = 通用 Multica 执行链。** `CreateAuroraGeneration` 预留积分并把任务入到 system agent 的 `agent_task_queue`（`handler/aurora.go:56-177`）；daemon 认领并运行后，终态由 `service/aurora_completion.go` 结算/退款（`handler/aurora.go:146-157`、`service/task.go:4452,5085,6010`）。
- 所以 “apps/aurora 依赖的 cloud runtime” 准确说是 **(b) 为宿主 + (c) 提供托管身份 + (d) 完成执行**；(a) 只是让 self-host 能复用 SaaS 的 node 代理路径。`apps/aurora` 本身零 runtime 依赖。

## 2. 架构与调用链

```
apps/aurora (Next.js，仅 skills/works/billing 三页)
  │ 同源 /api/** → 主 server（proxy.ts:60-72；next.config.ts:52-70）
  ▼
server (chi)
  ├─ POST /api/aurora/generations   handler/aurora.go:56
  │    ├─ EnsureSystemAgents → agent_runtime(provider=aurora_managed, runtime_mode=cloud, daemon_id NULL)
  │    │                       + 16 个 kind=system 的 aurora:* agent（aurora/agents.go:81-116）
  │    ├─ CreditService.Reserve（reference=generation.id）
  │    └─ EnqueueQuickCreateTask → agent_task_queue(runtime_id=该 managed runtime)
  ├─ POST /api/daemon/managed/register（无 DaemonAuth，AURORA_SANDBOX_TOKEN）
  │    └─ MarkAgentRuntimeOnline（offline → online + last_seen_at）
  ├─ POST /api/daemon/tasks/claim | /claim  （DaemonAuth；NULL daemon_id 的 cloud runtime 可被任意 daemon 认领）
  └─ POST /api/daemon/tasks/{id}/complete|fail → 结算/退款（service/aurora_completion.go:44-103）
        ▲
        │ 容器内 daemon（普通 register/claim/complete）
  aurorafleet :8081 ── docker run -d -e MULTICA_SERVER_URL -e AURORA_SANDBOX_TOKEN ──▶ sandbox container
（controller.go:122-140；docker.go:66-96）                                                   │
                                                                                容器内 daemon + claude CLI
外部触发（运维 / 后续分支 autoprovision）：POST :8081/api/v1/nodes
   或 server /api/cloud-runtime/nodes → cloudruntime.Client → MULTICA_CLOUD_URL
```

1. **UI 建生成**：composer 调 `api.createAuroraGeneration`（`packages/core/aurora/api.ts:39-45`）→ `POST /api/aurora/generations`（`server/cmd/server/router.go:1899`，human-only + 按用户限流）。
2. **seed + 入队**：`EnsureSystemAgents` 幂等建 1 个 managed runtime + 16 个 system agent/skill（`handler/aurora.go:179-207`、`queries/aurora_agents.sql:7-60`）；任务落在 managed runtime 上（`handler/aurora.go:146-157`）。
3. **供给节点（38 上不在调用链内）**：只能外部调 fleet `POST /api/v1/nodes`（`controller.go:122-140`），或经 web/desktop 的 cloud-runtime 弹窗 → `/api/cloud-runtime/nodes` → `cloudruntime.Client`（`handler/cloud_runtime.go:47-58`、`router.go:2269-2280`）。生成路径没有任何 provision 调用（`handler/aurora*.go`、`internal/aurora`、`service/aurora_completion.go` 均无 CloudRuntime 引用）。仓库内唯一可触发节点创建的 UI 是 web/desktop 的 Runtimes → Cloud 弹窗（`NEXT_PUBLIC_ENABLE_CLOUD_RUNTIME`，默认关），`apps/aurora` 未暴露任何节点控制入口。
4. **激活托管身份**：daemon 用 `AURORA_SANDBOX_TOKEN` 调 managed register，响应返回 managed runtime 投影（`handler/aurora_runtime.go:31-79`）。
5. **claim**：daemon 调 `/api/daemon/tasks/claim`（`router.go:1481-1482`）；handler 对 `daemon_id IS NULL` 的 cloud runtime 不做机器归属校验（`handler/daemon.go:1810-1815`），SQL 再要求 runtime online + `last_seen_at` 新鲜（`agent.sql:777-779`）。
6. **执行**：daemon `runTask` 对 `aurora:*` 任务选 provider（仅 claude）并注入 `MaxTurns/PermissionMode/DisallowedTools`（`daemon.go:7673-7678,8688-8699`）。
7. **结算**：completed 标记 `credits_charged=reserved`；failed/cancelled/sweeper 失败全额退款（`service/aurora_completion.go:60-103`）。
8. **缺口**：第 3 步产出的容器 daemon 既没有可用的 daemon 鉴权 token（fleet 只注入两个 env，`controller.go:246-253`；daemon 用本地存储的 CLI 登录 token，`server/cmd/multica/cmd_daemon.go:528-534`），也没有把 managed runtime id 加进 claim 集的机制（`daemon.go:2223-2231`）。所以 38 上真正跑通一次 generation 需要外部补丁，代码只提供了组件。

## 3. 文件清单

| 模块 | 文件（行数） | 职责 |
| --- | --- | --- |
| 节点控制器 | `server/internal/aurorafleet/{controller.go 292, backend.go 60, node.go 43, docker.go 250, memory.go 131, doc.go 16}` + `{controller,docker}_test.go` | 292 行 chi 路由 + 兼容 cloudruntime 的 node API；Docker CLI 起容器（`docker.go:66-96`）、动作/状态/exec/List（`:131-164`）；内存实现供测试。 |
| fleet 入口 | `server/cmd/aurora-fleet/main.go` 81 | env 配置（`AURORA_FLEET_ADDR/BACKEND/SANDBOX_IMAGE`、`MULTICA_SERVER_URL`、`AURORA_SANDBOX_TOKEN`）、HTTP server、graceful shutdown。 |
| Cloud 传输 | `server/internal/cloudruntime/{client.go 255, client_test.go 86}` | 通用代理客户端；**与 main 完全相同**（`git diff main..branch -- server/internal/cloudruntime` 为空），并服务于 billing。 |
| 代理路由 | `server/internal/handler/{cloud_runtime.go 210, cloud_billing.go 604}`、`router.go:2269-2280`、`auth/cloud_pat.go:24` | node/billing 代理；`mcn_` PAT 由 Cloud Fleet 校验（`cloud_pat.go:18-60`），aurorafleet 不签发。 |
| managed 注册 | `server/internal/handler/aurora_runtime.go 81` + `aurora_runtime_test.go 156`、`handler/handler.go:153-158`、`router.go:440,1451-1455` | 共享 token 常量时间比较 → 把预 seed runtime 翻 online；路由故意放在 DaemonAuth 之外（`cmd/server/aurora_runtime_routes_test.go:10-45`）。 |
| Aurora 域 | `server/internal/aurora/{catalog.go 95, agents.go 159, credit.go 156}` + tests | 16 技能目录；managed runtime/16 system agent seed；微积分钱包（reserve/refund/grant，幂等）。 |
| Aurora handler/service | `handler/aurora.go 738`、`handler/aurora_artifact.go 82`、`service/aurora_completion.go 103`、`service/task.go:4452,5085,6010` | 生成 CRUD/资产下载/计费读取；artifact 落库；终态结算与失败退款。 |
| 执行策略 | `server/internal/daemon/aurora_tool_surface.go 57`、`daemon.go:6238-6250,7673-7678,8688-8699`、`pkg/agent/agent.go:133-150`、`pkg/agent/claude.go:730-781` | system key 识别、turn 上限、permission/deny 参数；agent CLI 侧新增两个 opt-in 字段。 |
| 迁移 | `server/migrations/500_aurora_generation`、`501_aurora_asset`、`502_credit_balance`、`503_credit_ledger`、`504/505` 索引、`506-509` aurora 索引 | 无 FK；credit 仅被 aurora 使用（全分支仅 `aurora/credit.go` 引用）。 |
| SQL | `server/pkg/db/queries/{aurora,aurora_agents}.sql`、`agent.sql:742-810`、`server/pkg/db/generated/aurora*.go` | 生成/资产；managed runtime seed；claim 授权与新鲜度门。 |
| Aurora 前端 | `apps/aurora`（约 1.8k 行，见 §1）、`packages/core/aurora`（api 275/queries 179/mutations 55/schema 117/types 63）、`packages/views/aurora`（composer 415/works 335/billing 223/…）、locales `aurora.json` | 纯 UI + 同源代理；无 sandbox/node 调用。 |
| 共享 runtime UI | `packages/core/runtimes/cloud-runtime.ts`、`packages/views/runtimes/components/{cloud-runtime-dialog,runtimes-page}.tsx`、`apps/web/.../runtimes/page.tsx` | SaaS Cloud 节点弹窗（EC2 instance type），flag `NEXT_PUBLIC_ENABLE_CLOUD_RUNTIME`，**不在 apps/aurora**。 |
| 测试 | `cmd/server/{aurora_runtime,aurora_billing}_routes_test.go`、`handler/aurora{fleet_e2e,_artifact,_runtime,_rate_limit,_}_test.go`、`aurorafleet/*_test.go` | 路由接线、注册鉴权、fleet provision→claim、artifact 落库、限流。 |

## 4. 鉴权与生命周期

- **fleet API：无鉴权。** `Controller.Handler` 只挂 `middleware.Recoverer`（`controller.go:58-80`），依赖“私有网络/VPC”假设；`/healthz`、`/readyz` 公开，`ready` 以 `Backend.Ping`（docker info）为信号（`controller.go:81-96`、`docker.go:161-164`）。`/nodes/exec` 只是运维逃生口：Aurora 生成链路不调用（全分支只有 controller 定义与 handler 代理，无业务调用方），任务实例内的执行由 daemon 自己完成。
- **节点凭证模型：共享 secret 注入，无 per-node token。** 每个容器拿到 `MULTICA_SERVER_URL` + `AURORA_SANDBOX_TOKEN`（`controller.go:14-21,246-253`）；`createNode` 不允许调用方传 env（`controller.go:127-134`，`backend.go:9-11` 注释明确 controller 控制 env），token 也绝不回显（`controller_test.go:147-153`）。
- **与 SaaS 的差别：** multica-cloud 在创建 node 时自己签 `mcn_` node PAT 并注入（`handler/cloud_runtime.go:48-54`）；主 server 用 Fleet `/api/v1/pat/verify` 校验 `mcn_`（`auth/cloud_pat.go:24,253`）。**aurorafleet 38 没有这套**：它不签发 `mcn_`，也不校验 `mcn_`，只注入共享 token。
- **节点生命周期：** `provisioning/running/stopped/rebooting/terminating/error`（`node.go:14-33`），由 docker State 映射（`docker.go:192-206`）；`Create` 直接返回 running（`docker.go:86-96`）；没有 DB/期望状态/对账，`List` 以 `docker ps -a --filter label=multica-aurora-node=1` 为唯一事实源（`docker.go:17-20,147-165`）。
- **managed runtime 生命周期：** seed 时 `status=offline, last_seen_at=NULL`（`queries/aurora_agents.sql:22-28`）→ managed register 置 online 并刷新 `last_seen_at`（`aurora_runtime.go:68-74`）→ claim SQL 只放行 online + 150s 内新鲜的行（`agent.sql:777-779`、`task.go:195`）。没有 deregister/TTL 回收：daemon 停止后行会自然过期而拒 claim，但不会被自动删除。
- **任务生命周期：** queued → dispatched → running → completed/failed；Aurora 额外要求 generation 非终态才结算，重复终态事件 no-op（`service/aurora_completion.go:20-58`）。

## 5. 与 QCA 的对应关系

| 维度 | `aurorafleet`（aurora） | `qoderruntime` + `cloud_agent_connection`（QCA） |
| --- | --- | --- |
| 抽象层 | **节点宿主生命周期**（容器/VM 的 CRUD 与电源动作） | **远端 agent 会话桥**（provider session 的创建/续跑/事件/取消） |
| 生命周期 | create/terminate/start/stop/reboot/status/exec（`controller.go:66-78`） | bridge register→heartbeat→claim→complete/fail；连接 save/resume/stop（`handler/qoder_connection.go:257-265,369-410`） |
| 凭证 | 共享 `AURORA_SANDBOX_TOKEN` 注入 env；无 per-node token；fleet 不鉴权调用方 | 连接配置 + journal 用 `MULTICA_VCS_SECRET_KEY` 加密存 PG（`migrations/586_cloud_agent_connection.up.sql`）；内部 `daemon_token` 1h 自动续期（`qoder_connection.go:421`） |
| 注册 | server 预 seed 行 + daemon 调专用 managed register 翻 online（`aurora_runtime.go:31-79`） | 桥调通用 `/api/daemon/register` 自报 `runtime_mode=cloud, provider=qoder_cloud`（`bridge.go:204-229`；服务端接受字段见 `handler/daemon.go:576-580`） |
| claim | 通用 claim API；但 38 没把 managed runtime id 加入 daemon claim 集 | 通用 per-runtime claim（`bridge.go:284-304`），桥只 claim 自己注册的 runtime |
| 事件 | 无（fleet 不参与任务事件） | 轮询 provider events + 游标分页（`events.go:42-60`），逐条幂等回写 |
| 持久化 | Docker 容器清单 + PG 的 `agent_runtime`/`aurora_*`/credit | `cloud_agent_connection`（加密 config+journal+revision+busy_until）或 `state.json`+flock（`state.go:15-46`） |
| 执行 | 容器内完整 daemon + claude CLI + 文件系统/媒体工具链 | 远端 Qoder session；**不跑本地 CLI、不 checkout、不 shell、不传文件**（`README.md:7,137-142`） |
| 产物/计费 | `aurora_asset`（38 无 producer）；credits 结算 | 仅文本结果/PR 链接，无文件；无计费换算（`README.md:146-154`） |

- **重叠点**：都以 `agent_runtime(runtime_mode=cloud)` 出现在同一张表；都复用 `/api/daemon/{register,claim,complete,fail}`；都要求 runtime online + 新鲜；都是“服务端托管、非用户机器”的执行载体；都需要把凭证/密钥从控制面送到执行面。
- **不可互换点**：QCA 没有节点概念，无法响应 create/stop/exec；没有文件与 shell 能力；claim 载荷假设 issue 上下文并拒绝 custom skill/覆盖（`README.md:137-154`），而 Aurora 的模型是 16 个 system agent + skill + 产物；QCA 的 journal/事件游标在 aurorafleet 侧不存在，反向亦然。
- **结论**：两者都在实现广义 “cloud runtime”，但一个在**基础设施层**（host/node），一个在**provider 会话层**。把 QCA 当 aurora 的 “统一实现” 是层级错配；可统一的只有 runtime 注册/claim 契约。

## 6. 删除候选与风险

**A. Aurora 专属实现（“原来的 cloud runtime”真正的实现）**

1. `server/internal/aurorafleet/*`（含 tests）与 `server/cmd/aurora-fleet/*`。
2. `server/internal/handler/aurora_runtime.go` + test、`AURORA_SANDBOX_TOKEN` 配置（`handler.go:153-158`、`router.go:440`、`.env.example:174-177`）、`/api/daemon/managed/register` 路由（`router.go:1451-1455`）、`cmd/server/aurora_runtime_routes_test.go`。
3. Aurora 域与执行接线：`server/internal/aurora/*`、`handler/aurora.go`、`handler/aurora_artifact.go`、`service/aurora_completion.go`、`service/task.go` 的 3 个结算钩子、`daemon/aurora_tool_surface.go`、`daemon.go` 的 artifact/工具面片段、`pkg/agent` 的 `PermissionMode/DisallowedTools`、迁移 500-509、`queries/{aurora,aurora_agents}.sql` + 生成代码。
4. 前端：`apps/aurora`、`packages/core/aurora`、`packages/views/aurora`、locales `aurora.json`。
5. 测试：`handler/aurora*_test.go`、`cmd/server/aurora_billing_routes_test.go`、`aurorafleet` 全部测试。

**B. 必须保留的共享接线（不是 Aurora 的实现）**

- `server/internal/cloudruntime/*`：被 billing（`handler/cloud_billing.go:117-134`）和 node 代理共用；删了会断 SaaS 计费与 web/desktop 的 cloud runtime。
- `handler/cloud_runtime.go` + `router.go:2269-2280` + `packages/core/runtimes/cloud-runtime.ts` + `packages/views/runtimes/**` + `apps/web/.../runtimes/page.tsx`：SaaS Cloud 节点 UI/协议，flag 控制；`apps/aurora` 从未引用。
- `auth/cloud_pat.go`（`mcn_`）、`entitlement`/`seatcapacity`：SaaS Cloud 身份与席位，与 Aurora 无关。
- daemon 的 register/claim/complete/sweeper、`agent_task_queue`、`agent_runtime`、对象存储上传：所有 agent 共用。
- 主 server 的 `runtime_mode=cloud` 注册能力（`handler/daemon.go:576-580`）：QCA 依赖它，Aurora 未来统一也应依赖它。

**C. 删除会破坏什么**

| 删除项 | 直接后果 |
| --- | --- |
| aurorafleet | self-host 无节点宿主；`/api/cloud-runtime` 只能回落到 MULTICA_CLOUD_URL，生成任务永远 queued（无 daemon 可 claim）。 |
| managed register + token | 预 seed 的 `aurora_managed` runtime 永远 offline；claim SQL 的 online+fresh 门直接拒绝（`agent.sql:777-779`），任务卡死。 |
| Aurora 工具面 | 回落到 provider 默认 autonomous（claude 默认 `bypassPermissions`，`pkg/agent/claude.go:734-743`），用户 prompt 在服务端容器里全工具执行——安全回归。 |
| 结算钩子 | generation 停留 queued、积分只扣不退；sweeper 失败路径不退款。 |
| 迁移 500-509 | works/billing 页面 500；已部署实例需 down migration，直接 drop 丢数据。 |
| 前端 aurora 包 | 只影响产品 UI，不动 runtime；可独立于后端删除。 |

## 7. 关键结论（能否复用 QCA 代码）

### 7.1 依赖事实（Q6）：Aurora 任务依赖进程执行 + 文件系统

- **进程执行**：daemon 在容器内 spawn `claude` CLI（argv 由 `pkg/agent/claude.go:730-781` 组），这是执行的前提；fleet 的 `/nodes/exec` 只是运维接口，不是执行路径。
- **文件系统**：workdir 准备、产物上传/storage 与 `aurora_asset` 落库都以文件为交付物（`daemon/types.go:320-326`、`handler/aurora_artifact.go:32-80` 的设计前提）；沙箱镜像还要求 media MCP、FFmpeg、headless Chrome（`docs/superpowers/plans/2026-09-22-aurora-sandbox-tool-surface.md:55-66`）。
- **工具面不等于执行面**：`Bash/WebFetch/WebSearch` 是对**模型**的 deny（`aurora_tool_surface.go:50-56`），并不取消 daemon 的进程执行与文件能力；反过来 QCA 桥连本地 CLI、checkout、shell、文件传输都不做（`README.md:137-142`）。
- 结论：**Aurora 执行模型强依赖 shell/exec/file；QCA 在能力上不可替代**（不是接一层适配器能补齐的，属能力边界差异）。

### 7.2 复用判断

1. **不能把 QCA 桥当 Aurora 执行后端。** QCA 的能力边界（无 shell/exec/文件传输、仅 issue、拒绝 custom skill、产物只有文本）与 Aurora 的交付形态（媒体文件、16 skill、容器内 CLI + MCP/FFmpeg/Chrome）不相容；证据：`README.md:7,137-154`、`daemon/types.go:320-326`、沙箱镜像要求见 `docs/superpowers/plans/2026-09-22-aurora-sandbox-tool-surface.md:55-66`。
2. **可复用的是“cloud runtime 注册/claim 抽象”，不是 QCA 实现。** QCA 已把 `runtime_mode=cloud` 做成一等注册字段（`main:handler/daemon.go:576-580`），Aurora 却走了“预 seed + 专用 managed register + 共享 secret”的旁路。把 Aurora 改成同一条注册路径，即可删掉 `AURORA_SANDBOX_TOKEN` 与 `ManagedRuntimeRegister`，同时保留 fleet 只做节点供给。
3. **fleet 与 QCA 可以并存分工**：fleet 负责“有 shell/文件/媒体工具链”的节点（Aurora），QCA 负责“provider 托管会话”的节点（Qoder）；两者共用 `agent_runtime`/claim/complete，不共享生命周期实现。
4. **38 不是可交付状态**：沙箱镜像、managed daemon 模式、自动 provision、artifact producer、managed runtime id 下发都缺失；删除/统一前应以后续分支（`feature/agency-aurora-ark-fleet`：新增 `auth.go`/`policy.go`/`reconcile.go`/`client.go`，`doc.go:12-13` 称 managed enrollment 改由 server 内部签发）为基线重新评估。

## 8. 未解问题

1. **沙箱 daemon 鉴权来源**：fleet 只注入两个 env，daemon 用存储的 CLI 登录 token（`cmd/multica/cmd_daemon.go:528-534`）；38 上谁、以什么凭证完成首次 `multica auth login` 未在仓库中定义。
2. **managed runtime id 如何进入 claim 集**：计划只写了两个待选方案（`docs/superpowers/plans/2026-09-11-aurora-execution.md:143`），代码两者都没实现。
3. **artifact producer / 上传协议**：只有 server 端 `ReportTaskArtifacts`（`handler/aurora_artifact.go:32-80`）与 daemon client（`daemon/client.go:570-580`），谁填充 `TaskResult.Artifacts`、上传到哪个 storage 端点未实现。
4. **部署面**：38 无 `deploy/aurora-sandbox/`、无 fleet 的 compose/helm/入口脚本；根 `Dockerfile`/`docker-compose` 与 Aurora 无关。
5. **自托管是否配置 `/api/cloud-runtime`**：代码只读 `MULTICA_CLOUD_URL`（`router.go:429`、`handler.go:529-533`），没有强制指向 fleet；web 弹窗还需 `NEXT_PUBLIC_ENABLE_CLOUD_RUNTIME=true`（`apps/web/.../runtimes/page.tsx:3-7`），两者在 Aurora 部署里是否启用无法从代码判断。
6. **SaaS UI 与 fleet 协议不兼容**：`CloudRuntimeNodeSchema` 要求 `owner_id/instance_id/region/instance_type/...`（`packages/core/api/schemas.ts:1539-1553`），fleet 只返回 `id/name/image/status/labels`（`node.go:35-43`）；DELETE 前端发 `{instance_id}`（`packages/core/api/client.ts:1868-1873`），fleet 读 `{id}`（`controller.go:104-108`）。即“路径兼容”不等于“载荷兼容”。
