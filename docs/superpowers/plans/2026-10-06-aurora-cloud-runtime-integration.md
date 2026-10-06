# Aurora ↔ 本地 Docker Cloud Runtime 对接实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 Aurora 的 13 个 skill 执行真正跑在本地 Docker Cloud Runtime（`server/internal/fleet`）管理的节点上：Fleet 成为唯一 Docker 控制面，节点运行 Aurora sandbox 运行时（MCP broker / egress / artifact staging），`apps/aurora` 直接消费 cloud runtime 的能力与执行状态；`server/internal/aurorafleet` 退役。

**Architecture:** `server/internal/fleet`（+ `cmd/fleet`、`/api/cloud-runtime/*` 代理、`packages/views/runtimes`）继续作为唯一 Docker 控制面，新增 Aurora 执行 profile：节点镜像为 digest 固定的 Aurora sandbox 镜像，bootstrap 改为投递一次性 `mse_` 注册密钥，`fleet-node run` 以 `--managed` 方式启动 daemon。Aurora 侧 `SandboxManager`/`SandboxReaper` 改经 fleet provision API 驱动 per-workspace 节点，保留 `aurora_sandbox_node` 生命周期与 `mse_`/`mdt_` 语义；daemon 仍走既有 claim/执行/产物/结算协议。

**Tech Stack:** Go、Chi、pgx/v5、sqlc、Docker Engine SDK、Node.js/Claude Code、TanStack Query、zod、Vitest、Playwright。

**依赖计划/规格：**
- [本地 Docker Cloud Runtime 实现计划](2026-10-04-local-docker-cloud-runtime.md) 与 [设计规格](../specs/2026-10-04-local-docker-cloud-runtime-design.md)（Fleet 的既有契约，本计划只做增量扩展）。
- [Aurora 执行层计划](2026-09-11-aurora-execution.md) 与 [沙箱工具面主计划](2026-09-22-aurora-sandbox-tool-surface.md)、[受管沙箱控制面](2026-09-25-aurora-managed-sandbox-control-plane.md)、[沙箱 skill runtime](2026-09-25-aurora-sandbox-skill-runtime.md)、[沙箱镜像与烟测](2026-09-25-aurora-sandbox-image-smoke.md)。
- [Aurora 内容创作应用设计](../specs/2026-09-11-aurora-content-creation-app-design.md) §4 执行层、§10 安全。

## 已确认决策（2026-10-06，用户批准）

1. **架构**：`server/internal/fleet` 成为唯一 Docker 控制面；`server/internal/aurorafleet` + `cmd/aurora-fleet` 退役。
2. **运行时**：Fleet 节点运行 Aurora sandbox 运行时；保留 per-workspace 节点与 `mse_`/`mdt_` 注册语义（等价实现在 Fleet 上）。
3. **范围**：先修订计划/规格，再实现首个垂直切片；其余任务按本计划顺序实现。
4. **应用层**：`apps/aurora` 增加 Runtime/节点与执行状态视图（后端接线 + 前端），不复制 `apps/web`/`apps/desktop` 整页。

## Global Constraints

- 不新增 Runtime mode 枚举；受管 metadata 仍为服务器写入（`managed_by` / `fleet_node_id`），沿用 `fleetguard` 领取/入队/注册屏障。
- Fleet 与 API 共用当前 checkout 的 PostgreSQL；不新增数据库、SQLite 或第二套队列。Aurora 的 `aurora_sandbox_node` 继续是控制面生命周期行。
- 无外键/级联删除/更新；新增索引一律 `CREATE [UNIQUE] INDEX CONCURRENTLY`，每个独立单语句迁移、事务外运行；SQL 变更后 `make sqlc`（不手改 generated）。
- 私密输入只经明确文件或一次性内存交接：`mse_` 注册密钥只写入节点 secrets volume（0600），Fleet 数据库只存哈希；模型密钥不进镜像、`Config.Env`、SQL、日志、浏览器响应或 Git。
- 模型密钥/供应商密钥只挂在节点只读 `/secrets` 与固定 `/run/secrets/*` 路径；`mcn_`/`mse_`/`mdt_` 不进模型 env、argv、prompt。
- 默认测试不依赖真实 Docker、真实模型或真实供应商账户；Docker 集成（`-tags=dockerintegration` + `MULTICA_RUN_DOCKER_INTEGRATION=1`）与真实烟测（`agentintegration` + `MULTICA_RUN_REAL_AGENT_SMOKE=1`）分别显式授权。
- 保持现有 `TaskResult` 无附件、产物走 out-of-band 上报 + `/api/agent/tasks/{id}/aurora-artifacts/upload` 的既有协议。
- core 无 UI/localStorage/process.env；views 无 store 定义与框架路由；Web/Desktop/Aurora 共享 `packages/views`，平台只注入导航/接线；5 种 locale 同步。
- 每任务结束跑定向测试、检查 diff、独立 conventional commit；不用 `git add .` 包含无关改动。

## 现状边界（2026-10-06 核实）

**Fleet（`server/internal/fleet`）**：`POST /api/v1/nodes` 只接受 `{name,spec}`；镜像由管理员 `Config.Image` 固定；容器 env 白名单仅 PATH/HOME/FLEET_NODE_MAX_RUNS；挂载固定 `/data` rw + `/secrets` ro；网络只建 namespace bridge，无 egress；无 exec。节点经 `fleet-node run` 写 CLI 配置后启动普通 daemon，API 以 `managed_by: local_fleet` 标记 Runtime。领取屏障在 `fleetguard`。
**aurorafleet（待退役）**：`PUT|GET|DELETE /internal/v1/workspace-nodes/{nodeID}`；Docker CLI backend 按不可变 `Policy` 创建 workspace `--internal` 网络 + egress sidecar + sandbox 容器（read-only rootfs、cap-drop ALL、seccomp/AppArmor、digest 固定镜像、只读 bind `/run/secrets/aurora-enrollment` 与 4 个供应商密钥文件），env 含 `MULTICA_MANAGED=1`、`MULTICA_MANAGED_ENROLLMENT_TOKEN_FILE`、`HTTP(S)_PROXY`。
**Aurora 执行链路**：`CreateAuroraGeneration` → `EnsureSystemAgents`（`aurora_managed` runtime + 13 个 `kind=system` agent）→ `SandboxManager.Ensure` 在 per-workspace advisory lock 下签发 `mse_` 并写 `aurora_sandbox_node` → `aurorafleet.ControlClient.EnsureWorkspaceNode` → daemon `POST /api/daemon/managed/enroll` → `SandboxEnrollmentService.Consume` 交换 `mdt_` 并绑定 `agent_runtime.daemon_id` → 普通 claim → `runTask` 的 `auroraToolSurface` fail-closed（claude、MaxTurns=30、deny 列表）→ `attachAuroraArtifacts` → `aurora_completion.go` 结算。
**已确认缺陷（本计划 Task 6 修复）**：broker 从未注册为 Claude 的 MCP server。`daemon.go` 在 Aurora 分支强制 `McpConfig = {"mcpServers":{}}`，注释声称由 managed sandbox 路径注入，但仓库内不存在该注入代码；`auroraSurface.allowed` 只被记日志，`pkg/agent.ExecOptions` 没有 AllowedTools 字段。9 个 `aurora.*` 工具因此不可达。
**apps/aurora**：只有 skills/works/billing 三页；`packages/core/aurora` 无任何 runtime/cloud/sandbox 引用；`packages/views/runtimes` 只接在 Web/Desktop。

## 文件结构与职责

已实现（Task 1，commit `cd759da6e`）：

- `server/internal/fleet/model/aurora.go`：`AuroraConfig{ServerURL}`、注册密钥格式校验、固定 secret 路径常量。
- `server/internal/fleet/model/config.go`、`types.go`：`Config.Aurora`、`Bootstrap.EnrollmentToken`。
- `server/internal/fleet/docker/bootstrap.go`：`installerTar` 按 profile 选择 payload；Aurora 只投递 layout manifest + `secrets/aurora-enrollment`，并做 canonical 字节校验。
- `server/internal/fleet/docker/provider.go`、`inspect.go`：Aurora 容器 env（`MULTICA_MANAGED`/`MULTICA_SERVER_URL`/`MULTICA_MANAGED_ENROLLMENT_TOKEN_FILE`）与 inspect 白名单；两 profile 永不共享 bootstrap。
- `server/cmd/fleet-node/main.go`：`runManagedNode` 以 `--managed` 启动 daemon，只接受固定路径、0600、格式合法的注册密钥。

拟新增/修改：

- `server/internal/fleet/store/aurora.go`、`server/pkg/db/queries/fleet.sql`：Aurora workspace-node intent（显式 node UUID、daemon UUID、镜像 digest、幂等键）。
- `server/internal/fleet/service.go`、`http.go`、`internal_dto.go`：`PUT|GET|DELETE /internal/v1/workspace-nodes/{nodeID}` 服务密钥路由与内存注册密钥交接。
- `server/internal/fleet/reconciler.go`、`scheduler.go`：Aurora profile 下用交接的注册密钥生成 `Bootstrap`。
- 删除 `server/internal/aurorafleet/` 与 `server/cmd/aurora-fleet/`（不留兼容 shim）。
- `server/internal/cloudruntime/client.go`、`fleet_internal.go`：新增 workspace-node 客户端方法。
- `server/internal/aurora/sandbox_manager.go`、`sandbox_reaper.go`：改用新的 `FleetProvisioner` 接口。
- `server/cmd/server/main.go`、`server/cmd/server/local_fleet.go`、`router.go`、`server/internal/handler/local_fleet_operations.go`：配置互斥与路由。
- `server/internal/daemon/daemon.go`、`aurora_tool_surface.go`、`server/pkg/agent/agent.go`、`claude.go`：broker 注入与工具白名单。
- `deploy/aurora-sandbox/Dockerfile`（或新增派生镜像）：镜像内提供 `fleet-node` 与固定 layout。
- `docker-compose.fleet.yml`、`scripts/fleet-env.sh`、`fleet-config.example.json`：Aurora profile 的配置/生命周期。
- `packages/core/aurora/{schema,api,queries,types}.ts`、`packages/views/aurora/runtime-status.tsx`、`apps/aurora/app/[workspaceSlug]/runtimes/page.tsx`、`apps/aurora/components/aurora-shell.tsx`、`apps/aurora/lib/routes.ts`、5 个 locale：Aurora 内 Runtime/执行状态视图。
- `e2e/aurora-cloud-runtime.spec.ts`、`server/internal/fleet/integration/aurora_test.go`：端到端与 Docker gated 验收。
- `docs/superpowers/plans/2026-10-06-aurora-cloud-runtime-integration.md`（本文件）、[AGENTS.md](../../../AGENTS.md)、[设计规格](../specs/2026-10-04-local-docker-cloud-runtime-design.md)。

## 跨任务契约

```go
// server/internal/fleet/model/aurora.go（已实现）
type AuroraConfig struct { ServerURL string `json:"server_url"` }
func (a AuroraConfig) Validate() error
func ValidEnrollmentToken(token string) bool
const (
    AuroraEnrollmentDir     = "/secrets"
    AuroraEnrollmentFile    = AuroraEnrollmentDir + "/aurora-enrollment"
    AuroraManagedEnv        = "MULTICA_MANAGED"
    AuroraServerURLEnv      = "MULTICA_SERVER_URL"
    AuroraEnrollmentFileEnv = "MULTICA_MANAGED_ENROLLMENT_TOKEN_FILE"
)

// server/internal/fleet/model/types.go（已实现）
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

// server/internal/fleet/model/types.go（Task 3 追加，均为公开 DTO 之外的领域字段）
type AuroraNodeRequest struct {
    DaemonID, ImageDigest, Name, Spec, IdempotencyKey string
    EnrollmentToken string // 只在内存中交接，绝不落库
}
```

```go
// server/internal/fleet/service.go（Task 3 产出）
// ProvisionAuroraNode 接受已认证 owner 的 workspace 节点意图，落库后把一次性
// 注册密钥登记到进程内交接表；协调器消费后立即删除。进程崩溃即丢失密钥，
// Aurora 侧 5 分钟 TTL 后重新 arm，不猜测成功。
func (s *Service) ProvisionAuroraNode(ctx context.Context, ownerID pgtype.UUID,
    nodeID pgtype.UUID, req model.AuroraNodeRequest) (model.Node, model.Operation, error)
func (s *Service) TakeAuroraEnrollment(nodeID pgtype.UUID) (string, bool)
func (s *Service) DeleteAuroraNode(ctx context.Context, ownerID, nodeID pgtype.UUID) error
```

```ts
// packages/core/aurora/schema.ts + api.ts（Task 8 产出）
export interface AuroraRuntimeNode {
  id: string; status: string; ready: boolean; provider: "docker";
  errorCode?: string; operationId?: string; createdAt: string;
}
export interface AuroraExecutionTarget {
  workspaceId: string; node: AuroraRuntimeNode | null;
  runtimeId: string | null; state: "unconfigured"|"provisioning"|"online"|"failed";
}
```

## 任务总览与实施顺序

三个可独立验收的里程碑：

1. **M1（Tasks 1–3）**：Fleet 具备 Aurora 执行 profile，并能按 workspace 受控地创建/删除节点。默认测试不需要 Docker。
2. **M2（Tasks 4–7）**：Aurora 改走 Fleet，aurorafleet 退役；MCP broker 真正接入；fake pipeline 端到端在 Fleet 节点上完成生成→产物→结算。
3. **M3（Tasks 8–9）**：`apps/aurora` 的 Runtime/执行状态视图、文档、回归与 gated 真实烟测。

严格按依赖顺序执行；每个任务只 stage 自己列出的文件。

### Task 1: Fleet Aurora 执行 profile（已完成）

**Dependencies:** 无。**Deliverable:** Fleet 支持按部署 profile 运行 Aurora sandbox 运行时；两 profile 的 bootstrap/容器 env 永不混用。

**Files（已提交 `cd759da6e`）:** `server/internal/fleet/model/{aurora.go,aurora_test.go,config.go,types.go}`、`server/internal/fleet/docker/{bootstrap.go,provider.go,inspect.go,aurora_test.go,inspect_test.go,offline_reports.go}`、`server/cmd/fleet-node/{main.go,aurora_test.go}`。

**Interfaces:** 见上节；`installerTar(n, cfg, b)` 是唯一 installer 生产者，Aurora profile 下投递 `secrets/aurora-enrollment`。

- [x] **Step 1:** 先写状态矩阵：`Config.Aurora` 的 URL/嵌套字段/类型拒绝矩阵；`ValidEnrollmentToken` 正反例；两 profile 互拒绝。
- [x] **Step 2:** `(cd server && go test ./internal/fleet/model -count=1)`；预期 profile 字段/校验未实现 FAIL。
- [x] **Step 3:** 实现 model profile、`installerTar` 分派、Aurora 容器 env、inspect 白名单、`runManagedNode`。
- [x] **Step 4:** `go test ./internal/fleet/model ./internal/fleet/docker ./cmd/fleet-node -count=1` 全绿（2026-10-06：model 1.2s、docker 31.9s、fleet-node 1.6s）。
- [x] **Step 5:** scoped commit `feat(fleet): add the Aurora managed-sandbox execution profile`（`cd759da6e`）。

**尚未完成（由后续任务交付）**：协调器还不会为 Aurora 节点产出 `Bootstrap`，因此该 profile 目前不可达；Task 3 负责使其可用。

### Task 2: 让 Aurora sandbox 镜像同时是 Fleet 节点镜像

**Dependencies:** Task 1。**Deliverable:** 一个 digest 固定的镜像同时满足 Aurora sandbox 与 Fleet 节点两份契约。

**Files:** Modify `deploy/aurora-sandbox/Dockerfile`、`deploy/aurora-sandbox/docker-bake.hcl`、`deploy/aurora-sandbox/versions.json`（如需要）；Modify `scripts/verify-aurora-sandbox-locks.mjs`、`scripts/verify-aurora-sandbox-image.sh`、`deploy/aurora-sandbox/runtime/package.json`（如需）；Modify `docker/runtime/Dockerfile` 或新增 `docker/runtime-aurora/Dockerfile`；Create `deploy/aurora-sandbox/fixture/Dockerfile.sandbox-fleet`（测试用假 CLI 镜像）。

**Interfaces:** 镜像必须提供 `/usr/local/bin/fleet-node`、`/usr/local/bin/multica`、`/data`+`/data/home`+`/data/workspaces`+`/secrets`（10001 属主）、broker 运行时与 patched vendor 树；`ENTRYPOINT ["/usr/local/bin/fleet-node","run"]`、`HEALTHCHECK fleet-node health`；`USER 10001:10001`。

- [ ] **Step 1:** 先扩展 `scripts/verify-aurora-sandbox-image.sh` 断言新契约（fleet-node 存在、layout 目录、entrypoint、healthcheck、无包管理器/密钥），跑一次预期 FAIL。
- [ ] **Step 2:** 修改 Dockerfile：`gobuilder` 同时 `go build ./cmd/fleet-node`；保留既有 apt/vendor/esbuild 加固；补 `install -d -o 10001 ... /data /data/home /data/workspaces /secrets`；替换 entrypoint/healthcheck。
- [ ] **Step 3:** `node scripts/verify-aurora-sandbox-locks.mjs --workflow` 与 `docker buildx bake -f deploy/aurora-sandbox/docker-bake.hcl sandbox --load --set '*.platform=linux/amd64'`（需授权）；`scripts/verify-aurora-sandbox-image.sh <digest>`。
- [ ] **Step 4:** Linux 安全验收脚本 `deploy/aurora-sandbox/docker-security-test.sh` 保持通过；容器化 fake pipeline smoke 保持通过。
- [ ] **Step 5:** scoped commit `build(aurora): ship fleet-node in the managed sandbox image`。

### Task 3: Fleet workspace-node provision API 与注册密钥交接

**Dependencies:** Tasks 1–2。**Deliverable:** Fleet 能按 Aurora 的 `PUT|GET|DELETE /internal/v1/workspace-nodes/{nodeID}` 契约受控创建/查询/删除节点，并把一次性注册密钥交给协调器。

**Files:** Create `server/internal/fleet/store/aurora.go`、`aurora_test.go`；Modify `server/pkg/db/queries/fleet.sql` + `make sqlc` 生成物；Modify `server/internal/fleet/{service.go,http.go,internal_dto.go,reconciler.go,scheduler.go,service_test.go,http_test.go,reconciler_test.go}`；Modify `server/internal/cloudruntime/{client.go,fleet_internal.go,client_test.go}`。

**Interfaces:** 见上节 `CreateAuroraIntent`/`ProvisionAuroraNode`/`TakeAuroraEnrollment`。HTTP 请求体严格为 `{workspace_id, runtime_id, daemon_id, image_digest, name, spec, idempotency_key}`；响应复用 `NodeDTO`。路由与既有内部路由同源：服务密钥 constant-time + 可信 `X-User-ID`。

- [ ] **Step 1:** 写 DB 回归：同一 (owner, idempotency_key) 重放返回同一 node/操作；不同 payload 同键 409；owner 限额；显式 daemon UUID 被持久化；密钥不落库（扫描 `fleet_nodes`/操作表无 `mse_` 明文）。
- [ ] **Step 2:** `make env-exec ARGS='-- bash -c "cd server && go test ./internal/fleet/store -run TestFleetAuroraIntent -count=1"'`；预期缺方法 FAIL。
- [ ] **Step 3:** 实现 store intent（owner 级 advisory lock → 幂等查询 → 限额 → 插入 node/op），路由与内存交接表；协调器 `initialize` 在 `cfg.Aurora != nil` 时取交接密钥构造 `Bootstrap{DaemonID, EnrollmentToken, ServerURL}`，缺失即按普通 bootstrap 失败处理（不猜成功）。
- [ ] **Step 4:** `go test ./internal/fleet/... -count=1`（含 fake provider 的 http/reconciler 回归）；断言无 Docker 调用即可验证创建到 `launching`。
- [ ] **Step 5:** scoped commit `feat(fleet): provision Aurora workspace nodes`。

### Task 4: Aurora 侧改走 Fleet，aurorafleet 退役

**Dependencies:** Task 3。**Deliverable:** `SandboxManager`/`SandboxReaper` 只依赖 provider 中立的 provision 接口，生产实现指向 Fleet；`aurorafleet` 与 `cmd/aurora-fleet` 删除。

**Files:** Modify `server/internal/aurora/{sandbox_manager.go,sandbox_manager_test.go,sandbox_reaper.go,sandbox_reaper_test.go}`；Create `server/internal/aurora/fleet_provisioner.go`、`fleet_provisioner_test.go`；Modify `server/cmd/server/{main.go,local_fleet.go,local_fleet_test.go,router.go}`、`server/internal/handler/{aurora_fleet_e2e_test.go,local_fleet_operations.go}`；Delete `server/internal/aurorafleet/`、`server/cmd/aurora-fleet/`；Modify `Makefile`（`make build` 目标改指 `./cmd/fleet`）。

**Interfaces:** `aurora.FleetProvisioner`：

```go
type FleetNode struct { ID, State, BackendID string }
type FleetEnsureRequest struct {
    NodeID, WorkspaceID, RuntimeID, DaemonID, EnrollmentToken, ImageDigest, Name, Spec string
}
type FleetProvisioner interface {
    EnsureWorkspaceNode(context.Context, FleetEnsureRequest) (FleetNode, error)
    DeleteWorkspaceNode(context.Context, string) error
}
```

- [ ] **Step 1:** 先用 fake provisioner 写回归：state 复用/重 arm/失败标记与今天的语义逐条一致（迁移既有 `sandbox_manager_test.go` 断言）。
- [ ] **Step 2:** `go test ./internal/aurora -run 'TestSandboxManager|TestSandboxReaper' -count=1`；预期接口未替换 FAIL。
- [ ] **Step 3:** 实现 `fleet_provisioner.go`（经 `cloudruntime.Client` 调用 Task 3 路由，服务密钥来自明确文件，永不来自 caller）；`main.go` 改为 `MULTICA_LOCAL_FLEET_URL` + `MULTICA_LOCAL_FLEET_SECRET_FILE` + `AURORA_SANDBOX_IMAGE` 三者齐备才启用，缺一即 fail-closed（保持今日 503 语义）；删除 aurorafleet。
- [ ] **Step 4:** `go build ./...`、`go test ./internal/aurora ./cmd/server ./internal/handler -run 'Aurora|Fleet|Sandbox' -count=1`；确认无 `aurorafleet` 引用（`grep -r aurorafleet`）。
- [ ] **Step 5:** scoped commit `refactor(aurora): run workspace sandboxes on the local Docker Fleet`。

### Task 5: 节点容器与 Aurora 安全边界对齐

**Dependencies:** Task 4。**Deliverable:** Fleet 节点容器达到 Aurora sandbox 现有隔离与出网契约（不含任意 exec）。

**Files:** Modify `server/internal/fleet/model/{aurora.go,config.go}`、`server/internal/fleet/docker/{provider.go,inspect.go}`、`server/internal/fleet/model/layout.go`；Modify `docker-compose.fleet.yml`、`fleet-config.example.json`、`scripts/fleet-env.sh`、`scripts/fleet-env.test.sh`；Create `server/internal/fleet/docker/egress.go`、`egress_test.go`。

**Interfaces:** `AuroraConfig` 扩展 `{ProxyImage, SeccompProfile, EgressHosts []string, AnthropicBaseURL, AnthropicModel, ProviderSecretFiles map[string]string, ReadonlyRootfs bool}`；`NodeHostConfig` 增加 seccomp/AppArmor/read-only rootfs 与固定 `Tmpfs`（`/workspace`）；容器新增只读 bind 供应商密钥到固定 `/run/secrets/*`，并加入 egress sidecar（alias `egress`）与 `HTTP(S)_PROXY`。

- [ ] **Step 1:** 先写资源构造单测（`TestAuroraNodeHostConfigRestricted`、`TestEgressSidecarPolicy`），断言无 socket、无 privileged、非 root、固定 seccomp/AppArmor、只读 rootfs、仅允许的 bind 与 env。
- [ ] **Step 2:** `go test ./internal/fleet/docker -run 'Aurora|Egress' -count=1`；预期 builder/策略 FAIL。
- [ ] **Step 3:** 实现策略；`inspectEnvironment`/`validateNodeInspection` 只在 Aurora profile 下放行代理与供应商密钥 env；`fleet-env.sh` 增加 Aurora profile 的私有配置字段（不含密钥值）。
- [ ] **Step 4:** `bash scripts/fleet-env.test.sh`、`go test ./internal/fleet/... -count=1`；断言普通 profile 行为逐字节不变。
- [ ] **Step 5:** scoped commit `feat(fleet): match the Aurora sandbox isolation and egress contract`。

### Task 6: 把 MCP broker 真正接给 skill 执行

**Dependencies:** Task 4。**Deliverable:** Aurora 任务把 9 个 `aurora.*` broker 工具作为唯一执行面接入 Claude，并把允许集交给 CLI。

**Files:** Modify `server/internal/daemon/{daemon.go,aurora_tool_surface.go,aurora_tool_surface_test.go}`、`server/pkg/agent/{agent.go,claude.go,claude_test.go}`；Modify `deploy/aurora-sandbox/runtime/src/server.mjs`（如需固定启动参数）；Create `server/internal/daemon/aurora_broker.go`、`aurora_broker_test.go`。

**Interfaces:** `daemon.auroraMcpConfig(surface auroraSurface, env execenv.SidecarPaths) (json.RawMessage, error)` 产出 `{"mcpServers":{"aurora":{"command":"node","args":[...],"env":{...}}}}`；`agent.ExecOptions` 新增 `AllowedTools []string`，Claude backend 映射为 `--allowedTools`（与既有 `--disallowedTools` 并存）。

- [ ] **Step 1:** 先写 fake CLI 断言（不执行真实 agent）：Aurora 任务的 argv 含 `--allowedTools`，MCP 配置只含 `aurora` server，非 Aurora 任务不受影响；敌意 agent MCP 配置被覆盖。
- [ ] **Step 2:** `go test ./pkg/agent ./internal/daemon -run 'AuroraBroker|AuroraToolSurface|Claude' -count=1`；预期注入缺失 FAIL。
- [ ] **Step 3:** 实现 `aurora_broker.go` 并在 `runTask` 的 Aurora 分支替换 `{"mcpServers":{}}`；保留 fail-closed：broker 配置或上下文缺失即任务失败并退款。
- [ ] **Step 4:** `go test -race ./internal/daemon ./pkg/agent -run 'Aurora|Claude' -count=1`；确认普通 agent 的 MCP 合并语义不变。
- [ ] **Step 5:** scoped commit `feat(aurora): inject the reviewed MCP broker for skill execution`。

### Task 7: 生成→产物→结算在 Fleet 节点上端到端

**Dependencies:** Tasks 2、3、5、6。**Deliverable:** fake pipeline 下，一次 Aurora generation 在 Fleet 节点上完成、产物入库、积分结算；真实模型仍 gated。

**Files:** Create `server/internal/fleet/integration/aurora_test.go`、`e2e/aurora-cloud-runtime.spec.ts`；Modify `e2e/fixtures.ts`、`playwright.config.ts`、`server/internal/fleet/integration/docker_test.go`（复用 harness）。

**Interfaces:** `RoundTripAuroraGeneration(ctx, t) error`：API 建 generation → 节点 provision → daemon managed enroll → skill 执行（fake pipeline）→ `aurora_asset` 落行 → generation `completed` 且 `credits_charged` 生效；再覆盖 stop/start 身份保持、维护批准边界 crash、delete 撤凭据。

- [ ] **Step 1:** 先写 gate 测试骨架（`//go:build dockerintegration`，首动作检查 `MULTICA_RUN_DOCKER_INTEGRATION`），不设 gate 时 SKIP 且不碰 socket。
- [ ] **Step 2:** `make env-exec ARGS='-- bash -c "cd server && MULTICA_RUN_DOCKER_INTEGRATION=1 go test -tags=dockerintegration ./internal/fleet/integration -run TestAuroraRoundTrip -count=1 -v"'`（需授权）；最初 FAIL。
- [ ] **Step 3:** 实现 round trip，复用 `deploy/aurora-sandbox/fixtures/smoke/aurora-fake-pipelines.mjs` 与 fixture 镜像；浏览器用例经 `TestApiClient` 建节点/agent/session 并在 UI 内发 prompt、观察产物与状态，不用 sleep 猜顺序。
- [ ] **Step 4:** Docker gated Go + Playwright（`--project=fleet-docker`）通过；只清理本测试 labels + SQL owner 资源，无 prune。
- [ ] **Step 5:** scoped commit `test(aurora): verify skill execution on a Fleet node end to end`。

### Task 8: apps/aurora 的 Runtime/执行状态视图

**Dependencies:** Task 3（能力发现）、Task 7（真实状态）。**Deliverable:** Aurora 内可见本 workspace 的执行节点与生成执行状态。

**Files:** Modify `packages/core/aurora/{schema.ts,schema.test.ts,api.ts,api.test.ts,queries.ts,queries.test.ts,types.ts,mutations.ts,mutations.test.tsx}`；Create `packages/views/aurora/runtime-status.tsx`、`runtime-status.test.tsx`；Modify `packages/views/aurora/index.ts`、`packages/views/locales/{en,zh-Hans,fr,ja,ko}/aurora.json`；Modify `apps/aurora/app/[workspaceSlug]/runtimes/page.tsx`、`apps/aurora/lib/routes.ts`、`lib/routes.test.ts`、`components/aurora-shell.tsx`、`components/aurora-shell.test.tsx`、`packages/core/paths/*`（若新增路由）。

**Interfaces:** 后端新增只读 `GET /api/aurora/runtime`（返回 `AuroraExecutionTarget`）复用 `SandboxManager` 的 node 行与 Runtime 绑定；core 用 zod + `parseWithFallback`，未知状态安全降级。

- [ ] **Step 1:** 先写 schema 回归：未知 `state`/malformed 响应降级为 `unconfigured`；缺 provider 字段不 crash。
- [ ] **Step 2:** `pnpm --filter @multica/core test -- aurora/schema.test.ts aurora/api.test.ts`、`pnpm --filter @multica/views test -- aurora/runtime-status.test.tsx`；预期缺失 FAIL。
- [ ] **Step 3:** 实现查询/组件/页面：显示 `provisioning/online/failed` 与可恢复指引；执行中展示 generation 状态（沿用既有 3s 轮询，不新增实时协议）；危险操作仍在 runtime 管理页，不在 Aurora 重复。
- [ ] **Step 4:** `pnpm typecheck`、`pnpm lint`、5 个 locale key 一致；`apps/aurora` 窄 typecheck。
- [ ] **Step 5:** scoped commit `feat(aurora): show managed execution runtime and generation status`。

### Task 9: 文档、回归与交付门

**Dependencies:** Tasks 1–8。**Deliverable:** 可复现的本地 Aurora-on-Fleet 操作说明与验收记录。

**Files:** Modify `AGENTS.md`（Aurora Roadmap）、`docs/superpowers/specs/2026-10-04-local-docker-cloud-runtime-design.md`（§15 指向本计划）、`apps/docs/content/docs/developers/local-docker-fleet.{mdx,zh.mdx}`、`CONTRIBUTING.md`；Create `docs/superpowers/plans/2026-10-06-aurora-cloud-runtime-acceptance.md`（验收记录，实施时填）。

- [ ] **Step 1:** 文档说明：`aurora` profile 配置字段、`mse_` 交接与崩溃语义、镜像必须同时满足两份契约、MCP broker 为唯一执行面、未跑项与原因。
- [ ] **Step 2:** 完整回归：`make sqlc`（无新 diff）、`make test`、`pnpm typecheck`、`pnpm lint`、`pnpm test`、`bash scripts/fleet-env.test.sh`、`bash scripts/dev-env.test.sh`、`git diff --check`。
- [ ] **Step 3:** Docker gated：Task 7 的 Go + Playwright；记录实际命令、URL、digest 与耗时。
- [ ] **Step 4:** 真实模型烟测保持未授权状态；若授权，按既有 `agentintegration` + `MULTICA_RUN_REAL_AGENT_SMOKE=1` 闸门执行并单独记录。
- [ ] **Step 5:** scoped commit `docs(fleet): document Aurora on the local Docker runtime`。

## Spec Coverage 自审表

| 设计要求 | 任务 | 关键证据 |
| --- | --- | --- |
| Fleet 唯一控制面 / aurorafleet 退役 | 3、4 | 无 `aurorafleet` 引用、`make build` 指向 `cmd/fleet` |
| per-workspace 节点 + `mse_`/`mdt_` 语义 | 3、4 | 幂等 intent、注册密钥只入 secrets volume |
| 节点运行 Aurora sandbox 运行时 | 1、2 | 镜像双契约验证、`fleet-node run --managed` |
| skills 执行面真实可用 | 6 | fake CLI argv/`mcpServers` 断言、fail-closed |
| 隔离/出网/供应商密钥 | 5 | HostConfig/egress 单测、普通 profile 字节不变 |
| 生成→产物→结算 | 7 | Docker gated round trip + 浏览器 UI 断言 |
| `apps/aurora` 直接用 cloud runtime | 4、8 | 能力发现、Runtime/执行状态视图 |
| crash/unknown 不猜测 | 3、7 | 密钥交接缺失即失败、维护屏障沿用 |
| default tests 无 Docker/模型 | 全任务 | 定向单测；gated 命令单独列出 |
| 文档/边界/sqlc | 3、9 | `make sqlc` 无 diff、AGENTS 同步 |

## 不在本计划范围

- AWS/生产部署、`/data/multica` 自托管栈变更、EC2 路径、自动扩缩容与多机调度。
- SaaS Billing/entitlement/seat capacity 行为；本地模式仍不启用它们。
- Mobile。
- 真实 provider 调用的强制通过：Task 7 只要求 fake pipeline；真实烟测仍逐项授权。
- 卷的物理擦除/硬磁盘配额保证；Docker 不是强多租户安全边界。

## 风险与诚实标注

- **密钥交接**：Task 3 的注册密钥只存在于 Fleet 进程内交接表。协调器崩溃即丢失，节点 bootstrap 失败；Aurora 侧 5 分钟 TTL 后重新 arm。该行为必须在 Task 3 Step 1 的回归中固定，不得改为落库明文或超时自动重试。
- **镜像双契约**：Task 2 让同一镜像既做 Aurora sandbox 又做 Fleet 节点，任何一侧的加固断言都不能放宽；`scripts/verify-aurora-sandbox-image.sh` 是权威门。
- **MCP 注入**：Task 6 之前 Aurora skill 执行链路在真实模型下不可用；这是既有缺陷，不是本计划引入。
- **aurorafleet 删除**：Task 4 之前需要确认没有部署仍依赖 `AURORA_FLEET_URL`；删除后配置项与文档必须同步移除，不保留兼容 shim。
- **未执行的真实模型**：本计划不声称任何真实 Claude/Seedance/Seedream/ASR/OpenAI 调用已验证。

## 执行交接

按 Task 2 → 9 顺序执行 red→green→review；Task 1 已提交（`cd759da6e`），并在下表中登记已完成项。

1. **Subagent-Driven（推荐）**：每 task 一个 fresh subagent + 两阶段审查。
2. **Inline Execution**：当前会话按批实施，在 M1/M2/M3 门暂停审查。

环境前置：Task 3/7 需要 managed environment 的 `DATABASE_URL`（`make env-exec`）；Task 2/7 需要 Docker 构建与运行许可；真实模型账户始终单独授权。未授权时按 SKIP 记录，不记为通过。

## 实施进度（2026-10-06）

| 任务 | 状态 | 证据 |
| --- | --- | --- |
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
