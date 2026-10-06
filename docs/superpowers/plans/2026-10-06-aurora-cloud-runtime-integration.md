# Aurora Cloud Runtime 对接实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 Aurora 的 13 个 skill 执行真正跑在本地 Docker Cloud Runtime（`server/internal/fleet` + `server/cmd/fleet`）管理的节点上：Fleet 成为仓库唯一 Docker 控制面，节点运行 Aurora sandbox 运行时（MCP broker / egress sidecar / artifact staging），Aurora 的 `SandboxManager`/`SandboxReaper` 经 Fleet provision API 驱动 per-workspace 节点并保留 `mse_/mdt_` 注册语义，`server/internal/aurorafleet` + `server/cmd/aurora-fleet` 退役，`apps/aurora` 直接消费 Fleet 的能力与真实执行状态。同时修复既有缺陷：把 9 个 `aurora.*` broker 工具真正注册为 Claude 的 MCP server。

**Architecture:** 两条独立 Docker 控制面合并为一条。`server/internal/fleet` 通过管理员配置 `Config.Aurora` 选择 Aurora 托管沙箱 profile：同一 Fleet 进程、同一 Store、同一 Reconciler 与同一 `fleetguard` 领取屏障，但当 profile 启用时引导 payload 改为一次性 `mse_` 注册密钥，节点容器以固定 `fleet-node run` entrypoint 启动并 `exec` 为 `multica daemon start --managed`；Aurora sandbox 镜像同时满足 Fleet 节点与 Aurora sandbox 两份契约（Task 2）。Aurora 侧新增 `FleetProvisioner` 接缝，生产实现经 `cloudruntime.Client` 调用 Fleet 的 `PUT|GET|DELETE /internal/v1/workspace-nodes/{nodeID}`；`aurorafleet` 全量退役。`server/internal/daemon` 为 Aurora 任务注入 broker 的 `McpConfig` 与 `--allowedTools`，模型第一次可以真正调用 `aurora.*` 工具；产物仍走 manifest → staging → `ReportTaskArtifacts` → `server/internal/service/aurora_completion.go` 结算链路。

**Tech Stack:** Go、Chi、pgx/v5、sqlc、PostgreSQL、Docker Engine SDK、Node.js（MCP stdio broker + patched Volcengine vendor trees）、Claude Code CLI、TanStack Query、zod、Vitest、Playwright。

**Spec / 依赖计划：**

- 本地 Docker 控制面：[本地 Docker Cloud Runtime 实现计划](2026-10-04-local-docker-cloud-runtime.md)、[设计规格](../specs/2026-10-04-local-docker-cloud-runtime-design.md)、[Task 3 生产者契约](../specs/2026-10-04-local-docker-cloud-runtime-task-3-contract.md)、[namespace/operator 补缺](2026-10-05-local-fleet-namespace-operator.md)（Fleet 的既有契约，本计划只做增量扩展）。
- Aurora 执行层与工具面：[Aurora 执行层计划（Plan 3）](2026-09-11-aurora-execution.md)、[沙箱工具面主计划](2026-09-22-aurora-sandbox-tool-surface.md) 及其四个子计划：[受管沙箱控制面 A](2026-09-25-aurora-managed-sandbox-control-plane.md)、[fleet 隔离与 egress B](2026-09-25-aurora-sandbox-fleet-isolation.md)、[沙箱 skill runtime C](2026-09-25-aurora-sandbox-skill-runtime.md)、[沙箱镜像与烟测 D](2026-09-25-aurora-sandbox-image-smoke.md)。
- 最终验收记录：[2026-09-28 Aurora Sandbox Final Verification and Acceptance Record](2026-09-28-aurora-sandbox-acceptance-record.md)（`mse_/mdt_`、加固参数、镜像 digest、gated smoke 的既有事实来源）。
- 领域设计：[Aurora 内容创作应用设计](../specs/2026-09-11-aurora-content-creation-app-design.md) §4 执行层、§10 安全。

## P0：优先修复 `aurora_runtime_unavailable`（2026-10-06 用户指定）

**现象**：任何 `POST /api/aurora/generations` 返回 `503 {"code":"aurora_runtime_unavailable","error":"aurora runtime unavailable"}`。

**两条返回路径**（均在 `server/internal/handler/aurora.go`）：

1. `ensureWorkspaceSandbox` 在 `h.SandboxManager == nil` 时直接返回（`aurora.go:445-447`）。这是本地/自托管默认值：`newWorkspaceSandboxManager`（`server/cmd/server/main.go:999-1016`）只有在 `AURORA_FLEET_URL`、`AURORA_FLEET_CONTROL_TOKEN_FILE`、`AURORA_SANDBOX_IMAGE` 三者同时存在时才构造 manager。本 checkout 的 `.env.worktree` 未设置任何 `AURORA_*`，因此 `SandboxManager` 为 nil。
2. `SandboxManager.Ensure` 失败（`aurora.go:448-450`），例如 `aurorafleet` 不可达、节点 arm 或 profile 失败。

**根因**：Aurora 的沙箱控制面只认外部 `aurorafleet`（`AURORA_FLEET_URL` + `PUT /internal/v1/workspace-nodes/{id}`），而本仓库真正维护的 Docker 控制面是 `server/internal/fleet`。两者从未对接，所以"本机有 Docker、仓库有 Fleet 代码"仍然得到 503。这不是配置遗漏，是缺失的对接。

**修复定义：P0 = Task 3 + Task 4**

- Task 3：Fleet 新增 `PUT|GET|DELETE /internal/v1/workspace-nodes/{nodeID}`，按 owner/namespace 受控创建/查询/删除节点，并把一次性 `mse_` 注册密钥交给协调器。
- Task 4：Aurora 新增 `FleetProvisioner`，生产实现经 `cloudruntime.Client` 调用 Task 3 路由；`main.go` 在 `MULTICA_LOCAL_FLEET_URL` + `MULTICA_LOCAL_FLEET_SECRET_FILE` + `AURORA_SANDBOX_IMAGE` 齐备时启用，缺一保持今日 fail-closed 语义。

**验收标准（可判定，且不掩盖 503）**

1. 未配置任何运行时时**仍然返回 503**，不排队、不预留积分——这是既有契约（`server/internal/handler/aurora_test.go:2717`、`2769` 已有断言），不得改成静默成功或占位 runtime。
2. 配置本地 Fleet 后，`POST /api/aurora/generations` 返回 **201**；`aurora_sandbox_node.state='starting'` 且 `backend_node_id` 写为 Fleet node UUID；响应中不再出现 `aurora_runtime_unavailable`。
3. `Ensure` 失败（Fleet 不可达、被拒绝、密钥交接缺失）仍返回 503，且节点被 `FailAuroraSandboxNode` 标记，不留下 live enrollment、不残留 barrier。
4. 新增 canonical 回归：`TestCreateAuroraGenerationOnLocalFleet`（fake provisioner → 201）与 `TestCreateAuroraGenerationFleetFailureIs503`（fake 失败 → 503 + failed 行）；既有 nil-manager 503 断言保持不变。
5. P0 可独立验收：节点创建/查询/删除 + 注册密钥交接 + 协调器消费，默认测试不需要 Docker（`make env-exec` 提供 `DATABASE_URL`）。

**边界（必须诚实标注）**：P0 只让运行时可获得、503 消失。节点真正变为 `ready` 并执行 skill 仍依赖 Task 2（镜像同时满足两份契约）与 Task 6（MCP broker 注入）；P0 交付时**不得声称生成已经能跑完**。

**执行顺序调整**：先 **Task 3 → Task 4（P0）**，再 Task 2 → Task 5 → Task 6 → Task 7 → Task 8 → Task 9。Task 3 不依赖 Task 2。

## Global Constraints

- **已确认决策（2026-10-06，用户批准）**：`server/internal/fleet` 成为唯一 Docker 控制面；`server/internal/aurorafleet` + `server/cmd/aurora-fleet` 退役；fleet 托管节点运行 Aurora sandbox 运行时；Aurora 的 `SandboxManager/SandboxReaper` 驱动 fleet；`apps/aurora` 增加 Runtime/节点与执行状态视图（复用 `packages/views/runtimes` 的展示语义，不复制 `apps/web`/`apps/desktop` 整页）。
- **保留 `mse_/mdt_` 与 per-workspace 节点语义**：`aurora_sandbox_node` 表、`SandboxEnrollmentService.Consume`、`BindAuroraManagedRuntime`、`daemon_id/state` 生命周期与 `server/internal/handler/aurora_runtime.go` 的 `/api/daemon/managed/enroll` 不变；只替换 `EnsureWorkspaceNode`/`DeleteWorkspaceNode` 的传输目标。每 workspace 恰一个节点。
- **不新增 DB、表族或 SQLite**：Fleet 与 API 共用当前 checkout 的同一 PostgreSQL；Aurora 的 `aurora_sandbox_node` 仍是控制面生命周期行。若需 `workspace_id/runtime_id` 等节点维度，只对既有 `fleet_*` 表做 additive migration；不建第二个 DB/队列/独立卷。
- **无外键、级联删除或更新**；所有新增索引用 `CREATE [UNIQUE] INDEX CONCURRENTLY`，每个独立单语句迁移、事务外运行；条件 DDL 用 `IF EXISTS`/`IF NOT EXISTS`。SQL 变更后 `make sqlc`，不手改 generated。
- **约束顺序**：无新 Runtime mode 枚举；受管 metadata 仍由服务器写入（`managed_by` / `fleet_node_id`），沿用 `fleetguard` 的领取/入队/注册屏障；不增加第二套任务协议。
- **不动 SaaS/Billing**：不触碰 `aurora_completion.go` 结算语义、Stripe、entitlement、seat capacity；`MULTICA_LOCAL_FLEET_URL` 与 `MULTICA_CLOUD_URL` 仍互斥。
- **私密输入只经明确文件或一次性内存交接**：`mse_` 只写入节点 secrets volume（0600，目录 0700），Fleet SQL 只存哈希；模型密钥不进镜像、`Config.Env`、SQL、日志、浏览器响应或 Git。`mcn_`/`mse_`/`mdt_` 不进模型 env、argv、prompt。
- **节点容器只读挂载**：provider 凭证只以只读 bind 出现在固定 `/run/secrets/*`；节点无 socket、无 privileged、非 root（`10001:10001`）、无 host network/PID、无端口映射，Task 5 后达到与 `server/internal/aurorafleet/policy.go` 等价的 read-only rootfs / cap-drop ALL / no-new-privileges / seccomp / AppArmor。Docker 不是强多租户安全边界。
- **默认测试不依赖真实 Docker、真实模型或真实供应商账户**；Docker 集成用 `dockerintegration` + `MULTICA_RUN_DOCKER_INTEGRATION=1`，真实烟测用 `agentintegration` + `MULTICA_RUN_REAL_AGENT_SMOKE=1`，两者分别显式授权；未授权时 SKIP 不得记作 PASS。
- **协议保持**：现有 `TaskResult` 无附件、产物走 out-of-band 上报 + `/api/agent/tasks/{id}/aurora-artifacts/upload` 的既有协议；不改 daemon claim/complete/fail 字段。
- **包边界**：`packages/core` 无 UI/localStorage/process.env；`packages/views` 无 store 定义、无框架路由（用 `NavigationAdapter`/`useNavigation`/`AppLink`）；`packages/nextjs` 做 Next.js app-shell helper；平台接线留在 `apps/aurora`。core/views 的响应过 zod/`parseWithFallback`。5 种 locale（en/zh-Hans/fr/ja/ko）同步。Mobile 不在范围。
- 业务资源请求继续走现有 UUID loader/`parseUUIDOrBadRequest`；可信 SQL UUID 才能直接转换。新增 JSON 严格拒绝未知字段。
- 每任务结束跑定向测试、检查 diff、独立 conventional commit；不用 `git add .` 纳入无关改动。缺环境不是有效 red；保留 red→green→review 节奏。

## 范围、依赖与实施顺序

单一端到端子系统，不拆出计费或通用云平台。三个可独立验收里程碑：

1. **M1（Tasks 1–3）**：Fleet 具备 Aurora 执行 profile，并能按 workspace 受控地创建/查询/删除节点、把一次性注册密钥交给协调器。默认测试不需要 Docker，只需要 fake provider / fake engine 或 managed environment 的 `DATABASE_URL`。
2. **M2（Tasks 4–7）**：Aurora 改走 Fleet，`aurorafleet` 退役；隔离/出网对齐；MCP broker 真正接入；fake pipeline 端到端在 Fleet 节点上完成“生成 → 产物 → 结算”。Docker 链路单独门控。
3. **M3（Tasks 8–9）**：`apps/aurora` 的 Runtime/执行状态视图、文档、完整回归与 gated 真实烟测。

严格按依赖顺序执行；每个任务只 stage 自己列出的文件。未请求 Agent Teams，不创建 teammate；若选择子代理执行，仍逐任务两阶段审查，禁止重叠编辑。

**已知不在本计划范围**（详见文末）：

- AWS/生产部署、`/data/multica` 自托管栈、`docker-compose.selfhost.aws.yml`、EC2、自动扩缩容、多机调度。
- 真实 provider/Claude 调用的强制通过；真实烟测只接门控与入口，执行需 owner 明确授权、凭证与环境。
- 发布镜像、SBOM/Trivy/签名、`verify-published`；`CVE-2026-19534`/`CVE-2026-84961` 供应链阻断是独立开放工作。
- Slide/deck 生成、Cloud entitlement `GateAurora*`、Mobile、卷的物理擦除/硬磁盘配额保证、生产强多租户隔离。

## 文件结构与职责

**已实现（Task 1，commit `cd759da6e`）**：

- `server/internal/fleet/model/aurora.go`、`aurora_test.go`：`AuroraConfig{ServerURL}`、`Validate`、`ValidEnrollmentToken`、固定常量 `AuroraEnrollmentDir=/secrets`、`AuroraEnrollmentFile=/secrets/aurora-enrollment`、`MULTICA_MANAGED`/`MULTICA_SERVER_URL`/`MULTICA_MANAGED_ENROLLMENT_TOKEN_FILE`。
- `server/internal/fleet/model/config.go`、`types.go`：`Config.Aurora`、`Bootstrap.EnrollmentToken`。
- `server/internal/fleet/docker/bootstrap.go`：`installerTar(n,cfg,b)` 按 profile 选择 payload；Aurora 只投递 layout manifest + `secrets/aurora-enrollment`，Claude profile 投递 `secrets/bootstrap.json`，并做 canonical 字节与交叉 profile 拒绝。
- `server/internal/fleet/docker/provider.go`、`inspect.go`：Aurora 容器 env（`MULTICA_MANAGED=1`、`MULTICA_SERVER_URL`、`MULTICA_MANAGED_ENROLLMENT_TOKEN_FILE`）与 `inspectEnvironment` 白名单；secrets volume 只读挂到 `/secrets`；两 profile 永不共享 bootstrap。
- `server/cmd/fleet-node/main.go`、`aurora_test.go`：`runNode` 在受管 profile 下调用 `runManagedNode`，校验固定路径、0600 与非 symlink，`exec` `multica daemon start --managed --foreground --managed-enrollment-token-file=<path>`。

**本计划新增/修改**（按任务）：

- Task 2：`deploy/aurora-sandbox/Dockerfile`、`docker-bake.hcl`、`fixture/Dockerfile.sandbox-fleet`、`scripts/verify-aurora-sandbox-locks.mjs`、`scripts/verify-aurora-sandbox-image.sh`、`deploy/aurora-sandbox/docker-security-test.sh`（如需要）。
- Task 3：`server/internal/fleet/store/aurora.go`、`aurora_test.go`、`server/pkg/db/queries/fleet.sql` + sqlc 生成物、`server/migrations/583_fleet_nodes_aurora_dimensions.{up,down}.sql`、`584_fleet_nodes_workspace_index.{up,down}.sql`、`server/cmd/migrate/main.go`、`server/internal/fleet/{service.go,http.go,internal_dto.go,reconciler.go,scheduler.go}` 及旁边测试、`server/internal/cloudruntime/{client.go,fleet_internal.go,client_test.go}`。
- Task 4：`server/internal/aurora/{fleet_provisioner.go,fleet_provisioner_test.go,sandbox_manager.go,sandbox_manager_test.go,sandbox_reaper.go,sandbox_reaper_test.go}`、`server/cmd/server/{main.go,local_fleet.go,local_fleet_test.go,router.go}`、`server/internal/handler/{aurora_fleet_e2e_test.go,local_fleet_operations.go}`；删除 `server/internal/aurorafleet/` 与 `server/cmd/aurora-fleet/`；`Makefile`。
- Task 5：`server/internal/fleet/model/{aurora.go,config.go,layout.go}`、`server/internal/fleet/docker/{provider.go,inspect.go,egress.go,egress_test.go}`、`docker-compose.fleet.yml`、`scripts/fleet-env.sh`、`scripts/fleet-env.test.sh`、`fleet-config.example.json`。
- Task 6：`server/internal/daemon/{daemon.go,aurora_tool_surface.go,aurora_tool_surface_test.go,aurora_broker.go,aurora_broker_test.go}`、`server/pkg/agent/{agent.go,claude.go,claude_test.go}`、`deploy/aurora-sandbox/runtime/src/server.mjs`（如需要）。
- Task 7：`server/internal/fleet/integration/aurora_test.go`、`e2e/aurora-cloud-runtime.spec.ts`、`e2e/fixtures.ts`、`playwright.config.ts`、`server/internal/fleet/integration/docker_test.go`。
- Task 8：`packages/core/aurora/{schema.ts,schema.test.ts,api.ts,api.test.ts,queries.ts,queries.test.ts,types.ts}`、`packages/views/aurora/runtime-status.tsx`、`runtime-status.test.tsx`、`packages/views/aurora/index.ts`、`packages/views/locales/{en,zh-Hans,fr,ja,ko}/aurora.json`、`apps/aurora/app/[workspaceSlug]/runtimes/page.tsx`、`apps/aurora/lib/routes.ts`、`apps/aurora/components/aurora-shell.tsx` 及测试、`server/internal/handler/aurora_runtime_view.go` + 测试 + `server/cmd/server/router.go`。
- Task 9：`AGENTS.md`、`docs/superpowers/specs/2026-10-04-local-docker-cloud-runtime-design.md`、`apps/docs/content/docs/developers/local-docker-fleet.{mdx,zh.mdx}`、`CONTRIBUTING.md`、`docs/superpowers/plans/2026-10-06-aurora-cloud-runtime-acceptance.md`。

现有接入位置（只读核实）：

- [Fleet provider](<../../../server/internal/fleet/docker/provider.go>)、[bootstrap](<../../../server/internal/fleet/docker/bootstrap.go>)、[Engine](<../../../server/internal/fleet/docker/engine.go>)、[Reconciler](<../../../server/internal/fleet/reconciler.go>)、[scheduler](<../../../server/internal/fleet/scheduler.go>)、[Service](<../../../server/internal/fleet/service.go>)、[HTTP](<../../../server/internal/fleet/http.go>)、[internal DTO](<../../../server/internal/fleet/internal_dto.go>)、[Store](<../../../server/internal/fleet/store/store.go>)、[fleet.sql](<../../../server/pkg/db/queries/fleet.sql>)、[Fleet 入口](<../../../server/cmd/fleet/main.go>)、[fleet-node](<../../../server/cmd/fleet-node/main.go>)。
- [SandboxManager](<../../../server/internal/aurora/sandbox_manager.go>)、[SandboxReaper](<../../../server/internal/aurora/sandbox_reaper.go>)、[SandboxEnrollmentService](<../../../server/internal/aurora/sandbox_enrollment.go>)、[server 端组装](<../../../server/cmd/server/main.go>)。
- [ManagedRuntimeEnroll/Shutdown](<../../../server/internal/handler/aurora_runtime.go>)、[生成创建 handler](<../../../server/internal/handler/aurora.go>)、[artifact staging](<../../../server/internal/handler/aurora_artifact_upload.go>)、[settlement](<../../../server/internal/service/aurora_completion.go>)。
- [Aurora tool surface](<../../../server/internal/daemon/aurora_tool_surface.go>)、[daemon runTask/aurora 策略](<../../../server/internal/daemon/daemon.go>)、[managed provider secrets](<../../../server/internal/daemon/managed_secrets.go>)、[artifact manifest](<../../../server/internal/daemon/aurora_manifest.go>)、[agent ExecOptions](<../../../server/pkg/agent/agent.go>)、[claude backend](<../../../server/pkg/agent/claude.go>)。
- [broker 入口](<../../../deploy/aurora-sandbox/runtime/src/server.mjs>)、[task context](<../../../deploy/aurora-sandbox/runtime/src/task-context.mjs>)、[sandbox Dockerfile](<../../../deploy/aurora-sandbox/Dockerfile>)。
- [core runtimes cloud-runtime](<../../../packages/core/runtimes/cloud-runtime.ts>)、[capabilities](<../../../packages/core/runtimes/cloud-runtime-capabilities.ts>)、[views runtimes 页](<../../../packages/views/runtimes/components/runtimes-page.tsx>)、[CloudNodeActions](<../../../packages/views/runtimes/components/cloud-node-actions.tsx>)。
- [core/aurora](<../../../packages/core/aurora/index.ts>)、[views/aurora](<../../../packages/views/aurora/index.ts>)、[Aurora shell](<../../../apps/aurora/components/aurora-shell.tsx>)、[Aurora 路由](<../../../apps/aurora/lib/routes.ts>)、[locale](<../../../packages/views/locales/en/aurora.json>)。

## 现状边界（2026-10-06 核实，供后续 Task 对比）

**Fleet（`server/internal/fleet`，Task 1 后）**：`Config.Aurora` 启用时 installer payload 为 layout manifest + `secrets/aurora-enrollment`，容器 env 为 `HOME`/`FLEET_NODE_MAX_RUNS` + `MULTICA_MANAGED`/`MULTICA_SERVER_URL`/`MULTICA_MANAGED_ENROLLMENT_TOKEN_FILE`，secrets volume 只读挂到 `/secrets`；普通 profile 行为不变。协调器尚不能为 Aurora 节点取到注册密钥，任务因此不可达。

**aurorafleet（待退役，Task 4）**：`PUT|GET|DELETE /internal/v1/workspace-nodes/{nodeID}`；Docker CLI backend 按不可变 `Policy` 创建 workspace `--internal` 网络 + egress sidecar + sandbox 容器（read-only rootfs、cap-drop ALL、seccomp/AppArmor、digest 固定镜像、只读 bind `/run/secrets/aurora-enrollment` 与 4 个供应商密钥文件），env 含 `MULTICA_MANAGED=1`、`MULTICA_MANAGED_ENROLLMENT_TOKEN_FILE`、`HTTP(S)_PROXY`。Task 5 把这一契约搬到 Fleet provider。

**Aurora 执行链路**：`CreateAuroraGeneration` → `EnsureSystemAgents`（`aurora_managed` runtime + 13 个 `kind=system` agent）→ `SandboxManager.Ensure` 在 per-workspace advisory lock 下签发 `mse_` 并写 `aurora_sandbox_node` → `aurorafleet.ControlClient.EnsureWorkspaceNode` → daemon `POST /api/daemon/managed/enroll` → `SandboxEnrollmentService.Consume` 交换 `mdt_` 并绑定 `agent_runtime.daemon_id` → 普通 claim → `runTask` 的 `auroraToolSurface` fail-closed（claude、MaxTurns=30、deny 列表）→ `attachAuroraArtifacts` → `aurora_completion.go` 结算。Task 4 只替换链路中的传输目标，其余不变。

**已确认缺陷（Task 6 修复）**：broker 从未注册为 Claude 的 MCP server。`daemon.go` 在 Aurora 分支强制 `McpConfig = {"mcpServers":{}}`，注释声称由 managed sandbox 路径注入，但仓库内不存在该注入代码；`auroraSurface.allowed` 只被记日志，`pkg/agent.ExecOptions` 没有 `AllowedTools` 字段。9 个 `aurora.*` 工具因此不可达。

**apps/aurora**：只有 skills/works/billing 三页；`packages/core/aurora` 无任何 runtime/cloud/sandbox 引用；`packages/views/runtimes` 只接在 Web/Desktop。Task 8 新增应用内 Runtime/执行状态视图。

## 跨任务契约

下面名称是本计划锁定的新接缝，不是假定仓库中已经存在；Go 使用 `pgtype.UUID`，公开 JSON DTO 使用字符串 UUID。

```go
// server/internal/fleet/model/aurora.go（Task 1 已实现）
type AuroraConfig struct { ServerURL string `json:"server_url"` }
func (a AuroraConfig) Validate() error
func ValidEnrollmentToken(token string) bool // ^mse_[0-9a-f]{40}$
const (
    AuroraEnrollmentDir     = "/secrets"
    AuroraEnrollmentFile    = AuroraEnrollmentDir + "/aurora-enrollment"
    AuroraManagedEnv        = "MULTICA_MANAGED"
    AuroraServerURLEnv      = "MULTICA_SERVER_URL"
    AuroraEnrollmentFileEnv = "MULTICA_MANAGED_ENROLLMENT_TOKEN_FILE"
)
// server/internal/fleet/model/types.go（Task 1 已实现）
type Bootstrap struct {
    NodeToken, APIKey, BaseURL, Model, ServerURL, DaemonID string
    EnrollmentToken string `json:"enrollment_token,omitempty"`
}
```

```go
// server/internal/fleet/store/aurora.go（Task 3 产出）
// CreateAuroraIntent 是 Aurora workspace 节点的唯一创建入口：显式 node UUID 与
// daemon UUID 由 Aurora 决定，Fleet 只校验 owner/namespace/镜像/规格并原子落库。
// 同一 (namespace, owner, idempotency_key) 重放返回原 node/操作。
func (s *Store) CreateAuroraIntent(ctx context.Context, ownerID, nodeID pgtype.UUID,
    req model.AuroraNodeRequest) (model.Node, model.Operation, bool, error)
func (s *Store) GetAuroraNode(ctx context.Context, ownerID, nodeID pgtype.UUID) (model.Node, error)

// server/internal/fleet/model/types.go（Task 3 追加，公开 DTO 之外的领域字段）
type AuroraNodeRequest struct {
    WorkspaceID, RuntimeID pgtype.UUID
    DaemonID, ImageDigest, Name, Spec, IdempotencyKey string
    EnrollmentToken string // 只在内存中交接，绝不落库
}
```

```go
// server/internal/fleet/service.go（Task 3 产出）
// ProvisionAuroraNode 接受已认证 owner 的 workspace 节点意图，落库后把一次性
// 注册密钥登记到进程内交接表；协调器消费后立即删除。进程崩溃即丢失密钥，
// Aurora 侧 5 分钟 TTL 后重新 arm，不猜测成功。
func (s *Service) ProvisionAuroraNode(ctx context.Context, ownerID, nodeID pgtype.UUID,
    req model.AuroraNodeRequest) (model.Node, model.Operation, error)
func (s *Service) TakeAuroraEnrollment(nodeID pgtype.UUID) (string, bool)
func (s *Service) DeleteAuroraNode(ctx context.Context, ownerID, nodeID pgtype.UUID) error

// HTTPS 内部路由（Task 3，镜像 aurorafleet 的既有 URL 形状）
// PUT    /internal/v1/workspace-nodes/{nodeID}  body {workspace_id,runtime_id,daemon_id,image_digest,name,spec,idempotency_key}
// GET    /internal/v1/workspace-nodes/{nodeID}  -> NodeDTO
// DELETE /internal/v1/workspace-nodes/{nodeID}  -> 204
// 认证：service key constant-time + 可信 X-User-ID；owner/namespace/node 全部 SQL 校验。
```

```go
// server/internal/fleet/reconciler.go / scheduler.go（Task 3 产出）
// initialize 在 cfg.Aurora != nil 时从 Service.TakeAuroraEnrollment(n.ID) 取一次性密钥，
// 构造 Bootstrap{DaemonID:n.DaemonID, EnrollmentToken:token, ServerURL:cfg.Aurora.ServerURL}；
// 取不到即按普通 bootstrap 失败处理（不猜成功、不留 barrier）。
func (r *Reconciler) auroraBootstrap(ctx context.Context, n model.Node) (model.Bootstrap, error)
```

```go
// server/internal/aurora/fleet_provisioner.go（Task 4 产出）
type FleetNode struct { ID, State, BackendID string }
type FleetEnsureRequest struct {
    NodeID, WorkspaceID, RuntimeID, DaemonID, EnrollmentToken, ImageDigest, Name, Spec string
}
type FleetProvisioner interface {
    EnsureWorkspaceNode(context.Context, FleetEnsureRequest) (FleetNode, error)
    DeleteWorkspaceNode(context.Context, string) error
}
// Fleet 节点 owner 不由 caller 自报：SandboxManager.arm 已在 per-workspace 锁内读到
// aurora_managed runtime 行，节点 owner 取该行 OwnerID（AgentRuntime.OwnerID），以可信
// X-User-ID 传给 Fleet。WorkspaceSandboxManager.Ensure 与 SandboxManager.Ensure 签名不变。
```

```go
// server/internal/fleet/model/aurora.go（Task 5 扩展）
type AuroraConfig struct {
    ServerURL        string `json:"server_url"`
    ProxyImage       string `json:"proxy_image"`
    SeccompProfile   string `json:"seccomp_profile"`
    AppArmorProfile  string `json:"apparmor_profile"`
    EgressHosts      []string `json:"egress_hosts"`
    AnthropicBaseURL string `json:"anthropic_base_url"`
    AnthropicModel   string `json:"anthropic_model"`
    ProviderSecretFiles map[string]string `json:"provider_secret_files"` // 固定目标名 -> 主机只读文件
    ReadonlyRootfs   bool `json:"readonly_rootfs"`
    UplinkNetwork    string `json:"uplink_network"`
}
// server/internal/fleet/docker/egress.go（Task 5 产出）
func EgressProxyArgs(cfg model.Config, proxyName, workspaceNetwork string) ([]string, error)
func EgressNetworkConnectArgs(proxyName, network string) []string
```

```go
// server/pkg/agent/agent.go（Task 6 追加）
type ExecOptions struct {
    // ... 既有字段
    AllowedTools []string // Claude --allowedTools，逗号连接；空表示不注入
}
// server/internal/daemon/aurora_broker.go（Task 6 产出）
const auroraBrokerEntrypoint = "/opt/aurora/runtime/deploy/aurora-sandbox/runtime/src/server.mjs"
type auroraBrokerContext struct {
    SkillID, ServerOrigin, TaskID, GenerationID, WorkspaceID string
    Prompt string
    InputRoot, OutputRoot, ContextPath, TaskTokenPath string
    ArkKeyFile, OpenAIKeyFile, VolcASRKeyFile string
}
func (d *Daemon) writeAuroraBrokerContext(task Task, env execenv.Environment) (auroraBrokerContext, error)
func auroraBrokerMcpConfig(bc auroraBrokerContext) (json.RawMessage, error)
// runTask 的 aurora 分支：execOpts.McpConfig = broker config；execOpts.AllowedTools = surface.allowed；
// 缺失上下文或合并不安全即任务失败（fail-closed，退款）。
```

```ts
// packages/core/aurora/schema.ts + api.ts（Task 8 产出）
export interface AuroraRuntimeNode {
  id: string; status: string; ready: boolean; provider: "docker";
  errorCode?: string; operationId?: string; createdAt: string;
}
export interface AuroraExecutionTarget {
  workspaceId: string; node: AuroraRuntimeNode | null;
  runtimeId: string | null; state: "unconfigured" | "provisioning" | "online" | "failed";
}
export function auroraRuntimeOptions(wsId: string): QueryOptions;
```

本计划不新增第二套任务协议、第二套 Runtime 枚举或第二个数据库。

### Task 1: Fleet Aurora 执行 profile（已完成）

**Dependencies:** 无。**Deliverable:** Fleet 支持按部署 profile 运行 Aurora sandbox 运行时；两 profile 的 bootstrap/容器 env 永不混用。

**Files（已提交 `cd759da6e`）:** `server/internal/fleet/model/{aurora.go,aurora_test.go,config.go,types.go}`、`server/internal/fleet/docker/{bootstrap.go,provider.go,inspect.go,aurora_test.go,inspect_test.go,offline_reports.go}`、`server/cmd/fleet-node/{main.go,aurora_test.go}`。

**Interfaces:** 见上节；`installerTar(n,cfg,b)` 是唯一 installer 生产者，Aurora profile 下投递 `secrets/aurora-enrollment`；`runManagedNode` 只接受固定路径、0600、`mse_` 格式。

- [x] **Step 1:** 先写状态矩阵：`Config.Aurora` 的 URL/嵌套字段/类型拒绝矩阵；`ValidEnrollmentToken` 正反例；两 profile 互拒绝。
- [x] **Step 2:** `(cd server && go test ./internal/fleet/model -count=1)`；预期 profile 字段/校验未实现 FAIL。
- [x] **Step 3:** 实现 model profile、`installerTar` 分派、Aurora 容器 env、`inspectEnvironment` 白名单、`runManagedNode`。
- [x] **Step 4:** `(cd server && go test ./internal/fleet/model ./internal/fleet/docker ./cmd/fleet-node -count=1)` 全绿（2026-10-06：model 1.2s、docker 31.9s、fleet-node 1.6s）。
- [x] **Step 5:** scoped commit `feat(fleet): add the Aurora managed-sandbox execution profile`（`cd759da6e`）。

**尚未完成（由后续任务交付）**：协调器还不会为 Aurora 节点产出 `Bootstrap`，因此该 profile 目前不可达；Task 3 负责使其可用。

### Task 2: 让 Aurora sandbox 镜像同时是 Fleet 节点镜像

**Dependencies:** Task 1。**Deliverable:** 一个 digest 固定的镜像同时满足 Aurora sandbox 与 Fleet 节点两份契约；不满足任一侧即镜像构建/内容校验失败。

**Files:** Modify `deploy/aurora-sandbox/Dockerfile`、`deploy/aurora-sandbox/docker-bake.hcl`、`scripts/verify-aurora-sandbox-locks.mjs`、`scripts/verify-aurora-sandbox-image.sh`、`deploy/aurora-sandbox/docker-security-test.sh`（如需要）；Create `deploy/aurora-sandbox/fixture/Dockerfile.sandbox-fleet`（测试用假 CLI 镜像）。

**Interfaces:** 镜像必须提供 `/usr/local/bin/fleet-node`、`/usr/local/bin/multica`、`/data`+`/data/home`+`/data/workspaces`+`/secrets`（10001 属主）、broker 运行时与 patched vendor 树、Chromium/FFmpeg；`ENTRYPOINT ["/usr/local/bin/fleet-node","run"]`、`HEALTHCHECK fleet-node health`、`USER 10001:10001`。`scripts/verify-aurora-sandbox-image.sh` 是权威门，任何一侧的加固断言都不放宽。

- [ ] **Step 1:** 先扩展 `scripts/verify-aurora-sandbox-image.sh` 断言新契约（fleet-node 存在、layout 目录 10001 属主、固定 entrypoint、healthcheck、无包管理器/密钥），跑一次预期 FAIL。
- [ ] **Step 2:** 修改 Dockerfile：`gobuilder` 同时 `go build ./cmd/fleet-node`；保留既有 apt/vendor/esbuild 加固；补 `install -d -o 10001 ... /data /data/home /data/workspaces /secrets`；替换 entrypoint/healthcheck。
- [ ] **Step 3:** `node scripts/verify-aurora-sandbox-locks.mjs --workflow` 与 `docker buildx bake -f deploy/aurora-sandbox/docker-bake.hcl sandbox --load --set '*.platform=linux/amd64'`（需授权）；`scripts/verify-aurora-sandbox-image.sh <digest>`。
- [ ] **Step 4:** Linux 安全验收脚本 `deploy/aurora-sandbox/docker-security-test.sh` 保持通过；容器化 fake pipeline smoke 保持通过；fixture 镜像可跑 fake CLI/fake pipeline。
- [ ] **Step 5:** 检查 diff 并 scoped commit `build(aurora): ship fleet-node in the managed sandbox image`。

### Task 3: Fleet workspace-node provision API 与注册密钥交接

**Dependencies:** Task 1（P0 优先级最高；Task 2 镜像只影响节点就绪与真实执行，不阻塞本任务）。**Deliverable:** Fleet 能按 Aurora 的 `PUT|GET|DELETE /internal/v1/workspace-nodes/{nodeID}` 契约受控创建/查询/删除节点，并把一次性注册密钥交给协调器；数据库记录节点身份但不存密钥明文。

**Files:** Create `server/internal/fleet/store/aurora.go`、`aurora_test.go`、`server/internal/fleet/http_aurora.go`、`http_aurora_test.go`；Modify `server/pkg/db/queries/fleet.sql` + `make sqlc` 生成物、`server/migrations/583_fleet_nodes_aurora_dimensions.{up,down}.sql`、`584_fleet_nodes_workspace_index.{up,down}.sql`、`server/cmd/migrate/main.go`、`server/internal/fleet/{service.go,http.go,internal_dto.go,reconciler.go,scheduler.go,service_test.go,http_test.go,reconciler_test.go}`、`server/internal/cloudruntime/{client.go,fleet_internal.go,client_test.go}`。

**Interfaces:** 见上节 `CreateAuroraIntent`/`ProvisionAuroraNode`/`TakeAuroraEnrollment`/`DeleteAuroraNode`。HTTP 请求体严格为 `{workspace_id,runtime_id,daemon_id,image_digest,name,spec,idempotency_key}`；响应复用 `NodeDTO`。路由与既有内部路由同源：service key constant-time + 可信 `X-User-ID`。`Config.Aurora != nil` 时才注册该路由；否则 404，不暴露空的 Aurora surface。

**迁移（additive，无 FK）**：

- `583_fleet_nodes_aurora_dimensions`：`ALTER TABLE fleet_nodes ADD COLUMN IF NOT EXISTS workspace_id uuid`、`ADD COLUMN IF NOT EXISTS runtime_id uuid`（一文件多语句，无内联索引/约束）。
- `584_fleet_nodes_workspace_index`：单语句 `CREATE INDEX CONCURRENTLY IF NOT EXISTS fleet_nodes_workspace_idx ON fleet_nodes(workspace_id);`；down 单语句 `DROP INDEX CONCURRENTLY IF EXISTS fleet_nodes_workspace_idx;`。
- 执行前 glob 确认 583/584 未占用，main 推进则连续重编号；在 `server/cmd/migrate/main.go` 的 `concurrentIndexCleanups` 登记 `584`，down 同步登记，保持 `TestEveryConcurrentUpBuildHasCleanup` 与 `TestFleetInvalidConcurrentIndexRetry` 不变。

- [ ] **Step 1:** 写 DB 回归：同一 (owner, idempotency_key) 重放返回同一 node/操作；不同 payload 同键 409；owner 限额；显式 node/daemon UUID 被持久化；密钥不落库（扫描 `fleet_nodes`/操作表无 `mse_` 明文）；跨 owner/namespace 查询拒绝。
- [ ] **Step 2:** `make env-exec ARGS='-- bash -c "cd server && go test ./internal/fleet/store -run TestFleetAuroraIntent -count=1"'`；预期缺方法 FAIL（不使用真实 Docker）。
- [ ] **Step 3:** 实现 store intent（owner 级 advisory lock → 幂等查询 → 限额 → 插入 node/op，`workspace_id/runtime_id/image` 落库）；路由、严格 DTO 与进程内交接表（`sync.Map` + node UUID）；协调器 `initialize` 在 `cfg.Aurora != nil` 时取交接密钥构造 `Bootstrap{DaemonID, EnrollmentToken, ServerURL}`，缺失即按普通 bootstrap 失败处理（不猜成功、不超时重试）。
- [ ] **Step 4:** `make sqlc`；`go test ./internal/fleet/... -count=1`（含 fake provider 的 http/reconciler 回归），断言无 Docker 调用即可验证创建到 `launching`；`go test ./cmd/migrate -run 'EveryConcurrent|FleetInvalid' -count=1` PASS。
- [ ] **Step 5:** 检查 diff 并 scoped commit `feat(fleet): provision Aurora workspace nodes`。

### Task 4: Aurora 侧改走 Fleet，aurorafleet 退役

**Dependencies:** Task 3。**Deliverable:** `SandboxManager`/`SandboxReaper` 只依赖 provider 中立的 provision 接口，生产实现指向 Fleet；`aurorafleet` 与 `cmd/aurora-fleet` 删除；配置互斥且 fail-closed。

**Files:** Create `server/internal/aurora/fleet_provisioner.go`、`fleet_provisioner_test.go`；Modify `server/internal/aurora/{sandbox_manager.go,sandbox_manager_test.go,sandbox_reaper.go,sandbox_reaper_test.go}`、`server/cmd/server/{main.go,local_fleet.go,local_fleet_test.go,router.go}`、`server/internal/handler/{aurora_fleet_e2e_test.go,local_fleet_operations.go}`；Delete `server/internal/aurorafleet/`、`server/cmd/aurora-fleet/`；Modify `Makefile`（build 目标不含 `aurora-fleet`）。

**Interfaces:** `FleetProvisioner`（见跨任务契约）。节点 owner 由锁内 `GetAuroraManagedRuntime` 的 `OwnerID` 派生，`Ensure` 签名不变；`SandboxReaper` 的 `ErrNodeNotFound` 由 aurora 包定义。生产默认：`MULTICA_LOCAL_FLEET_URL` + `MULTICA_LOCAL_FLEET_SECRET_FILE` + `AURORA_SANDBOX_IMAGE` 三者齐备才启用；缺一即 fail-closed 返回 nil（保持今日 503 `aurora_runtime_unavailable` 语义）。`AURORA_FLEET_URL` 与 `MULTICA_LOCAL_FLEET_URL` 指向同一 host:port 时启动失败，避免同进程两种契约解释。

- [ ] **Step 1:** 先用 fake provisioner 写回归：state 复用/重 arm/失败标记与今天的语义逐条一致（迁移既有 `sandbox_manager_test.go` 断言）；新增 `TestSandboxManagerEnsureResolvesOwner` 与跨 owner 拒绝。
- [ ] **Step 2:** `(cd server && go test ./internal/aurora -run 'TestSandboxManager|TestSandboxReaper' -count=1)`；预期接口未替换 FAIL。
- [ ] **Step 3:** 实现 `fleet_provisioner.go`（经 `cloudruntime.Client` 调用 Task 3 路由，服务密钥来自明确文件，永不来自 caller；loopback 允许 http、非 loopback 要求 https；`CheckRedirect` 拒绝跨源重放）；`main.go` 改为读 `MULTICA_LOCAL_FLEET_URL` + `MULTICA_LOCAL_FLEET_SECRET_FILE` + `AURORA_SANDBOX_IMAGE`；删除 aurorafleet 与 cmd/aurora-fleet，移除 `AURORA_FLEET_*` 读取与文档引用，不保留兼容 shim。
- [ ] **Step 4:** `go build ./...`、`go test ./internal/aurora ./cmd/server ./internal/handler -run 'Aurora|Fleet|Sandbox' -count=1`；确认无 `aurorafleet` 引用（`grep -r aurorafleet`），`make build` 指向 `./cmd/fleet`。
- [ ] **Step 5:** 检查 diff 并 scoped commit `refactor(aurora): run workspace sandboxes on the local Docker Fleet`。

### Task 5: 节点容器与 Aurora 安全边界对齐

**Dependencies:** Task 4。**Deliverable:** Fleet 节点容器达到 Aurora sandbox 现有隔离与出网契约（不含任意 exec）；普通 Claude profile 行为逐字节不变。

**Files:** Modify `server/internal/fleet/model/{aurora.go,config.go,layout.go}`、`server/internal/fleet/docker/{provider.go,inspect.go}`；Create `server/internal/fleet/docker/egress.go`、`egress_test.go`；Modify `docker-compose.fleet.yml`、`fleet-config.example.json`、`scripts/fleet-env.sh`、`scripts/fleet-env.test.sh`。

**Interfaces:** `AuroraConfig` 扩展 `{ProxyImage, SeccompProfile, AppArmorProfile, EgressHosts, AnthropicBaseURL, AnthropicModel, ProviderSecretFiles, ReadonlyRootfs, UplinkNetwork}`；`NodeHostConfig` 在 Aurora profile 下增加 seccomp/AppArmor/read-only rootfs 与固定 `Tmpfs`（`/workspace`、`/tmp`、`/run`）；容器新增只读 bind 供应商密钥到固定 `/run/secrets/{anthropic,ark,openai,volc-asr}-api-key`，并加入 egress sidecar（alias `egress`）与 `HTTP(S)_PROXY`/`NO_PROXY`；workspace 网络 `--internal`，sandbox 只加入该网络。

- [ ] **Step 1:** 写资源构造单测：`TestAuroraNodeHostConfigRestricted`（无 socket/无 privileged/非 root/固定 seccomp+AppArmor/只读 rootfs/固定 tmpfs）、`TestEgressSidecarPolicy`（proxy 无密钥、只上 uplink、alias `egress`）、`TestAuroraProviderSecretMountsAreReadOnly`、`TestClaudeProfileEnvUnchanged`。
- [ ] **Step 2:** `(cd server && go test ./internal/fleet/docker -run 'Aurora|Egress' -count=1)`；预期 builder/策略 FAIL。
- [ ] **Step 3:** 实现策略；`inspectEnvironment`/`validateNodeInspection` 只在 Aurora profile 下放行代理与供应商密钥 env；`fleet-env.sh` 增加 Aurora profile 私有配置字段（不含密钥值）；确保 secrets volume 仍只读挂到 `/secrets`、镜像 entrypoint 仍为 `fleet-node run`。
- [ ] **Step 4:** `bash scripts/fleet-env.test.sh`、`go test ./internal/fleet/... -count=1`；断言普通 profile 的 env/mount/网络行为逐字节不变；hermetic 脚本无真实 Docker。
- [ ] **Step 5:** 检查 diff 并 scoped commit `feat(fleet): match the Aurora sandbox isolation and egress contract`。

### Task 6: 把 MCP broker 真正接给 skill 执行

**Dependencies:** Task 4（可与 5 并行设计）。**Deliverable:** Aurora 任务把 9 个 `aurora.*` broker 工具作为唯一执行面接入 Claude，并把允许集交给 CLI；普通 agent 语义不变。

**Files:** Create `server/internal/daemon/aurora_broker.go`、`aurora_broker_test.go`；Modify `server/internal/daemon/{daemon.go,aurora_tool_surface.go,aurora_tool_surface_test.go}`、`server/pkg/agent/{agent.go,claude.go,claude_test.go}`、`deploy/aurora-sandbox/runtime/src/server.mjs`（如需要固定启动参数）。

**Interfaces:** `daemon.auroraBrokerMcpConfig(bc) (json.RawMessage, error)` 产出 `{"mcpServers":{"aurora":{"command":"node","args":[...],"env":{...}}}}`；`agent.ExecOptions` 新增 `AllowedTools []string`，Claude backend 映射为 `--allowedTools`（与既有 `--disallowedTools` 并存），`--strict-mcp-config` 在 `McpConfig` 非空时由既有逻辑加入。broker env 只含 `AURORA_SERVER_ORIGIN`、`AURORA_TASK_CONTEXT_FILE`、`AURORA_INPUT_ROOT`、`AURORA_OUTPUT_ROOT`、`AURORA_ARTIFACT_IMPORT_PATH`、`ARK_API_KEY_FILE`、`OPENAI_API_KEY_FILE`、`VOLC_ASR_API_KEY_FILE`、`AURORA_TASK_TOKEN_FILE`。task context 严格 `schema=com.multica.aurora.task-context`、`version=1`、`task_id/generation_id/workspace_id`、`skill_id`、`prompt`、`attachments`、`output_root`、`server_origin`、`task_token_file`；`output_root` 必须与 `attachAuroraArtifacts` 固定读取的 `/workspace/output` 一致。

- [ ] **Step 1:** 先写 fake CLI 断言（不执行真实 agent）：Aurora 任务 argv 含 `--allowedTools`；MCP 配置只含 `aurora` server 且 command/args/env 固定；非 Aurora 任务 MCP 合并语义不变；敌意 agent 自带 `mcp_config` 被覆盖；broker 上下文缺失即 fail-closed。
- [ ] **Step 2:** `(cd server && go test ./pkg/agent ./internal/daemon -run 'AuroraBroker|AuroraToolSurface|Claude' -count=1)`；预期注入缺失 FAIL。
- [ ] **Step 3:** 实现 `aurora_broker.go`：`writeAuroraBrokerContext` 在任务 workdir 下写 context 与 task token（0400，token 用 `task.AuthToken`）；在 `runTask` 的 Aurora 分支用 `surface.mcpConfig` 替换 `{"mcpServers":{}}`，设置 `execOpts.AllowedTools = surface.allowed`，`PermissionMode="default"`；保留 fail-closed：broker 配置或上下文缺失即任务失败并退款。
- [ ] **Step 4:** `go test -race ./internal/daemon ./pkg/agent -run 'Aurora|Claude' -count=1`；确认普通 agent 的 MCP 合并语义与 argv 不变，真实 broker 仅由 Docker 集成执行。
- [ ] **Step 5:** 检查 diff 并 scoped commit `feat(aurora): inject the reviewed MCP broker for skill execution`。

### Task 7: 生成→产物→结算在 Fleet 节点上端到端

**Dependencies:** Tasks 2、3、5、6。**Deliverable:** fake pipeline 下，一次 Aurora generation 在 Fleet 节点上完成、产物入库、积分结算；真实模型仍 gated。

**Files:** Create `server/internal/fleet/integration/aurora_test.go`、`e2e/aurora-cloud-runtime.spec.ts`；Modify `e2e/fixtures.ts`、`playwright.config.ts`、`server/internal/fleet/integration/docker_test.go`（复用 harness）。

**Interfaces:** `RoundTripAuroraGeneration(ctx, t) error`：API 建 generation → 节点 provision → daemon managed enroll → skill 执行（fake pipeline）→ `aurora_asset` 落行 → generation `completed` 且 `credits_charged` 生效；再覆盖 stop/start 身份保持、维护批准边界 crash、delete 撤凭据。

- [ ] **Step 1:** 先写 gate 测试骨架（`//go:build dockerintegration`，首动作检查 `MULTICA_RUN_DOCKER_INTEGRATION`），不设 gate 时 SKIP 且不碰 socket。
- [ ] **Step 2:** `make env-exec ARGS='-- bash -c "cd server && MULTICA_RUN_DOCKER_INTEGRATION=1 go test -tags=dockerintegration ./internal/fleet/integration -run TestAuroraRoundTrip -count=1 -v"'`（需授权）；最初 FAIL；skip 不算 green。
- [ ] **Step 3:** 实现 round trip，复用 `deploy/aurora-sandbox/fixtures/smoke/aurora-fake-pipelines.mjs` 与 fixture 镜像；浏览器用例经 `TestApiClient` 建节点/agent/session 并在 UI 内发 prompt、观察产物与状态，不用 sleep 猜顺序。
- [ ] **Step 4:** Docker gated Go + Playwright（`--project=fleet-docker`）通过；只清理本测试 labels + SQL owner 资源，无 prune；保留截图/trace/脱敏 IDs。
- [ ] **Step 5:** 检查 diff 并 scoped commit `test(aurora): verify skill execution on a Fleet node end to end`。

### Task 8: apps/aurora 的 Runtime/执行状态视图

**Dependencies:** Task 3（能力发现）、Task 7（真实状态）。**Deliverable:** Aurora 内可见本 workspace 的执行节点与生成执行状态；危险操作仍留在 runtime 管理页，不在 Aurora 重复。

**Files:** Create `packages/views/aurora/runtime-status.tsx`、`runtime-status.test.tsx`、`server/internal/handler/aurora_runtime_view.go`、`aurora_runtime_view_test.go`；Modify `packages/core/aurora/{schema.ts,schema.test.ts,api.ts,api.test.ts,queries.ts,queries.test.ts,types.ts}`、`packages/views/aurora/index.ts`、`packages/views/locales/{en,zh-Hans,fr,ja,ko}/aurora.json`、`apps/aurora/app/[workspaceSlug]/runtimes/page.tsx`、`apps/aurora/lib/routes.ts`、`routes.test.ts`、`apps/aurora/components/aurora-shell.tsx`、`aurora-shell.test.tsx`、`server/cmd/server/router.go`。

**Interfaces:** 后端新增只读 `GET /api/aurora/runtime`（返回 `AuroraExecutionTarget`）复用 `SandboxManager` 的 node 行与 Runtime 绑定；core 用 zod + `parseWithFallback`，未知状态安全降级；视图消费 `packages/views/runtimes` 的展示语义（状态/恢复指引），不定义 store、不引入框架路由。

- [ ] **Step 1:** 先写 schema 回归：未知 `state`/malformed 响应降级为 `unconfigured`；缺 provider 字段不 crash；端点 malformed-response 测试按仓库规则补齐。
- [ ] **Step 2:** `pnpm --filter @multica/core test -- aurora/schema.test.ts aurora/api.test.ts`、`pnpm --filter @multica/views test -- aurora/runtime-status.test.tsx`；预期缺失 FAIL。
- [ ] **Step 3:** 实现查询/组件/页面：显示 `provisioning/online/failed` 与可恢复指引；执行中展示 generation 状态（沿用既有 3s 轮询，不新增实时协议）；5 个 locale key 一致。
- [ ] **Step 4:** `pnpm typecheck`、`pnpm lint`、`pnpm test`；`apps/aurora` 窄 typecheck；views 不 mock 框架 routing、不定义 store。
- [ ] **Step 5:** 检查 diff 并 scoped commit `feat(aurora): show managed execution runtime and generation status`。

### Task 9: 文档、回归与交付门

**Dependencies:** Tasks 1–8。**Deliverable:** 可复现的本地 Aurora-on-Fleet 操作说明与验收记录；真实烟测保持 gated。

**Files:** Modify `AGENTS.md`（Aurora Roadmap）、`docs/superpowers/specs/2026-10-04-local-docker-cloud-runtime-design.md`（§15 指向本计划）、`apps/docs/content/docs/developers/local-docker-fleet.{mdx,zh.mdx}`、`CONTRIBUTING.md`；Create `docs/superpowers/plans/2026-10-06-aurora-cloud-runtime-acceptance.md`（实施时填）。

**Interfaces:** 每任务的配置/操作/测试契约。

- [ ] **Step 1:** 文档说明：`aurora` profile 配置字段、`mse_` 交接与崩溃语义、镜像必须同时满足两份契约、MCP broker 为唯一执行面、未跑项与原因；不声称 Aurora 已在 Fleet 上运行（Task 3 前）。
- [ ] **Step 2:** 完整回归：`make sqlc`（无新 diff）、`make test`、`pnpm typecheck`、`pnpm lint`、`pnpm test`、`bash scripts/fleet-env.test.sh`、`bash scripts/dev-env.test.sh`、`git diff --check`。
- [ ] **Step 3:** Docker gated：Task 7 的 Go + Playwright；记录实际命令、URL、digest 与耗时。
- [ ] **Step 4:** 真实模型烟测保持未授权状态；若授权，按既有 `agentintegration` + `MULTICA_RUN_REAL_AGENT_SMOKE=1` 闸门执行并单独记录（具体 test 名在实现时锁定）。
- [ ] **Step 5:** 检查 diff 并 scoped commit `docs(fleet): document Aurora on the local Docker runtime`。

## Spec Coverage 自审表

| 设计要求 | 任务 | 关键证据 |
| --- | --- | --- |
| Fleet 唯一控制面 / aurorafleet 退役 | 3、4 | 无 `aurorafleet` 引用、`make build` 指向 `cmd/fleet` |
| per-workspace 节点 + `mse_/mdt_` 语义 | 3、4 | 幂等 intent、注册密钥只入 secrets volume、SQL 无明文 |
| 节点运行 Aurora sandbox 运行时 | 1、2 | 镜像双契约验证、`fleet-node run --managed` |
| fleet 端 workspace-node 路由镜像 aurorafleet | 3 | `PUT|GET|DELETE /internal/v1/workspace-nodes/{nodeID}` + service key |
| 审批镜像/节点维度迁移/sqlc/store | 3 | 583/584 additive migration、`make sqlc` 无手改 generated |
| skills 执行面真实可用 | 6 | fake CLI argv/`mcpServers` 断言、`--allowedTools`、fail-closed |
| 隔离/出网/供应商密钥 | 1、5 | HostConfig/egress 单测、普通 profile 字节不变 |
| 生成→产物→结算 | 7 | Docker gated round trip + 浏览器 UI 断言 |
| `apps/aurora` 直接用 cloud runtime | 4、8 | 能力发现、Runtime/执行状态视图、5 locale |
| crash/unknown 不猜测 | 3、7 | 密钥交接缺失即失败、维护屏障沿用 |
| default tests 无 Docker/模型 | 全任务 | 定向单测；gated 命令单独列出 |
| 文档/边界/sqlc | 3、9 | `make sqlc` 无 diff、AGENTS 同步 |

## 不在本计划范围 / 需单独授权

- AWS/生产部署、`/data/multica` 自托管栈变更、`docker-compose.selfhost.aws.yml`、EC2 路径、自动扩缩容与多机调度。
- SaaS Billing/entitlement/seat capacity/Stripe 行为；本地模式仍不启用它们。Cloud entitlement `GateAurora*` 仍 deferred。
- Mobile；Slide/deck 生成。
- 真实 provider 调用的强制通过：Task 7 只要求 fake pipeline；真实烟测仍逐项授权（`agentintegration` + `MULTICA_RUN_REAL_AGENT_SMOKE=1`），执行需要明确的环境、变量与凭证。
- 发布镜像/SBOM/Trivy/签名/`verify-published`；`CVE-2026-19534`/`CVE-2026-84961` 供应链阻断是独立开放工作。
- 卷的物理擦除/硬磁盘配额保证；Docker 不是强多租户安全边界。
- 需要明确授权才可执行的命令：`docker buildx bake`/镜像构建、`make env-exec`（需要 managed environment `DATABASE_URL`）、`MULTICA_RUN_DOCKER_INTEGRATION=1` Go/Playwright、`MULTICA_RUN_REAL_AGENT_SMOKE=1` 真实烟测。

## 风险与诚实标注

- **密钥交接**：Task 3 的注册密钥只存在于 Fleet 进程内交接表。协调器崩溃即丢失，节点 bootstrap 失败；Aurora 侧 5 分钟 TTL 后重新 arm。该行为必须在 Task 3 Step 1 的回归中固定，不得改为落库明文或超时自动重试。
- **镜像双契约**：Task 2 让同一镜像既做 Aurora sandbox 又做 Fleet 节点，任何一侧的加固断言都不能放宽；`scripts/verify-aurora-sandbox-image.sh` 是权威门。
- **MCP 注入**：Task 6 之前 Aurora skill 执行链路在真实模型下不可用；这是既有缺陷，不是本计划引入。`auroraSurface.allowed` 只有变成 `--allowedTools` 才真正约束模型。
- **aurorafleet 删除**：Task 4 之前需要确认没有部署仍依赖 `AURORA_FLEET_URL`；删除后配置项与文档必须同步移除，不保留兼容 shim。
- **未执行的真实模型**：本计划不声称任何真实 Claude/Seedance/Seedream/ASR/OpenAI 调用已验证。
- **索引与迁移**：583/584 是拟用编号，执行前必须 glob 核验并对齐 `cmd/migrate` 的 cleanup 登记；down 只在测试自有 schema/数据库验证，不回滚业务库。
- **owner 作用域（已裁决）**：Fleet 节点按 owner 落库，owner 取 per-workspace 锁内读到的 `aurora_managed` runtime 行 `OwnerID`（`AgentRuntime.OwnerID`，`GetAuroraManagedRuntime` 已返回），**不由 caller 自报、也不改 `Ensure` 签名**；Fleet 内部路由用该 owner 作为可信 `X-User-ID`。若实现时发现 `GetAuroraManagedRuntime` 不返回 owner，再回到该决定并同步更新本节与 Task 4 接口。

## 执行交接

先做 P0（Task 3 → Task 4），再按 Task 2 → Task 5 → Task 6 → Task 7 → Task 8 → Task 9 执行 red→green→review；Task 1 已提交（`cd759da6e`），并在下表中登记已完成项。

1. **Subagent-Driven（推荐）**：每 task 一个 fresh subagent + 两阶段审查。
2. **Inline Execution**：当前会话按批实施，在 M1/M2/M3 门暂停审查。

环境前置：Task 3/7 需要 managed environment 的 `DATABASE_URL`（`make env-exec`）；Task 2/7 需要 Docker 构建与运行许可；真实模型账户始终单独授权。未授权时按 SKIP 记录，不记为通过。

## 实施进度（2026-10-06）

| 任务 | 状态 | 证据 |
| --- | --- | --- |
| **P0 修复 `aurora_runtime_unavailable`（Task 3 + Task 4）** | 未开始 | 当前 `.env.worktree` 无 `AURORA_*` → `SandboxManager` nil → 必然 503；先于其它任务实施 |
| Task 1 Fleet Aurora 执行 profile | 已提交 | `cd759da6e`；`go test ./internal/fleet/model ./internal/fleet/docker ./cmd/fleet-node -count=1` 全绿 |
| Task 2 镜像双契约 | 未开始 | 需 Docker/registry 授权 |
| Task 3 provision API 与密钥交接 | 未开始 | 需 managed environment `DATABASE_URL` |
| Task 4 Aurora 改走 Fleet / aurorafleet 退役 | 未开始 | 依赖 Task 3 |
| Task 5 隔离与出网对齐 | 未开始 | 依赖 Task 4 |
| Task 6 MCP broker 注入 | 未开始 | 既有缺陷 |
| Task 7 端到端生成 | 未开始 | 需 Docker 授权 |
| Task 8 apps/aurora Runtime 视图 | 未开始 | 依赖 Task 3/7 |
| Task 9 文档与交付门 | 未开始 | 依赖全部 |

**Task 1 的已知限制**：Fleet 协调器尚未为 Aurora 节点产出 `Bootstrap`，因此该 profile 目前不可达；Task 3 交付后可用。这一限制在 Task 3 Step 4 前必须保持可见，不得在文档中声称 Aurora 已经在 Fleet 上运行。
