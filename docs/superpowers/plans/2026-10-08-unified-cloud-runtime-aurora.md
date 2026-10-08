# 统一 Cloud Runtime 与 Aurora 执行层实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 `docker/runtime/Dockerfile` 做成**唯一**的节点发布镜像（一个 OCI image index digest），让 Fleet 成为唯一 Docker 控制面、普通任务与 Aurora 任务共用同一套 daemon / 注册 / 领取 / 生命周期实现，工作区创建时异步准备运行时、就绪前不生成不预留积分，生成→积分→入队→产物→结算在崩溃点可恢复，并把现有 AppArmor 目录级执行通配收紧为按固定入口授权的进程策略。

**Architecture:** 三种互相独立的改造同步收敛到一条链路。**镜像**：`deploy/aurora-sandbox/Dockerfile` 的 5 个 stage 与 `Dockerfile.egress` 折叠进 `docker/runtime/Dockerfile` 的单一最终运行 stage，`/usr/local/bin/claude` 成为唯一 Claude 入口，`/usr/local/bin/fleet-node`（默认 `run`）、`/usr/local/bin/aurora-egress-proxy`、`/usr/local/libexec/multica-mcp-client` 是三个按可执行路径授权的固定入口；节点与 egress sidecar 取同一个 `image` 字段。**身份**：`agent_runtime` 的受管载体从 `runtime_mode='cloud' + provider='aurora_managed'` 统一为 `provider='claude' + runtime_mode='local'`，受管性只由服务器写入的 `metadata.managed_by='local_fleet'` / `metadata.fleet_node_id` 表达；daemon 注册凭据落盘 `/data/identity/session.json` 并新增 renew/renew-ack 轮换。**事务**：准备意图（`fleet_nodes.status='waiting'` + `fleet_node_operations.action='prepare'`）在创建工作区的事务内提交，协调器在配置/容量检查通过后的短事务里把同一个 operation 推进到既有 create 阶段；生成提交改为单个 `runFleetTx` 下完成准入、generation、积分、task 与关联；新增独立的结果协调服务按可验证事实查询/导入/结算或升级 `needs_review`，不再把「不确定」直接当失败退款。

**Tech Stack:** Go 1.26（Chi、pgx/v5、sqlc 1.31.1、Docker Engine SDK）、PostgreSQL 17、Node 22（MCP stdio broker + 已打补丁的 Volcengine vendor 树）、Claude Code CLI、AppArmor + seccomp + Docker 只读根、TanStack Query + zod + Vitest、Playwright。

**Spec：** [统一 Cloud Runtime 与 Aurora skills 执行层设计](../specs/2026-10-06-unified-cloud-runtime-aurora-design.md)（r2，2026-10-07）

**审查记录：** [统一 Cloud Runtime 与 Aurora 设计深度审查](../specs/2026-10-07-unified-cloud-runtime-aurora-review.md)（R1–R10）

**依赖计划（本计划取代其中相冲突的设计，不继承其完成状态）：**

- [本地 Docker Cloud Runtime 实施计划](2026-10-04-local-docker-cloud-runtime.md) 与[设计规格](../specs/2026-10-04-local-docker-cloud-runtime-design.md)、[Task 3 生产者契约](../specs/2026-10-04-local-docker-cloud-runtime-task-3-contract.md)、[namespace/operator 补缺](2026-10-05-local-fleet-namespace-operator.md) —— Fleet 仍是权威控制面，本计划只做增量。
- [Aurora Cloud Runtime 对接实施计划](2026-10-06-aurora-cloud-runtime-integration.md) —— 其 Task 1–9 已完成；本计划取代其中「双节点镜像」「独立 `aurora_managed` 生命周期」「生成时首次准备」三处。
- [沙箱工具面主计划](2026-09-22-aurora-sandbox-tool-surface.md) 及四个子计划（[A](2026-09-25-aurora-managed-sandbox-control-plane.md)、[B](2026-09-25-aurora-sandbox-fleet-isolation.md)、[C](2026-09-25-aurora-sandbox-skill-runtime.md)、[D](2026-09-25-aurora-sandbox-image-smoke.md)）—— 13 个 skill 路由、broker 工具面、vendor 加固继续有效。
- 历史证据（保留、不作为本计划的通过依据）：[最终验收记录](2026-09-28-aurora-sandbox-acceptance-record.md)、[API 链路验收](2026-10-06-aurora-cloud-runtime-acceptance.md)、[AWS 共置验收](2026-10-07-aurora-cloud-runtime-aws-acceptance.md)。

**已确认的执行环境决定（2026-10-08，用户选择）：** G0 的目标平台是**在一台 Linux VM 上运行的 Docker Engine**（可以位于本机 Mac 之上，但必须是真正的 Linux 内核），而不是当前这台 Mac 上的 Docker Desktop。规格 §8.3 的只读查询已证明当前 Docker Desktop 不报告 AppArmor，按 fail-closed 要求它不能作为支持平台；本计划不在 Docker Desktop 上做任何隔离验证，也不为其保留弱化后备。

---

## 0. 执行前必读：规格措辞与代码现实的对照

规格 r2 是方向权威，但其中若干措辞是在没有逐行核对代码的前提下写的。下表列出**已核实的差异**与本计划采用的落地方式。执行者按「本计划采用」列实现；不要按规格的字面枚举去找不存在的值。

| 规格措辞（§） | 已核实的代码现实 | 本计划采用 |
| --- | --- | --- |
| operation `phase=waiting_config\|waiting_capacity\|queued\|running\|succeeded\|failed`（§5.2） | `fleet_node_operations.phase` 现有值恰好六个：`queued`、`preparing`、`prepared`、`applying`、`completed`、`failed`；`recoverable` / `claimable` 集合是 `fleet.sql` 里的显式枚举谓词，`fleet/internal_dto.go` 的 `OperationReviewResponseDTO.Validate` 也只接受这些 | **新增** `waiting_config`、`waiting_capacity` 两个值（Task 10），**不重命名** `applying`/`completed`。规格要的语义（等待不占资源、不消费重试、协调器持续扫描、有明确阻塞原因）全部满足 |
| `fleet_nodes.status=waiting`、`ready=false`（§5.2） | `status`/`desired` 是无 Go 常量、无 CHECK 的自由字符串；`ready` 由 SQL 表达式 `ready=(@ready::boolean AND NOT maintenance AND NOT revoked AND desired='running')` 派生（`fleet.sql:487`） | 新增 `status='waiting'`，并把 `status='waiting'` 加进 `ready` 表达式与 `workerNativeReady` 的否定条件（Task 10） |
| `next_attempt_at` 沿用操作表 | 已存在（`577_fleet_recovery_fences`） | 直接沿用，不新增列 |
| 「每个 Fleet namespace 中每个工作区最多一个未终止的自动受管节点」（§5.1） | `fleet_nodes.workspace_id` 已存在（`583`）且被写入，但 `fleet.sql` 里**没有任何查询读它**；只有一条 `fleet_nodes_workspace_idx`（`584`） | 新增 `(namespace, workspace_id)` 的局部唯一索引，并把 `workspace_id` 纳入节点查询（Task 10） |
| 「不再额外创建 `aurora_managed` 载体…统一为 `provider=claude`、`runtime_mode=local`」（§5.1） | 受管载体是 `runtime_mode='cloud' + provider='aurora_managed'`，由 `526_aurora_managed_runtime_workspace_idx` 这条局部唯一索引保护；daemon 与 handler 各有硬编码谓词；`BindAuroraManagedRuntime` 是 SQL 字面量 | 全量改为 `provider='claude' + runtime_mode='local'`，并**重建 526 的索引为同一谓词**（否则唯一性失效）、同步注册/claim/SQL/系统智能体（Task 7） |
| `/usr/local/bin/claude` 是统一入口（§4.1） | `model.AuroraClaudePath = "/opt/aurora/runtime/node_modules/.bin/claude"`，由 Fleet 写进容器 env，并在 adoption 检查（`docker/inspect.go`）与 `scripts/verify-aurora-sandbox-image.sh` 中钉死 | `AuroraClaudePath` 改为 `/usr/local/bin/claude`，删除第二份 Claude 安装（Task 4） |
| egress 代理与执行节点**同镜像同 digest**（§1.6、§4.2） | `AuroraConfig.ProxyImage` 是独立字段，`Dockerfile.egress` 是独立 2-stage 构建，`egressProxySpec` 取 `a.ProxyImage` | 删除 `ProxyImage` 字段与 `Dockerfile.egress` 构建入口，sidecar 改取 `Config.Image`（Task 4） |
| MCP 客户端是 `/usr/local/libexec/multica-mcp-client`，daemon 监督 broker stdio（§8.2） | 今天 Claude 的 MCP config 直接 `command: node` + `args: [server.mjs]`，**broker 由 Claude 启动**，daemon 不监督它 | 新增单功能客户端 + daemon 侧任务级 socket 桥（Task 2） |
| `/data/identity/session.json` 持久身份（§5.3） | daemon token **只在内存**（`client.go` 的 `token` 字段）；`daemon_token_expires_at` 被解析但从不读取；仓库里不存在 `data/identity` 路径 | 新增持久会话、renew/renew-ack（Task 8、Task 9） |
| 生成/积分/入队单事务（§7） | 今天跨 4 个事务：`createAuroraGenerationWithEntitlement`（自带 tx）→ `Credit.Reserve`（自带 tx）→ `EnqueueQuickCreateTask`（`runFleetTx`）→ `UpdateAuroraGenerationTask`（单语句） | 抽取 `qtx` 原语，合成一个 `runFleetTx`（Task 12） |
| 「一个工作区最多一个未终止的自动受管节点」与「多重绑定拒绝自动选择」（§5.1、§10.3） | 现有手工（Claude profile）节点与 Aurora 节点共用 `fleet_nodes`，靠 `profile_ref` 区分（Aurora 行 `profile_ref=''`） | 迁移映射按 `workspace_id` + `profile_ref` 分类；冲突时**拒绝**而非猜测（Task 18） |

**规格中无需修改即可成立的部分**（已核对）：Fleet 是唯一 Docker 控制面（`aurorafleet` 已退役）；`fleetguard` 的三类屏障（claim/register/enqueue）已存在；`egress_pins` 与 `IsPublicIP` 已在 `auroraegress` 实现；`seccomp.json` 已在 seccomp 层封死 mount/ptrace/namespace 系列；`GetAuroraRuntime` 已是只读投影且非锁查询。

---

## Global Constraints

- **唯一发布产物。** 一个节点镜像、一个 OCI image index digest、一套 Cloud Runtime。不保留普通版/Aurora 版两个最终 target，不保留独立 Aurora 节点镜像、第二套 bootstrap 或内部双写。节点与 egress sidecar 解析到**同一**架构 manifest。
- **fail-closed，不静默降级。** 隔离策略无法生效时禁止执行并返回 `runtime_policy_unavailable`；不得退回继承宽权限，不得关闭 no-new-privileges，不得用「镜像里没有 Git」代替进程策略。
- **不新建数据库、队列或状态机。** 复用 `fleet_nodes` / `fleet_node_operations` / `agent_runtime` / `aurora_*` 既有表；共享 PostgreSQL。
- **迁移规则（仓库既有，强制执行）：** 无外键、无级联删除/更新；每个迁移创建的索引一律 `CREATE [UNIQUE] INDEX CONCURRENTLY`，且**单独一个单语句文件**；条件 DDL 用 `IF EXISTS`/`IF NOT EXISTS`；SQL 改完 `make sqlc`，不手改 generated。新增并发索引必须登记进 `server/cmd/migrate/main.go` 的 `concurrentIndexCleanups`。
- **迁移编号以实施时仓库占用为准。** 本计划写 `6xx` 占位是禁止的：执行每个迁移任务时先 `ls server/migrations | sort -n | tail -1` 取当前最高号，再顺延。当前最高号为 `584`。
- **锁顺序不变量。** `fleetguard.lockBindings` 的顺序是：namespace（按前缀排序）→ node（按 UUID 排序）→ capacity → task/workspace → agent；`FleetOwnerExclusiveLock` 在每个 create/provision/delete 事务里最先取。新增的任何写作路径必须复用同一顺序；不得在持锁事务里调用会自行开事务的 `Credit.Reserve` 或 `EnqueueQuickCreateTask`。
- **数据库事务不跨 Docker/网络 I/O。** 资源创建结果未知时先检查原资源身份，不因超时创建第二份资源。
- **私密输入。** `mse_`/`mdt_`/provider 密钥不进 SQL 明文、不进日志、不进 argv、不进模型上下文、不进 `Config.Env`；只以 owner-only 文件（0600 / 目录 0700）或一次性内存交接。
- **默认测试不依赖真实 Docker、真实模型或真实供应商账户。** Docker 链路用 `dockerintegration` + `MULTICA_RUN_DOCKER_INTEGRATION=1`；真实 CLI 用 `agentintegration` + `MULTICA_RUN_REAL_AGENT_SMOKE=1`；两者分别显式授权，未授权的运行记录为 **SKIP，绝不记 PASS**。默认测试不得解析或执行用户安装的 agent CLI（`scripts/agent-cli-command-names.txt` 的 PATH 哨兵会捕获）。
- **包边界。** `packages/core` 无 UI/localStorage/`process.env`；`packages/views` 无 store 定义、无 `next/*`/`react-router-dom`（用 `NavigationAdapter`/`useNavigation`/`<AppLink>`）；Next.js app-shell 助手放 `packages/nextjs`；平台接线留 `apps/aurora`。新增响应一律过 zod + `parseWithFallback`。
- **文案。** 新增 i18n 键必须五个 locale（en / zh-Hans / fr / ja / ko）齐备，否则 `packages/views/locales/parity.test.ts` 红。中文称「运行时」，`skill` 保持英文；改动与无障碍名称一起提交。
- **代码注释用英文。** 不新增内部兼容 shim、双写或旧字段回退链（API 响应边界的兼容是唯一例外）。
- **提交与交付。** 每个任务结束跑定向测试 → 检查 diff → 一个原子 conventional commit。不用 `git add .` 纳入无关改动。缺环境不是有效的 red。
- **G0 未过不得进入发布切换。** 本计划的 Task 1–3 是门；Task 4 及以后可以并行开发，但把统一镜像作为可发布产物（Task 5 的 publish 路径）与切换生产（Task 18）必须先有 G0 的实证记录。

---

## 范围、依赖与实施顺序

按规格 §12 的 G0–G5 组织为 M0–M5，共 20 个任务。每个任务结束都有可独立判定的交付物；**不允许**用小测试替代最终闭环，也不允许把某个中间任务的绿灯当作整条链路已验收。

| 里程碑 | 任务 | 可判定交付 | 依赖 |
| --- | --- | --- | --- |
| **M0 · G0 平台与隔离原型** | 1–3 | Linux VM 上的 API/Fleet 共置与 guest 路径核查通过；daemon 与 broker/Claude 的进程分离完成；按固定入口收紧的进程策略在目标内核上实测加载，并跑通「允许 + 越权拒绝」矩阵 | 用户指定的 Linux VM |
| **M1 · G1 唯一镜像与受控服务契约** | 4–6 | 单一发布产物构建、内容校验、供应链门、签名与唯一 index digest；受控 provider fixture 的固定公网 IP、TLS、SSE/provider/media 合同实证 | G0 |
| **M2 · G2 统一注册与身份** | 7–9 | 单次注册、响应丢失恢复、重启持久化、8 小时到期前轮换、claim/HTTP/WS 代次屏障 | G0；用 G1 候选镜像验证 |
| **M3 · G3 准备/生成/结果事务** | 10–13 | waiting 意图、owner/capacity/删除规则、原子生成提交、请求幂等、独立结算协调与 `needs_review` 运维闭环 | G2 |
| **M4 · G4 应用接线** | 14–17 | 准备/恢复/就绪门禁、Query 更新、旧响应兼容、五语文案与浏览器交互 | G3 的 API/schema 合同 |
| **M5 · G5 迁移与最终验收** | 18–20 | 每检查点中断恢复、同一镜像 13-route、浏览器、真实服务「已运行或明确未授权」的分项证据 | G1–G4 |

**并行边界：** Task 4（镜像折叠）与 Task 7/10/12（Go 侧）互不依赖，可并行开发；但 Task 5 的 publish 路径与 Task 18 的切换必须等 G0 的实证记录（Task 3 的输出）。Task 15–17 依赖 Task 14 锁定的 wire 合同。

---

## 文件结构与职责

### 新增文件

- `server/internal/daemon/aurorabridge/bridge.go` — daemon 侧的任务级 MCP 桥：监督 broker 子进程的 stdio，在任务专用 Unix socket 上转发当前任务的 MCP 流量，校验 peer credential 与活动 attempt。
- `server/internal/daemon/aurorabridge/frame.go` — JSON-RPC 帧边界常量与读写（1 MiB 单帧上限、最多 8 个未完成请求、有界发送缓冲）。
- `server/internal/daemon/aurorabridge/bridge_test.go`、`frame_test.go` — 桥与帧的单元测试。
- `server/cmd/multica-mcp-client/main.go` — 单功能 MCP 客户端（stdio ↔ 任务 socket），装到 `/usr/local/libexec/multica-mcp-client`；无 `fleet-node` 子命令、不能指定目标 socket 之外的东西。
- `server/cmd/multica-mcp-client/main_test.go` — 固定 argv/环境契约与 fail-closed 测试。
- `server/internal/daemon/identity.go`（新增文件，与既有 `identity.go` 同包名不同职责时改用 `identity_store.go`）— 见 Task 8 的说明：`/data/identity/session.json` 的原子读写与 pending 轮换状态。
- `server/internal/aurora/rotation.go` — 服务端的 daemon 凭据轮换（pending/CAS/ack）。
- `server/internal/aurora/reconcile.go` — Aurora 结果协调的领域规则（按可验证事实决定动作）。
- `server/internal/service/aurora_admission.go` — 原子生成准入（一个 `runFleetTx`）。
- `server/internal/service/aurora_reconcile.go` — 结果协调器的有界扫描循环。
- `server/internal/handler/aurora_runtime_prepare.go` — `POST /api/aurora/runtime/prepare` 与投影扩展。
- `server/pkg/db/queries/fleet_prepare.sql` — 准备意图的 SQL（等待节点、promote、容量计数、tombstone）。
- `server/pkg/db/queries/daemon_token_rotation.sql` — 轮换表 SQL。
- `server/pkg/db/queries/aurora_idempotency.sql` — 生成请求幂等键 SQL。
- `deploy/aurora-sandbox/fixture/matrix/` — G0 的允许/拒绝矩阵脚本（`run-matrix.sh` + 用例清单）。
- `scripts/aurora-runtime-host-preflight.sh` — Linux 宿主的只读前置检查（内核、AppArmor、cgroup v2、Docker 版本与 security options、guest 路径）。
- `docs/superpowers/plans/2026-10-08-unified-cloud-runtime-aurora.md` — 本文件。

### 修改文件

- `docker/runtime/Dockerfile` — 折叠为单一最终运行 stage。
- `deploy/aurora-sandbox/Dockerfile`、`deploy/aurora-sandbox/Dockerfile.egress` — 删除构建入口（保留 `runtime/`、`vendor/`、锁文件与安全策略作为源目录）。
- `deploy/aurora-sandbox/docker-bake.hcl` — 单一 target、单一 tag。
- `deploy/aurora-sandbox/multica-aurora-sandbox.apparmor` — 按固定入口收紧。
- `deploy/aurora-sandbox/versions.json`、`apt-packages.lock` — Claude 版本来源统一为 `docker/runtime/claude-version.txt`。
- `scripts/verify-aurora-sandbox-image.sh`、`scripts/verify-aurora-sandbox-locks.mjs` — 单一产物契约。
- `.github/workflows/aurora-sandbox.yml` — 单一镜像的 build/scan/sign/publish/verify 步骤。
- `server/internal/fleet/model/aurora.go` — 删除 `ProxyImage`，`AuroraClaudePath` 改路径，新增轮换/身份常量。
- `server/internal/fleet/model/types.go` — 新增 `Prepare` action；`AuroraNodeRequest` 增加准备字段。
- `server/internal/fleet/store/aurora.go`、`store/intents.go`、`store/store.go` — 准备意图与 promote。
- `server/internal/fleet/service.go`、`http_aurora.go`、`internal_dto.go` — 准备路由与阶段校验。
- `server/internal/fleet/reconciler.go`、`scheduler.go` — 准备推进分支。
- `server/internal/fleet/docker/provider.go`、`inspect.go`、`egress.go` — 单镜像、新入口、Claude 路径、sidecar 镜像来源。
- `server/internal/daemon/managed.go`、`client.go`、`daemon.go`、`health.go`、`aurora_broker.go`、`aurora_tool_surface.go` — 身份统一、持久会话、桥接线、轮换。
- `server/internal/aurora/agents.go`、`sandbox_enrollment.go`、`sandbox_manager.go`、`sandbox_reaper.go`、`credit.go` — 身份统一与事务原语。
- `server/internal/handler/aurora.go`、`aurora_runtime_view.go`、`aurora_runtime.go`、`daemon.go`、`handler.go` — 准备 API、投影、事务调用、注册触达门。
- `server/internal/service/task.go`、`aurora_completion.go` — 事务原语与结算恢复。
- `server/cmd/fleet-node/main.go`、`bootstrap.go` — 固定入口与身份持久化接线。
- `server/cmd/server/router.go`、`main.go` — 新路由与接线。
- `server/pkg/db/queries/fleet.sql`、`aurora_sandbox_node.sql`、`aurora_agents.sql`、`aurora.sql`、`runtime.sql`、`daemon_token.sql` — 谓词与查询。
- `packages/core/aurora/{api,schema,queries,mutations,types}.ts` — 准备/恢复/就绪合同。
- `packages/views/aurora/{runtime-status,generation-composer,history-list}.tsx` — 准备与就绪 UI。
- `packages/views/locales/*/aurora.json` — 五语文案。
- `apps/aurora/app/[workspaceSlug]/runtimes/page.tsx` — 准备动作接线。
- `AGENTS.md`（本仓库）与 `docs/superpowers/specs/2026-10-06-unified-cloud-runtime-aurora-design.md` 的状态行 — 实施后回填。

### 现有接入位置（执行时按行号定位，行号以本计划写作时的 HEAD `bf01c2754` 为准）

- `server/internal/handler/aurora.go:84` `CreateAuroraGeneration`；`:185` 唯一的 `aurora_runtime_unavailable` 503 生产点；`:347` `createAuroraGenerationWithEntitlement`；`:444` `ensureWorkspaceSandbox`。
- `server/internal/handler/aurora_runtime_view.go:25-43` 投影结构体；`:151` `auroraExecutionState`。
- `server/internal/daemon/managed.go:20` `bootstrapManaged`；`:54` `installManagedEnrollment`；`:65` 载体谓词。
- `server/internal/aurora/sandbox_enrollment.go:99` `Issue`；`:189` `Consume`；`:30-31` 两个 TTL。
- `server/internal/fleet/store/aurora.go:179` `CreateAuroraIntent`；`:215-221` 容量检查。
- `server/internal/fleet/reconciler.go:105-166` tick 与 dispatch；`scheduler.go:38-59` 单槽位。
- `server/pkg/db/queries/fleet.sql:250` `CountFleetProvisionedNodes`；`:265` `InsertFleetAuroraNode`；`:392` `ListFleetRecoverable`；`:418` `FleetClaimBootstrap`；`:487` `ready` 表达式。
- `deploy/aurora-sandbox/Dockerfile:135` 最终 stage；`:235` ENTRYPOINT；`Dockerfile.egress:19` `FROM scratch`。
- `server/internal/daemon/aurora_broker.go:32` `auroraBrokerEntrypoint`；`:271` `auroraBrokerMcpConfig`。
- `server/internal/daemon/aurora_tool_surface.go:34` `auroraExecutionProvider`；`:41` `auroraMaxTurns`。

---

# M0 · G0 平台与隔离原型

> **这一里程碑是门。** 规格 §12 与审查 R1/R2 把「目标平台」和「进程策略实际生效」列为阻断项：未通过不得进入发布与切换。Task 1–3 的每一个「未通过」结论都必须原样记录，不得改写成「待完善」。

## Task 1: Linux 执行环境与 API/Fleet 共置

规格 §8.3 要求完整执行的候选是**能加载所需 AppArmor 策略的 Linux Docker Engine**，并且要求 API/Fleet 位置、数据库身份、`API↔Fleet` loopback 契约、容器回调与私密文件的 guest 路径**一起**验证 —— 不能只把 Docker socket 指向另一台引擎而沿用 Mac 上的 bind 路径。

**Files:**
- Create: `scripts/aurora-runtime-host-preflight.sh`
- Create: `scripts/aurora-runtime-host-preflight.test.sh`
- Modify: `docs/superpowers/plans/2026-10-08-unified-cloud-runtime-aurora.md`（G0 记录小节，见 Step 7）

**Interfaces:**
- Produces: 一台可用的 Linux VM（Docker Engine ≥ 27、cgroup v2、AppArmor 可用），其上运行 PostgreSQL、Multica API、Fleet 三个进程，且 `MULTICA_LOCAL_FLEET_URL` 为 `http://127.0.0.1:<port>`（loopback，因为 `orchestration/local_fleet.go` 对 URL 要求 loopback）。
- Produces: 后续任务的运行约定 —— 所有 Docker/隔离命令在这台 guest 内执行；Mac 上不跑隔离验证。

- [ ] **Step 1: 写只读前置检查脚本的失败测试**

创建 `scripts/aurora-runtime-host-preflight.test.sh`：

```bash
#!/usr/bin/env bash
# Hermetic test: the preflight must refuse every non-conforming host and must
# never touch Docker, the database or the network beyond reading /proc and /sys.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script="$repo_root/scripts/aurora-runtime-host-preflight.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

# 1. It must fail closed when the fake host reports no AppArmor.
mkdir -p "$tmp/no-apparmor/sys/module/apparmor/parameters"
printf 'Y\n' >"$tmp/no-apparmor/sys/module/apparmor/parameters/enabled"
printf 'name=seccomp,profile=builtin\nname=cgroupns\n' >"$tmp/no-apparmor/security-options"
if AURORA_PREFLIGHT_ROOT="$tmp/no-apparmor" "$script" >/dev/null 2>&1; then
  fail "accepted a host with no AppArmor in Docker security options"
fi

# 2. It must refuse cgroup v1.
mkdir -p "$tmp/cgroupv1"
printf 'name=apparmor\n' >"$tmp/cgroupv1/security-options"
printf 'Y\n' >"$tmp/cgroupv1/apparmor-enabled"
printf 'tmpfs\n' >"$tmp/cgroupv1/cgroup-v1"
if AURORA_PREFLIGHT_ROOT="$tmp/cgroupv1" "$script" >/dev/null 2>&1; then
  fail "accepted a cgroup v1 host"
fi

# 3. It must accept a conforming host and print the four evidence lines.
mkdir -p "$tmp/ok"
printf 'name=apparmor\nname=seccomp,profile=builtin\n' >"$tmp/ok/security-options"
printf 'Y\n' >"$tmp/ok/apparmor-enabled"
printf 'cgroup2fs\n' >"$tmp/ok/cgroup-v2"
printf 'Linux 6.8.0 x86_64\n' >"$tmp/ok/uname"
out="$(AURORA_PREFLIGHT_ROOT="$tmp/ok" "$script")" || fail "refused a conforming host"
for want in 'apparmor=enabled' 'cgroup=v2' 'kernel=Linux 6.8.0 x86_64' 'apparmor_parser=present'; do
  printf '%s\n' "$out" | grep -q "$want" || fail "missing evidence line: $want"
done

printf 'ok\n'
```

- [ ] **Step 2: 运行测试确认失败**

Run: `bash scripts/aurora-runtime-host-preflight.test.sh`
Expected: FAIL —— `scripts/aurora-runtime-host-preflight.sh: No such file or directory`

- [ ] **Step 3: 实现脚本**

创建 `scripts/aurora-runtime-host-preflight.sh`：

```bash
#!/usr/bin/env bash
# Read-only preflight for the Linux execution host the Aurora isolation policy
# requires. It reads /proc, /sys and `docker info` output only: it never starts a
# container, never writes a profile, never opens the database.
#
# AURORA_PREFLIGHT_ROOT overrides the root the script reads from, so its own test
# can drive every branch without a real host. `docker info` is skipped when the
# override is set; callers on a real host get name=... lines for free.
set -euo pipefail

root="${AURORA_PREFLIGHT_ROOT:-}"
read_host() {
  if [ -n "$root" ]; then
    printf '%s\n' "$1" | sed "s#^/$#${root%/}/#"
  else
    printf '%s\n' "$1"
  fi
}

fail() { printf 'aurora host preflight: %s\n' "$1" >&2; exit 1; }

uname_line="$(cat "$(read_host /proc/sys/kernel/osrelease)" 2>/dev/null || true)"
printf 'kernel=%s\n' "${uname_line:-unknown}"

# 1. cgroup v2. The seccomp profile and the read-only rootfs posture are only
#    validated on a unified hierarchy; v1 changes which syscalls the engine
#    itself issues and invalidates the acceptance run.
if [ -f "$(read_host /sys/fs/cgroup/cgroup.controllers)" ] && [ ! -f "$(read_host /sys/fs/cgroup/cgroup-v1-placeholder)" ]; then
  printf 'cgroup=v2\n'
else
  fail "cgroup v2 is required (no /sys/fs/cgroup/cgroup.controllers)"
fi

# 2. The kernel must have AppArmor and the admin must have enabled it.
if [ ! -d "$(read_host /sys/module/apparmor)" ]; then
  fail "the kernel has no AppArmor"
fi
enabled="$(cat "$(read_host /sys/module/apparmor/parameters/enabled)" 2>/dev/null || printf 'N')"
[ "$enabled" = "Y" ] || fail "AppArmor is present but not enabled"
printf 'apparmor=enabled\n'

# 3. apparmor_parser must exist: the policy is loaded by the operator, not by
#    Docker, and a missing parser means the profile can never be attached.
command -v apparmor_parser >/dev/null 2>&1 || fail "apparmor_parser is not on PATH"
printf 'apparmor_parser=present\n'

# 4. The Docker daemon must report AppArmor. `name=apparmor` is the only
#    accepted evidence: a configured profile name in a compose file is not
#    proof, as the design review records.
security_options=""
if [ -n "$root" ]; then
  [ -f "$(read_host /security-options)" ] || fail "test override is missing security-options"
  security_options="$(cat "$(read_host /security-options)")"
else
  security_options="$(docker info --format '{{range .SecurityOptions}}{{println .}}{{end}}' 2>/dev/null || true)"
fi
printf '%s\n' "$security_options" | grep -q '^name=apparmor$' \
  || fail "the Docker daemon does not report AppArmor in its security options"
printf 'docker_security_options=%s\n' "$(printf '%s' "$security_options" | tr '\n' ',')"

# 5. The local-Fleet client only accepts a loopback URL, so API and Fleet must be
#    able to reach each other over 127.0.0.1 on this host. The override path is
#    used by the test only.
if [ -z "$root" ]; then
  docker version --format '{{.Server.Version}}' >/dev/null 2>&1 || fail "the Docker daemon is unreachable"
fi

printf 'preflight=ok\n'
```

- [ ] **Step 4: 运行测试确认通过**

Run: `bash scripts/aurora-runtime-host-preflight.test.sh`
Expected: `ok`

同时把新脚本挂进仓库既有的脚本测试门：

Run: `bash scripts/check.sh` 中的 `script-checks` 等价命令 —— `pnpm test` 不覆盖 shell，改跑 `bash scripts/aurora-runtime-host-preflight.test.sh`，并把它加入 `.github/workflows/ci.yml` 的 `script-checks` job（与其它 `*.test.sh` 并列）。

- [ ] **Step 5: 在 guest 上建环境并跑前置检查**

以下命令在 **Linux guest 内**执行（不是 Mac）。它是操作步骤，不是可选建议：G0 的全部实证都在这台机器上产生。

```bash
apparmor_parser -Q deploy/aurora-sandbox/multica-aurora-sandbox.apparmor
bash scripts/aurora-runtime-host-preflight.sh
```

Expected: 末行 `preflight=ok`，且第 4 行的 `docker_security_options=` 里含 `name=apparmor`。

- [ ] **Step 6: 在 guest 内起 API + Fleet + PostgreSQL 并核对契约**

在 guest 内按 `docker-compose.local-aurora.yml` 的同一拓扑启动，但 `MULTICA_LOCAL_FLEET_URL` 保持 `http://127.0.0.1:8090`（loopback 约束），`DATABASE_URL` 指向 guest 自己的 PostgreSQL：

```bash
make up C=api,fleet
make status
curl -fsS http://127.0.0.1:8090/readyz
curl -fsS "http://127.0.0.1:8080/health"
```

Expected: `make status` 两个组件均为 running；`/readyz` 返回 200；`/health` 返回 200。**若 Fleet 因为 `aurora apparmor: profile ... refusing to start` 拒绝启动**（`server/cmd/fleet/main.go` 的 preflight），说明 profile 名未配置或引擎不报告能力 —— 停在 Step 5，不要改配置绕过。

- [ ] **Step 7: 记录 G0 环境事实**

在本计划文件末尾新增 `## G0 实证记录` 小节，逐条写入：guest 的内核版本、`docker info` 的 security options 原文、`apparmor_parser -Q` 的输出、`make status` 输出、两个 HTTP 探测的响应码、执行日期与执行人。**只写实际发生的输出。** 任何一项没跑的，写「未执行」并说明原因。

- [ ] **Step 8: Commit**

```bash
git add scripts/aurora-runtime-host-preflight.sh scripts/aurora-runtime-host-preflight.test.sh .github/workflows/ci.yml docs/superpowers/plans/2026-10-08-unified-cloud-runtime-aurora.md
git commit -m "feat(aurora): add the Linux execution host preflight and record its evidence"
```

## Task 2: daemon 侧 broker 桥与单功能 MCP 客户端

规格 §8.2 要求 daemon 分别启动 broker 与 Claude、清理各自环境并监督生命周期；broker **不再**由 Claude 以拥有 provider 文件权限的通用 Node 命令启动。今天的实现正好相反：`auroraBrokerMcpConfig` 让 Claude 直接 `command: "node", args: ["/opt/aurora/.../server.mjs"]` 启动 broker。

**Files:**
- Create: `server/internal/daemon/aurorabridge/bridge.go`
- Create: `server/internal/daemon/aurorabridge/frame.go`
- Create: `server/internal/daemon/aurorabridge/peer_linux.go`
- Create: `server/internal/daemon/aurorabridge/peer_other.go`
- Create: `server/internal/daemon/aurorabridge/bridge_test.go`
- Create: `server/internal/daemon/aurorabridge/frame_test.go`
- Create: `server/cmd/multica-mcp-client/main.go`
- Create: `server/cmd/multica-mcp-client/main_test.go`
- Modify: `server/internal/daemon/aurora_broker.go`（`auroraBrokerMcpConfig` 改为指向新客户端）
- Modify: `docker/runtime/Dockerfile:22`（新增 `/usr/local/libexec/multica-mcp-client` 的构建与安装；本任务只加 `go build` 与 COPY，最终镜像折叠在 Task 4）

**Interfaces:**
- Produces: `aurorabridge.Start(ctx context.Context, cfg aurorabridge.Config) (*aurorabridge.Bridge, error)`。
- Produces: `(*aurorabridge.Bridge).SocketPath() string`、`.Wait() error`、`.Close(ctx context.Context) error`。
- Produces: `aurorabridge.Config{ SocketDir, BrokerEntrypoint string; BrokerEnv []string; NodeID, TaskID string; Attempt int32; ExpectedClientUID uint32; ExpectedClientLabel string }`。
- Produces: `/usr/local/libexec/multica-mcp-client <socket-path>`，stdio ↔ socket 双向转发，无子命令。
- Consumes: `model.AuroraClaudePath`、`auroraBrokerEntrypoint`（既有常量，本任务不改它们的值）。

- [ ] **Step 1: 写帧边界的失败测试**

创建 `server/internal/daemon/aurorabridge/frame_test.go`：

```go
package aurorabridge

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestFrameLimitsAreIndependentOfCallerInput(t *testing.T) {
	if MaxFrameBytes != 1<<20 {
		t.Fatalf("MaxFrameBytes = %d, want 1 MiB", MaxFrameBytes)
	}
	if MaxOutstandingCalls != 8 {
		t.Fatalf("MaxOutstandingCalls = %d, want 8", MaxOutstandingCalls)
	}
}

func TestReadFrameRejectsOversizeWithoutConsumingTheStream(t *testing.T) {
	oversize := bytes.Repeat([]byte("a"), MaxFrameBytes+1)
	payload, err := readFrame(bufio.NewReader(oversize))
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
	if payload != nil {
		t.Fatalf("payload = %d bytes, want nil", len(payload))
	}
}

func TestReadFrameReturnsWholeLinesAndEOF(t *testing.T) {
	r := bufio.NewReader(bytes.NewReader([]byte("{\"jsonrpc\":\"2.0\"}\n")))
	frame, err := readFrame(r)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if string(frame) != "{\"jsonrpc\":\"2.0\"}" {
		t.Fatalf("frame = %q", frame)
	}
	if _, err := readFrame(r); !errors.Is(err, io.EOF) {
		t.Fatalf("second read err = %v, want io.EOF", err)
	}
}

func TestSendBufferIsBounded(t *testing.T) {
	buf := newSendBuffer(MaxFrameBytes)
	if err := buf.write([]byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := buf.write(bytes.Repeat([]byte("y"), MaxFrameBytes)); !errors.Is(err, ErrSendBufferFull) {
		t.Fatalf("err = %v, want ErrSendBufferFull", err)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `(cd server && go test ./internal/daemon/aurorabridge -run 'TestFrame|TestReadFrame|TestSendBuffer' -count=1)`
Expected: FAIL —— `no Go files in .../aurorabridge`

- [ ] **Step 3: 实现 `frame.go`**

```go
// Package aurorabridge supervises the Aurora MCP broker as a child of the
// daemon and forwards exactly one task's MCP traffic over a task-scoped Unix
// socket. It is not an exec helper: nothing in this package accepts a command,
// a path, a provider address or a target socket from a caller.
package aurorabridge

import (
	"bufio"
	"errors"
	"io"
	"sync"
)

// Frame and concurrency bounds for one connection. They are constants, not
// configuration: a caller cannot widen them.
const (
	// MaxFrameBytes caps one newline-delimited JSON-RPC frame in either
	// direction. A larger frame rejects the connection; it never kills the
	// daemon.
	MaxFrameBytes = 1 << 20
	// MaxOutstandingCalls caps requests awaiting a response by id.
	MaxOutstandingCalls = 8
)

var (
	ErrFrameTooLarge   = errors.New("aurora bridge: frame too large")
	ErrSendBufferFull  = errors.New("aurora bridge: send buffer full")
	ErrPeerRejected    = errors.New("aurora bridge: peer rejected")
	ErrBridgeClosed    = errors.New("aurora bridge: closed")
)

// readFrame reads one newline-delimited frame. It refuses a frame longer than
// MaxFrameBytes without draining the stream: the caller rejects the connection.
func readFrame(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			buf = append(buf, chunk...)
			if len(buf) > MaxFrameBytes {
				return nil, ErrFrameTooLarge
			}
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(buf)+len(chunk) == 0 {
				return nil, io.EOF
			}
			if len(buf)+len(chunk) > MaxFrameBytes {
				return nil, ErrFrameTooLarge
			}
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
		}
		buf = append(buf, chunk...)
		break
	}
	buf = bytesTrimNewline(buf)
	if len(buf) == 0 {
		return nil, ErrFrameTooLarge
	}
	return buf, nil
}

func bytesTrimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// sendBuffer is a bounded FIFO of outbound frames. Overflow rejects the
// connection instead of growing without limit.
type sendBuffer struct {
	mu     sync.Mutex
	max    int
	buf    []byte
	closed bool
}

func newSendBuffer(max int) *sendBuffer { return &sendBuffer{max: max} }

func (s *sendBuffer) write(frame []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrBridgeClosed
	}
	if len(s.buf)+len(frame)+1 > s.max {
		return ErrSendBufferFull
	}
	s.buf = append(s.buf, frame...)
	s.buf = append(s.buf, '\n')
	return nil
}

func (s *sendBuffer) take() ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) == 0 {
		return nil, false
	}
	out := s.buf
	s.buf = nil
	return out, true
}

func (s *sendBuffer) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.buf = nil
}
```

- [ ] **Step 4: 运行帧测试确认通过**

Run: `(cd server && go test ./internal/daemon/aurorabridge -run 'TestFrame|TestReadFrame|TestSendBuffer' -count=1)`
Expected: PASS（`readFrame` 需要 `bufio.Reader`；若签名报错，把测试里的 `bytes.NewReader` 用 `bufio.NewReader` 包一层，两处一起改）

- [ ] **Step 5: 写 peer 校验的失败测试（Linux 语义在非 Linux 上必须 fail closed）**

在 `bridge_test.go` 追加：

```go
package aurorabridge

import (
	"errors"
	"runtime"
	"testing"
)

func TestPeerCheckFailsClosedOffLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("covered by the Linux path")
	}
	_, err := verifyPeer(1, 0, "unconfined")
	if !errors.Is(err, ErrPeerRejected) {
		t.Fatalf("err = %v, want ErrPeerRejected off Linux", err)
	}
}

func TestConfigRejectsIncompleteIdentity(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no socket dir":     {BrokerEntrypoint: "/opt/x", TaskID: "t", NodeID: "n", Attempt: 1},
		"no task id":        {SocketDir: "/run/x", BrokerEntrypoint: "/opt/x", NodeID: "n", Attempt: 1},
		"no node id":        {SocketDir: "/run/x", BrokerEntrypoint: "/opt/x", TaskID: "t", Attempt: 1},
		"no attempt":        {SocketDir: "/run/x", BrokerEntrypoint: "/opt/x", TaskID: "t", NodeID: "n"},
		"relative socket":   {SocketDir: "run/x", BrokerEntrypoint: "/opt/x", TaskID: "t", NodeID: "n", Attempt: 1},
		"relative entrypoint": {SocketDir: "/run/x", BrokerEntrypoint: "./server.mjs", TaskID: "t", NodeID: "n", Attempt: 1},
	} {
		if cfg.validate() == nil {
			t.Fatalf("%s: validate() accepted an incomplete config", name)
		}
	}
}
```

- [ ] **Step 6: 运行测试确认失败**

Run: `(cd server && go test ./internal/daemon/aurorabridge -run 'TestPeerCheckFailsClosedOffLinux|TestConfigRejectsIncompleteIdentity' -count=1)`
Expected: FAIL —— `undefined: verifyPeer`、`cfg.validate undefined`

- [ ] **Step 7: 实现 `bridge.go` 与两个 peer 实现**

`bridge.go`（完整实现；`BrokerEnv` 由调用方给出固定 allowlist，本包不读 `os.Environ()`）：

```go
package aurorabridge

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SocketFileMode is the DAC baseline. It is not task isolation on its own: two
// processes under the same UID can both open a 0600 socket, which is why the
// bridge additionally verifies the peer credential and the AppArmor label.
const SocketFileMode os.FileMode = 0o600

// SocketName is fixed so the daemon-written MCP config can name it without
// accepting a caller-supplied path.
const SocketName = "mcp.sock"

type Config struct {
	SocketDir        string
	BrokerEntrypoint string
	BrokerEnv        []string
	NodeID           string
	TaskID           string
	Attempt          int32
	// ExpectedClientUID is the sandbox user the MCP client must run as.
	ExpectedClientUID uint32
	// ExpectedClientLabel is the AppArmor label the MCP client must carry. Read
	// from /proc/<pid>/attr/current; an empty value disables only this check and
	// is never used by the managed profile.
	ExpectedClientLabel string
}

func (c Config) validate() error {
	if !filepath.IsAbs(c.SocketDir) || !filepath.IsAbs(c.BrokerEntrypoint) {
		return fmt.Errorf("%w: socket dir and broker entrypoint must be absolute", ErrPeerRejected)
	}
	if strings.TrimSpace(c.NodeID) == "" || strings.TrimSpace(c.TaskID) == "" || c.Attempt <= 0 {
		return fmt.Errorf("%w: incomplete task identity", ErrPeerRejected)
	}
	return nil
}

// Bridge owns one broker child process and one task-scoped socket.
type Bridge struct {
	cfg      Config
	socket   string
	listener *net.UnixListener
	cmd      *exec.Cmd

	closeOnce sync.Once
	done      chan struct{}
	waitErr   error
}

// Start launches the broker as a child of the daemon and begins serving the
// task socket. The broker's own environment is exactly cfg.BrokerEnv: the
// daemon's environment, the node token and every other role's credentials are
// deliberately absent.
func Start(ctx context.Context, cfg Config) (*Bridge, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.SocketDir, 0o700); err != nil {
		return nil, err
	}
	socket := filepath.Join(cfg.SocketDir, SocketName)
	// A stale socket from a previous attempt must not be adopted.
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socket, SocketFileMode); err != nil {
		_ = ln.Close()
		return nil, err
	}

	cmd := exec.Command(cfg.BrokerEntrypoint)
	cmd.Env = cfg.BrokerEnv
	cmd.Dir = cfg.SocketDir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	cmd.Stderr = nil // the broker's diagnostics never reach the task or the model
	if err := cmd.Start(); err != nil {
		_ = ln.Close()
		return nil, err
	}

	b := &Bridge{cfg: cfg, socket: socket, listener: ln, cmd: cmd, done: make(chan struct{})}
	go b.serve(ctx, stdin, stdout)
	go func() {
		b.waitErr = cmd.Wait()
		b.closeOnce.Do(func() { close(b.done) })
	}()
	return b, nil
}

func (b *Bridge) SocketPath() string { return b.socket }

// Wait blocks until the broker exits.
func (b *Bridge) Wait() error {
	<-b.done
	return b.waitErr
}

// Close stops accepting new tool calls first, then closes the socket and the
// broker. Cancellation order is part of the contract: a half-closed bridge must
// never leave the model able to start a new provider call.
func (b *Bridge) Close(ctx context.Context) error {
	_ = b.listener.Close()
	var err error
	if b.cmd.Process != nil {
		_ = b.cmd.Process.Signal(os.Interrupt)
		select {
		case <-b.done:
		case <-time.After(5 * time.Second):
			err = b.cmd.Process.Kill()
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	return err
}

// serve accepts at most one live client per attempt. A reconnect closes the
// previous connection rather than running two.
func (b *Bridge) serve(ctx context.Context, stdin ioWriteCloser, stdout ioReadCloser) {
	// implementation continues in Step 9
	_ = stdin
	_ = stdout
	_ = ctx
}
```

`peer_linux.go`：

```go
//go:build linux

package aurorabridge

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// verifyPeer answers whether a connecting process is the expected MCP client.
// It checks three independent facts: the credential the kernel reports for the
// peer socket, the sandbox UID, and the load-verified AppArmor label of the
// peer. A 0600 socket alone proves none of them.
func verifyPeer(fd int, wantUID uint32, wantLabel string) (Peer, error) {
	raw, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return Peer{}, fmt.Errorf("%w: peer credential: %v", ErrPeerRejected, err)
	}
	if raw.Uid != wantUID {
		return Peer{}, fmt.Errorf("%w: peer uid %d, want %d", ErrPeerRejected, raw.Uid, wantUID)
	}
	label, err := peerLabel(raw.Pid)
	if err != nil {
		return Peer{}, err
	}
	if wantLabel != "" && label != wantLabel {
		return Peer{}, fmt.Errorf("%w: peer label %q, want %q", ErrPeerRejected, label, wantLabel)
	}
	return Peer{PID: raw.Pid, UID: raw.Uid, GID: raw.Gid, Label: label}, nil
}

func peerLabel(pid int32) (string, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/attr/current", pid))
	if err != nil {
		return "", fmt.Errorf("%w: peer label: %v", ErrPeerRejected, err)
	}
	return strings.TrimSpace(string(raw)), nil
}

type Peer struct {
	PID   int32
	UID   uint32
	GID   uint32
	Label string
}
```

`peer_other.go`：

```go
//go:build !linux

package aurorabridge

import "fmt"

// Peer is never produced off Linux: the bridge is a Linux-only execution
// surface and must fail closed rather than fall back to UID equality.
type Peer struct {
	PID   int32
	UID   uint32
	GID   uint32
	Label string
}

func verifyPeer(fd int, wantUID uint32, wantLabel string) (Peer, error) {
	return Peer{}, fmt.Errorf("%w: peer verification requires Linux", ErrPeerRejected)
}
```

- [ ] **Step 8: 运行测试确认通过**

Run: `(cd server && go test ./internal/daemon/aurorabridge -count=1)`
Expected: PASS（macOS 上 `TestPeerCheckFailsClosedOffLinux` 走 `peer_other.go` 分支；Linux 上该用例 Skip，由 Task 3 的矩阵覆盖实际语义）

Run: `(cd server && go vet ./internal/daemon/aurorabridge)`
Expected: 无输出

- [ ] **Step 9: 实现 `serve` 的完整转发与超限拒绝**

把 `bridge.go` 的 `serve` 换成完整实现。要点：只接受一个活动客户端；超过 `MaxOutstandingCalls` 的请求按 id 计数；任一方向超帧即关闭该连接而不杀 daemon；客户端断开时关闭 broker 的 stdin。

```go
type ioWriteCloser interface{ Write([]byte) (int, error); Close() error }
type ioReadCloser  interface{ Read([]byte) (int, error); Close() error }

func (b *Bridge) serve(ctx context.Context, stdin ioWriteCloser, stdout ioReadCloser) {
	for {
		conn, err := b.listener.AcceptUnix()
		if err != nil {
			return // listener closed: Close() was called
		}
		if err := b.serveConn(ctx, conn, stdin, stdout); err != nil {
			// One rejected client never takes the daemon or the broker down:
			// the loop stays alive so a legitimate attempt can reconnect.
			continue
		}
	}
}

func (b *Bridge) serveConn(ctx context.Context, conn *net.UnixConn, stdin ioWriteCloser, stdout ioReadCloser) error {
	fd, err := conn.File()
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer func() { _ = fd.Close() }()
	if _, err := verifyPeer(int(fd.Fd()), b.cfg.ExpectedClientUID, b.cfg.ExpectedClientLabel); err != nil {
		_ = conn.Close()
		return err
	}
	defer func() { _ = conn.Close() }()

	out := newSendBuffer(MaxFrameBytes)
	done := make(chan struct{})
	go b.pumpBrokerToClient(stdout, out, conn, done)
	b.pumpClientToBroker(ctx, conn, stdin, out)
	close(done)
	return nil
}
```

同时实现两个方向的泵：

```go
// pumpClientToBroker forwards one client's frames to the broker. Cancellation
// stops accepting new tool calls first (the client's next request is refused),
// and only then does the connection close.
func (b *Bridge) pumpClientToBroker(ctx context.Context, conn *net.UnixConn, stdin ioWriteCloser, out *sendBuffer) {
	reader := bufio.NewReader(conn)
	pending := 0
	for {
		if ctx.Err() != nil {
			return
		}
		frame, err := readFrame(reader)
		if err != nil {
			return // EOF, oversize, or a closed connection: this client only
		}
		if isRequest(frame) {
			pending++
			if pending > MaxOutstandingCalls {
				return // reject the connection, never the daemon
			}
		}
		if isCancellation(frame) {
			if pending > 0 {
				pending--
			}
		}
		if _, err := stdin.Write(append(frame, '\n')); err != nil {
			return
		}
		if id, ok := responseID(frame); ok {
			_ = id
		}
	}
}

// pumpBrokerToClient forwards the broker's frames back to the client, draining
// the bounded buffer. A full buffer is not an error here: the client pump fails
// closed on the request that overflows it.
func (b *Bridge) pumpBrokerToClient(stdout ioReadCloser, out *sendBuffer, conn *net.UnixConn, done <-chan struct{}) {
	reader := bufio.NewReader(stdout)
	for {
		select {
		case <-done:
			_ = stdout.Close()
			return
		default:
		}
		frame, err := readFrame(reader)
		if err != nil {
			return
		}
		if err := out.write(frame); err != nil {
			return
		}
		for {
			chunk, ok := out.take()
			if !ok {
				break
			}
			if _, err := conn.Write(chunk); err != nil {
				return
			}
		}
	}
}
```

`isRequest` / `isCancellation` / `responseID` 是三个小型 JSON 判定（解析 `jsonrpc`、`id`、`method`），放在 `frame.go` 里，各自有表驱动测试。

- [ ] **Step 10: 写并实现单功能 MCP 客户端**

创建 `server/cmd/multica-mcp-client/main.go`：

```go
// multica-mcp-client is the only executable Claude may use to reach the Aurora
// MCP broker. It has no subcommands, no target selection and no provider
// credential: its single argument is the task socket the daemon created, and
// everything else is fixed by this file.
package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
)

// usage is deliberately not a help text: an unrecognised invocation fails
// closed and prints nothing a model could use to discover another surface.
const expectedArgs = 1

func main() {
	status, err := run(os.Args[1:], os.Stdin, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "multica-mcp-client failed")
	}
	os.Exit(status)
}

func run(args []string, stdin io.Reader, stdout io.Writer) (int, error) {
	if len(args) != expectedArgs {
		return 2, errors.New("expected exactly one socket path")
	}
	path := args[0]
	if len(path) == 0 || path[0] != '/' {
		return 2, errors.New("socket path must be absolute")
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		return 1, err
	}
	defer func() { _ = conn.Close() }()
	return pipe(stdin, stdout, conn), nil
}
```

`pipe` 的两个方向（完整实现，放在同一文件）：

```go
// pipe forwards both directions until either side ends. It never buffers more
// than one frame and never interprets the payload: the client is a transport,
// not a policy point.
func pipe(stdin io.Reader, stdout io.Writer, conn net.Conn) int {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(conn, stdin)
		_ = conn.(*net.UnixConn).CloseWrite()
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(stdout, conn)
		done <- struct{}{}
	}()
	<-done
	_ = conn.Close()
	return 0
}
```

`main_test.go` 覆盖：0 个参数 → 退出码 2；两个参数 → 2；相对路径 → 2；不存在的绝对路径 → 退出码 1 且 stdout 为空。

- [ ] **Step 11: 把 daemon 的 MCP 配置改指向新客户端**

修改 `server/internal/daemon/aurora_broker.go`：`auroraBrokerMcpConfig` 产出的 server 条目从

```go
Command: "node", Args: []string{auroraBrokerEntrypoint}
```

改为

```go
Command: auroraMCPClientPath, Args: []string{socketPath}
```

并新增常量：

```go
// auroraMCPClientPath is the fixed single-function MCP client. Claude's config
// must not name `node` or the broker entrypoint directly: the broker is a child
// of the daemon, and the model never gets an interpreter it can point anywhere.
const auroraMCPClientPath = "/usr/local/libexec/multica-mcp-client"
```

`auroraBrokerEntrypoint` 保留（daemon 用它启动子进程）。`writeAuroraBrokerContext` 与 `stageAuroraBrokerAttachments` 不变。

- [ ] **Step 12: 更新 daemon 侧测试**

Run: `(cd server && go test ./internal/daemon -run 'TestAuroraBroker' -count=1)`
Expected: 先 FAIL —— `TestAuroraBrokerMcpConfigShape` 仍断言 `command == "node"`。把该断言改为 `command == "/usr/local/libexec/multica-mcp-client"`，`args` 改为单个绝对 socket 路径，并新增一条断言：配置里**不出现** `node`、`server.mjs` 或 `fleet-node`。

- [ ] **Step 13: 运行并通过**

Run: `(cd server && go test ./internal/daemon -run 'TestAuroraBroker|TestAuroraSurface' -count=1)`
Expected: PASS

Run: `(cd server && go test ./internal/daemon -run 'TestManagedSecret|TestProviderSecretMount' -count=1)`
Expected: PASS（镜像内自检也跑这一组）

- [ ] **Step 14: Commit**

```bash
git add server/internal/daemon/aurorabridge server/cmd/multica-mcp-client server/internal/daemon/aurora_broker.go server/internal/daemon/aurora_broker_test.go
git commit -m "feat(aurora): supervise the MCP broker from the daemon behind a task socket"
```

## Task 3: 进程隔离原型与允许/拒绝矩阵

现有 profile 用 `/{,usr/}bin/** mr` + `/{,usr/}bin/** ix`、`/opt/** ix` 这类**目录级执行通配**授权（`deploy/aurora-sandbox/multica-aurora-sandbox.apparmor:84-106`），审查 R2 明确指出：同一镜像里既有管理入口又有模型入口时，按路径通配授权等于把管理权限一起交出去。

本任务把它改成**父 profile + 按可执行路径的 `cx` 子 profile**，并在目标内核上实测「允许路径照常、越权路径被拒」。规格 §8.2 特别提醒：`no_new_privs` 与 LSM 的 exec 收紧存在相互作用，**必须实测**，不能凭配置推定。

**Files:**
- Modify: `deploy/aurora-sandbox/multica-aurora-sandbox.apparmor`
- Create: `deploy/aurora-sandbox/fixture/matrix/run-matrix.sh`
- Create: `deploy/aurora-sandbox/fixture/matrix/cases.json`
- Modify: `server/cmd/aurora-sandbox-probe/main.go`、`run_linux.go`
- Modify: `deploy/aurora-sandbox/docker-security-test.sh`（调用矩阵脚本并把它纳入 `AURORA_DOCKER_SECURITY_COUNT` 的重复次数）

**Interfaces:**
- Produces: 父 profile `multica-aurora-sandbox`（Docker `--security-opt apparmor=` 引用它）与子 profile `multica-aurora-node-{daemon,claude,broker,render,mcp-client}`、`multica-aurora-egress`。
- Produces: `aurora-sandbox-probe --case <id>`，只接受 `cases.json` 里枚举的 id；每个 id 输出一行 JSON `{"case":"<id>","attempt":"<what>","errno":"<n>","verdict":"allowed|denied|absent"}`。
- Consumes: Task 2 的 `/usr/local/libexec/multica-mcp-client`、`/usr/local/bin/fleet-node`、`/usr/local/bin/claude`。

- [ ] **Step 1: 写矩阵用例清单**

创建 `deploy/aurora-sandbox/fixture/matrix/cases.json`。每条含 `id`、`profile`（用哪个子 profile 起进程）、`expect`（`allow` 或 `deny`）、`why`。至少包含：

```json
{
  "schema": "com.multica.aurora.isolation-matrix",
  "version": 1,
  "cases": [
    {"id": "claude-version",        "profile": "multica-aurora-node-claude", "expect": "allow", "why": "the model's own executable must run"},
    {"id": "broker-node-entry",     "profile": "multica-aurora-node-broker", "expect": "allow", "why": "the trusted broker runs a fixed Node entry"},
    {"id": "mcp-client-connect",    "profile": "multica-aurora-node-mcp-client", "expect": "allow", "why": "the single-function client must reach the task socket"},
    {"id": "render-chromium",       "profile": "multica-aurora-node-render", "expect": "allow", "why": "HyperFrames/Chromium PDF print must work"},
    {"id": "render-loopback",       "profile": "multica-aurora-node-render", "expect": "allow", "why": "the renderer uses an internal loopback static server"},
    {"id": "egress-proxy-serve",    "profile": "multica-aurora-egress", "expect": "allow", "why": "the sidecar is the only controlled egress"},
    {"id": "claude-exec-shell",     "profile": "multica-aurora-node-claude", "expect": "deny", "why": "/bin/sh must not be reachable from the model"},
    {"id": "claude-exec-node",      "profile": "multica-aurora-node-claude", "expect": "deny", "why": "a generic interpreter would become the whole escape"},
    {"id": "claude-exec-fleet-node", "profile": "multica-aurora-node-claude", "expect": "deny", "why": "management entrypoints are not the model's"},
    {"id": "claude-read-ark-key",   "profile": "multica-aurora-node-claude", "expect": "deny", "why": "provider credentials belong to the broker"},
    {"id": "claude-read-session",   "profile": "multica-aurora-node-claude", "expect": "deny", "why": "node identity files are daemon-only"},
    {"id": "claude-read-peer-task", "profile": "multica-aurora-node-claude", "expect": "deny", "why": "another task's socket and files are out of scope"},
    {"id": "claude-node-options",   "profile": "multica-aurora-node-claude", "expect": "deny", "why": "NODE_OPTIONS injection must not load a writable module"},
    {"id": "claude-ld-preload",     "profile": "multica-aurora-node-claude", "expect": "deny", "why": "the dynamic loader must not read a writable library"},
    {"id": "claude-direct-egress",  "profile": "multica-aurora-node-claude", "expect": "deny", "why": "no path to the public internet except the sidecar"},
    {"id": "claude-exec-writable",  "profile": "multica-aurora-node-claude", "expect": "deny", "why": "the workspace tmpfs is noexec"},
    {"id": "daemon-read-ark-key",   "profile": "multica-aurora-node-daemon", "expect": "deny", "why": "the daemon supervises, it does not hold provider secrets"},
    {"id": "broker-exec-shell",     "profile": "multica-aurora-node-broker", "expect": "deny", "why": "user input must never become a command"},
    {"id": "render-read-ark-key",   "profile": "multica-aurora-node-render", "expect": "deny", "why": "the transcoder needs inputs, not credentials"},
    {"id": "egress-exec-fleet-node", "profile": "multica-aurora-egress", "expect": "deny", "why": "the sidecar must not run management entries"}
  ]
}
```

- [ ] **Step 2: 写矩阵脚本的失败测试（先证明它会拒绝一个错误结论）**

在 `deploy/aurora-sandbox/fixture/matrix/` 下新增 `run-matrix.test.sh`，断言：当某个 `expect=deny` 的用例实际返回 `allowed` 时脚本以非零退出并打印该用例 id；当输入为空时以退出码 3（环境无法评估）退出。先写测试、跑红：

Run: `bash deploy/aurora-sandbox/fixture/matrix/run-matrix.test.sh`
Expected: FAIL —— `run-matrix.sh: No such file or directory`

- [ ] **Step 3: 实现 `run-matrix.sh`**

要点（沿用 `deploy/aurora-sandbox/fleet-sandbox-acceptance.sh` 的既有约定）：

- 门：`AURORA_RUN_DOCKER_SECURITY_TEST` 必须为 `1`，否则打印 `--- SKIP: ...` 并以退出码 3 结束（**诚实跳过，绝不记为通过**）。
- `uname -s` 必须是 Linux；`apparmor_parser` 必须存在，否则退出码 3 并说明。
- 加载 profile：`apparmor_parser -Q` 校验后 `apparmor_parser -r deploy/aurora-sandbox/multica-aurora-sandbox.apparmor`。
- 每个用例在 **fixture 探针镜像**（`AURORA_SANDBOX_IMAGE`，来自 `fixture/Dockerfile.sandbox`）里跑一次，带上真实 seccomp 与 AppArmor：`--security-opt "seccomp=$seccomp_profile" --security-opt "apparmor=$apparmor_profile"`，`--user 10001:10001`，`--read-only`，`--cap-drop ALL`，`--security-opt no-new-privileges:true`，`--tmpfs /workspace:rw,nosuid,nodev,noexec,size=2147483648,uid=10001,gid=10001,mode=0700`（与 `docker/egress.go` 的 `auroraWorkspaceTmpfs` 一致）。
- 每个用例解析探针的 JSON 行，比对 `expect`；不一致就打印 `--- FAIL: <id> (expect=deny got=allowed)` 并置累计失败。
- 结束时打印 `--- PASS: <id> (<n>s)` / `--- FAIL: ...` / `--- SKIP: ...`，退出码 0/1/3 与既有脚本一致。

**子 profile 的切换方式**（这是本任务的技术核心）：父 profile 用 `cx` 把固定入口切到子 profile；`cx` 不需要特权，因此在 `no_new_privs` 下仍可用，但**必须在目标内核上实测**。矩阵脚本的第 0 号用例就是这条实测：

```json
{"id": "policy-transition", "profile": "multica-aurora-sandbox", "expect": "allow", "why": "cx transitions must work under no-new-privileges on the target kernel"}
```

探针 `--case policy-transition` 会 exec `/usr/local/libexec/multica-mcp-client --print-profile` 并回读 `/proc/self/attr/current`。**若该用例失败**（`cx` 在 NNP 下被拒），按规格 §8.2「切换失败不能退回继承宽权限，不能关闭 no-new-privileges 换取通过」：**G0 判负**，停止 M1 之后的发布型工作，把结论写进 G0 记录，交回设计修订。不要用单一宽 profile 代替进程分离。

- [ ] **Step 4: 收紧 profile**

改写 `deploy/aurora-sandbox/multica-aurora-sandbox.apparmor`：

- 保留现有全部 `deny` 规则（网络 raw/packet、`/proc/*/mem`、ptrace、`/sys/**` 等），保留 `#include <abstractions/base>`。**不要**改成 `deny /**`（文件头部已说明这种写法会盖掉必须的允许）。
- 删除 `/{,usr/}bin/** ix`、`/{,usr/}sbin/** ix`、`/usr/local/** ix`、`/usr/libexec/** ix`、`/opt/** ix` 这六条目录级执行通配，用 `mr` 保留映射与读取（动态库加载需要）。
- 逐条添加固定入口的执行授权，每个入口显式 `cx -> <child>`：

```
/usr/local/bin/fleet-node            cx -> multica-aurora-node-daemon,
/usr/local/bin/multica               cx -> multica-aurora-node-daemon,
/usr/local/bin/claude                cx -> multica-aurora-node-claude,
/usr/local/bin/node                  cx -> multica-aurora-node-broker,
/usr/local/libexec/multica-mcp-client cx -> multica-aurora-node-mcp-client,
/usr/bin/chromium                    cx -> multica-aurora-node-render,
/usr/bin/chromium-browser            cx -> multica-aurora-node-render,
/usr/bin/ffmpeg                      cx -> multica-aurora-node-render,
/usr/bin/ffprobe                     cx -> multica-aurora-node-render,
/usr/bin/convert                     cx -> multica-aurora-node-render,
/usr/bin/pdftoppm                    cx -> multica-aurora-node-render,
/usr/bin/pdfinfo                     cx -> multica-aurora-node-render,
/usr/bin/unzip                       cx -> multica-aurora-node-render,
/dev/init                            cx -> multica-aurora-node-daemon,
```

- 新增六个子 profile，各自只放该角色需要的东西。要点（每个子 profile 都从 `#include <abstractions/base>` 起，且不继承父 profile 的任意执行权）：

| 子 profile | 允许 | 必须显式 deny |
| --- | --- | --- |
| `multica-aurora-node-daemon` | `/data/** rwk`、`/secrets/** r`、`/run/** rwk`、`/usr/local/sbin:/usr/local/bin` 下**仅** `fleet-node`、`multica` 的执行 | `/run/secrets/**`（provider 密钥）、当前任务外的 `/run/aurora/**` |
| `multica-aurora-node-claude` | 自己的会话目录 `/data/workspaces/**`、模型连接、`/usr/local/libexec/multica-mcp-client` 执行 | `sh`/`bash`/`node`/`fleet-node`/`multica` 的执行、`/run/secrets/**`、`/data/identity/**`、非本任务 socket |
| `multica-aurora-node-broker` | `/opt/aurora/runtime/**` 只读 + `node` 执行、`/run/secrets/{ark,openai,volc-asr}-api-key r`、本任务输入输出目录 | `/run/secrets/anthropic-api-key`、`/data/identity/**`、shell |
| `multica-aurora-node-mcp-client` | 本任务的 `/run/aurora/<task>/mcp.sock rw` | 其余一切 |
| `multica-aurora-node-render` | `/usr/bin/{chromium,ffmpeg,ffprobe,convert,pdftoppm,pdfinfo,unzip}`、任务输入输出、自己的 loopback | `/run/secrets/**`、daemon health/control、提供方代理 |
| `multica-aurora-egress` | 仅 `/usr/local/bin/aurora-egress-proxy` 与 CA bundle | `/data/**`、`/run/secrets/**`、`fleet-node`、shell |

`/proc/**/attr/current` 的读取要允许（peer label 校验需要），但 `/proc/*/mem` 与 ptrace 的 deny 必须保持。

- [ ] **Step 5: 扩展探针**

在 `server/cmd/aurora-sandbox-probe` 里新增 `--case <id>` 与 `--print-profile`。`--case` 的实现：把 `cases.json` 的 id 映射到一段**固定代码**（不是从 argv 取路径），执行动作并把结果编码成一行 JSON。`errno` 用 `errors.Is(err, os.ErrPermission)` → `EACCES`、`os.IsNotExist` → `ENOENT`，并区分 `verdict`：`allowed`（动作成功）、`denied`（EACCES/EPERM）、`absent`（ENOENT）。

`--print-profile` 打印 `/proc/self/attr/current` 并退出 0；它是 `policy-transition` 用例的观测点。

Run: `(cd server && go build ./cmd/aurora-sandbox-probe && go test ./cmd/aurora-sandbox-probe/... -count=1)`
Expected: PASS

- [ ] **Step 6: 在 guest 上跑矩阵**

```bash
bash deploy/aurora-sandbox/fixture/matrix/run-matrix.test.sh
AURORA_RUN_DOCKER_SECURITY_TEST=1 AURORA_DOCKER_SECURITY_COUNT=2 AURORA_SANDBOX_IMAGE="$(docker build -q -f deploy/aurora-sandbox/fixture/Dockerfile.sandbox -o type=image .)" bash deploy/aurora-sandbox/fixture/matrix/run-matrix.sh
```

Expected: 每个用例各出现两次 `--- PASS: <id>`；末行退出码 0。任何 `--- FAIL` 或退出码 3 都必须原样记录，不得改写。

- [ ] **Step 7: 把矩阵接进既有验收脚本与 CI**

修改 `deploy/aurora-sandbox/docker-security-test.sh`：在调用 `fleet-sandbox-acceptance.sh` 之后追加矩阵调用，沿用同一个 `AURORA_DOCKER_SECURITY_COUNT`，并把矩阵报告写进 `AURORA_ACCEPTANCE_REPORT_DIR`。`docker-security-test.sh` 的 `honest_skip` 语义保持不变：环境不可评估 → 退出码 3。

Run: `bash scripts/check.sh` 里的 `script-checks` 不覆盖 `deploy/`；改跑 `node scripts/verify-aurora-sandbox-locks.mjs --workflow` 确认工作流策略检查仍通过。

- [ ] **Step 8: 记录 G0 结论**

在本计划末尾的 `## G0 实证记录` 里追加：profile 加载输出、`policy-transition` 用例的 `attr/current` 回读值、每个 `expect=deny` 用例的实测 `errno`、Chromium 与 HyperFrames 的实际入口路径（可能是 shell wrapper；若是，记录最终解析出的真实可执行文件并把 `cx` 规则指向它）、执行日期。**G0 未通过时，M1 的 publish 路径与 M5 的切换一律不得开始。**

- [ ] **Step 9: Commit**

```bash
git add deploy/aurora-sandbox/multica-aurora-sandbox.apparmor deploy/aurora-sandbox/fixture/matrix deploy/aurora-sandbox/docker-security-test.sh server/cmd/aurora-sandbox-probe
git commit -m "feat(aurora): authorize the node by fixed entrypoint and prove the isolation matrix"
```

---

# M1 · G1 唯一镜像与受控服务契约

## Task 4: 折叠为单一节点镜像与入口收敛

`docker/runtime/Dockerfile`（31 行，通用节点）与 `deploy/aurora-sandbox/Dockerfile`（241 行，5 个 stage，Aurora 专用）合并为一个文件、一个最终运行 stage；`Dockerfile.egress` 的独立构建入口删除，egress 改成用同一个镜像的固定代理入口启动。

**Files:**
- Modify: `docker/runtime/Dockerfile`（成为唯一构建入口）
- Create: `docker/runtime/.dockerignore`
- Delete: `deploy/aurora-sandbox/Dockerfile`、`deploy/aurora-sandbox/Dockerfile.egress`、`docker/runtime/claude-version.txt`
- Modify: `deploy/aurora-sandbox/docker-bake.hcl`
- Modify: `server/internal/fleet/model/aurora.go`（删 `ProxyImage`、改 `AuroraClaudePath`）
- Modify: `server/internal/fleet/docker/{provider.go,egress.go,inspect.go}`
- Modify: `deploy/aurora-sandbox/versions.json`
- Modify: `fleet-config.example.json`

**Interfaces:**
- Produces: 镜像契约 —— `ENTRYPOINT ["/usr/local/bin/fleet-node","run"]`；可用入口 `/usr/local/bin/aurora-egress-proxy`、`/usr/local/libexec/multica-mcp-client`；`/usr/local/bin/claude`；`USER 10001:10001`；`HOME=/data/home`；`/data`、`/data/home`、`/data/workspaces`、`/secrets` 属 `10001:10001`。
- Produces: `model.AuroraClaudePath = "/usr/local/bin/claude"`。
- Consumes: Task 2 的 `cmd/multica-mcp-client`、Task 3 的子 profile 名。

- [ ] **Step 1: 先改契约常量，让编译器强制暴露所有引用点**

在 `server/internal/fleet/model/aurora.go` 把

```go
AuroraClaudePath = "/opt/aurora/runtime/node_modules/.bin/claude"
```

改为

```go
// AuroraClaudePath is the single Claude entry for both the ordinary and the
// Aurora execution profile. It is root-owned and read-only inside the image, so
// a workspace PATH or a user file cannot shadow it.
AuroraClaudePath = "/usr/local/bin/claude"
```

并删除 `AuroraConfig.ProxyImage` 字段、`proxy_image` 的 JSON tag 与 `Validate()` 里对应的 digest 校验块。

- [ ] **Step 2: 运行测试，收集所有必须一起改的断言**

Run: `(cd server && go build ./... && go test ./internal/fleet/... -count=1)`
Expected: FAIL，且失败点应至少覆盖：`model/aurora.go` 的 `proxy_image` 校验测试、`docker/egress.go` 的 `a.ProxyImage`、`docker/provider.go` 的 `nodeEnv`、`fleet-config.example.json` 的加载测试。逐个改掉：

- `docker/egress.go` `egressProxySpec` 的 `Image: a.ProxyImage` → `Image: cfg.Image`（`Config` 的节点镜像，与节点同一个 digest）。
- `docker/provider.go` `nodeEnv` 里的 `model.AuroraClaudePathEnv` 值随之变为 `/usr/local/bin/claude`（不写死字符串，继续引用常量）。
- `docker/inspect.go` 的 adoption 检查同样引用常量，因此只需确认它比较的是 `nodeEnv` 的输出而不是字面量。
- `fleet-config.example.json` 删除 `proxy_image` 行。

- [ ] **Step 3: 写单一镜像的契约测试（红）**

在 `server/internal/fleet/model/aurora_test.go` 追加：

```go
func TestAuroraConfigHasNoSeparateProxyImage(t *testing.T) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(exampleAuroraConfigJSON(t)), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := raw["proxy_image"]; present {
		t.Fatal("proxy_image still appears in the Aurora config: node and sidecar must share one image")
	}
}

func TestAuroraClaudePathIsTheUnifiedEntry(t *testing.T) {
	if AuroraClaudePath != "/usr/local/bin/claude" {
		t.Fatalf("AuroraClaudePath = %q, want the unified entry", AuroraClaudePath)
	}
}
```

Run: `(cd server && go test ./internal/fleet/model -run 'TestAuroraConfigHasNoSeparateProxyImage|TestAuroraClaudePathIsTheUnifiedEntry' -count=1)`
Expected: 先 FAIL（`proxy_image` 仍在样例 JSON 里），删掉后 PASS

- [ ] **Step 4: 写统一 Dockerfile**

做法是**移动既有内容**而不是重写：`deploy/aurora-sandbox/Dockerfile` 的五个 stage 逐行搬进 `docker/runtime/Dockerfile`，digest 与锁定版本一律不重新抄写。逐条执行：

1. 删除 `docker/runtime/Dockerfile` 现有的全部 31 行（它的 `builder`/`claude`/`runtime` 三个 stage 被下面的 stage 取代；`git rm docker/runtime/claude-version.txt`）。
2. 把 `deploy/aurora-sandbox/Dockerfile:1-18` 的头注释搬到新文件顶部，并按新的单一产物语义改写（节点与代理同镜像、两个固定入口；四个入口；不再有第二个 target）。
3. 逐字搬 `deploy/aurora-sandbox/Dockerfile:23-52`（`gobuilder` stage），**只改两处**：
   - `go build` 行增加 `-o /out/aurora-egress-proxy ./cmd/aurora-egress-proxy` 与 `-o /out/multica-mcp-client ./cmd/multica-mcp-client`（沿用同一组 `-trimpath -buildvcs=false -ldflags="-s -w -buildid="`）；
   - 保留 `:41-42` 的 `go test ./internal/daemon -run 'TestManagedSecret|TestProviderSecretMount' -count=1` 自检。
4. 逐字搬 `:54-67`（`esbuildbuilder`）、`:69-103`（`nodedeps`）、`:105-133`（`vendor`），一字不改。这些 stage 里的 `@anthropic-ai/claude-code` 安装是镜像里**唯一**的 Claude 安装。
5. 逐字搬 `:135-218`（最终 stage 的 apt 快照、锁定包集、已删除清单与权限收紧），**只加三行**：
   - `COPY --from=gobuilder --chown=0:0 /out/aurora-egress-proxy /usr/local/bin/aurora-egress-proxy`
   - `COPY --from=gobuilder --chown=0:0 /out/multica-mcp-client /usr/local/libexec/multica-mcp-client`（先 `install -d -o 0 -g 0 -m 0755 /usr/local/libexec`）
   - `RUN ln -s /opt/aurora/runtime/node_modules/.bin/claude /usr/local/bin/claude`
6. 逐字搬 `:220-240` 的收尾注释、`ENV HOME`、`USER`、`WORKDIR`、`ENTRYPOINT`、`CMD`、`HEALTHCHECK`。
7. 把每一条 `COPY runtime/...`、`COPY vendor/...` 之类原本相对 `deploy/aurora-sandbox` 的路径改为经 `reporoot` 命名上下文引用，例如 `COPY --from=reporoot deploy/aurora-sandbox/vendor /src/deploy/aurora-sandbox/vendor`。

**明确删掉的**：`git`（它今天在 `docker/runtime/Dockerfile:17` 的 apt 安装列表里，而 `deploy/aurora-sandbox/Dockerfile` 的锁定包集本来就没有它；它没有运行用途，且「镜像里没有 Git」不再是隔离保证，见规格 §1.7 与 §8 —— 保证由 Task 3 的进程策略提供）；`npm install --global --prefix /opt/claude` 与 `docker/runtime/claude-version.txt`。`deploy/aurora-sandbox/Dockerfile`、`Dockerfile.egress` 在本步骤结束时 `git rm`。

创建 `docker/runtime/.dockerignore`：

```
# Deny-all: every input arrives through an explicit Bake named context.
**
```

搬完后 `git diff --stat` 应当显示新文件的行数与 `deploy/aurora-sandbox/Dockerfile` 的删除行数接近 —— 如果新文件短得多，说明有内容被重写或漏搬，停下核对。

- [ ] **Step 5: 收敛 bake 与配置样例**

`deploy/aurora-sandbox/docker-bake.hcl`：`group "default"` 只保留一个 target；`target "node"` 的 `dockerfile = "../../docker/runtime/Dockerfile"`、`context = "../../docker/runtime"`、`contexts = { server = "server"; reporoot = "."; scripts = "scripts" }`、`tags = ["${REGISTRY}/multica-aurora-node:${TAG}"]`。删除 `target "egress"`。

`deploy/aurora-sandbox/versions.json`：删除 `runtime_packages` 里的 Claude 版本行以外的第二来源；把 `@anthropic-ai/claude-code` 统一为本镜像实际安装的版本（选 `2.1.289`，即被删掉的 `claude-version.txt` 的值），并在 `apt-packages.lock` / 校验器里同步。

- [ ] **Step 6: 构建并核对内容契约**

```bash
docker buildx bake -f deploy/aurora-sandbox/docker-bake.hcl --load
scripts/verify-aurora-sandbox-image.sh "$(docker images -q ghcr.io/eanfs/multica-aurora-node:dev)"
```

Expected（本步骤第一次跑必然 FAIL，因为 Task 5 才改校验器；此时应**只**剩校验器的期望值不匹配，且失败项逐条可对应到上面的契约）：
- entrypoint 与 healthcheck 一致；
- `/usr/local/bin/claude --version` 能跑，且 `Config.User == 10001:10001`；
- `Config.Env` 只有 `PATH` 与 `HOME`（不得出现 `NODE_VERSION`/`YARN_VERSION`/`MULTICA_CLAUDE_PATH`）；
- `/usr/local/libexec/multica-mcp-client` 与 `/usr/local/bin/aurora-egress-proxy` 存在；
- 禁止程序清单里 `git` 检出为「不存在」；
- `docker history --no-trunc` 与全 rootfs 扫描无密钥模式。

- [ ] **Step 7: Commit**

```bash
git add docker/runtime deploy/aurora-sandbox/docker-bake.hcl deploy/aurora-sandbox/versions.json server/internal/fleet fleet-config.example.json
git commit -m "refactor(aurora): publish one node image for the node and the egress sidecar"
```

## Task 5: 校验器、供应链与唯一 index 对齐

**Files:**
- Modify: `scripts/verify-aurora-sandbox-image.sh`
- Modify: `scripts/verify-aurora-sandbox-locks.mjs`
- Modify: `.github/workflows/aurora-sandbox.yml`
- Modify: `server/internal/fleet/docker/aurora_test.go`、`aurora_real_engine_test.go`（测试用的镜像引用）

**Interfaces:**
- Consumes: Task 4 的单一镜像契约。
- Produces: 一个 GHCR 仓库、一个 OCI index digest、一份 SBOM、一次签名与一份 provenance。

- [ ] **Step 1: 更新内容校验器**

`scripts/verify-aurora-sandbox-image.sh`：usage 变为 `<node-image>`（去掉第二参数）；删除 `:480-510` 的 egress 段（entrypoint / 无 healthcheck / file_count ≤ 3 / 禁止清单），改成对**同一个镜像**的代理入口断言：

```bash
# The sidecar is this image started with the fixed proxy entrypoint: the binary
# must be present, must be executable by the sandbox user, and must not be a
# shell wrapper.
expected_proxy="/usr/local/bin/aurora-egress-proxy"
# ... assert it exists, is mode 0555, and `--help`-less invocation exits non-zero
```

并把 `aurora_claude_path` 从 `/opt/aurora/runtime/node_modules/.bin/claude` 改为 `/usr/local/bin/claude`，把 `:254-264` 的 Claude 断言改为「`/usr/local/bin/claude` 是指向 `/opt/aurora/runtime/node_modules/.bin/claude` 的符号链接，链接目标可执行且 > 1 MiB」。

Run: `scripts/verify-aurora-sandbox-image.sh "$(docker images -q ghcr.io/eanfs/multica-aurora-node:dev)"`
Expected: PASS（全部检查项逐条打印）

- [ ] **Step 2: 更新锁校验器**

`scripts/verify-aurora-sandbox-locks.mjs`：`:35-36` 的 `dockerfile` / `dockerfileEgress` 合并为一条 `dockerfile: "docker/runtime/Dockerfile"`；删除 egress 分支；`:525-541` 的「沙箱镜像同时是 Fleet 节点镜像」断言改为「唯一镜像包含四个固定入口」；`:819-828` 的工作流策略里 `AURORA_IMAGE` 正则从 `multica-aurora-(?:sandbox|egress)` 改为 `multica-aurora-node`，并要求工作流里**只出现一个**镜像引用与一次 `cosign sign`。

Run: `node scripts/verify-aurora-sandbox-locks.mjs && node scripts/verify-aurora-sandbox-locks.mjs --workflow`
Expected: PASS（`--workflow` 在 Step 3 之前会红，属于预期）

- [ ] **Step 3: 收敛工作流**

`.github/workflows/aurora-sandbox.yml`：

- `env` 只留 `NODE_IMAGE: ghcr.io/eanfs/multica-aurora-node`。
- `verify` 里两个 build 步骤合并为一个 `Build host-platform node image without pushing`（tag `:ci`），`Verify built image contents` 传单参数，两个 Trivy `image-ref` 合并为一个。
- `publish` 里 `Build and push the sandbox OCI index` 与 `Build and push the egress OCI index` 合并为一个 `Build and push the node OCI index`；SBOM/签名/attest/attest-build-provenance 各自只保留一次；`Record published digest references` 只写一行。
- `verify-published` 的 `Verify signatures, attestations, digests, and architectures` 断言：引用是一个 index digest；`docker buildx imagetools inspect` 恰好列出 `linux/amd64` 与 `linux/arm64` 两个子 manifest；两者属于同一 index。

Run: `node scripts/verify-aurora-sandbox-locks.mjs --workflow`
Expected: PASS

- [ ] **Step 4: 修供应链阻断**

`Enforce supply-chain release policy` 目前因 `undici 7.29.0` 的 `CVE-2026-19534`、`CVE-2026-84961`（HIGH）失败（CI run 37127134369）。这是**开放工作，不是 flake**：升级或替换到不带该公告的版本，让扫描通过；不允许把它们加进 `.github/aurora-sandbox-vex.json` 豁免，除非能写出该漏洞在本镜像的 broker 调用路径上不可达的具体论证与证据。

- [ ] **Step 5: 更新测试里的镜像引用**

`server/internal/fleet/docker/aurora_real_engine_test.go` 的 `MULTICA_AURORA_TEST_NODE_IMAGE` 默认值改为统一镜像的新 digest（从 Step 6 的 publish 记录里取），`aurora_test.go` 里被删掉的 `ProxyImage` 测试值一并删除。

Run: `(cd server && go test ./internal/fleet/docker -count=1)`
Expected: PASS（不带 `dockerintegration` tag 的默认套件）

- [ ] **Step 6: 跑一次主分支 publish 并记录 digest**

合并到 `main` 触发 `publish`；随后从 run 的 `Record published digest references` 步骤取回：

```bash
gh run list --repo eanfs/multica --workflow aurora-sandbox.yml --limit 5
gh run view <run-id> --repo eanfs/multica --log | grep -A2 'published'
```

Expected: 一个 index digest 行；`verify-published` 绿。把该 digest 与 run id 写入本计划的 G0/G1 记录与 `AGENTS.md` 的 Aurora 段落。**本地无法用 cosign 验证**（macOS 主机无 `cosign`，GHCR 拒绝匿名访问），信任来自该 run 自己的 `verify-published` job —— 这一点必须连同结论写明。

- [ ] **Step 7: Commit**

```bash
git add scripts/verify-aurora-sandbox-image.sh scripts/verify-aurora-sandbox-locks.mjs .github/workflows/aurora-sandbox.yml server/internal/fleet/docker
git commit -m "ci(aurora): verify, publish, and sign exactly one node image index"
```

## Task 6: 受控 provider fixture 服务

规格 §9.1：完整闭环需要一个**操作者控制的、通过生产 `IsPublicIP` 的固定可路由 IP**，并把它用既有 `egress_pins` 指给获准的 provider host:443 —— 把 provider 域名 DNS 指到本地 fixture 会被 `auroraegress` 拒绝。这个环境今天不存在，是 G1 的交付物，不是既有能力。服务只用假凭据与合成素材。

**Files:**
- Create: `deploy/aurora-sandbox/fixture/provider/server.mjs`
- Create: `deploy/aurora-sandbox/fixture/provider/certs.sh`
- Create: `deploy/aurora-sandbox/fixture/provider/README.md`
- Create: `server/internal/auroraegress/pins_fixture_test.go`（钉住 fixtures 的 pin 语义，**不改生产 IP 拒绝逻辑**）

**Interfaces:**
- Produces: 一个 HTTPS 服务，证书覆盖被模拟的 provider 主机名，提供四类合同：模型 SSE + tool-use、provider create/poll、媒体下载、API 产物导入目标。
- Consumes: `auroraegress` 的 `egress_pins`（`map[string][]string`，值必须是**公网**地址）与 `IsPublicIP`。

- [ ] **Step 1: 写 pin 语义的失败测试（证明生产规则未被放宽）**

```go
// server/internal/auroraegress/pins_fixture_test.go
package auroraegress

import (
	"net"
	"testing"
)

// TestFixturePinsMustStillBePublic is the regression that keeps the closed-loop
// fixture from becoming a production relaxation: the address the fixture is
// reached at must pass IsPublicIP, exactly like any other pin.
func TestFixturePinsMustStillBePublic(t *testing.T) {
	fixture := net.ParseIP("198.51.100.10") // documentation range, deliberately rejected
	if IsPublicIP(fixture) {
		t.Fatal("IsPublicIP accepted a documentation address: the production rejection was widened")
	}
	if err := ValidateEgressPins(map[string][]string{"api.anthropic.com": {fixture.String()}}, nil); err == nil {
		t.Fatal("ValidateEgressPins accepted a non-public fixture address")
	}
}
```

Run: `(cd server && go test ./internal/auroraegress -run TestFixturePinsMustStillBePublic -count=1)`
Expected: PASS（这是在钉住既有正确行为；若 FAIL，说明有人放宽了 `IsPublicIP`，必须先回退）

- [ ] **Step 2: 实现 fixture 服务**

`server.mjs`：只依赖 `node:https`/`node:http`，读环境变量 `FIXTURE_HOST`、`FIXTURE_PORT`、`FIXTURE_TLS_CERT`、`FIXTURE_TLS_KEY`。路由：

| 路径 | 合同 |
| --- | --- |
| `POST /v1/messages` | 模型 SSE：按请求里的工具名回一个 `tool_use` 块，再回 `end_turn`；响应体固定，不含时间戳 |
| `POST /v3/chat/completions` | OpenAI 兼容形状：同样的固定 `tool_use` 回包与 `end_turn`，供 `provider-openai` 适配器使用 |
| `POST /provider/create` | 返回固定 `external_id`（例如 `fixture-job-1`），幂等：相同 `request_sha256` 返回同一个 id |
| `GET /provider/status/:external_id` | 固定 `succeeded` 与一个媒体 URL |
| `GET /media/:name` | 合成素材（一张 PNG、一段 1 秒 MP4、一份 WAV、一份 Markdown），字节固定 |
| `PUT /api/agent/tasks/:taskID/aurora-artifacts/import` | 记录调用并返回 200 |

服务必须对每个请求打印一行结构化日志（method、path、是否带凭据），并在启动时打印**它实际绑定的地址**，供 Step 4 核对是否落在公网地址上。

- [ ] **Step 3: 证书脚本**

`certs.sh`：用固定 subject 生成一次性 CA 与服务证书，`SAN` 覆盖 `api.anthropic.com`、`ark.cn-beijing.volces.com`、`api.openai.com`、`openspeech.bytedance.com`。输出到 `$AURORA_FIXTURE_CERT_DIR`（默认 `.scratch/aurora-fixture-certs`），并把 CA 证书复制成固定只读路径供测试注入。**不使用** `NODE_TLS_REJECT_UNAUTHORIZED=0`；信任只经 CA 文件与 Node/Claude 的受信配置注入。

Run: `bash deploy/aurora-sandbox/fixture/provider/certs.sh && openssl x509 -in "${AURORA_FIXTURE_CERT_DIR:-.scratch/aurora-fixture-certs}/server.crt" -noout -ext subjectAltName`
Expected: 列出四个 SAN

- [ ] **Step 4: 部署到一个受控公网地址并实证**

在操作者控制的公网主机（不是 RFC1918、不是文档网段）上跑服务，然后：

```bash
curl -sS "https://<fixture-ip>/media/poster.png" -o /tmp/fixture.png --cacert "$AURORA_FIXTURE_CERT_DIR/ca.crt" -H "Host: api.anthropic.com"
( cd server && go test ./internal/auroraegress -run 'TestNewPolicy|TestBuildPins' -count=1 )
```

Expected: `curl` 得到非零字节；`buildPins` 接受该地址（因为它通过 `IsPublicIP`）。**若该地址被 `IsPublicIP` 拒绝，换一个地址，不要改 `IsPublicIP`。**

- [ ] **Step 5: 写 fleet 配置与运行说明**

`deploy/aurora-sandbox/fixture/provider/README.md` 给出完整的一份 Fleet 配置片段（`egress_hosts` 里加上被模拟的主机，`egress_pins` 指向 fixture 地址，`server_url` 指向测试 API），以及运行闭环所需的四个环境变量（`AURORA_SANDBOX_IMAGE`、`FLEET_CONFIG_FILE`、`MULTICA_LOCAL_FLEET_URL`、`MULTICA_LOCAL_FLEET_SECRET_FILE`）。明确标注：该地址、证书与假密钥都只存在于这台测试主机与部署文件里，绝不进入仓库镜像或 CI 变量。

- [ ] **Step 6: Commit**

```bash
git add deploy/aurora-sandbox/fixture/provider server/internal/auroraegress/pins_fixture_test.go
git commit -m "test(aurora): add the operator-controlled provider fixture contract"
```

---

# M2 · G2 统一注册与身份

## Task 7: 运行时身份统一

受管载体今天被 `runtime_mode='cloud' + provider='aurora_managed'` 三重硬编码：注册谓词、claim 查询、SQL 字面量、局部唯一索引。本任务把它统一为 `provider='claude' + runtime_mode='local'`，受管性只由服务器写入的 `metadata.managed_by='local_fleet'` / `metadata.fleet_node_id` 表达（这两个键今天已经由 `handler/local_fleet_identity.go` 写入），并**重建 526 的局部唯一索引**，否则「每工作区一个受管运行时」的唯一性会静默失效。

**Files:**
- Modify: `server/internal/aurora/agents.go`、`sandbox_enrollment.go`、`sandbox_manager.go`、`sandbox_reaper.go`
- Modify: `server/internal/daemon/managed.go`、`aurora_tool_surface.go`
- Modify: `server/internal/handler/{aurora_runtime_view.go,daemon.go}`
- Modify: `server/pkg/db/queries/{aurora_agents.sql,aurora_sandbox_node.sql,aurora.sql}` → `make sqlc`
- Create: `server/migrations/<n>_aurora_managed_runtime_local_index.up.sql/.down.sql`（新条件索引）
- Create: `server/migrations/<n+1>_aurora_managed_runtime_convert.up.sql/.down.sql`（数据转换）
- Create: `server/migrations/<n+2>_drop_aurora_managed_cloud_index.up.sql/.down.sql`（删除旧索引）
- Modify: `server/cmd/migrate/main.go`（登记两个并发索引的 `concurrentIndexCleanups` 条目）
- Modify: 相关测试（见 Step 6）

**Interfaces:**
- Produces: `aurora` 包新增三个可导出常量 —— `ExecutionProvider = "claude"`、`RuntimeModeLocal = "local"`、`ManagedByLocalFleet = "local_fleet"`；`daemon` 与 `handler` 删除各自的重复字面量并引用它们（`handler/aurora_runtime_view.go:16` 的注释正是为了绕开「常量未导出」才复制了一份）。
- Produces: `auroraExecutionNodeProvider = "docker"` 保持不变 —— 它是节点后端提供方，不是运行时身份。

- [ ] **Step 1: 导出常量并替换所有字面量（编译器驱动）**

在 `server/internal/aurora/agents.go` 把 `managedRuntimeProvider`、`managedRuntimeName` 一带改为导出：

```go
// ExecutionProvider is the only provider with a reviewed Aurora sandbox surface,
// and the only execution identity a managed enrollment may install.
const ExecutionProvider = "claude"

// RuntimeModeLocal marks a runtime bound to a node on this deployment's Fleet.
const RuntimeModeLocal = "local"

// ManagedByLocalFleet is the metadata value the server writes on a runtime it
// manages through the local Fleet. It is never taken from a request.
const ManagedByLocalFleet = "local_fleet"
```

`server/internal/daemon/aurora_tool_surface.go:34` 的 `auroraExecutionProvider` 与 `handler/aurora_runtime_view.go:16` 的 `auroraManagedRuntimeProvider` 删除，改成引用 `aurora.ExecutionProvider`。

- [ ] **Step 2: 写身份谓词的失败测试**

在 `server/internal/daemon/managed_test.go` 把 fixture 的 `Runtime{Provider: "aurora_managed", RuntimeMode: "cloud"}` 改为 `Provider: aurora.ExecutionProvider, RuntimeMode: aurora.RuntimeModeLocal`，并新增：

```go
func TestInstallManagedEnrollmentRejectsLegacyCarrier(t *testing.T) {
	d := newManagedTestDaemon(t)
	resp := managedEnrollmentFixture()
	resp.Runtime.Provider = "aurora_managed"
	resp.Runtime.RuntimeMode = "cloud"
	if err := d.installManagedEnrollment(resp); !errors.Is(err, ErrInvalidManagedEnrollmentResponse) {
		t.Fatalf("err = %v, want ErrInvalidManagedEnrollmentResponse for the retired carrier identity", err)
	}
}
```

Run: `(cd server && go test ./internal/daemon -run 'TestInstallManagedEnrollment' -count=1)`
Expected: FAIL —— `undefined: aurora.ExecutionProvider`（Step 1 之前）或旧载体被接受

- [ ] **Step 3: 改注册谓词**

`managed.go:65`：

```go
if runtime.Provider != aurora.ExecutionProvider || runtime.RuntimeMode != aurora.RuntimeModeLocal {
	return fmt.Errorf("%w: persisted runtime provider=%q mode=%q", ErrInvalidManagedEnrollmentResponse, runtime.Provider, runtime.RuntimeMode)
}
```

同时把 `:71-73` 那段「持久化载体保持 aurora_managed」的注释改写成新语义：**持久化行与执行身份现在是同一个**，`installManagedEnrollment` 不再改写 provider，只补齐 `DaemonID`。

- [ ] **Step 4: 改 SQL 谓词并重新生成**

三个文件里的字面量：

- `server/pkg/db/queries/aurora_agents.sql`：`GetAuroraManagedRuntime` 的 `runtime_mode = 'cloud'` → `'local'`；`CreateAuroraManagedRuntime` 的两处 `'cloud'` → `'local'`；`UpsertAuroraSystemAgent` 的 `'cloud'` → `'local'`。函数名保持不变（它们仍是「工作区的受管运行时」），避免无谓的引用扩散。
- `server/pkg/db/queries/aurora_sandbox_node.sql:86-87` 与 `:139`：`runtime_mode = 'cloud' AND provider = 'aurora_managed'` → `runtime_mode = 'local' AND provider = 'claude'`。
- `server/pkg/db/queries/aurora.sql`：检查是否存在同类字面量（`grep -n "aurora_managed\|'cloud'" server/pkg/db/queries/aurora.sql`），有则一并改。

Run: `make sqlc`
Expected: 生成成功；`git diff server/pkg/db/generated` 只包含谓词字面量的变化

- [ ] **Step 5: 三个迁移：先建新索引、再转换数据、最后删旧索引**

必须先取当前最高编号：

```bash
ls server/migrations | sed -n 's/^\([0-9]\+\)_.*/\1/p' | sort -n | tail -1
```

设结果为 `N`，则新文件编号为 `N+1`、`N+2`、`N+3`。

`(N+1)_aurora_managed_runtime_local_index.up.sql`（单语句、并发、登记进 cleanups）：

```sql
-- Exactly one managed runtime per workspace under the unified identity. The
-- retired predicate (runtime_mode='cloud' AND provider='aurora_managed') no
-- longer matches any new row, so the old index would silently stop enforcing
-- uniqueness. This replacement is created BEFORE the data conversion so the
-- conversion itself fails closed if two rows in one workspace would collide.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS agent_runtime_managed_local_workspace_uidx ON agent_runtime (workspace_id) WHERE runtime_mode = 'local' AND provider = 'claude';
```

`(N+2)_aurora_managed_runtime_convert.up.sql`（幂等数据转换）：

```sql
-- Convert the managed carrier rows to the unified identity. Idempotent: the
-- WHERE clause matches nothing on a second run. It deliberately does not touch
-- the managed metadata keys, which the server already wrote.
UPDATE agent_runtime SET provider = 'claude', runtime_mode = 'local', updated_at = now()
WHERE provider = 'aurora_managed' AND runtime_mode = 'cloud';
```

`(N+3)_drop_aurora_managed_cloud_index.up.sql`：

```sql
DROP INDEX CONCURRENTLY IF EXISTS agent_runtime_aurora_managed_workspace_uidx;
```

对应的三个 `.down.sql`：分别 `DROP INDEX CONCURRENTLY IF EXISTS agent_runtime_managed_local_workspace_uidx;`、把 `WHERE provider='claude' AND runtime_mode='local' AND ...` 反向 UPDATE（`metadata` 里有 `fleet_node_id` 的行才算受管，避免误伤普通 claude runtime）、以及按 526 的原文重建旧索引。`.down.sql` 也应各自单语句。

在 `server/cmd/migrate/main.go` 的两个 cleanup 表里登记 `agent_runtime_managed_local_workspace_uidx`（up）与 `agent_runtime_aurora_managed_workspace_uidx`（down）。

Run: `make migrate-up && (cd server && go run ./cmd/migrate up)`
Expected: 三段迁移各打印 `up <version>`；再跑一次 `make migrate-up` 无变化（幂等）

- [ ] **Step 6: 清理剩余引用并跑测试**

```bash
grep -rn "aurora_managed" server --include='*.go' --include='*.sql' | grep -v pkg/db/generated
```

Expected: 只剩 Step 3 的**否定**断言与测试 fixture（测试若仍需构造旧行，写成显式字面量并加注释说明它测的是「旧载体被拒绝」）。逐处处理：

- `server/internal/handler/daemon.go:1329` 的 `if rt.Provider == "aurora_managed" && rt.DaemonID.Valid` → `if fleetguard.Managed(rt) && rt.DaemonID.Valid`。这里**不能**只换 provider 字面量为 `"claude"`：那会把所有普通 Claude 运行时都吸进受管触达路径。
- `server/internal/handler/aurora_test.go` 的 `cleanupAuroraSystemAgents` 里的 `agent_runtime WHERE provider = 'aurora_managed' AND daemon_id IS NULL` → `WHERE metadata->>'managed_by' = 'local_fleet' AND daemon_id IS NULL`。
- `aurora_runtime_view_test.go`、`agents_test.go`、`sandbox_enrollment_test.go`、`sandbox_manager_test.go`、`sandbox_reaper_test.go` 的 fixture provider/mode。

Run: `(cd server && go test ./internal/aurora ./internal/daemon ./internal/handler ./internal/fleetguard ./internal/service -count=1)`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add server/internal server/pkg/db server/migrations server/cmd/migrate/main.go
git commit -m "refactor(aurora): unify the managed runtime on the claude/local identity"
```

## Task 8: 注册凭据持久化与重启恢复

今天 `bootstrapManaged` 把兑换来的 `mdt_` 只放进进程内存（`client.go` 的 `token` 字段），`daemon_token_expires_at` 被解析但从不读取，仓库里不存在 `/data/identity`。规格 §5.3 要求：验证后把 daemon credential、有效期、node/runtime/daemon ID 与注册代次**原子写入** `/data/identity/session.json`（目录 0700、文件 0600、拒绝 symlink、fsync 文件与目录），持久化失败不得开始领取任务；重启优先读持久身份并向服务器核验，不得再次消费原一次性 enrollment。

**Files:**
- Create: `server/internal/daemon/managed_session.go`、`managed_session_test.go`
- Modify: `server/internal/daemon/managed.go`（`bootstrapManaged` 改为「有会话则核验、无会话才消费 enrollment」）
- Modify: `server/internal/daemon/client.go`（新增 `ManagedSession` 请求）
- Create: `server/internal/handler/managed_session.go` + 路由
- Modify: `server/cmd/server/router.go`
- Modify: `server/internal/fleet/docker/bootstrap.go`（installer tar 里创建 `/data/identity`）

**Interfaces:**
- Produces: `daemon.ManagedSessionDir = "/data/identity"`、`daemon.ManagedSessionFile = "/data/identity/session.json"`。
- Produces: `type ManagedSession struct { WorkspaceID, DaemonID, RuntimeID, NodeID string; TokenHash string; ExpiresAt time.Time; EnrollmentGeneration int64 }`；`LoadManagedSession(path string) (ManagedSession, error)`、`WriteManagedSession(path string, s ManagedSession, token string) error`。
- Produces: `POST /api/daemon/managed/session`（bearer = 持久化的 `mdt_`），返回当前绑定与轮换状态。
- Consumes: Task 9 的轮换状态字段（本任务先返回 `rotation: null`）。

- [ ] **Step 1: 写原子写与拒绝 symlink 的失败测试**

```go
// server/internal/daemon/managed_session_test.go
package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteManagedSessionIsOwnerOnlyAndAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "identity", "session.json")
	s := ManagedSession{WorkspaceID: "w", DaemonID: "d", RuntimeID: "r", NodeID: "n", ExpiresAt: time.Unix(0, 0).UTC()}
	if err := WriteManagedSession(path, s, "mdt_abc"); err != nil {
		t.Fatalf("WriteManagedSession: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 0600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %o, want 0700", dirInfo.Mode().Perm())
	}
	// No temporary file may survive the successful write.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "session.json" {
		t.Fatalf("leftover files after write: %v", entries)
	}
	got, err := LoadManagedSession(path)
	if err != nil {
		t.Fatalf("LoadManagedSession: %v", err)
	}
	if got.WorkspaceID != "w" || got.ExpiresAt.IsZero() {
		t.Fatalf("round trip lost identity: %+v", got)
	}
}

func TestWriteManagedSessionRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(dir, "session.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := WriteManagedSession(link, ManagedSession{}, "mdt_abc"); err == nil {
		t.Fatal("WriteManagedSession followed a symlink")
	}
	if raw, err := os.ReadFile(target); err != nil || string(raw) != "{}" {
		t.Fatalf("target was modified through the symlink: %q %v", raw, err)
	}
}

func TestLoadManagedSessionRejectsNonOwnerFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	if err := os.WriteFile(path, []byte(`{"workspace_id":"w"}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadManagedSession(path); err == nil {
		t.Fatal("LoadManagedSession accepted a group/world-readable session file")
	}
}
```

Run: `(cd server && go test ./internal/daemon -run 'TestWriteManagedSession|TestLoadManagedSession' -count=1)`
Expected: FAIL —— `undefined: ManagedSession`

- [ ] **Step 2: 实现持久会话**

`managed_session.go` 要点：`WriteManagedSession` 先用 `os.Lstat` 拒绝既有 symlink，再 `os.MkdirAll(dir, 0o700)`，在**同目录**创建 `session.json.tmp`（`os.OpenFile(tmp, O_WRONLY|O_CREATE|O_EXCL, 0o600)`），写入 JSON（含明文 token —— 它只对本机 daemon 可读，不进 SQL、不进日志），`f.Sync()`、`Close()`、`os.Rename(tmp, final)`，最后打开目录 fd 并 `Sync()` 以固化目录项。`LoadManagedSession` 先校验 `Lstat` 是普通文件且 `Perm() == 0o600`，再读。

Run: `(cd server && go test ./internal/daemon -run 'TestWriteManagedSession|TestLoadManagedSession' -count=1)`
Expected: PASS

- [ ] **Step 3: 写 `bootstrapManaged` 的会话优先测试**

```go
func TestBootstrapManagedPrefersThePersistedSession(t *testing.T) {
	d := newManagedTestDaemon(t)
	d.cfg.Managed.SessionFile = writeSessionFixture(t, ManagedSession{WorkspaceID: "ws-1", DaemonID: "d-1", RuntimeID: "rt-1"})
	d.client = &fakeClient{session: ManagedSessionResponse{WorkspaceID: "ws-1", DaemonID: "d-1", DaemonTokenExpiresAt: time.Now().Add(8 * time.Hour)}}
	if err := d.bootstrapManaged(context.Background()); err != nil {
		t.Fatalf("bootstrapManaged: %v", err)
	}
	if d.client.(*fakeClient).enrolled != 0 {
		t.Fatalf("consumed %d enrollment(s) with a persisted session, want 0", d.client.(*fakeClient).enrolled)
	}
}

func TestBootstrapManagedConsumesEnrollmentWithoutASession(t *testing.T) { /* enrolled == 1 */ }

func TestBootstrapManagedFailsClosedWhenTheSessionIsUnknownToTheServer(t *testing.T) {
	// server answers 401 -> the daemon must not claim tasks and must not
	// silently re-consume the one-time enrollment.
}
```

Run: `(cd server && go test ./internal/daemon -run 'TestBootstrapManaged' -count=1)`
Expected: FAIL

- [ ] **Step 4: 实现会话优先的 bootstrap 与核验端点**

`bootstrapManaged` 的新流程：

1. `session, err := LoadManagedSession(cfg.Managed.SessionFile)`；
2. 若会话存在：`resp, err := client.VerifyManagedSession(ctx, session.Token)`；`err == nil` → `installManagedEnrollment(resp)`、`SetToken(session.Token)`、`markManagedEnrolled()`，**不读 enrollment 文件**；401 → 记 `markManagedUnready("session revoked")` 并返回错误（交运维按 §5.3 的重新注册流程处理），**不**自动消费原一次性 enrollment；
3. 若会话不存在：走今天的路径，但成功后在 `SetToken` **之前** `WriteManagedSession(...)`；写盘失败 → 返回错误且**不** `SetToken`（「持久化失败不能开始领取任务」）。

新增客户端方法 `VerifyManagedSession(ctx context.Context, token string) (ManagedSessionResponse, error)` → `POST /api/daemon/managed/session`，用一个只读 token 发请求（沿用 `postJSONWithToken`）。

服务端 `server/internal/handler/managed_session.go`：`func (h *Handler) ManagedRuntimeSession(w http.ResponseWriter, r *http.Request)`，挂在 `DaemonAuth` 组内（`router.go:1602` 附近），身份来自 `middleware.DaemonWorkspaceIDFromContext` / `DaemonIDFromContext`，**不接受请求体里的 workspace/daemon/runtime**；用 `GetAuroraSandboxNodeByWorkspace` + `GetAuroraManagedRuntime` 组装响应，`daemon_token_expires_at` 取当前凭据的到期时间。

- [ ] **Step 5: installer 创建 identity 目录**

`server/internal/fleet/docker/bootstrap.go` 的 `auroraBootstrapTar` 目录清单加上 `data/identity/`，模式 0700、属 `10001:10001`（与既有的 `data/`、`data/home/`、`data/workspaces/`、`secrets/` 一致）。

Run: `(cd server && go test ./internal/fleet/docker -run 'Bootstrap' -count=1)`
Expected: PASS（若已有断言固定目录清单，同步更新）

- [ ] **Step 6: 运行全套**

Run: `(cd server && go test ./internal/daemon ./internal/handler ./internal/fleet/docker -count=1)`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add server/internal/daemon server/internal/handler server/cmd/server/router.go server/internal/fleet/docker/bootstrap.go
git commit -m "feat(aurora): persist the managed node session and verify it across restarts"
```

## Task 9: renew / renew-ack 轮换协议

规格 §5.3 给了完整顺序，逐条都要落：剩余 30 分钟时停止新领取并启动轮换；daemon 用密码学随机源生成新 token，把新旧身份与稳定轮换 ID 先落到 pending 文件；以有效旧 credential 上传新 token **哈希**；服务端只为当前绑定 CAS 建立一个 pending 轮换，同 ID/同哈希重放返回同结果、不同输入拒绝；daemon 收到确认后**先**原子替换并 fsync 本地 session，**再**以新 credential 发 ack；服务端在 ack 事务内激活新 credential 并撤销旧 credential；不能先撤销旧 credential 再保存新明文。

**Files:**
- Create: `server/migrations/<n>_daemon_token_rotation.up.sql/.down.sql` + 两个索引文件
- Create: `server/pkg/db/queries/daemon_token_rotation.sql` → `make sqlc`
- Create: `server/internal/aurora/rotation.go`、`rotation_test.go`
- Create: `server/internal/handler/managed_renew.go`、`managed_renew_test.go`
- Modify: `server/cmd/server/router.go`
- Modify: `server/internal/daemon/client.go`、`managed_session.go`、`daemon.go`（轮换循环）、`health.go`（暂停领取）
- Modify: `server/internal/daemon/managed.go`

**Interfaces:**
- Produces: `POST /api/daemon/managed/renew`（bearer = 当前有效 `mdt_`）→ 202 `{"rotation_id": "...", "replayed": bool}`；`POST /api/daemon/managed/renew/ack`（bearer = **pending 新** `mdt_`，body `{"rotation_id": "..."}`）→ 200。
- Produces: `type ManagedPendingRotation struct { RotationID, Token string; StartedAt time.Time }`；`daemon.ManagedPendingFile = "/data/identity/session.pending.json"`。
- Produces: `aurora.(*SandboxEnrollmentService).BeginRotation(ctx, workspaceID, daemonID pgtype.UUID, rotationID, newTokenHash string, now time.Time) (RotationBegin, error)` 与 `.AckRotation(ctx, rotationID, newTokenHash string) error`。
- Consumes: `auth.GenerateDaemonToken`、`auth.HashToken`、`daemon_token` 表与其缓存失效路径。

- [ ] **Step 1: 迁移与 SQL**

```sql
-- (n)_daemon_token_rotation.up.sql
CREATE TABLE IF NOT EXISTS daemon_token_rotation (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  workspace_id uuid NOT NULL,
  daemon_id uuid NOT NULL,
  rotation_id text NOT NULL,
  new_token_hash text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  activated_at timestamptz
);
```

无外键、无级联（仓库规则）。两个索引各自单文件、并发：`daemon_token_rotation_rotation_id_uidx`（unique on `rotation_id`）、`daemon_token_rotation_binding_idx`（on `(workspace_id, daemon_id)` WHERE `activated_at IS NULL`，用于「每个绑定至多一个未激活轮换」的 CAS）。

`daemon_token_rotation.sql`（名字与语义一一对应）：

```sql
-- name: InsertDaemonTokenRotation :one
-- CAS: only the current binding may open one pending rotation. A same-id,
-- same-hash replay returns the existing row; any other input for an open
-- rotation is pgx.ErrNoRows, which the caller reports as a conflict.
INSERT INTO daemon_token_rotation (workspace_id, daemon_id, rotation_id, new_token_hash)
SELECT @workspace_id, @daemon_id, @rotation_id, @new_token_hash
WHERE NOT EXISTS (
  SELECT 1 FROM daemon_token_rotation r
  WHERE r.workspace_id = @workspace_id AND r.daemon_id = @daemon_id AND r.activated_at IS NULL
    AND NOT (r.rotation_id = @rotation_id AND r.new_token_hash = @new_token_hash))
RETURNING *;

-- name: GetDaemonTokenRotationByHash :one
SELECT * FROM daemon_token_rotation WHERE new_token_hash = @new_token_hash AND activated_at IS NULL;

-- name: GetDaemonTokenRotation :one
SELECT * FROM daemon_token_rotation WHERE workspace_id = @workspace_id AND daemon_id = @daemon_id
 AND rotation_id = @rotation_id AND activated_at IS NULL;

-- name: ActivateDaemonTokenRotation :execrows
UPDATE daemon_token_rotation SET activated_at = clock_timestamp()
WHERE rotation_id = @rotation_id AND new_token_hash = @new_token_hash AND activated_at IS NULL;
```

Run: `make sqlc && make migrate-up`
Expected: 成功

- [ ] **Step 2: 写轮换服务的失败测试**

```go
// server/internal/aurora/rotation_test.go
func TestBeginRotationIsIdempotentForTheSameInput(t *testing.T)      // 同 rotation_id 同 hash → 同结果，pending 数仍为 1
func TestBeginRotationRejectsDifferentInputOnAnOpenRotation(t *testing.T) // 不同 rotation_id 或不同 hash → 冲突
func TestAckRotationActivatesTheNewCredentialAndRevokesTheOld(t *testing.T)
func TestAckRotationReplayIsIdempotent(t *testing.T)                // ack 响应丢失后重放同轮换 → 同结果，不产生第二条 credential
func TestExpiredOldCredentialCanStillCompleteItsAck(t *testing.T)   // 旧 token 过期后，仅 pending 新 credential 能完成原 ack
```

Run: `(cd server && go test ./internal/aurora -run 'Rotation' -count=1)`
Expected: FAIL —— `undefined: ...Rotation`

- [ ] **Step 3: 实现服务端轮换**

`BeginRotation`：在一个事务里按 `LockAuroraSandboxEnrollmentWorkspace` 的既有工作区锁顺序取锁 → 校验当前 `daemon_token` 属于该 (workspace, daemon) → `InsertDaemonTokenRotation` → `ErrNoRows` 时读回 `GetDaemonTokenRotation` 判同输入并返回 `replayed=true`，否则 `model.ErrConflict`。

`AckRotation`：单个事务里 `ActivateDaemonTokenRotation` → 同事务内 `CreateDaemonToken`（新哈希、同一 daemon）→ `DeleteDaemonTokensByWorkspaceAndDaemons`（旧哈希）→ commit。提交后调用方 `h.DaemonTokenCache.Invalidate(ctx, oldHash)` 并撤销旧 WebSocket 的领取权（复用 `ManagedRuntimeShutdown` 的失效路径）。`RowsAffected == 0` 且该 rotation 的 `activated_at` 已非空 → 幂等成功；否则冲突。

**不得**先删旧凭据再写新凭据：行级顺序由上面的事务保证。

- [ ] **Step 4: 两个端点**

`server/cmd/server/router.go`：两个路由都注册在 `DaemonAuth` 组**之外**（与 `POST /api/daemon/managed/enroll` 同样的理由），但用各自的 bearer 语义：

```go
// Renew and renew/ack carry credentials DaemonAuth cannot yet resolve: the
// rotation's new token is deliberately not a daemon_token row until the ack
// transaction activates it. Each handler therefore authenticates by itself and
// never falls back to JWT, PAT or cloud credentials.
r.Post("/api/daemon/managed/renew", h.ManagedRuntimeRenew)
r.Post("/api/daemon/managed/renew/ack", h.ManagedRuntimeRenewAck)
```

`ManagedRuntimeRenew`：bearer 必须命中 `GetDaemonTokenByHash`（未过期），body `{"rotation_id","new_token_hash"}`（daemon 上传的是**哈希**，不发明文）；返回 202 `{"rotation_id":...,"replayed":...}`。`ManagedRuntimeRenewAck`：bearer 必须命中 `GetDaemonTokenRotationByHash`，body `{"rotation_id"}`；成功 200、重放 200、未知 401、冲突 409。

- [ ] **Step 5: daemon 侧轮换与 pending 恢复**

`daemon.go` 的 managed 分支新增 `rotationLoop`（与既有 `heartbeatLoop` 并列，间隔 30 秒）：

- 读 `client.DaemonTokenExpiresAt` 与本地 session；剩余 ≤ `rotationLeadTime`（30 分钟）且无 pending → 停止新领取（`markManagedUnready("rotating credential")`，`managedReady()` 因此为 false，容器 healthcheck 随之失败——这正是「不能延长旧 token 上限」的可见信号）→ `auth.GenerateDaemonToken()` 生成新 token → **先** `WriteManagedSession(pendingPath, ...)`（新 token + rotation id）→ `client.BeginRotation` → 收到 202 后原子替换 session 并 fsync → `client.AckRotation`（用新 token）→ 成功后删除 pending 并恢复领取。
- 启动时若 pending 存在：**不生成新 token**，直接用 pending 的新 credential 重发 ack；`rotation_id` 不在服务端（已激活或已清理）→ 当作已 ack，原子替换 session、清 pending。
- ack 响应丢失：下次循环用 pending 的新 credential 重放同轮换。

测试（默认套件，用 fake client，不碰真实 HTTP）：

```go
func TestRotationStopsAdmissionAtThirtyMinutesAndResumesAfterAck(t *testing.T)
func TestRotationResumesFromPendingWithoutGeneratingASecondToken(t *testing.T)
func TestRotationDoesNotRenewPastTheEightHourLimit(t *testing.T)
```

- [ ] **Step 6: 运行全套**

Run: `(cd server && go test ./internal/daemon ./internal/aurora ./internal/handler -count=1)`
Expected: PASS

Run: `(cd server && go test ./cmd/server -count=1)`
Expected: PASS（`aurora_runtime_routes_test.go` 断言「已退役的 `/api/daemon/managed/register` 不存在」需要继续成立；新增的两个路由要在同一测试里列出）

- [ ] **Step 7: Commit**

```bash
git add server/migrations server/pkg/db server/internal/aurora server/internal/handler server/cmd/server/router.go server/internal/daemon
git commit -m "feat(aurora): rotate the managed daemon credential with a durable pending state"
```

---

# M3 · G3 准备/生成/结果事务

> **本里程碑的关键设计选择（已记录，供审查）：** 规格 §5.2 要求「创建工作区的事务不调用 Docker 或 Fleet HTTP」，同时一次性 `mse_` 明文只能经进程内存或私密文件交接。因此本计划把职责切成两半 —— **API 事务只写意图**（`fleet_nodes` 的 waiting 行 + `prepare` operation + `aurora_sandbox_node` 行与哈希），**API 侧的协调器**负责把新铸的一次性密钥交给 Fleet 并请求推进，**Fleet 仍然做全部 Docker 工作**（唯一控制面不因此改变）。协调器用会话级 advisory lock 保证同一时刻只有一个进程在跑。

## Task 10: prepare 意图与 waiting 节点模型

**Files:**
- Create: `server/pkg/db/queries/fleet_prepare.sql` → `make sqlc`
- Create: `server/migrations/<n>_fleet_nodes_waiting_workspace_uidx.up.sql/.down.sql`（两个并发索引文件）
- Modify: `server/pkg/db/queries/fleet.sql`（`ready` 表达式、`CountFleetProvisionedNodes` 注释）
- Modify: `server/internal/fleet/model/types.go`、`fleet/model/policy.go`
- Modify: `server/internal/fleet/store/{store.go,aurora.go}`、`reconciler.go`
- Create: `server/internal/fleet/{prepare_test.go,integration/prepare_test.go}`

**Interfaces:**
- Produces: `model.Prepare Action = "prepare"`。
- Produces: `Store.InsertPrepareIntent(ctx, owner, nodeID pgtype.UUID, req model.AuroraPrepareRequest) (model.Node, model.Operation, bool, error)`。
- Produces: `Store.PromotePrepareIntent(ctx, owner, nodeID pgtype.UUID, image, specConfig, requestHash string) (model.Node, model.Operation, error)`。
- Produces: 节点状态 `waiting`（`ready=false`、`container_id=''`），操作阶段 `waiting_config`、`waiting_capacity`。

- [ ] **Step 1: 加 action 与阶段校验**

`fleet/model/types.go`：

```go
const (
	Create  Action = "create"
	Start   Action = "start"
	Stop    Action = "stop"
	Reboot  Action = "reboot"
	Delete  Action = "delete"
	// Prepare is the durable intent to make a workspace runtime ready. It owns
	// no physical resources until it is promoted into the create action.
	Prepare Action = "prepare"
)
```

`fleet/model/types.go` 里 phase 的常量（若原本是字面量，先补一组）：

```go
const (
	PhaseWaitingConfig   = "waiting_config"
	PhaseWaitingCapacity = "waiting_capacity"
	PhaseQueued          = "queued"
	PhasePreparing       = "preparing"
	PhasePrepared        = "prepared"
	PhaseApplying        = "applying"
	PhaseCompleted       = "completed"
	PhaseFailed          = "failed"
)
```

`fleet/internal_dto.go` 的 `OperationReviewResponseDTO.Validate` 与 `CanApplyOperation` **不新增** prepare 的可应用条件（prepare 从不被 worker 领取，只在 HTTP 推进路径里变换），但要在评审 DTO 的接受集合里显式排除 `waiting_*`（它们不是可评审阶段）。

- [ ] **Step 2: 写 SQL**

`server/pkg/db/queries/fleet_prepare.sql`：

```sql
-- name: InsertFleetPreparedAuroraNode :one
-- The logical binding of a workspace whose runtime is not admitted yet. It owns
-- no container, no image decision and no spec snapshot: those are pinned at
-- promotion. Volume names are durable node identity, exactly as on the create
-- path, so a promoted node keeps the volumes it was prepared with.
INSERT INTO fleet_nodes (id, daemon_id, namespace, owner_id, workspace_id, runtime_id, name, spec, image, profile_ref, spec_config, data_volume, secrets_volume, status, desired, ready)
VALUES (@node_id, @daemon_id, @namespace, @owner_id, @workspace_id, @runtime_id, @name, @spec, '', '', '{}'::jsonb, @data_volume, @secrets_volume, 'waiting', 'running', false)
RETURNING *;

-- name: InsertFleetPrepareOperation :one
INSERT INTO fleet_node_operations (namespace, owner_id, node_id, action, idempotency_key, request_hash, phase, error_code)
VALUES (@namespace, @owner_id, @node_id, 'prepare', @idempotency_key, @request_hash, @phase, @error_code)
RETURNING *;

-- name: FleetSetPreparePhase :execrows
-- Records why a prepared node is not admitted yet. The reason is public: the
-- runtime projection reads error_code, so an operator never has to guess whether
-- Docker has started creating anything (it has not).
UPDATE fleet_node_operations o SET phase=@phase, error_code=@error_code, error_message='', updated_at=now()
WHERE o.namespace=@namespace AND o.owner_id=@owner_id AND o.node_id=@node_id AND o.id=@operation_id
 AND o.action='prepare' AND o.generation=@generation AND o.phase IN ('waiting_config','waiting_capacity');

-- name: ListFleetWaitingNodes :many
-- Keyset scan for the prepare coordinator. Only a committed intent that has not
-- been promoted, failed or tombstoned is returned, and only for a workspace
-- whose logical binding still exists.
SELECT n.* FROM fleet_nodes n
WHERE n.namespace=@namespace AND n.status='waiting' AND n.container_id='' AND NOT n.revoked AND NOT n.maintenance
 AND n.id>@after_id::uuid
ORDER BY n.id LIMIT 100;

-- name: CountFleetAdmittedNodes :one
-- Capacity counts admitted preparations and physical nodes. The existing
-- CountFleetProvisionedNodes deliberately keeps counting a waiting node as well:
-- it answers "does this owner already have a binding", while admission needs
-- "how many resources may this owner hold".
SELECT count(*) FROM fleet_nodes WHERE namespace=@namespace AND owner_id=@owner_id
 AND NOT (desired='terminated' AND status='terminated') AND status<>'waiting';

-- name: FleetPromotePrepareOperation :one
-- The single admission point for a prepared workspace node. It pins the
-- administrator-approved image and the declared spec snapshot, applies capacity,
-- and turns the SAME operation into the existing create intent: operation id,
-- idempotency key and the logical node/runtime/daemon identity all survive, so
-- the caller's progress reference stays valid across promotion. Zero rows means
-- "not admitted here" (unknown node, already promoted, revoked, or over
-- capacity) and the caller must not create anything.
WITH capacity AS (
  SELECT count(*) AS admitted FROM fleet_nodes w
  WHERE w.namespace=@namespace AND w.owner_id=@owner_id
    AND NOT (w.desired='terminated' AND w.status='terminated') AND w.status<>'waiting'
), promoted AS (
  UPDATE fleet_nodes n SET image=@image, spec_config=@spec_config, status='creating', ready=false, updated_at=now()
  FROM capacity c
  WHERE n.namespace=@namespace AND n.owner_id=@owner_id AND n.id=@node_id AND n.generation=@generation
    AND n.status='waiting' AND n.container_id='' AND NOT n.revoked AND NOT n.maintenance
    AND c.admitted < @max_nodes
  RETURNING n.id
)
UPDATE fleet_node_operations o SET action='create', phase='queued', request_hash=@request_hash,
 error_code='', error_message='', attempts=0, non_retryable=false, next_attempt_at=NULL, updated_at=now()
FROM promoted p
WHERE o.namespace=@namespace AND o.owner_id=@owner_id AND o.node_id=@node_id AND o.id=@operation_id
 AND o.action='prepare' AND o.generation=@generation
RETURNING o.*;

-- name: FleetTombstonePrepareIntents :execrows
-- A workspace delete tombstones every unfinished prepare intent in the same
-- transaction, so a coordinator that already read an intent cannot create
-- resources for a workspace that no longer exists.
UPDATE fleet_node_operations SET phase='failed', non_retryable=true, error_code='workspace_deleted',
 error_message='', next_attempt_at=NULL, updated_at=now()
WHERE namespace=@namespace AND owner_id=@owner_id AND node_id=@node_id AND action='prepare'
 AND phase NOT IN ('completed','failed');

-- name: FleetDeleteWaitingNode :execrows
-- A waiting node owns no physical resource, so deleting it is one statement: no
-- container, volume or network was ever created for it.
UPDATE fleet_nodes SET desired='terminated', status='terminated', revoked=true, ready=false, updated_at=now()
WHERE namespace=@namespace AND owner_id=@owner_id AND id=@node_id AND status='waiting' AND container_id='';
```

Run: `make sqlc`
Expected: 生成成功，`fleet_prepare.sql.go` 出现

- [ ] **Step 3: 让 `ready` 永不把等待节点读成就绪**

`fleet.sql` 的记录观察语句（`FleetRecordObservation`，约 `:487`）与 `reconciler.go` 的 `workerNativeReady`：

```sql
ready=(@ready::boolean AND NOT maintenance AND NOT revoked AND desired='running' AND status<>'waiting')
```

```go
func workerNativeReady(n model.Node, o model.Observation) bool {
	return n.Status != "waiting" && /* ...既有全部条件不变... */
}
```

- [ ] **Step 4: 唯一绑定索引**

`(n)_fleet_nodes_waiting_workspace_uidx.up.sql`：

```sql
-- At most one unterminated automatic managed node per workspace per namespace.
-- The partial predicate is what makes a deleted (terminated) binding re-creatable
-- while a live one cannot be duplicated by a concurrent prepare.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS fleet_nodes_workspace_live_uidx ON fleet_nodes (namespace, workspace_id) WHERE workspace_id IS NOT NULL AND NOT (desired = 'terminated' AND status = 'terminated');
```

第二个文件 `(n+1)_fleet_nodes_waiting_status_idx.up.sql`：

```sql
CREATE INDEX CONCURRENTLY IF NOT EXISTS fleet_nodes_waiting_idx ON fleet_nodes (namespace, id) WHERE status = 'waiting';
```

两个都登记进 `server/cmd/migrate/main.go` 的 `concurrentIndexCleanups`。**注意**：这条唯一索引会拒绝「同一工作区已有被 revoke 但未 terminated 的节点时再准备一个」——那正是规格要求的 fail closed；冲突时 `InsertFleetPreparedAuroraNode` 返回唯一性冲突，调用方按 `model.ErrConflict` 处理并提示已有冲突绑定。

- [ ] **Step 5: 写 store 层的失败测试**

```go
// server/internal/fleet/prepare_test.go（使用 testutil.NewFleetFixture 与真实数据库）
func TestPrepareIntentCreatesAWaitingNodeWithNoResources(t *testing.T)
func TestPrepareIntentIsIdempotentForTheSameKey(t *testing.T)
func TestPromotePrepareOperationPinsImageAndSpecAndKeepsTheOperationID(t *testing.T)
func TestPromotePrepareOperationRefusesOverCapacity(t *testing.T)      // 占满 maxNodes 后 promote 返回 0 行
func TestPromotePrepareOperationDoesNotCountWaitingNodesAgainstCapacity(t *testing.T)
func TestWaitingNodeIsNotClaimedByTheCreateWorker(t *testing.T)         // FleetClaimBootstrap 对该节点返回 ErrNoRows
func TestTombstoneOnWorkspaceDeleteMakesPromotionImpossible(t *testing.T)
```

Run: `(cd server && go test ./internal/fleet -run 'TestPrepare|TestPromote|TestWaiting|TestTombstone' -count=1)`
Expected: FAIL —— `undefined: model.Prepare`

- [ ] **Step 6: 实现 store 方法**

`store.InsertPrepareIntent`：`WithTx` → `CheckNamespaceAdmission` → `FleetOwnerExclusiveLock` → `GetFleetIntentByKey`（同 key 同 node 且 hash 相同 → 返回原意图，`replayed=true`；其它情况 `model.ErrConflict`）→ `FleetOwnerExists` → 检查该 workspace 无活跃绑定 → `InsertFleetPreparedAuroraNode` + `InsertFleetPrepareOperation`（`phase` 由部署配置决定：配置齐备 → `queued`；缺 image → `waiting_config`；已满容 → `waiting_capacity`，`error_code` 取 `image_unconfigured` / `owner_capacity_exceeded`）。

`store.PromotePrepareIntent`：`WithTx` → `CheckNamespaceAdmission` → `FleetOwnerExclusiveLock` → `FleetPromotePrepareOperation`；零行 → `model.ErrConflict`（调用方据此把 `waiting_capacity` 写回并保持等待）。

- [ ] **Step 7: 跑测试并通过**

Run: `(cd server && go test ./internal/fleet/... -count=1)`
Expected: PASS

- [ ] **Step 8: Commit**

```bash
git add server/pkg/db server/migrations server/internal/fleet server/cmd/migrate/main.go
git commit -m "feat(fleet): model a prepared workspace node as a waiting intent"
```

## Task 11: 准备协调器与工作区创建接线

**Files:**
- Create: `server/internal/aurora/prepare.go`、`prepare_coordinator.go`、`prepare_test.go`
- Modify: `server/internal/aurora/sandbox_manager.go`（`Ensure` 拆成 arm 与交付两步）
- Modify: `server/internal/handler/aurora.go`（生成路径不再负责首次创建）、新建 `aurora_runtime_prepare.go`
- Modify: `server/cmd/server/main.go`（启动协调器）
- Modify: `server/internal/fleet/service.go`、`http_aurora.go`、`store/aurora.go`（`ProvisionAuroraNode` 支持推进既有 waiting 节点）
- Create: `server/internal/handler/workspace_prepare_test.go`（两个创建入口的事务性）

**Interfaces:**
- Produces: `aurora.(*PrepareCoordinator).Run(ctx context.Context)`，每 5 秒一个 tick，用会话级 advisory lock 保证单实例。
- Produces: `aurora.(*SandboxManager).ArmPrepare(ctx, workspaceID, runtimeID pgtype.UUID, nodeID string) error`（只写事务内数据，不调用 Fleet）。
- Produces: `FleetProvisioner.EnsureWorkspaceNode` 语义收窄为「交付一次性密钥并请求推进既有意图」，请求体新增 `enrollment_token` 之外的**无**变化；不在等待态时返回 `model.ErrInvalidRequest`。

- [ ] **Step 1: 写协调器的失败测试**

```go
func TestPrepareCoordinatorAdmitsACommittedIntentOnce(t *testing.T)   // 两次 tick 只调用 Fleet 一次
func TestPrepareCoordinatorRecordsAnUnconfiguredDeploymentAndRetriesAfterConfiguration(t *testing.T)
func TestPrepareCoordinatorDoesNotActOnADeletedWorkspace(t *testing.T) // tombstone 后不再调用 Fleet、不创建节点
func TestPrepareCoordinatorReissuesTheEnrollmentSecretAfterAFleetRestart(t *testing.T)
func TestPrepareCoordinatorLeavesNoAgentIdentityBehindWhenArmingFails(t *testing.T)
func TestPrepareCoordinatorKeepsTheResourceOwnerAcrossAMemberRetry(t *testing.T) // OwnerID 始终是创建者，不因点重试的成员改变
```

Run: `(cd server && go test ./internal/aurora -run 'TestPrepareCoordinator' -count=1)`
Expected: FAIL —— `undefined: PrepareCoordinator`

- [ ] **Step 2: 实现协调器**

一个 tick 的算法（全部读走非锁查询，只有推进是事务）：

1. `select pg_try_advisory_lock(hashtextextended('aurora-prepare-coordinator', 0))` 在一个**长连接**上；拿不到就跳过本次 tick。
2. keyset 扫 `ListFleetWaitingNodes`（`namespace` 取当前 Fleet namespace；limit 100）。
3. 对每个 waiting 节点：读该节点对应的 `aurora_sandbox_node`（按 `backend_node_id`）与受管 runtime；若 sandbox 行已 `stopped`/`failed` 或工作区已 tombstone（operation `error_code='workspace_deleted'`）→ 跳过并调 `FleetDeleteWaitingNode`。
4. 配置检查：`AURORA_SANDBOX_IMAGE` 为空或 `SandboxManager == nil` → `FleetSetPreparePhase(waiting_config, image_unconfigured)`，**不创建任何东西**，等到下一 tick。
5. 铸一次性密钥：`auth.GenerateManagedEnrollmentToken()` → `LockAuroraSandboxEnrollmentWorkspace` + `RotateAuroraSandboxEnrollment`（幂等，身份保留）在一个小事务里；明文只放在本次调用的内存里。
6. 调 `FleetProvisioner.EnsureWorkspaceNode(ctx, ownerID, FleetEnsureRequest{... EnrollmentToken: 明文 ...})`。
7. 成功 → 明文随调用消失；Fleet 侧已把 operation 推进到 `queued`，节点 `status='creating'`。
8. 失败：`model.ErrConflict`（容量）→ `FleetSetPreparePhase(waiting_capacity, owner_capacity_exceeded)`；其他错误 → `FleetSetPreparePhase(waiting_config, <稳定码>)` 并记录日志。**绝不**标记 failed（规格：等待不是失败）。

- [ ] **Step 3: 工作区创建接线（两个入口）**

两个入口都要接：注册产生的个人工作区，与显式新建工作区。二者都已在一个事务里创建 workspace 行；在同一事务内追加：

```go
// Inside the existing workspace-creation transaction: no Docker call, no Fleet
// HTTP. The intent is a row; the prepare coordinator consumes it after commit.
if err := s.prepare.ArmPrepare(ctx, qtx, workspaceID, ownerID); err != nil {
	return err
}
```

`ArmPrepare` 的顺序：`ensureManagedRuntime`（写 `agent_runtime`，`provider='claude'`、`runtime_mode='local'`、`metadata` 留空等注册时由服务器写入）→ 生成 node/daemon UUID → `InsertPrepareIntent` → 写 `aurora_sandbox_node`（`state='starting'`，`backend_node_id = nodeID`）。失败不影响工作区创建以外的东西：**只有数据库写入失败才回滚整个事务**。

测试（两个入口各一条）：

```go
func TestRegisteringAWorkspaceCommitsAWaitingIntentWithoutFleet(t *testing.T)
func TestCreatingAWorkspaceCommitsAWaitingIntentWithoutFleet(t *testing.T)
func TestWorkspaceCreationSucceedsWhenTheRuntimeIsUnconfigured(t *testing.T) // 仍能创建，投影显示不可用原因
```

- [ ] **Step 4: 生成路径不再创建节点**

`handler/aurora.go:444` 的 `ensureWorkspaceSandbox` 从生成路径删除；`:183-186` 的 503 分支改为读投影：未配置 → 503 `aurora_runtime_unavailable`（保留原码与字段，可增加 `reasonCode`）；等待中 → 409 或 503 带 `runtime_preparing`；就绪 → 继续。**`aurora_runtime_unavailable` 的既有契约不变**（`aurora_test.go:2847/2899/3069/3177` 的断言继续成立）。

Run: `(cd server && go test ./internal/handler -run 'TestCreateAuroraGeneration' -count=1)`
Expected: PASS（若既有断言依赖 503 由 `ensureWorkspaceSandbox` 产生，改为断言投影来源的同一状态码与错误码）

- [ ] **Step 5: 让 Fleet 支持推进既有 waiting 节点**

`fleet/service.go` 的 `ProvisionAuroraNode`：先 `GetAuroraNode(owner, nodeID)`；命中且 `status='waiting'` → 校验 `image_digest == s.provisioning.Image`、`daemon_id` 一致、workspace/runtime 一致 → `Store.PromotePrepareIntent`；未命中 → 保持既有 `CreateAuroraIntent`（操作员直接创建节点的路径，今天由 `aurora_fleet_e2e_test.go` 覆盖）。拒绝：节点已 `creating` 或已 `revoked` → `model.ErrConflict`。

Run: `(cd server && go test ./internal/fleet ./internal/handler -run 'Aurora|Prepare' -count=1)`
Expected: PASS

- [ ] **Step 6: 启动协调器并跑门控测试**

`server/cmd/server/main.go` 在启用 Cloud Runtime 的分支里 `go prepareCoordinator.Run(ctx)`。

Run: `(cd server && go test ./internal/aurora ./internal/handler ./cmd/server -count=1)`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add server/internal/aurora server/internal/handler server/cmd/server server/internal/fleet
git commit -m "feat(aurora): prepare the workspace runtime from a committed intent"
```

## Task 12: 原子生成准入与请求幂等

今天 `CreateAuroraGeneration` 跨四个事务：`createAuroraGenerationWithEntitlement`（自带 tx）→ `Credit.Reserve`（自带 tx）→ `EnqueueQuickCreateTask`（`runFleetTx`）→ `UpdateAuroraGenerationTask`（单语句）。进程在任意两次提交之间退出都会留下「无任务的 generation」或「已预留未关联的运行」。

**Files:**
- Create: `server/internal/service/aurora_admission.go`、`aurora_admission_test.go`
- Create: `server/pkg/db/queries/aurora_idempotency.sql` + 迁移
- Modify: `server/internal/aurora/credit.go`（新增 `qtx` 原语）
- Modify: `server/internal/service/task.go`（把 `admitEnqueueAgent` 之上的封装抽成可复用的 `qtx` 路径）
- Modify: `server/internal/handler/aurora.go`（改为一次调用）

**Interfaces:**
- Produces: `service.(*TaskService).AdmitAuroraGeneration(ctx context.Context, req AuroraAdmissionRequest) (AuroraAdmission, error)`。

```go
type AuroraAdmissionRequest struct {
	WorkspaceID, UserID, AgentID pgtype.UUID
	SkillID, Prompt              string
	AttachmentIDs                []pgtype.UUID
	CreditsMicro                 int64
	IdempotencyKey               string // empty for a legacy client
	RequestFingerprint           string
}

type AuroraAdmission struct {
	Generation db.AuroraGeneration
	Task       db.AgentTaskQueue
	Replayed   bool
}
```

- Produces: `aurora.(*CreditService).ReserveInTx(ctx context.Context, qtx *db.Queries, userID, workspaceID pgtype.UUID, amountMicro int64, reference string) error` —— 与既有 `Reserve` 同样的余额行锁与账本幂等键，但写入调用方的 `qtx`，因此 service 包能在同一个事务里组合它。`Reserve` 保留为它的单事务包装。
- Produces: 生成请求幂等键表，唯一键 `(user_id, workspace_id, idempotency_key)`，值含请求指纹与 generation id。

- [ ] **Step 1: 幂等键迁移与 SQL**

```sql
-- (n)_aurora_idempotency.up.sql
CREATE TABLE IF NOT EXISTS aurora_generation_request (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL,
  workspace_id uuid NOT NULL,
  idempotency_key text NOT NULL,
  request_fingerprint text NOT NULL,
  generation_id uuid,
  created_at timestamptz NOT NULL DEFAULT now()
);
```

索引各自单文件、并发：`aurora_generation_request_key_uidx` unique on `(user_id, workspace_id, idempotency_key)`；`aurora_generation_request_created_idx` on `(created_at)`（清理用）。

```sql
-- name: InsertAuroraGenerationRequest :one
INSERT INTO aurora_generation_request (user_id, workspace_id, idempotency_key, request_fingerprint)
VALUES (@user_id, @workspace_id, @idempotency_key, @request_fingerprint)
RETURNING *;

-- name: GetAuroraGenerationRequest :one
SELECT * FROM aurora_generation_request WHERE user_id=@user_id AND workspace_id=@workspace_id AND idempotency_key=@idempotency_key;

-- name: LinkAuroraGenerationRequest :execrows
UPDATE aurora_generation_request SET generation_id=@generation_id
WHERE user_id=@user_id AND workspace_id=@workspace_id AND idempotency_key=@idempotency_key AND generation_id IS NULL;
```

- [ ] **Step 2: 写故障矩阵测试（红）**

在 `aurora_admission_test.go` 里，对 generation / ledger / task / link 四步各注入一次失败，断言**全部回滚**：

```go
func TestAdmissionRollsBackEverythingWhenTheLedgerFails(t *testing.T)
func TestAdmissionRollsBackEverythingWhenTheTaskInsertFails(t *testing.T)
func TestAdmissionRollsBackEverythingWhenTheLinkFails(t *testing.T)
func TestAdmissionReplayReturnsTheSameGeneration(t *testing.T)        // 同键同指纹 → 同一 generation，账本只扣一次
func TestAdmissionRejectsADifferentFingerprintOnTheSameKey(t *testing.T) // 409
func TestAdmissionWithoutAKeyStaysCompatibleAndIsNotClaimedIdempotent(t *testing.T)
func TestUnreadyAdmissionWritesNoGenerationAndNoLedgerRow(t *testing.T)  // 不占用提交键
```

Run: `(cd server && go test ./internal/service -run 'TestAdmission' -count=1)`
Expected: FAIL —— `undefined: AdmitAuroraGeneration`

- [ ] **Step 3: 实现单事务准入**

`AdmitAuroraGeneration` 的骨架（顺序是契约的一部分）：

```go
func (s *TaskService) AdmitAuroraGeneration(ctx context.Context, req AuroraAdmissionRequest) (AuroraAdmission, error) {
	return s.runFleetTx(ctx, req.AgentID, func(ctx context.Context, qtx *db.Queries) error {
		// 1. The fleet barrier for the agent's runtime is already held by
		//    runFleetTx (namespace -> node -> capacity -> task/workspace).
		locked, err := s.admitEnqueueAgent(ctx, qtx, req.AgentID)
		if err != nil {
			return err
		}
		// 2. Per-user admission. The Aurora entitlement lock is the same key the
		//    existing create path uses, so the monthly and concurrency counters
		//    cannot be raced by the two paths.
		if err := qtx.LockAuroraEntitlement(ctx, req.UserID); err != nil {
			return err
		}
		limits, err := aurora.LimitsForUser(ctx, qtx, s.Tiers, req.UserID)
		if err != nil {
			return err
		}
		// ... monthly + concurrency checks (same sentinels as today) ...
		// 3. Credit: balance row lock then ledger insert, in the same qtx. Never
		//    Credit.Reserve, which opens its own transaction.
		if err := s.Credit.ReserveInTx(ctx, qtx, req.UserID, req.WorkspaceID, req.CreditsMicro, generationReference); err != nil {
			return err
		}
		// 4. Generation, idempotency row, task, link. The task row is created
		//    last so a failure above leaves no orphan enqueue.
		// ...
		return nil
	})
}
```

要点：

- 事务前完成（无副作用）：审核、附件可读性、月度额度初始化、配置解析。事务内**重做**最终成员/绑定/容量/额度检查。
- 锁顺序：`runFleetTx` 的屏障先持有，再取 Aurora 用户额度锁，最后余额行锁。其它触及这些锁的路径必须遵守同一顺序。
- 提交后才发 WebSocket/唤醒提示（`NotifyTaskEnqueued`），提示丢失由既有队列扫描恢复。
- 新客户端在一次明确提交时持久化 `Idempotency-Key`（用户 + 工作区作用域）；同键同指纹返回同一 generation，不同指纹 409；旧客户端不带键时由服务器为该请求生成唯一 ID，**不宣称**跨请求自动重试幂等。
- 未就绪请求在写任何行之前返回，不消耗该提交键。

- [ ] **Step 4: 改 handler 调用点**

`handler/aurora.go` 的 11–14 步（`createAuroraGenerationWithEntitlement` → `Reserve` → `EnqueueQuickCreateTask` → `UpdateAuroraGenerationTask`）替换为一次 `h.TaskService.AdmitAuroraGeneration(...)`；`failGenerationAndRefund` 保留用于审核/附件阶段的事务前失败（那时还没有行可回滚）。

- [ ] **Step 5: 跑测试**

Run: `(cd server && go test ./internal/service ./internal/handler -run 'Aurora|Admission' -count=1)`
Expected: PASS

Run: `(cd server && go test ./internal/aurora -count=1)`
Expected: PASS（`CreditService` 的既有测试继续覆盖 `Reserve`/`Refund`；新增 `ReserveInTx` 的用例断言同一 `qtx` 下余额与账本同时可见）

- [ ] **Step 6: Commit**

```bash
git add server/internal/service server/internal/handler server/internal/aurora/credit.go server/pkg/db server/migrations
git commit -m "feat(aurora): admit a generation, its credits, and its task in one transaction"
```

## Task 13: 结果协调服务与 needs_review 闭环

今天 `settleAuroraOnFailed` 对失败任务退款，`aurora_provider_run` 的 `ambiguous` 分支也以失败退款为既有语义；但超时、心跳丢失或回调暂时不可读都**不能**证明 provider 没有执行。规格 §7 要求新增独立的结果协调服务，按可验证事实决定查询原 ID、导入资产、结算或进入 `needs_review`，不自动重试创建、不自动扣第二次款。

**Files:**
- Create: `server/internal/aurora/reconcile.go`、`reconcile_test.go`
- Create: `server/internal/service/aurora_reconcile.go`、`aurora_reconcile_test.go`
- Create: `server/migrations/<n>_aurora_generation_recovery.up.sql/.down.sql` + 索引文件
- Modify: `server/pkg/db/queries/aurora.sql`（`CountActiveGenerations`、新增恢复查询）→ `make sqlc`
- Modify: `server/internal/service/aurora_completion.go`（失败路径先询问协调器）
- Modify: `server/internal/handler/aurora_provider_run.go`（ambiguous 不再直接等价于退款）
- Modify: `server/cmd/server/main.go`
- Create: `server/internal/handler/aurora_reconcile_ops.go` + 路由（受服务认证的运维入口）

**Interfaces:**
- Produces: `aurora.RecoveryState` 常量 `RecoveryNone = "none"`、`RecoveryReconciling = "reconciling"`、`RecoveryNeedsReview = "needs_review"`。
- Produces: `aurora.(*Reconciler).Plan(facts Facts) Action`（纯函数，可表驱动测试）。
- Produces: `POST /internal/v1/aurora/reconcile`（服务密钥认证）提交 provider/资产证据，追加审计记录后推进同一个 generation；**不能**任意改余额。
- Consumes: `aurora_provider_run` 的 `external_id` 与状态、`aurora_asset` 的合规资产、`credit_ledger` 的幂等键。

- [ ] **Step 1: 迁移**

```sql
-- (n)_aurora_generation_recovery.up.sql
ALTER TABLE IF EXISTS aurora_generation ADD COLUMN IF NOT EXISTS recovery_state text NOT NULL DEFAULT 'none';
ALTER TABLE IF EXISTS aurora_generation ADD COLUMN IF NOT EXISTS recovery_reason text;
ALTER TABLE IF EXISTS aurora_generation ADD COLUMN IF NOT EXISTS recovery_deadline timestamptz;
ALTER TABLE IF EXISTS aurora_generation ADD COLUMN IF NOT EXISTS recovery_updated_at timestamptz;
```

索引各单文件、并发：`aurora_generation_recovery_idx` on `(recovery_deadline)` WHERE `recovery_state = 'reconciling'`。

**`aurora_generation.status` 不新增枚举值**：旧客户端的 status 不改为未知枚举；业务状态与 `recovery_state` 分离。

- [ ] **Step 2: 写表驱动的规则测试（红）**

```go
func TestReconcilePlanFollowsTheVerifiedFactsTable(t *testing.T) {
	cases := []struct {
		name  string
		facts aurora.Facts
		want  aurora.Action
	}{
		{"no provider run, trusted barrier stopped, no pending report", aurora.Facts{TrustedStop: true}, aurora.ActionFailAndRefund},
		{"external id and the provider can be queried", aurora.Facts{ExternalID: "job-1", Queryable: true}, aurora.ActionQueryOriginal},
		{"provider failed with no deliverable asset", aurora.Facts{ProviderTerminal: "failed"}, aurora.ActionRefundTerminal},
		{"creating or ambiguous with no external id", aurora.Facts{ProviderState: "ambiguous"}, aurora.ActionNeedsReview},
		{"compliant asset exists but the generation is not terminal", aurora.Facts{HasCompliantAsset: true, TaskTerminal: true}, aurora.ActionReplaySettlement},
		{"task terminal but the terminal update failed", aurora.Facts{TaskTerminal: true, TerminalUpdateFailed: true}, aurora.ActionRetryLedgerKey},
	}
	// table-driven: each case must map to exactly the listed action
}

func TestReconcileEscalatesAfterFifteenMinutesWithoutDeclaringFailure(t *testing.T)
func TestReconcileNeverResubmitsWithoutAnExternalID(t *testing.T)
func TestCountActiveGenerationsExcludesNeedsReview(t *testing.T)
```

Run: `(cd server && go test ./internal/aurora -run 'TestReconcile|TestCountActiveGenerations' -count=1)`
Expected: FAIL —— `undefined: aurora.Facts`

- [ ] **Step 3: 实现领域规则与有界扫描**

`reconcile.go`：`Facts` 结构体 + `Plan`（纯函数，逐条对应规格 §7 的表）+ `Action` 枚举（`ActionFailAndRefund`、`ActionQueryOriginal`、`ActionRefundTerminal`、`ActionNeedsReview`、`ActionReplaySettlement`、`ActionRetryLedgerKey`）。

`service/aurora_reconcile.go`：每 30 秒有界扫描（keyset、LIMIT、单个 tick 总时限 20 秒），只读取已有 generation/task/provider-run/asset/ledger 事实并持久化修复状态；明确可查询的恢复最多自动观察 15 分钟，然后转 `needs_review` 并展示原因。**超时不是失败判定**，只是升级人工处理。迟到回调先按原 task attempt / 租约 / 注册代次 / 身份核对，作为**证据**进入协调服务，不能复活已终态任务或跨代次覆盖结果。

`CountActiveGenerations` 改为：

```sql
-- name: CountActiveGenerations :one
SELECT count(*) FROM aurora_generation g
LEFT JOIN agent_task_queue t ON t.id = g.task_id
WHERE g.user_id = $1
  AND g.recovery_state <> 'needs_review'
  AND (
    (g.task_id IS NULL AND g.status = 'queued')
    OR t.status IN ('queued', 'dispatched', 'running', 'waiting_local_directory', 'deferred')
  );
```

已发生的月度生成计数**不因此清零**。

- [ ] **Step 4: 改失败路径的入口**

`settleAuroraOnFailed` 与 `handler/aurora_provider_run.go` 的 `ambiguous` 分支：先构造 `Facts` 交给协调器；`ActionFailAndRefund` 才退款。`ActionNeedsReview` 时**不退款**、保留预留，并把 `recovery_state` 置为 `needs_review`、`recovery_reason` 写稳定码。既有 `aurora_provider_run` 的 `ambiguous` 语义因此被**明确改变**（不是「直接复用」）——在提交信息与 `AGENTS.md` 里写清。

- [ ] **Step 5: 运维入口与审计**

`POST /internal/v1/aurora/reconcile`：服务密钥认证（复用 `X-Fleet-Service-Key` 的校验路径），body 为 `{generation_id, evidence: {...}}`；处理流程是「追加审计记录 → 在协调器规则内推进同一个 generation」，**没有**改余额的字段。

- [ ] **Step 6: 跑测试**

Run: `(cd server && go test ./internal/aurora ./internal/service ./internal/handler -run 'Reconcil|Settle|Aurora' -count=1)`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add server/internal/aurora server/internal/service server/internal/handler server/pkg/db server/migrations server/cmd/server
git commit -m "feat(aurora): reconcile uncertain provider results instead of refunding them"
```

---

# M4 · G4 应用接线

## Task 14: 准备/唤醒 API 与运行时投影

**Files:**
- Create: `server/internal/handler/aurora_runtime_prepare.go`、`aurora_runtime_prepare_test.go`
- Modify: `server/internal/handler/aurora_runtime_view.go`、`aurora_runtime_view_test.go`
- Modify: `server/cmd/server/router.go`

**Interfaces:**
- Produces: `POST /api/aurora/runtime/prepare`（`RequireHumanActor` + `RequireWorkspaceMember`）。

```go
type AuroraRuntimePrepareResponse struct {
	WorkspaceID string  `json:"workspaceId"`
	NodeID      *string `json:"nodeId"`
	RuntimeID   *string `json:"runtimeId"`
	OperationID *string `json:"operationId"`
	State       string  `json:"state"`
	Ready       bool    `json:"ready"`
	ReasonCode  *string `json:"reasonCode,omitempty"`
}
```

- Produces: 扩展后的投影（保留既有字段，新增可选字段；旧客户端不因新字段失败）：

```go
type AuroraRuntimeNodeResponse struct {
	ID          string  `json:"id"`
	Status      string  `json:"status"`
	Ready       bool    `json:"ready"`
	Provider    string  `json:"provider"`
	ErrorCode   *string `json:"errorCode,omitempty"`
	OperationID *string `json:"operationId,omitempty"`
	CreatedAt   string  `json:"createdAt"`
}

type AuroraExecutionTargetResponse struct {
	WorkspaceID    string                     `json:"workspaceId"`
	Node           *AuroraRuntimeNodeResponse `json:"node"`
	RuntimeID      *string                    `json:"runtimeId"`
	State          string                     `json:"state"`
	Ready          bool                       `json:"ready"`
	AllowedActions []string                   `json:"allowedActions"`
	SkillReadiness map[string]AuroraSkillReadiness `json:"skillReadiness"`
	ErrorCode      *string                    `json:"errorCode,omitempty"`
}

type AuroraSkillReadiness struct {
	Ready       bool    `json:"ready"`
	ReasonCode  *string `json:"reasonCode,omitempty"`
}
```

- Produces: 公开错误码常量 `runtime_unconfigured`、`runtime_preparing`、`runtime_offline`、`runtime_busy`、`runtime_capacity_exceeded`、`runtime_policy_unavailable`、`skill_unavailable`、`provider_unconfigured`。

- [ ] **Step 1: 写投影状态优先级的失败测试**

```go
func TestRuntimeStatePriorityIsFixed(t *testing.T) {
	// deleting -> maintenance -> failed -> stopped -> provisioning (incl. waiting)
	// -> offline -> online
}
func TestWaitingReasonsAreNeverRenderedAsCreationInProgress(t *testing.T) {
	// waiting_config / waiting_capacity must carry a concrete reasonCode and must
	// not read as "Docker has started"
}
func TestRuntimeViewStaysReadOnly(t *testing.T)           // 既有 TestAuroraRuntimeViewIsReadOnly 必须继续通过
func TestPrepareReturns200WithoutAnOperationWhenAlreadyReady(t *testing.T)
func TestPrepareIsIdempotentAndReturnsTheSameOperationID(t *testing.T)
func TestPrepareRejectsANonMember(t *testing.T)
func TestPrepareCannotRestoreANodeInMaintenanceOrDeleting(t *testing.T)
```

Run: `(cd server && go test ./internal/handler -run 'TestRuntimeState|TestWaitingReasons|TestPrepare|TestAuroraRuntimeView' -count=1)`
Expected: FAIL

- [ ] **Step 2: 实现投影与准备端点**

`aurora_runtime_prepare.go`：

- 读路径继续用**非锁**查询（`GetAuroraSandboxNodeByWorkspace` + `GetAuroraManagedRuntime`），并按 `backend_node_id` 直读 `fleet_nodes` 行取 `status` / `error_code` / `operation_id` —— 这是共享数据库里的普通读，不取生命周期锁、不调 Fleet。
- `POST /prepare`：仅接受人类工作区成员（服务端重新验证身份，不信任前端缓存）；请求体**不接收**镜像、凭据、执行策略或运行时 ID；已就绪 → 200 当前投影且不创建操作；否则确保存在等待意图（已有则复用同一活动 operation，无则新建），返回 202 与稳定操作引用。
- `allowedActions` 按当前调用者计算：`prepare` / `retry` / `wake` / `none`。缺失或未知动作一律不返回。
- `skillReadiness` 按 skill ID 给出 `ready` 与**可公开**的阻塞原因；不暴露密钥值、私密路径、完整内部错误或宿主机信息。
- 生成接口继续保留 `aurora_runtime_unavailable` 响应兼容，可增加 `reasonCode`；不删除旧客户端依赖的字段。

- [ ] **Step 3: 路由**

`server/cmd/server/router.go` 在 `:2069` 附近加入，与既有 Aurora 路由同一守卫链：

```go
r.Group(func(r chi.Router) {
	r.Use(handler.RequireHumanActor)
	r.Use(handler.RequireWorkspaceMember)
	r.Post("/api/aurora/runtime/prepare", h.PrepareAuroraRuntime)
})
```

`/api/aurora/runtime/prepare` 不是新的**全局**路由，因此不需要改 `reserved_slugs.json`。

- [ ] **Step 4: 跑测试**

Run: `(cd server && go test ./internal/handler ./cmd/server -count=1)`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add server/internal/handler server/cmd/server/router.go
git commit -m "feat(aurora): expose runtime preparation and readiness in the projection"
```

## Task 15: core 契约（zod / query / mutation）

**Files:**
- Modify: `packages/core/aurora/{schema,types,api,queries,mutations}.ts`
- Modify: 对应 `*.test.ts`
- Modify: `e2e/fixtures.ts`（wire schema 对齐，见 Task 19）

**Interfaces:**
- Produces: `AuroraRuntimeState` 扩为 `"unconfigured" | "provisioning" | "online" | "failed" | "stopped" | "offline" | "maintenance" | "deleting"`，`normalizeAuroraRuntimeState` 对未知值继续回落 `unconfigured`。
- Produces: `AuroraExecutionTarget` 新增 `ready: boolean`（默认 `false`）、`allowedActions: string[]`（默认 `[]`）、`skillReadiness: Record<string, AuroraSkillReadiness>`（默认 `{}`）、`errorCode?: string`。
- Produces: `prepareAuroraRuntime(): Promise<ParseResult<AuroraRuntimePrepareResult>>`、`usePrepareAuroraRuntime()`；`auroraRuntimeOptions` 增加 `refetchInterval`。

- [ ] **Step 1: 先写 schema 与 normalize 的矩阵测试（红）**

```ts
// packages/core/aurora/schema.test.ts
it.each([
  ["deleting", "deleting"],
  ["maintenance", "maintenance"],
  ["offline", "offline"],
  ["stopped", "stopped"],
  ["paused", "unconfigured"],   // unknown from a newer server
  [undefined, "unconfigured"],
])("normalizes %s to %s", (input, want) => {
  expect(normalizeAuroraRuntimeState(input)).toBe(want);
});

it("defaults readiness fields so an older server still parses", () => {
  const parsed = auroraExecutionTargetSchema.parse({ workspaceId: "w", node: null, runtimeId: null, state: "online" });
  expect(parsed.ready).toBe(false);
  expect(parsed.allowedActions).toEqual([]);
  expect(parsed.skillReadiness).toEqual({});
});
```

Run: `pnpm --filter @multica/core test`
Expected: FAIL

- [ ] **Step 2: 实现 schema、api、query、mutation**

- `schema.ts`：按上面的 Interfaces 加字段，全部带默认值；`skillReadiness` 用 `z.record(z.string(), auroraSkillReadinessSchema)`；`errorCode` 用 `z.string().optional()`。
- `api.ts`：新增 `AURORA_RUNTIME_PREPARE_PATH = "/api/aurora/runtime/prepare"` 与 `prepareAuroraRuntime()`；沿用 `parseWithFallbackResult`，fallback 为 `{ workspaceId: "", node: null, runtimeId: null, operationId: null, state: "unconfigured", ready: false }`。响应 409（已就绪/冲突）不降级为失败：调用方随后重查投影。
- `queries.ts`：`auroraRuntimeOptions(wsId)` 增加轮询，规则与规格 §6.3 一致：

```ts
refetchInterval: (query) => {
  const state = query.state.data?.value?.state;
  const ready = query.state.data?.value?.ready ?? false;
  if (query.state.status === "error") return false;
  if (state === "online" && ready) return AURORA_RUNTIME_READY_POLL_MS;   // 10_000
  return AURORA_RUNTIME_PREPARING_POLL_MS;                                // 3_000
},
```

并在 `packages/core/query-client.ts` 的既有全局策略之外，确认 `refetchOnWindowFocus` 对该 query 为真（窗口恢复焦点立即重查；后台标签由 React Query 的默认行为停止）。离开工作区即随 `wsId` 变化取消订阅，不复用其它工作区的数据。
- `mutations.ts`：`usePrepareAuroraRuntime()`，`onSettled` 同时失效 `auroraKeys.runtime(wsId)` 与 `auroraKeys.generations(wsId)`。

轮询常量加到 `types.ts`，与既有 `AURORA_GENERATION_POLL_MS = 3_000` 并列。

- [ ] **Step 3: 用既有的 options 直调风格钉住轮询接线**

`queries.test.ts` 沿用把 `refetchInterval` 抽成普通函数直接调用的既有做法：

```ts
it.each([
  [{ state: "provisioning", ready: false }, 3_000],
  [{ state: "online", ready: false }, 3_000],
  [{ state: "online", ready: true }, 10_000],
])("polls %o at %dms", (value, want) => {
  expect(runtimePollInterval({ status: "success", data: { value, degraded: false } })).toBe(want);
});
it("stops polling on an error", () => {
  expect(runtimePollInterval({ status: "error" })).toBe(false);
});
```

Run: `pnpm --filter @multica/core test`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add packages/core/aurora
git commit -m "feat(core): contract for runtime preparation and readiness"
```

## Task 16: views 与 apps/aurora 接线

**Files:**
- Modify: `packages/views/aurora/runtime-status.tsx`、`runtime-status.test.tsx`
- Modify: `packages/views/aurora/generation-composer.tsx`、`generation-composer.test.tsx`
- Modify: `apps/aurora/app/[workspaceSlug]/runtimes/page.tsx`

**Interfaces:**
- Consumes: Task 15 的 `useAuroraRuntime()`（含新字段）、`usePrepareAuroraRuntime()`。
- Produces: 运行时屏的三种操作与对应文案键（Task 17 填五语），并在生成抽屉上加入就绪门禁。

- [ ] **Step 1: 写运行时屏的失败测试**

`runtime-status.test.tsx` 沿用既有「不 mock hooks、只替换 `useWorkspaceId`、按 path 路由 `requestJson`」的接线风格，新增：

```tsx
it("offers preparation for a workspace with no binding", async () => { /* allowedActions: ["prepare"] -> button */ });
it("shows a concrete reason while waiting and does not claim Docker started", async () => { /* waiting_config + reasonCode */ });
it("offers a retry after a failure", async () => { /* state: failed, allowedActions: ["retry"] */ });
it("offers a wake action for a stopped node", async () => { /* state: stopped, allowedActions: ["wake"] */ });
it("never renders a readiness it was not told about", async () => { /* ready: false -> no ready affordance */ });
```

- [ ] **Step 2: 实现运行时屏**

`RuntimeStatus` 增加操作区：`allowedActions` 含 `prepare`/`retry` → 「准备运行时」/「重试准备」；含 `wake` → 「唤醒运行时」；状态不明 → 显示恢复状态，**不**显示虚假的已完成。恢复操作等待服务器接受后更新操作引用，再追踪状态，不乐观标为就绪。`STATE_VISUAL` 补上 `stopped`/`offline`/`maintenance`/`deleting` 四条（沿用 `packages/views/runtimes/components/shared.tsx` 的视觉词汇）。

`apps/aurora/app/[workspaceSlug]/runtimes/page.tsx` 保持 server component 一行不变：操作按钮属于 view，页面只挂载它。不新增 store，也不在页面里放业务状态。

- [ ] **Step 3: 写生成抽屉的就绪门禁测试**

```tsx
it("disables submit while the runtime is preparing and says why", async () => {});
it("keeps the draft when the runtime state changes", async () => {});
it("still submits when the runtime is ready", async () => {});
it("maps a 503 aurora_runtime_unavailable to a runtime-specific message", async () => {});
```

最后一条是今天的实际缺口：503 目前落进通用 `composer.create_failed`，用户被引导去重试一个不会成功的操作。

- [ ] **Step 4: 实现门禁**

`canSubmit` 增加 `runtime.ready === true` 与 `skillReadiness[skill.id]?.ready !== false`；运行时状态变化**不清空**草稿；`SubmitFailure` 增加 `isAuroraRuntimeUnavailableError`（按 `ApiError.code === "aurora_runtime_unavailable"` 判定，而不是只按状态码，因为 503 也被支付未配置使用）、`isAuroraRuntimePreparingError`（`code === "runtime_preparing"`）两个分类器，放在 `packages/core/aurora/api.ts` 与既有分类器并列，并补 `api.test.ts` 的用例。

- [ ] **Step 5: 跑前端检查**

Run: `pnpm typecheck && pnpm lint && pnpm test`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add packages/views/aurora packages/core/aurora apps/aurora
git commit -m "feat(aurora): prepare and gate generation on runtime readiness"
```

## Task 17: 五语文案与无障碍名称

**Files:**
- Modify: `packages/views/locales/{en,zh-Hans,fr,ja,ko}/aurora.json`

**Interfaces:**
- Consumes: Task 16 引用的全部新键。
- Produces: 五语齐备的 `aurora` namespace（否则 `packages/views/locales/parity.test.ts` 红）。

- [ ] **Step 1: 收集新增键并先写英文基准**

`runtime.*` 新增：`status.stopped`/`status.offline`/`status.maintenance`/`status.deleting`、`action.prepare`/`action.retry`/`action.wake`、`reason.image_unconfigured`/`reason.owner_capacity_exceeded`/`reason.policy_unavailable`/`reason.workspace_deleted`、`preparing.title`/`preparing.description`、`stopped.description`、`skill.blocked`。`composer.*` 新增：`runtime_not_ready_title`/`runtime_not_ready_description`、`runtime_unavailable_title`/`runtime_unavailable_description`、`runtime_preparing_title`/`runtime_preparing_description`。

文案规则（`AGENTS.md` 的 UI Copy 节）：描述默认可省，不重述标题或按钮动作；只在「非显然的选择、约束、后果或下一步」时给帮助文案；**费用与准备限制必须可见**。中文称「运行时」，`skill` 保持英文。

- [ ] **Step 2: 填充五种语言**

先读 `apps/docs/content/docs/developers/conventions.mdx` 与 `conventions.zh.mdx` 再落笔。英语与 zh-Hans 逐键人工过一遍；fr/ja/ko 同样必须齐备（parity 测试要求），术语与既有 `runtime.title` 一致。

顺手修掉已被记录的两处不一致：`history.skill_label` 的 zh-Hans 值改为与 `detail.skill_label` 一致（都读「技能」）。

- [ ] **Step 3: 跑 parity 与组件测试**

Run: `pnpm --filter @multica/views test`
Expected: PASS（含 `locales/parity.test.ts`）

- [ ] **Step 4: 检查无障碍名称**

所有新按钮与状态提示都要有可读的名称；`role="alert"` 只用于需要立刻读出的阻塞原因。运行：

Run: `pnpm --filter @multica/views test -- runtime-status generation-composer`
Expected: PASS（若单文件参数形式不被 turbo 接受，运行整个 `pnpm --filter @multica/views test` 并在报告中说明**实际**跑的命令）

- [ ] **Step 5: Commit**

```bash
git add packages/views/locales
git commit -m "feat(aurora): runtime preparation copy in all five locales"
```

---

# M5 · G5 迁移与最终验收

## Task 18: 迁移映射、检查点与切换

这是对当前本地 Fleet 的**受控升级**，不支持双运行时长期并行或内部双写。顺序不允许调换：不能先拆除旧执行链路再准备新镜像。

**Files:**
- Create: `server/internal/fleet/store/migration.go`、`migration_test.go`
- Create: `server/pkg/db/queries/fleet_migration.sql` → `make sqlc`
- Create: `server/migrations/<n>_fleet_node_migration_log.up.sql/.down.sql` + 索引文件
- Create: `deploy/aurora-sandbox/migration/README.md`（操作手册）

**Interfaces:**
- Produces: 每节点持久记录 `inventory_verified → admission_closed → drained → old_resources_stopped → binding_converted → new_resources_ready → admitted` 的迁移日志表，保留旧/新镜像、精确容器/卷/网络、runtime/agent/token 映射与完成证明。
- Produces: 冲突报告 —— 多重运行时或资源归属不明时**拒绝自动合并**并输出可审阅的映射。

- [ ] **Step 1: 迁移日志表**

```sql
-- (n)_fleet_node_migration_log.up.sql
CREATE TABLE IF NOT EXISTS fleet_node_migration_log (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  namespace text NOT NULL,
  owner_id uuid NOT NULL,
  node_id uuid NOT NULL,
  workspace_id uuid,
  checkpoint text NOT NULL,
  reason text,
  detail jsonb NOT NULL DEFAULT '{}'::jsonb,
  recorded_at timestamptz NOT NULL DEFAULT now()
);
```

索引各单文件、并发：`fleet_node_migration_log_node_idx` on `(namespace, node_id, recorded_at)`；`fleet_node_migration_log_checkpoint_idx` on `(checkpoint)`。

- [ ] **Step 2: 切换前的只读清点**

对目标 namespace 依次核对并写入日志（全部只读，任何一项不明就停）：实际镜像与 digest（节点与 sidecar 必须解析到同一架构 manifest）、节点归属（`workspace_id`/`runtime_id`/`profile_ref`）、数据库身份、活跃任务、待回传报告、未完成的 provider/账务结果。

冲突处理（规格 §10.3）：多重运行时或资源归属不明 → **拒绝自动合并**，输出映射表交原资源管理员决定迁移范围。既有跨工作区的手工节点不自动拆分或迁移。

- [ ] **Step 3: 逐检查点推进**

按顺序执行，每一步只有在事实被证明后才记录：

1. 经既有屏障**停止新准入**（保持维护屏障），让旧任务与回传收尾；**不强杀执行、不因超时销毁未知资源**；API 继续处理结果直到确认静止。
2. 应用 additive 数据变更（Task 7/10/12/13/18 的迁移）；新增迁移日志存储先于新控制程序启动。
3. 经原资源所有权检查关闭旧节点与代理，记录 `old_resources_stopped`。
4. 转换绑定（保留原 `runtime_id`，转换 provider/mode/metadata 并更新智能体关系）；核查 `(workspace_id, owner_id, runtime_id, system_key)` 的系统智能体唯一约束，**不得通过重新 seed 产生重复智能体**；历史 task/generation/账本不改写。
5. 以统一引导启动新资源；`aurora_sandbox_node` 停止参与新生命周期，确认 token、reaper、heartbeat、workspace delete 与 SQL 引用迁移后退役。
6. 新节点通过隔离、注册、健康与能力检查后，才解除准入屏障。

每个检查点都要有中断恢复测试（`migration_test.go`），覆盖：清点中途失败、排空超时、旧资源停止后崩溃、绑定转换后崩溃、新资源就绪前崩溃。

- [ ] **Step 4: 回滚边界**

默认**向前恢复**：保留维护屏障、原操作、映射与数据，按最后已证明的检查点向前。逆向迁移**只允许**在解除准入前且新节点没有任何业务写入的条件下按已审查的逐行补偿执行。整库快照恢复**不是**本计划的回滚步骤：Fleet 与 API 共用数据库，恢复整库会覆盖其它工作区的新任务与账本；它是需要全库停写与独立授权的灾难恢复操作。恢复前不允许新旧节点同时领取任务。

- [ ] **Step 5: 跑测试与 Commit**

Run: `(cd server && go test ./internal/fleet/... -count=1)`

```bash
git add server/internal/fleet server/pkg/db server/migrations deploy/aurora-sandbox/migration
git commit -m "feat(fleet): record and gate the node migration checkpoints"
```

## Task 19: 13-route 受控闭环与浏览器验收

规格 §9 要求确定性闭环跑**真正的统一镜像、daemon、Claude CLI 与 broker**，通过受控模型/provider HTTP 服务返回稳定响应与媒体素材；镜像里**不得**加入 fake daemon、fake Claude、测试专用 skill 或安全绕过开关，也不发布 fake 节点镜像。

**Files:**
- Modify: `e2e/aurora-cloud-runtime.spec.ts`、`e2e/fixtures.ts`
- Create: `server/internal/aurora/acceptance_13routes_test.go`（`//go:build dockerintegration`）
- Create: `docs/superpowers/plans/2026-10-08-unified-cloud-runtime-acceptance.md`（新的验收记录文件，不与历史的混写）

**Interfaces:**
- Consumes: Task 6 的 fixture 服务、Task 5 的已发布 digest、Task 14 的 API。
- Produces: 逐条记录的命令与观察结果；`PASS` / `FAIL` / `SKIP` / 身份未核验分别表达。

- [ ] **Step 1: 13 条 skill 路由的运行证据**

对 `server/internal/aurora/execution_policy.go` 里 13 个 skill 各跑一次真实镜像闭环，断言：输入约束、工具路由、真实适配器输出契约、产物入库、审核、作品可见、结算/退款各一次且只有一次。未开放的目录项**不能**被计作可运行。

Run（需显式授权）：

```bash
( cd server && MULTICA_RUN_DOCKER_INTEGRATION=1 MULTICA_RUN_REAL_AGENT_SMOKE=1 \
  go test -tags='dockerintegration agentintegration' ./internal/aurora -run '^TestAuroraUnifiedImageRoutes$' -count=1 -v )
```

Expected: 13 个子用例各自 PASS；未授权的 provider 子用例记为 SKIP 并写明原因。

- [ ] **Step 2: 浏览器路径**

`e2e/aurora-cloud-runtime.spec.ts` 扩展为：注册或新建工作区 → 准备中不能生成（断言按钮禁用与原因可见）→ 失败可重试 → 就绪后生成 → 进度更新 → 作品预览/下载 → 余额变化 → 离线恢复 → 刷新 → 切换工作区。

**同时修正该 spec 的登录路径**：它目前经 `loginFleetBrowser` 往 `localStorage` 注入 `multica_token`，而 Aurora 的 provider 是 `cookieAuth`；这个不一致从未被验证过（spec 默认 skip）。改成与生产一致的 cookie 会话，并把「本 spec 从未在本地跑过」这件事写进验收记录。

Run:

```bash
MULTICA_RUN_DOCKER_INTEGRATION=1 make env-exec ARGS="-- pnpm exec playwright test --project=fleet-docker"
```

Expected: 全部通过；任何失败按实际输出记录。

- [ ] **Step 3: 真实 provider 与受控服务分开记录**

受控服务闭环证明**工程集成**，真实 Claude/model/provider 烟测证明**外部服务可用**。真实调用继续沿用 `agentintegration` + `MULTICA_RUN_REAL_AGENT_SMOKE=1` + owner 明确授权，**不从本设计确认推导调用或消费授权**；未授权时明确保留未验证状态，不声称线上服务已经验证。

- [ ] **Step 4: 证据清单**

每次验收记录：代码 HEAD、唯一 index digest、实际架构 manifest/image ID、Docker Engine/平台、策略版本与加载结果、受管 API URL、workspace/node/runtime/task/generation 关联、命令与结果、资产及账本断言、浏览器 trace/screenshot。**不得记录密钥或 token。**

- [ ] **Step 5: Commit**

```bash
git add e2e docs/superpowers/plans/2026-10-08-unified-cloud-runtime-acceptance.md server/internal/aurora
git commit -m "test(aurora): record the unified runtime acceptance evidence"
```

## Task 20: 退役旧入口与文档回填

**Files:**
- Delete: `server/internal/aurora/sandbox_reaper.go` 的旧生命周期分支（若 Task 18 已确认无引用）
- Modify: `AGENTS.md`（本仓库）
- Modify: `docs/superpowers/specs/2026-10-06-unified-cloud-runtime-aurora-design.md` 的状态行
- Modify: `docs/superpowers/plans/2026-10-06-aurora-cloud-runtime-integration.md`、`2026-10-04-local-docker-cloud-runtime.md`（加取代说明）
- Modify: `deploy/aurora-sandbox/README.md`、`.env.example`

- [ ] **Step 1: 删除旧入口**

以全仓搜索为准，逐项确认后再删：独立发布任务、Aurora 专用 bootstrap/生命周期入口、失效配置字段（`proxy_image` 已在 Task 4 删除）、旧计划链接。

```bash
grep -rn "aurora-sandbox\|aurora-egress\b" --include='*.go' --include='*.yml' --include='*.hcl' --include='*.sh' --include='*.json' . | grep -v node_modules | grep -v '/.git/'
```

- [ ] **Step 2: 回填文档**

- `AGENTS.md`：镜像身份从「双镜像对」改为单一 index digest；记录 Task 5 Step 6 的 digest 与 run id；记录 `ambiguous` 语义已被协调服务取代；G0 的实测结论与平台边界（Docker Desktop 不在支持集合内）。
- 规格状态行改为反映实施状态（保留 r2 的修订号）。
- 两个被取代的计划加 `> **2026-10-08 取代说明：**` 段，指向本计划，并明确旧任务完成状态**不**等于新设计已实施。

- [ ] **Step 3: 跑全量检查**

Run: `make check`
Expected: PASS（若某个门因环境缺失无法运行，在报告里写明实际跑过的命令与跳过的项，不写成通过）

Run: `(cd server && go test ./internal/daemon -run 'TestManagedSecret|TestProviderSecretMount' -count=1)`
Expected: PASS

- [ ] **Step 4: Commit 并开 PR**

```bash
git add AGENTS.md docs/superpowers deploy/aurora-sandbox/README.md .env.example server
git commit -m "docs(aurora): retire the dual-image entries and record the unified runtime status"
git push -u origin "$(git rev-parse --abbrev-ref HEAD)"
gh pr create --repo eanfs/multica --fill
```

（不用 `git add -A`：只 stage 上面列出的路径，未跟踪的本地文件不得进提交。）

---

## Spec Coverage 自审表

| 规格要求 | 章节 | 任务 | 关键证据 |
| --- | --- | --- | --- |
| 一个发布镜像、同一个 digest、一套 Cloud Runtime | §1.1、§4.1、§4.2 | 4、5 | 单一 `docker/runtime/Dockerfile`；唯一的 OCI index；`verify-published` 列出恰好两个同 index 子 manifest |
| 节点与 sidecar 同镜像同 digest，sidecar 只以固定入口启动 | §1.6、§4.1 | 4、5 | 删除 `ProxyImage`；`egressProxySpec` 取 `Config.Image`；镜像校验器断言代理入口存在且非 shell wrapper |
| 统一 Claude 入口，删除第二套安装 | §4.1 | 4 | `/usr/local/bin/claude`；`AuroraClaudePath` 与校验器同步；`Config.Env` 只有 `PATH`/`HOME` |
| 收紧执行权限，隔离不能生效时禁止执行 | §1.7、§8.2、§8.3 | 3 | 固定入口 `cx` 子 profile；`policy-transition` 实测；`runtime_policy_unavailable` |
| 唯一身份：`provider=claude`、`runtime_mode=local`、服务器写 metadata | §5.1 | 7 | 526 索引重建为同一谓词；注册/claim/SQL/系统智能体同步；`fleetguard.Managed` 是唯一受管判据 |
| 每工作区至多一个未终止受管节点；多重绑定拒绝自动选择 | §5.1、§10.3 | 10、18 | `fleet_nodes_workspace_live_uidx`；冲突时输出映射并停止 |
| 创建工作区与准备意图同事务，事务内不调 Docker/Fleet HTTP | §5.2 | 11 | 两个创建入口的测试：提交后只有 waiting 行与 `prepare` operation |
| waiting 不占资源、不消费重试、有具体阻塞原因 | §5.2 | 10、14 | `status='waiting'` + `phase IN (waiting_config,waiting_capacity)`；节点不被 create worker 领取；投影给出 `reasonCode` |
| owner/capacity/删除 tombstone 规则 | §5.2、§5.4 | 10、11 | `CountFleetAdmittedNodes`；`FleetTombstonePrepareIntents`；owner 不因成员重试改变 |
| 注册凭据持久化、重启恢复、8 小时到期前轮换 | §5.3 | 8、9 | `/data/identity/session.json` 原子写 + symlink 拒绝；renew/ack 三态测试；pending 恢复不重复生成 token |
| 生成/积分/入队/关联单事务 + 请求幂等 | §7 | 12 | 四步故障注入全回滚；同键同指纹重放只扣一次；未就绪不写行不占键 |
| 不确定 provider 结果不直接退款，独立协调 + `needs_review` | §7 | 13 | `Plan` 表驱动规则；15 分钟升级而非判定失败；`CountActiveGenerations` 排除 needs_review |
| 不暴露密钥、私密路径、内部错误 | §6.2 | 14 | 投影与 `skillReadiness` 的公开错误码测试 |
| 就绪前不生成、不预留；准备/唤醒需要成员身份 | §6.2、§6.3 | 11、14、16 | 503/409 分支零 generation/ledger 写入；`RequireWorkspaceMember`；前端禁用与原因 |
| 应用可恢复、可看作品、草稿保留 | §6.3 | 15、16 | 轮询接线测试；草稿在状态变化时保留；works/history 既有链路回归 |
| 13-route 真实镜像闭环与浏览器 | §9.1、§9.2 | 19 | 13 个子用例的运行记录；浏览器 trace/screenshot |
| 真实 provider 未授权不调用，SKIP 不记 PASS | §9.3 | 6、19 | 受控 fixture 只用假凭据；未授权子用例逐条记 SKIP |
| 迁移检查点、向前恢复、整库恢复不是回滚步骤 | §10 | 18 | 每检查点的中断恢复测试；日志表保留原资源身份 |
| 退役旧入口与文档 | §10.8、§11 | 20 | 全仓搜索无残留；`AGENTS.md` 与规格状态行回填 |

**规格中本计划未覆盖的部分（有意）：** §3 的「普通任务与 Aurora 任务共用同一套运行时实现」在代码层面已经成立（同一 Fleet、同一 daemon、同一 claim 集合），Task 7 只改身份谓词，不需要额外接线；§4.2 的「容器接管检查同时验证引用、实际 image ID、角色、命名空间、卷及隔离策略」由既有 `validateNodeInspection` 满足，Task 4 只改了它比较的镜像来源。

## 仍未执行与阻塞（不得声称已通过）

| 项 | 状态 | 说明 |
| --- | --- | --- |
| G0 平台与隔离原型 | **未开始** | 用户已选择 Linux VM 作为目标；本机 Docker Desktop 不报告 AppArmor，不在支持集合内。Task 1–3 的事实记录必须由 guest 上真实运行的输出填满 |
| G1 受控 provider fixture 的公网地址与证书 | **未建立** | Task 6 是交付物，不是既有能力；没有它，§9.1 的完整闭环保持未运行 |
| 13-route 真实镜像闭环 | **未运行** | 依赖 G0/G1 与 Task 19 的显式授权 |
| 真实 Claude/model/provider 烟测 | **未授权、未运行** | 需要 owner 明确授权与凭据；在任何记录里保持 SKIP，绝不记 PASS |
| 本地 `cosign` 验证 | **不可能** | macOS 主机无 `cosign`，GHCR 拒绝匿名访问；信任只能来自 CI 的 `verify-published` job，结论必须连同这条限制写明 |
| `Enforce supply-chain release policy` | **当前红** | `undici 7.29.0` 的 `CVE-2026-19534`、`CVE-2026-84961`（HIGH）；Task 5 Step 4 处理，不加豁免 |
| 浏览器 Aurora spec 的真实登录路径 | **未验证** | 现有 spec 注入 `localStorage` token 而生产是 cookie auth；Task 19 Step 2 修正并记录 |
| 既有欠账（与本计划无关但会被读到） | 未处理 | history 列表只有第一页；`asset-download` 忽略 `window.open` 返回 null、空 body 存 0 字节；daemon 阶段时间线未持久化；Aurora 无 task 深链 |

## 执行交接

**Plan complete and saved to `docs/superpowers/plans/2026-10-08-unified-cloud-runtime-aurora.md`.** 两种执行方式：

1. **Subagent-Driven（推荐）** —— 每个任务派一个新的子代理，任务之间由我审查；迭代快，且 M0 的门任务适合单独审。
2. **Inline Execution** —— 在本会话内按 executing-plans 批量执行，带检查点。

**无论选哪种，先确认两件事：**

- Task 1 的 Linux guest 是否已就绪。没有它，M0 只能停在「脚本写完」，Task 3 的矩阵无法运行，M1 的 publish 与 M5 的切换都必须继续等待。
- 本计划的三处**规格偏差**（§0 表格：phase 词汇、不重命名既有阶段、等待态用新增值）是否接受。若不接受、要求逐字对齐规格的 `waiting_config|waiting_capacity|queued|running|succeeded|failed`，则 Task 10 之前需要先做一次 `applying→running`、`completed→succeeded` 的重命名（涉及 `fleet.sql` 的每一处枚举谓词、`internal_dto.go` 的校验与全部相关测试），本计划不包含该重命名。
