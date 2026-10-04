# 本地 Docker Cloud Runtime Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在当前仓库交付可运行的 Docker Fleet，接入现有本地 API/Web，节点仅运行 Claude Code，基础设施状态复用原 PostgreSQL 数据库。

**Architecture:** 独立 Go Fleet 用 Docker Engine API 管理容器和节点卷，API 与 Fleet 共享当前 checkout 数据库、sqlc 查询和事务级节点锁。复用既有 Daemon、Runtime、任务队列和回传协议；本地节点服务与 SaaS Billing/配额客户端分离。

**Tech Stack:** Go、Chi、pgx/v5、sqlc、PostgreSQL、Docker Engine SDK、Node.js/Claude Code、TanStack Query、zod、Vitest、Playwright。

**Spec:** [已批准设计](<../specs/2026-10-04-local-docker-cloud-runtime-design.md>)，以用户批准的 PostgreSQL 修订版为准。

## Global Constraints

- 用户已明确授权开发；本次准备只修订限定文档，不执行产品代码/迁移/构建/服务。后续实施先完成下方 managed environment 前置检查；Docker、registry 查询、共享数据库建立和真实模型仍须独立安全授权。
- 实施开始时使用 using-git-worktrees 检查隔离工作区；不复制主 checkout 的私密环境文件，不自行建立第二个 PostgreSQL 实例。
- Fleet 与 API 使用当前 checkout 同一个 PostgreSQL 数据库；无 SQLite、第二套数据库或独立数据库卷。数据库复用现有 pgx/sqlc。
- 不安装 Omp、Codex 或其他智能体 CLI，不自动创建业务智能体。不增加 Docker 专属任务协议或 Runtime mode；保持 `local` 模式。
- 本地 Fleet URL 与 `MULTICA_CLOUD_URL` 互斥；本地模式不启用 Billing、entitlement、seat capacity。
- 默认：2 CPU、4 GiB 内存、256 个进程，每节点同时 1 次运行，每个配置用户最多 2 个节点。
- 默认数据库查询/锁等待超时 2 秒、固定诊断超时 5 秒、普通 Docker 操作超时 30 秒、初始化窗口 5 分钟、协调周期 5 秒；有界重试最多 5 次；节点健康 30 秒后过期。
- 无外键、级联删除或更新；所有新增索引用 `CREATE [UNIQUE] INDEX CONCURRENTLY`，每个独立单语句迁移、事务外运行。SQL 变更后 `make sqlc`。
- 忙碌、待回传、未知健康时不能危险操作；第一版无 force。删除前取消排队/延后运行。故障不凭超时解除屏障。
- Fleet 有 Docker socket 高权限，节点无 socket、无 privileged、非 root、无 host network/PID；端口只绑定 loopback。
- Claude 凭证独立配置，不读用户 home/OAuth；模型密钥不进镜像、Docker Config.Env、SQL、日志、浏览器响应或 Git。
- 业务资源请求通过现有 UUID loader/parseUUIDOrBadRequest；可信 DB UUID 才能直接转换。所有者、namespace、工作区权限分别校验。
- core 无 UI/localStorage/process.env；views 无 stores/框架路由。服务器状态由 Query 管，响应过 zod/parseWithFallback，所有支持的 Web/Desktop 翻译同步。Mobile 不在范围。
- 默认测试必须使用测试生成的假 Claude，无真实账户和 CLI。Docker 集成与真实模型烟测分别 opt-in，未授权不执行真实模型。
- 各任务结束定向测试、检查 diff、独立 conventional commit；不得用 `git add .` 包含无关改动。

## 范围、依赖与实施顺序

这是单一端到端子系统，不拆出计费或通用云平台。三个可独立验收里程碑：

1. **M1（Tasks 1–7）**：fake Provider + PostgreSQL + HTTP 契约 + API 认证/领取屏障，默认测试可验证，不需要 Docker。
2. **M2（Tasks 8–10、13）**：Docker 节点/镜像/恢复进程 + 本地环境入口，能用 fake Claude 镜像完成 API 运行链路。
3. **M3（Tasks 11–12、14–15）**：共享界面、浏览器全链路、回归与文档。

按任务依赖执行；安全写范围是任务列出的文件。没有请求 Agent Teams，不创建 teammate。若之后选择子代理执行，仍逐任务执行和审查，禁止重叠编辑。

## 文件结构与职责

新增（拟创建，尚不存在）：

- `server/internal/fleet/model/{types,policy,config}.go`：无 I/O 的状态、DTO、错误和安全判断。
- `server/internal/fleet/store/{store,intents,credentials,maintenance,recovery}.go`：唯一 PostgreSQL 数据访问边界，禁止直接 Docker/HTTP I/O。
- `server/internal/fleet/{service,http,credentials,reconciler}.go`：节点服务、路由、profile 投影、恢复协调。
- `server/internal/fleet/docker/{provider,bootstrap,inspect}.go`：固定资源布局、Engine 调用、固定诊断；不暴露任意 exec。
- `server/internal/fleetguard/{claim,maintenance}.go`：API 领取和维护事务，无 fleet HTTP 领取许可。
- `server/cmd/fleet/main.go`：独立程序；`server/cmd/fleet-node/main.go`：固定 bootstrap/entrypoint/health 工具。
- `server/pkg/db/queries/fleet.sql`：共享 sqlc 查询；Fleet 迁移进入现有迁移目录。
- `docker/fleet/Dockerfile`、`docker/runtime/Dockerfile`、`docker/runtime/claude-version.txt`、`docker-compose.fleet.yml`：固定镜像和 Fleet 部署。
- `scripts/fleet-env.sh`、`scripts/fleet-config.example.json`：当前 checkout 的容器可达配置与安全生命周期工具。
- `e2e/cloud-runtime.spec.ts`、`server/internal/fleet/integration/docker_test.go`：分别浏览器与真实 Docker/fake Claude 验证。

现有接入位置：

- [API 组装](<../../../server/cmd/server/router.go#L430-L486>)、[handler 字段](<../../../server/internal/handler/handler.go>)、[Cloud 代理](<../../../server/internal/handler/cloud_runtime.go>)、[Billing 代理](<../../../server/internal/handler/cloud_billing.go>)。
- [Token 验证](<../../../server/internal/auth/cloud_pat.go>)、[普通认证](<../../../server/internal/middleware/auth.go>)、[Daemon 认证](<../../../server/internal/middleware/daemon_auth.go>)。
- [领取事务](<../../../server/internal/service/task.go#L3489-L3590>)、[批量入口](<../../../server/internal/service/task.go#L3915-L4113>)、[Daemon handler](<../../../server/internal/handler/daemon.go>)、[WS RPC](<../../../server/internal/handler/daemon_rpc.go>)。
- [core 节点逻辑](<../../../packages/core/runtimes/cloud-runtime.ts>)、[API client](<../../../packages/core/api/client.ts#L2014-L2055>)、[schemas](<../../../packages/core/api/schemas.ts#L1643-L1677>)。
- [共享节点弹窗](<../../../packages/views/runtimes/components/cloud-runtime-dialog.tsx>)、[共享运行时页](<../../../packages/views/runtimes/components/runtimes-page.tsx>)、[Desktop 接线](<../../../apps/desktop/src/renderer/src/components/desktop-runtimes-page.tsx>)、[Web 接线](<../../../apps/web/app/%5BworkspaceSlug%5D/%28dashboard%29/runtimes/page.tsx>)。
- [环境脚本](<../../../scripts/dev-env.sh>)、[Makefile](<../../../Makefile#L157-L176>)、[E2E 客户端](<../../../e2e/fixtures.ts>)。

## 执行前置检查（顺序实施，不增加第 16 项任务）

基线为 eanfs/multica main ef253c73884ac9de2863075b195766b8abe45d80；保留输入/修订文档 SHA-256 于准备报告。开发已授权，本次只准备文档，不提交、不改 progress.md。保留 red→green→review；缺环境不是有效 red。

1. 检查 worktree/branch、AGENTS/中英文 conventions、依赖工具。Task 1 用 `(cd server && go test ./internal/fleet/model -count=1)`，不依赖 DB/env-exec。
2. DB 任务前核验 managed registry/manifest DIR 与 worktree 一致，环境文件由管理脚本生成，DB 名/端口与登记一致；不复制主 checkout 私密文件、不打印 URL/凭证。登记/共享 DB 尚未建立时报告 setup pending，请求安全授权，不执行 env-exec 制造 red。
3. 授权后按既有 managed environment 流程建立登记/DB，再迁移、env-exec；先 fixture 连通/setup 检查，之后才 regression red。setup gate 与产品 FAIL/SKIP 分开记录。
4. Docker、npm registry 版本查询、镜像构建、真实账户分别 opt-in；本次未执行。Task 9 在 registry 许可前不查版本、不写 latest；可继续 fake CLI/helper 单测。
5. 最新迁移 563；执行 Task 2 再用 glob 核验 564–575 无占用，main 推进则同步连续重编号及 runner map/tests，禁止覆盖 Aurora 512–517。API/Core/Views 不移入 Aurora，框架 helper 仍归 packages/nextjs。

## 跨任务契约

下面名称是本计划锁定的新接口，不是假定仓库中已经存在。Go 使用 `pgtype.UUID`，公开 JSON DTO 使用字符串 UUID；domain 与 DB model 通过显式转换隔离。

```go
// Package model: server/internal/fleet/model/types.go
// Imports: time, github.com/jackc/pgx/v5/pgtype.
type Action string
const (
    Create Action = "create"
    Start Action = "start"
    Stop Action = "stop"
    Reboot Action = "reboot"
    Delete Action = "delete"
)
type Node struct {
    ID, OwnerID pgtype.UUID
    Namespace, ContainerID, DaemonID, Name, Spec, Image, ProfileRef string
    StartEpoch, DataVolume, SecretsVolume, ErrorCode, ErrorMessage string
    Desired, Status string
    Generation int64
    Ready bool
    HealthAt time.Time
    CreatedAt, UpdatedAt time.Time
    Resources Spec
    ActiveRuns, PendingReports, FailedReports int
    Maintenance bool
    Revoked bool
}
type Operation struct {
    ID, NodeID, OwnerID pgtype.UUID
    Action Action
    Phase, IdempotencyKey, RequestHash, PriorDesired string
    Generation int64
    Approved bool
    Attempts int
    CreatedAt, UpdatedAt time.Time
}
type CreateRequest struct { Name, Spec, IdempotencyKey string }
type Bootstrap struct {
    NodeToken string `json:"node_token"`
    APIKey string `json:"api_key"`
    BaseURL string `json:"base_url,omitempty"`
    Model string `json:"model,omitempty"`
    ServerURL string `json:"server_url"`
    DaemonID string `json:"daemon_id"`
}
// Operation phases: queued, preparing, prepared, applying, completed, failed.
type Observation struct {
    ContainerID, Status, DaemonID, StartEpoch string
    Ready bool
    Agents []string
    RuntimeCount, ActiveRuns, PendingReports, FailedReports int
    ReportStatsKnown bool
    Offline bool
    DataVolume, LayoutVersion string
    ObservedAt time.Time
}
type Provider interface {
    Ensure(context.Context, Node, Bootstrap) (Observation, error)
    Inspect(context.Context, Node) (Observation, error)
    Apply(context.Context, Node, Action) (Observation, error)
    Delete(context.Context, Node) error
    Diagnose(context.Context, Node, OperationRef) (Observation, error)
}
```

`model` 的错误变量：`ErrBusy`、`ErrConflict`、`ErrForbidden`、`ErrUnavailable`、`ErrUnknownHealth`、`ErrProfileMissing`、`ErrInvalidRequest`。pure helpers：`CanClaim(Node, time.Time) bool`、`CanEnqueue(Node) bool`、`ValidateCreate(CreateRequest, Config) error`。`Provider` 的 context import 属于 types 文件；所有 snippets 在所属文件补正常 imports，不引入未定义 test 框架。

`store.New(pool *pgxpool.Pool, namespace string, opts ...store.Option) *store.Store`；`store.WithMaxNodes(limit int) store.Option`，默认2，main按Config.MaxNodes注入。；`Store.WithTx(ctx, func(*db.Queries) error) error`。sqlc 锁查询的参数统一 `Namespace string, NodeID pgtype.UUID`。必须先节点锁、再 capacity 锁、再 agent/task 行锁；普通节点不创建 Fleet 状态依赖。

### Task 1: 固定 domain、配置与安全判断

**审查补充（本任务所有权与 canonical tests）：** 新增 Files: `server/internal/fleet/model/layout.go`、`server/internal/fleet/model/layout_test.go`。Produces `OperationRef{Namespace string; NodeID,OperationID pgtype.UUID; Generation int64; Action Action}`；`LayoutIdentity{Namespace,FleetID,NodeID,DaemonID string}`；固定常量 DataMount=/data、NodeHome=/data/home、WorkspacesRoot=/data/workspaces、LayoutVersion=1、LayoutManifest=/data/fleet-layout.json。pure `ValidateLayoutManifest(raw []byte,want LayoutIdentity) error` 不访问 FS。Provider 新 `Diagnose(context.Context,Node,OperationRef)(Observation,error)`，Task 4 fake 与 Task 8 实现。ReportStatsKnown 默认 false，Offline 不表示 Ready。layout_test.go 拥有 identity/schema 矩阵。Task 1 无 DB，直接 go test，不使用 env-exec。

**Dependencies:** 无。**Deliverable:** 纯逻辑可测试、固定错误/状态契约。

**Files:** Create `server/internal/fleet/model/types.go`、`policy.go`、`config.go`、`policy_test.go`、`config_test.go`。

**Interfaces:** Produces 上述 types 和 helpers；`Config` 包含 `Namespace, FleetID, Image, APIURL string`、`Specs map[string]Spec`、`MaxNodes int`；`Spec{CPUs int, MemoryBytes int64, Pids int64, MaxRuns int}`；`LoadConfig(path string) (Config,error)` 从明确文件读且拒绝未知危险字段。私密 profile 与 public config 用不同结构，不将 Bootstrap json.Marshal 到日志。

- [x] **Step 1:** 写状态矩阵，至少以下回归：

```go
func TestCanClaimBlocksMaintenance(t *testing.T) {
    now := time.Now()
    node := Node{Desired:"running", Status:"running", Ready:true, HealthAt:now}
    if !CanClaim(node, now) { t.Fatal("ready node must claim") }
    node.Maintenance = true
    if CanClaim(node, now) { t.Fatal("maintenance must block") }
}
```

另加 revoked、健康31秒、starting、missing readiness分支；pending/failed reports危险操作矩阵归Task7，不重复测试；config 拒绝未知 image/path 字段和零/负资源限制。
- [x] **Step 2:** `(cd server && go test ./internal/fleet/model -run TestCanClaim -count=1)`；预期 undefined helper 或断言 FAIL，记录原因。
- [x] **Step 3:** 实现 pure 判断，初版：

```go
func CanClaim(n Node, now time.Time) bool {
    return n.Desired == "running" && n.Status == "running" && n.Ready &&
        !n.Maintenance && !n.Revoked && !n.HealthAt.IsZero() &&
        !n.HealthAt.After(now) && now.Sub(n.HealthAt) <= 30*time.Second
}
func CanEnqueue(n Node) bool {
    return !n.Revoked && n.Desired != "terminated" && n.Desired != "terminating"
}
```

待回传结果不永久阻止普通领取，已有任务回放沿原协议处理；它们是危险操作的忙碌条件，不误加到 CanClaim 中。ValidateCreate 只允许名称和已声明 spec，默认限制复制 Global Constraints。
- [x] **Step 4:** 同一命令跑 `./internal/fleet/model` 全套，预期 PASS；表驱动测试负责枚举，界面不重复这些矩阵。
- [x] **Step 5:** 检查 diff，提交 `feat(fleet): define local node configuration and policy`，只 stage 本任务文件。

### Task 2: 同库迁移、sqlc 与数据库测试 fixture

**审查补充（本任务所有权与 canonical tests）：** 新增 Modify: `server/internal/fleet/model/types.go`：Task 1 审查通过后由 Task 2 补充 Node/Operation 的 CreatedAt、UpdatedAt time.Time，并将持久时间从 sqlc row 显式映射；供 Task 4 既有 created_at/updated_at 响应和 Task 10 有界初始化/恢复使用，不能以响应时间伪造创建时间。store_test.go 用固定 fixture 时间验证回传。新增 Modify: `server/cmd/migrate/main.go`、`server/cmd/migrate/migrate_mul5999_index_retry_test.go`；Create `server/cmd/migrate/migrate_fleet_index_retry_test.go`。564 表迁移 id/node_id/owner_id/namespace 必需字段 NOT NULL，无内联 PK/UNIQUE/FK。565–575 各 index up/down 独立单语句，up CONCURRENTLY，down DROP INDEX CONCURRENTLY IF EXISTS。每个完整 basename→public.index 登记 concurrentIndexCleanups；仅 down 重建并发索引时登记 concurrentDownIndexCleanups（Fleet down 只有 DROP，不伪造 build）。保留 TestEveryConcurrentUpBuildHasCleanup/现有 down coverage invariant，新增 TestFleetInvalidConcurrentIndexRetry 验证 INVALID 清理重建、drop 中断重跑、up/down/up。DB setup 完成后 `go test ./cmd/migrate -run 'EveryConcurrent|FleetInvalid' -count=1`；down 只在获准测试自有 schema/数据库，不回滚业务库。执行前 glob main，重编号及两方向 map 同步。

**Dependencies:** Task 1。**Deliverable:** 四张 Fleet 表、锁/查询生成代码与真实 PostgreSQL 回归。

**Files:** Create `server/pkg/db/queries/fleet.sql`、`server/internal/fleet/store/store.go`、`store_test.go`、`server/internal/testutil/fleet.go`；Modify [sqlc 配置](<../../../server/sqlc.yaml>)（仅若新 UUID/JSON 映射确有需要），生成现有 generated 目录代码。

迁移基线当前最高 563；以下拟创建 .up/.down 成对文件。实施前用 glob 确认未占用，如仓库推进只按同顺序连续重编号，不覆盖文件：

- `564_fleet_tables`：四表 DDL，无内联索引、PK/UNIQUE/FK。
- `565_fleet_nodes_id_index`、`566_fleet_nodes_daemon_index`。
- `567_fleet_operations_id_index`、`568_fleet_operations_idempotency_index`。
- `569_fleet_credentials_id_index`、`570_fleet_credentials_hash_index`。
- `571_fleet_profiles_id_index`、`572_fleet_profiles_owner_index`。
- `573_fleet_nodes_owner_index`、`574_fleet_operations_pending_index`。
- `575_fleet_runtime_node_index`：现有 agent_runtime 的受管 metadata 关联索引。

每个 index .up 只有一个并发 CREATE；.down 对称并发 DROP。id 唯一索引足以保证身份，不为新表额外引入 FK 或隐式约束索引。

**Interfaces:** `testutil.NewFleetFixture(t *testing.T) (*pgxpool.Pool,*testutil.Fixture)` 要求环境已有 DATABASE_URL，使用 f.User/Workspace/Member 建自有测试行并注册 Cleanup；不自行建库。新增 `Fixture.FleetNode(t TB, namespace string, over ...Cols) string` 创建默认 ready node。SQL 提供 `FleetNodeSharedLock`、`FleetNodeExclusiveLock`、`FleetNodeCapacityLock`、`GetFleetNode`、`ListFleetNodesByOwner`、`GetFleetNodeForRuntime`、`CountFleetActiveRuns`、`CountFleetQueuedRuns`。跨表 busy 查询覆盖所有关联工作区，不只当前 header。

- [x] **Step 1:** 在 store_test 用 fixture 检查 namespace/owner 隔离与重复 ID/幂等键，测试需要的核心形状：

```go
func TestFleetNodeOwnerIsolation(t *testing.T) {
    pool, f := testutil.NewFleetFixture(t)
    nodeID := f.FleetNode(t, "test-isolation")
    s := New(pool, "test-isolation")
    owner, err := util.ParseUUID(f.UserID)
    if err != nil { t.Fatal(err) }
    nodes, err := s.ListNodes(context.Background(), owner, 20, 0)
    if err != nil || len(nodes) != 1 || util.UUIDToString(nodes[0].ID) != nodeID {
        t.Fatalf("nodes=%v err=%v", nodes, err)
    }
}
```

`Store.ListNodes(ctx, ownerID, limit, offset) ([]model.Node,error)` 在本任务定义。增加第二个 owner 和 namespace 断言，不在测试打印秘密。
- [x] **Step 2:** `make env-exec ARGS='-- bash -c "cd server && go test ./internal/fleet/store -run TestFleetNodeOwnerIsolation -count=1"'`；迁移未有表/方法未有时 FAIL。
- [x] **Step 3:** 先建四表，所有表含 `id uuid NOT NULL DEFAULT gen_random_uuid()`、`namespace text`、`owner_id uuid`、时间字段。字段完整取自 Spec 第6节；profile 只含引用，凭证只含 hash。锁 SQL：

```sql
-- name: FleetNodeSharedLock :exec
SELECT pg_advisory_xact_lock_shared(hashtextextended(
  sqlc.arg(namespace)::text || ':' || sqlc.arg(node_id)::uuid::text, 0));
-- name: FleetNodeExclusiveLock :exec
SELECT pg_advisory_xact_lock(hashtextextended(
  sqlc.arg(namespace)::text || ':' || sqlc.arg(node_id)::uuid::text, 0));
-- name: FleetNodeCapacityLock :exec
SELECT pg_advisory_xact_lock(hashtextextended(
  'capacity:' || sqlc.arg(namespace)::text || ':' || sqlc.arg(node_id)::uuid::text, 0));
```

所有查询 owner/namespace 显式过滤，Runtime metadata 的 node ID 用文本相等而非将任意客户端 JSON 强制 UUID cast。新增索引示例：

```sql
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS fleet_nodes_id_idx ON fleet_nodes(id);
```

- [x] **Step 4:** `make sqlc`，`make env-exec ARGS='-- bash -c "cd server && go run ./cmd/migrate up && go test ./internal/fleet/store ./internal/testutil -count=1"'`。确认测试未因数据库不达被 skip；检查 pg_indexes 与无 FK。只在测试自有数据验证 down/up，不对现有业务库运行 migrate down。
- [x] **Step 5:** 提交 `feat(fleet): persist node state in existing PostgreSQL`，明确 stage 本批 migrations、fleet.sql、生成代码、store 与 fixture。

### Task 3: 创建意图、幂等与节点凭证

**执行澄清（规范优先）：** [Task 3 producer contract](<../specs/2026-10-04-local-docker-cloud-runtime-task-3-contract.md>) 定义 private profile/business owner、管理员配置注入、资源快照、独立 credential generation 与 per-owner upsert 版本协议；新增范围按该契约。此前示例 New(pool,namespace) 仅适用读取，创建测试必须注入 WithProvisioningConfig（fake image/positive declared specs）。Task6/8/9 使用持久 Node.Resources 及派生 FLEET_NODE_MAX_RUNS。

**审查补充（本任务所有权与 canonical tests）：** 新增 Modify: `server/internal/fleet/store/store_test.go`，仅加强已审查 Task 2 的 M1 总事务时限回归：预算内第一次数据库调用必须成功、仅预算后调用失败；不复制矩阵或改事务生产逻辑。新增 Modify: `server/internal/testutil/fleet.go`、`server/internal/testutil/fleet_test.go`。Task 2 产出基础 fixture/FleetNode，Task 3 产出 FleetProfile，不追溯要求 Task 2 实现。canonical TestFleetProfileFixture 使用自有 profile 引用；intents_test.go 拥有 profile 缺失/并发限额/同键重放，无真实密钥。

**Dependencies:** Tasks 1–2。**Deliverable:** 无 Docker 的原子 create/list intent 和凭证生命周期。

**Files:** Create `server/internal/fleet/store/intents.go`、`credentials.go`、`intents_test.go`、`credentials_test.go`、`server/internal/fleet/credentials.go`、`credentials_test.go`；Modify `fleet.sql` 并再生成。

**Profile/限额连接：** Task3 在 Task2 产出的 fixture 文件另增 `Fixture.FleetProfile(t TB,namespace string,over ...Cols) string`，默认owner引用test私密profile；下面TestCreateIntentReplays先 `f.FleetProfile(t,"test-replay")`。Task3 `Store.UpsertProfiles(ctx context.Context,profiles map[pgtype.UUID]string,version int64) error` 投影管理员owner→profile；Service先确认profile存在可读才调用CreateIntent。main注入WithMaxNodes(cfg.MaxNodes)，不是在浏览器设quota。

**Interfaces:** `Store.CreateIntent(ctx, ownerID, model.CreateRequest) (model.Node,model.Operation,bool,error)`，bool 为 replayed；`Store.VerifyNodeToken(ctx, token string) (model.Node,error)`；`Store.RevokeNodeToken(ctx,nodeID) error`。`fleet.LoadProfile(path string) (model.Bootstrap,error)` 读取显式文件；profile reference 由 owner 配置匹配，不能从请求任意指定文件。

- [x] **Step 1:** 写 create twice same key 得同 node/op、不同 fingerprint 409、两个并发请求不突破限额、Token DB 无明文、revoke 即失效：

```go
func TestCreateIntentReplays(t *testing.T) {
    pool, f := testutil.NewFleetFixture(t)
    owner, err := util.ParseUUID(f.UserID)
    if err != nil { t.Fatal(err) }
    f.FleetProfile(t,"test-replay")
    s := New(pool, "test-replay")
    req := model.CreateRequest{Name:"local", Spec:"local-small", IdempotencyKey:"request-1"}
    first, op1, replay1, err := s.CreateIntent(context.Background(), owner, req)
    if err != nil || replay1 { t.Fatalf("first error=%v replay=%v", err, replay1) }
    second, op2, replay2, err := s.CreateIntent(context.Background(), owner, req)
    if err != nil || !replay2 || first.ID != second.ID || op1.ID != op2.ID {
        t.Fatalf("idempotency failed err=%v replay=%v", err, replay2)
    }
}
```

- [x] **Step 2:** 跑 `go test ./internal/fleet/store -run 'TestCreateIntent|TestNodeToken' -count=1`，通过 make env-exec 的 server 工作目录；预期缺方法/断言 FAIL。
- [x] **Step 3:** WithTx 内先所有者级 advisory lock，再限额/幂等查询、节点/op 插入。Token 使用 crypto/rand 32 bytes，`mcn_` + base64url；hash 用既有 auth.HashToken。私密 bootstrap 写入前崩溃，重试撤销旧 hash 并推进 credential generation，不能反推 hash。profile 解析 `DisallowUnknownFields`、权限和 owner 校验；只保留引用/版本进 SQL。

```go
func NewNodeToken() (token,hash string,err error) {
    raw := make([]byte,32)
    if _,err = rand.Read(raw); err != nil { return "","",err }
    token = "mcn_" + base64.RawURLEncoding.EncodeToString(raw)
    return token,auth.HashToken(token),nil
}
```

Store.MintNodeToken(ctx context.Context,nodeID pgtype.UUID) (token string,generation int64,err error) 调用上述NewNodeToken，再在事务中保存hash/generation；token 返回只供内部 bootstrap，不进入公开 DTO。
- [x] **Step 4:** store/profile 全测试 PASS；扫描测试 DB、HTTP JSON、logger 捕获无 marker key/token。`make sqlc` 如查询改变。
- [x] **Step 5:** 提交 `feat(fleet): add idempotent intents and node credentials`。

### Task 4: Fleet HTTP 契约和假 Provider 服务

**Task4 审查修订契约：** NewService defensively copies Specs（Store snapshot 不被重配置）；新 create 必须先查同-key replay，再 SQL-bound GetProfile +既有 LoadProfile 在事务外检查有效可读私密文件，最后 owner-lock 下 recheck 完整 profile identity/ref/version 再创建。允许 store/intents.go 及旁边测试增加只读 `LookupCreateIntent` 与 `CreateIntentForProfile` trusted-snapshot admission seams，公共API不接受profile；共享原指纹/replay逻辑，原 CreateIntent 不做文件IO。文件丢失/权限/格式错误不写新node/op，已有matchingintent重放不因当前profile损坏而失败；并发winner也应重查replay。readyz Schema independent2s，Provider.CheckAvailability independent5s（父请求原5s并非旧plan硬性值，此处明确选择独立5s避免schema占用provider budget），均服从caller cancellation；总顺序最多7s，不扩展普通Docker/Diagnose预算。canonical helper/HTTP回归各属其层，不加liveconfig锁/回退。

**Start 所有权澄清：** Task4 的 start 与 stop/reboot/delete 一样仅接受 SQL-bound `OperationRequestDTO`（action=start、owner/ns/node/op/generation 匹配、approved=true），返回异步接受，绝不调用 Provider.Apply/创建 start 意图。instance_id-only legacystart=>409。API Task7 负责 start-intent，Task10 物理启动/恢复；本任务假Provider断言 start 通知不产生副作用。

**执行接口补充（Task4 专属，不追溯 Task3）：** Modify model/types.go，仅在 Provider 加 `CheckAvailability(context.Context) error` 第六方法；测试 fake 同时实现，Task8 只读 Engine Ping 实现，不建资源/拉镜像/读模型账户。Create store/schema.go/schema_test.go 和对应 fleet.sql/sqlc probe（如需）：`Store.CheckSchema(ctx context.Context) error` 在2s预算内检查 PG17+、四表/必需列（含576资源快照和credential generation）与查询依赖可用，不执行DDL。readyz 分开调用 Schema 与 Provider availability，nil/failure=>503，只返回不敏感状态；healthz liveness保持200。不得用空 Node 的 Inspect/Ensure 代替 availability。

**认证补充：** 所有业务路由要求非空 service secret 的 constant-time 验证。节点路由和诊断额外校验可信 X-User-ID；`/api/v1/pat/verify` 仅要求 service key（auth caller 尚未知道 owner），忽略 caller owner header并仅从 SQL凭证返回 owner，body严格 `{token}`。healthz/readyz 可匿名，但仅状态、不暴露 DB/路径/错误详情。PAT valid响应兼容既有 owner_id/instance_id/instance_record_id，instance_record_id 为Node UUID、instance_id可在初始化时为空；禁用/撤销不得从未验证 owner header 推断身份。Task5 在既有 verifier 对 local mode 注入 service key，不改变 SaaS。

**审查补充（本任务所有权与 canonical tests）：** 新增 Create: `server/internal/fleet/internal_dto.go`、`server/internal/fleet/internal_dto_test.go`。先于 Task 7/10 产出私有 `OperationRequestDTO{Namespace,NodeID,OperationID string; Generation int64; Action model.Action}`，JSON namespace/node_id/operation_id/generation/action；`DiagnosticResponseDTO{Request OperationRequestDTO; Observation model.Observation}`。Observation wire fields 为 container_id/status/daemon_id/start_epoch/ready/runtime_count/active_runs/pending_reports/failed_reports/report_stats_known/observed_at/offline/data_volume/layout_version，与公开 DTO 分离。`Service.Diagnose(ctx context.Context,ownerID pgtype.UUID,ref model.OperationRef)(model.Observation,error)` SQL 匹配 owner/namespace/node/op/generation/action 后事务外 Provider.Diagnose。路由 POST /internal/v1/nodes/diagnose，service key + trusted X-User-ID，跨 owner/namespace、错代次/action 拒绝。fake Provider 完成本任务 availability 补充后六方法；canonical TestDiagnoseOperationIdentityAndEpoch 在 http_test.go，含 future timestamp/旧 epoch/offline unknown。stop/reboot/delete 意图由 API Task 7 prepare/approve，Fleet 不重建意图。

**Dependencies:** Tasks 1–3。**Deliverable:** httptest 可验证全部公开 API、拒绝任意 exec；无需 Docker。

**Files:** Create `server/internal/fleet/service.go`、`http.go`、`http_test.go`、`service_test.go`、`fake_provider_test.go`。

**Interfaces:** `fleet.NewService(repo *store.Store, cfg model.Config, provider model.Provider) *Service`；`Service.Handler(secret []byte) http.Handler`；Provider 是上方契约。Service 不直接执行维护，stop/reboot/delete 只接受已批准 operation，未批准返回 Conflict。公开 DTO 的 `id,owner_id,instance_id,region,instance_type,image_id,subnet_id,name,status,tags,metadata,created_at,updated_at` 保留，额外 `provider,operation_id,ready,error_code` 为可选。

- [x] **Step 1:** 写无服务密钥 401、伪造 X-User-ID、越权节点、spec/image/path 拒绝和 exec 未支持：

```go
func TestServiceRequiresSecret(t *testing.T) {
    pool, _ := testutil.NewFleetFixture(t)
    svc := NewService(store.New(pool,"http-test"), model.Config{Namespace:"http-test"}, nil)
    req := httptest.NewRequest(http.MethodGet,"/api/v1/nodes",nil)
    w := httptest.NewRecorder()
    svc.Handler([]byte("test-only-service-secret")).ServeHTTP(w,req)
    if w.Code != http.StatusUnauthorized { t.Fatalf("status=%d", w.Code) }
}
```

测试 secret 字符串只用于内存 httptest，不作为生产默认。create 必须有 idempotency header，list 精确 owner/namespace 过滤。
- [x] **Step 2:** `go test ./internal/fleet -run 'TestService|TestHTTP' -count=1`；预期路由/认证未实现 FAIL。
- [x] **Step 3:** Chi 固定路由；secret 用 constant-time 比较，body 上限 1 MiB，未知字段拒绝。注册 设计第5节的 routes；healthz 存活、readyz 需 DB/schema/Provider 检查。统一 map errors 到状态码；公开响应构造独立 DTO，绝不序列化 Bootstrap。fake Provider 六方法（新增只读 availability）实现于本任务的测试文件并记录动作，不能调用用户 Docker。

```go
if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Fleet-Service-Key")), secret) != 1 {
    http.Error(w,"unauthorized",http.StatusUnauthorized)
    return
}
```

- [x] **Step 4:** 完整 httptest 行为 PASS，明确 terminated/stopped/token revoked、分页、异常 JSON 与重复操作返回码；response 字段可被现有 core schema 解析。
- [x] **Step 5:** 提交 `feat(fleet): expose authenticated local node APIs`。

### Task 5: API 本地组装、SaaS 隔离和可信节点身份

**Identity SQL seam（已批准本任务范围）：** API 没有 namespace config，PAT wire也不携带namespace；不新增默认/猜测 namespace env。按已认证 context 中 nonzero InstanceRecordID UUID+owner UUID 查当前 checkout `fleet_nodes` 单行，以 SQL persisted Namespace 为权威，再核对非revoked/terminal、当前有效 credential hash/generation 与固定 DaemonID；raw token hash 从实际已验证 bearer 产生，不能信任caller自报身份或仅检查任意有效 credential。允许 fleet.sql+对应 sqlc outputs 为该仅NodeID/OwnerID引导定位增加 query；全局NodeID unique index 作为定位前提，后续关系/写入仍 owner+SQLnamespace+node fenced。现有字段getter保留三 identity strings、命名上下文不暴露token；Task6 才加shared lock register/heartbeat admission，Task5 不声称其TOCTOU闭合。测试 cross-owner/nonexistent UUID/current-credential revoke/fixed-daemon mismatch 和 namespace来自SQL而非伪造metadata，注释英文；无新DDL/手改generated。

**审查补充（本任务所有权与 canonical tests）：** 新增 Modify: `server/internal/handler/cloud_runtime.go`、`server/internal/handler/cloud_runtime_test.go`、`server/internal/cloudruntime/client_test.go`。proxy create/start/stop/reboot/delete 仅 allowlist Idempotency-Key；本地拒绝缺失/重复/>128 bytes key，SaaS 无 key 兼容。可信 client 最后写 X-Fleet-Service-Key/X-User-ID，caller Headers 不能覆盖身份，保留 Stripe-Signature 等非身份 passthrough。canonical TestCloudRuntimeIdempotencyFullHop 在 cloud_runtime_test.go：Task5 实际 create 用真实 API router/测试认证→真实 client→httptest Fleet/Store，same-key重放/不同body409/身份不覆盖；五 actions 的 key/可信 header forwarding 在 Task5 client/proxy层验证。其余 lifecycle 的真实同-op重放/不同payload冲突在 Task7 同 canonical test 扩充为五 actions（start/maintenance producer 尚未产出，Task5 不伪造该链路或超范围创建 intent）。Task 11 client.test.ts 拥有浏览器发 key，Task 14 trace 验全跳复用，不以单跳测试代替。

**Dependencies:** Tasks 1–4。**Deliverable:** 原 API 可代理 fake Fleet，本地验证 mcn，Billing 无误启用。

**Files:** Create `server/cmd/server/local_fleet.go`、`local_fleet_test.go`、`server/internal/handler/local_fleet_identity.go`、`local_fleet_identity_test.go`；Modify [router](<../../../server/cmd/server/router.go>)、[handler](<../../../server/internal/handler/handler.go>)、[cloudruntime client](<../../../server/internal/cloudruntime/client.go>)、[cloud Token](<../../../server/internal/auth/cloud_pat.go>)、[auth middleware](<../../../server/internal/middleware/auth.go>)、[daemon auth](<../../../server/internal/middleware/daemon_auth.go>)、[daemon handler](<../../../server/internal/handler/daemon.go>)、[Billing handler](<../../../server/internal/handler/cloud_billing.go>)，各自旁边 canonical tests。

**Interfaces:** `resolveLocalFleet(cloudURL,localURL,secretFile string) (LocalFleetConfig,error)`；`LocalFleetConfig{URL string, Secret []byte, Enabled bool}`。现有 CloudPATVerifierConfig 增加 ServiceSecret []byte；本地组装 Redis:nil 禁用正向缓存，SaaS原默认不变；不新增兼容 shim。middleware 新 `CloudNodeIdentity(ctx) (auth.CloudPATIdentity,bool)` getter。注册只能按 identity.InstanceRecordID + owner + namespace 查本地 node，再核对固定 DaemonID。

- [ ] **Step 1:** 配置回归：

```go
func TestLocalFleetRejectsMixedCloud(t *testing.T) {
    _, err := resolveLocalFleet("https://cloud.example","http://127.0.0.1:19001","unused")
    if err == nil { t.Fatal("mixed modes must fail before reading secrets") }
}
```

另写本地仅构造节点/认证客户端，Billing/Entitlements/SeatCapacity 未启用；旧 SaaS 路径不变；撤销 local token 后同一缓存不能继续通过；普通 daemon 自报 managed metadata 被剥离。
- [ ] **Step 2:** `go test ./cmd/server ./internal/auth ./internal/middleware ./internal/handler -run 'TestLocalFleet|TestCloudPAT' -count=1`；预期配置分离/身份保留断言 FAIL。
- [ ] **Step 3:** resolveLocalFleet 的执行顺序先互斥、再 URL/secret 解析；secret 来源文件，private headers 在可信 client 内写，不允许转发用户提供同名 header。h.CloudRuntime 为节点专用，新增 SaaS Billing 客户端字段以保持 Billing 原行为，原 SaaS 客户端连接必须继续共享同一个真实 Cloud URL。本地 token verifier 不写正向缓存。认证 identity 存 context，不靠伪造 header。注册 metadata 用服务器值覆盖 client 管理字段，heartbeat/upsert 保留服务器管理字段。所有身份匹配失败为 401/403，不授予节点任意 owner/DaemonID。

最小配置实现（Secret必须来自明确文件；附加filemode/URLlocality检查在同文件定向tests完成）：

```go
func resolveLocalFleet(cloudURL,localURL,secretFile string) (LocalFleetConfig,error) {
    if strings.TrimSpace(cloudURL) != "" && strings.TrimSpace(localURL) != "" {
        return LocalFleetConfig{},errors.New("local Fleet and SaaS Cloud are mutually exclusive")
    }
    if strings.TrimSpace(localURL) == "" { return LocalFleetConfig{},nil }
    u,err := url.ParseRequestURI(localURL)
    if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
        return LocalFleetConfig{},errors.New("invalid local Fleet URL")
    }
    secret,err := os.ReadFile(secretFile)
    if err != nil { return LocalFleetConfig{},err }
    secret = bytes.TrimSpace(secret)
    if len(secret) < 32 { return LocalFleetConfig{},errors.New("Fleet service key must contain at least 32 bytes") }
    return LocalFleetConfig{URL:u.String(),Secret:secret,Enabled:true},nil
}
```

- [ ] **Step 4:** 上述 package 测试 PASS，特别跑已有 seat capacity/CloudPAT/cache/registry tests；sqlc 查询若新增再次生成。不能只 mock config 然后不验证 actual router assembly。
- [ ] **Step 5:** 提交 `feat(fleet): isolate local runtime integration from SaaS policies`。

### Task 6: 在真实领取/入队事务接入维护与节点容量锁

**审查补充（本任务所有权与 canonical tests）：** 新增 Modify: `server/internal/handler/daemon.go`、`server/internal/handler/daemon_test.go`、`server/pkg/db/queries/runtime.sql`；Create `server/internal/handler/daemon_fleet_merge_test.go`。`fleetguard.CheckRuntimeMerge(ctx context.Context,q *db.Queries,namespace string,sourceID,targetID pgtype.UUID) error` 在 mergeLegacyRuntime 同一 tx、任何 ReassignTasksToRuntime/ReassignAgentsToRuntime/DeleteAgentRuntime 前执行。候选两端 metadata 找所有 node UUID、排序先 node shared locks，再既有 workspace fence/runtime rows，锁后重读 source/target，任一 managed marker 拒绝（缺 node 也 fail closed）。绑定改变必须回滚重试完整 tx，不能行锁后补 node 锁。runtime.sql 锁查询返回 metadata。canonical TestFleetLegacyMergeRejectsManagedSourceOrTarget 三方向（managed→ordinary/ordinary→managed/managed→managed），tasks/agents/runtime 均未变；TestFleetLegacyMergeMaintenanceRace channels 验证不死锁/不逃逸。ordinary→ordinary 保留既有 merge/teardown fence。受管 node-first→capacity→workspace→runtime→agent/task；ordinary 原 workspace-first。

**Dependencies:** Tasks 1–2、5。**Deliverable:** 任一领取路径不能穿透屏障；跨工作区每节点最多 1 次执行。

**Files:** Create `server/internal/fleetguard/claim.go`、`claim_test.go`、`server/internal/service/task_fleet_claim_test.go`、`task_fleet_enqueue_test.go`；Modify [task.go](<../../../server/internal/service/task.go#L3498-L3555>)、[agent.sql](<../../../server/pkg/db/queries/agent.sql#L749-L798>)、[runtime.sql](<../../../server/pkg/db/queries/runtime.sql#L75-L123>)、`fleet.sql`、[daemon RPC tests](<../../../server/internal/handler/daemon_rpc_test.go>)。

**Interfaces:** `fleetguard.CheckClaim(ctx context.Context,q *db.Queries,namespace string,runtimeID pgtype.UUID,now time.Time) error`；`CheckEnqueue` 同签名。用 `GetFleetNodeForRuntime` 取得受管 node；ordinary Runtime 无受管 metadata 时直接返回 nil。metadata 有受管标记但无匹配 node 必须拒绝，不降级 ordinary。node capacity 锁与维护 shared lock 是不同锁域。

- [ ] **Step 1:** DB 回归 setup 用 Task 2 fixture：创建 ready Fleet node、两个不同工作区 Runtime、分别绑定两个 Agent、各一 queued task；并发 `ClaimTaskForRuntime` 后只有一个 dispatched。再将 maintenance=true、revoked=true、health stale，所有单/批/WS/HTTP 入口均不得 claim。测试主干：

```go
func TestClaimBarrierSeesMaintenance(t *testing.T) {
    pool, f := testutil.NewFleetFixture(t)
    node := f.FleetNode(t,"claim-gate",testutil.Cols{"maintenance":true})
    rt := f.Runtime(t,"docker",testutil.Cols{
        "metadata":json.RawMessage(fmt.Sprintf(
            "{\"managed_by\":\"local_fleet\",\"fleet_node_id\":\"%s\"}",node)),
    })
    runtimeID, err := util.ParseUUID(rt)
    if err != nil { t.Fatal(err) }
    tx, err := pool.Begin(context.Background())
    if err != nil { t.Fatal(err) }
    defer tx.Rollback(context.Background())
    err = CheckClaim(context.Background(),db.New(tx),"claim-gate",runtimeID,time.Now())
    if !errors.Is(err,model.ErrBusy) { t.Fatalf("gate error=%v",err) }
}
```

- [ ] **Step 2:** `go test ./internal/fleetguard ./internal/service ./internal/handler -run 'TestClaimBarrier|TestFleetClaim|TestFleetEnqueue' -count=1`；确认至少一个真实事务竞态测试红，不只纯 policy 测试红。
- [ ] **Step 3:** 在 s.runInTx 内，先非锁定读取候选 agent/runtime ID，取得 shared node lock，再 capacity lock，再 GetAgentForClaimUpdate；锁定后重新核验 runtime 未重绑。CountFleetActiveRuns 覆盖 `dispatched/running/waiting_local_directory` 以及 preparation lease，不仅 Agent.MaxConcurrentTasks。旧 ClaimTaskForAgent 可走同一检查，不能跳过 runtime 空参路径。

```go
if err := qtx.FleetNodeSharedLock(ctx,db.FleetNodeSharedLockParams{
    Namespace:namespace, NodeID:node.ID,
}); err != nil { return err }
if !model.CanClaim(node,time.Now()) { return model.ErrBusy }
if err := qtx.FleetNodeCapacityLock(ctx,db.FleetNodeCapacityLockParams{
    Namespace:namespace, NodeID:node.ID,
}); err != nil { return err }
```

SQL/row 数据必须在 shared lock 后重读；capacity 计数在 capacity lock 后执行。EnqueueChat/Issue/Autopilot/QuickCreate 的最终插入事务用 CheckEnqueue；停止允许排队、terminating/terminated 不允许新任务。所有 cache 空队列 fast path 不得缓存放行许可。SQL upsert 明确保留 trusted managed keys，其他客户端传入 managed keys 不能覆盖。
- [ ] **Step 4:** 再跑此矩阵及现有 claim races，`make sqlc`，`go test -race ./internal/fleetguard ./internal/service -run 'Fleet|Claim' -count=1`；预期无 double claim/死锁，ordinary Runtime 回归不查不存在的 Fleet node。验证固定锁顺序。
- [ ] **Step 5:** 提交 `feat(fleet): guard node claims and capacity transactionally`。


**登记与批量锁补充（本任务同时实现）：** `CheckRegister(ctx context.Context,q *db.Queries,namespace string,nodeID pgtype.UUID) error`，在handler注册/upsert的同一事务shared-lock后重读node；initializing允许注册，revoked/terminating/terminated拒绝，不能利用新工作区Runtime绕过屏障。多node批量先按UUID排序shared锁，再同序capacity锁，最后agent/task行锁。CheckEnqueue在delete准备态必须看到desired=terminating；AbortMaintenance按Operation.PriorDesired恢复，stop保留正常排队。WS每次claim重新查同库，不凭旧认证放行；Fleet HTTP不可达但有效健康/DB状态仍允许已有WS路径。

### Task 7: 两段式维护授权、忙碌拒绝与删除准入

**Full-hop canonical test ownership:** 本任务在既有 cloud_runtime_test.go 的 TestCloudRuntimeIdempotencyFullHop 上扩充实际五 actions，用本任务 start/maintenance producers+已批准 Task4 HTTP，真实同 key/op、异payload409；Task5 已验证 create 与五 actions headers，不重复其头传递矩阵。此测试不是 Task14 的组合 trace 替代。

**Start producer（本任务已有 API lifecycle 所有权）：** 新增 `Store.CreateStartIntent(ctx context.Context,ownerID,nodeID pgtype.UUID,key string)(model.Operation,error)` 于 store/maintenance.go；API local start 经 `Maintainer.Request(action=Start)` 路由该 producer。节点 exclusive-lock 后校验 owner/ns、幂等key（同payload/action重放，冲突409）、非revoked/terminating/terminated且无 preparing/prepared/applying/其他未完成操作；按同节点 generation CAS 原子产生 queued/approved=true/start op，将 desired=running、ready=false，保留卷、profile/resource快照。Start 不做破坏性 maintenance prepare/diagnose/取消队列，不凭未知健康解除既有屏障；stopped/failed/missing 可请求安全恢复，运行中状态或既有 intent 冲突不再创建。操作写入与 generation advancement 同短事务，无IO。API 给 Fleet Task4 发送固定 OperationRequestDTO；Task10 才物理启动。canonical test 在本任务 maintenance_test.go：并发same-key replay、preparing/terminating冲突、owned跨ns隔离、卷/快照保持与无破坏性授权IO。

**审查补充（本任务所有权与 canonical tests）：** 新增 Modify: `server/internal/handler/runtime.go`、`server/internal/handler/runtime_test.go`（含 DeleteAgentRuntime/UnbindAgentsAndDeleteRuntime）、`server/cmd/server/router.go`、`server/internal/cloudruntime/client.go`、`server/internal/cloudruntime/client_test.go`、`server/internal/handler/handler.go`。`Maintainer.Diagnose func(context.Context,model.Node,model.OperationRef)(model.Observation,error)`；`Maintainer.Review(ctx context.Context,ownerID pgtype.UUID,ref model.OperationRef)(model.Operation,error)`。prepare commit→Diagnose(ref) tx 外→第二短 tx approve；unknown 先判，保留 barrier/ErrUnknownHealth，不当明确 busy abort；只有 known nonempty 409/abort prior desired，保留数据。stopped/missing delete 只接纳匹配 DataVolume/layout/current observation 的 offline report-zero，不以 SQL idle 替代报告。

Task 7 产出 API POST /internal/local-fleet/operations/review 的专用 service auth/router（非浏览器公开接口）+ trusted owner，SQL 匹配 namespace/node/operation/generation/action/owner。`cloudruntime.Client.DiagnoseNode(ctx context.Context,ownerID string,ref model.OperationRef)(model.Observation,error)` 与 `ReviewOperation(ctx context.Context,ownerID string,ref model.OperationRef)(model.Operation,error)` 使用固定 route/Task 4 DTO，service secret 不来自 caller。CAS 只更新同 preparing op/generation；无 HTTP/Docker in tx。canonical local_fleet_operations_test.go: TestFleetReviewSameOperationCAS/TestFleetOfflineDeleteProof/tx-free diagnose；runtime_test.go: TestManagedRuntimeDeleteAndUnbindRejected 拦旧客户端直接绕过且保留业务实体。批准后恢复原操作，start 与 preparing/terminating 冲突。

**Dependencies:** Tasks 1–6。**Deliverable:** fake Provider 测试证明 stop/reboot/delete 无检查后领取竞态，崩溃保持屏障。

**Files:** Create `server/internal/fleetguard/maintenance.go`、`maintenance_test.go`、`server/internal/fleet/store/maintenance.go`、`maintenance_test.go`、`server/internal/handler/local_fleet_operations.go`、`local_fleet_operations_test.go`；Modify `fleet.sql`、[Cloud handler](<../../../server/internal/handler/cloud_runtime.go>)。

**Interfaces:** `store.PrepareMaintenance(ctx,ownerID,nodeID,action,key) (model.Operation,error)`、`ApproveMaintenance(ctx,operationID,generation) error`、`AbortMaintenance(ctx,operationID,generation) error`。`fleetguard.Maintainer{Repo *store.Store, Diagnose func(context.Context,model.Node,model.OperationRef)(model.Observation,error)}`；`Maintainer.Request(ctx,ownerID,nodeID,action,key) (model.Operation,error)`。Diagnose 由 Task 4 的固定内部检查接口提供，测试用 function literal，无网络数据库事务混合。

- [ ] **Step 1:** active SQL row 拒绝、pending/failed reports 拒绝、未知 health 保留未批准屏障、删除 queued 拒绝、维护后 new enqueue 拒绝、两段之间 API crash 可重试：

```go
func TestReportsBlockMaintenance(t *testing.T) {
    obs := model.Observation{Status:"running",Ready:true,ReportStatsKnown:true,PendingReports:1}
    if !BusyObservation(obs) { t.Fatal("pending terminal report must block stop") }
    obs.PendingReports = 0
    obs.FailedReports = 1
    if !BusyObservation(obs) { t.Fatal("failed report must require attention") }
}
```

`BusyObservation(model.Observation) bool` 是本任务 helper，DB races 使用 Task 2 fixture 和 barriers/channels，不用 sleep 推测执行顺序。
- [ ] **Step 2:** `go test ./internal/fleetguard ./internal/handler -run 'TestReportsBlock|TestFleetMaintenance|TestFleetDelete' -count=1`；预期维护逻辑/竞态断言 FAIL。
- [ ] **Step 3:** 第一短事务 exclusive node lock、插入 preparing op、设置 maintenance=true、核验 SQL active/queued；busy 回滚。事务外固定诊断；明确 busy 在短事务 abort、返回 409，unknown health 保留 barrier/error。第二短事务重新取 exclusive lock，按 operation ID/generation CAS、重新核验 SQL active/queued，设置 approved=true。实现：

```go
func BusyObservation(o model.Observation) bool {
    return !o.ReportStatsKnown || o.ActiveRuns > 0 || o.PendingReports > 0 || o.FailedReports > 0
}
```

stop/reboot 不提前撤销 Token，保持结果回调；delete 批准后才 revoke，并在节点状态标记 terminating 防止新认证/准入。stopped/missing container 删除仍须 SQL 无活跃/排队及可信离线 data volume report-zero 证明，缺失/错误卷不是空卷；running container 必须有可信本代次诊断，不能沿用上一轮重启的健康。API 重审只处理同一未批准 op，服务密钥认证且 namespace 过滤。
- [ ] **Step 4:** race suite PASS，断言诊断执行时 SQL tx 已释放；模拟 second tx/approval crash，Worker 不执行未批准 delete。普通 Runtime 删除 handler 对 managed nodes 返回冲突，不静默删除。
- [ ] **Step 5:** 提交 `feat(fleet): authorize safe node maintenance and deletion`。

### Task 8: Docker Engine Provider 与资源归属

**审查补充（本任务所有权与 canonical tests）：** 新增 Create: `server/internal/fleet/docker/engine.go`、`server/internal/fleet/docker/engine_test.go`、`server/internal/fleet/docker/offline_reports.go`、`server/internal/fleet/docker/offline_reports_test.go`。Engine 新 `FixedOfflineReports(context.Context,model.Node,model.OperationRef)([]byte,error)`，无 argv/path/image override；Provider.Diagnose 实现 Task 1 seam。固定 cfg.Image approved digest；helper argv=[/usr/local/bin/fleet-node,report-stats]，只读 SQL DataVolume→/data，无 secrets/socket/network，UID/GID10001、read-only rootfs、drop ALL、no-new-privileges、0.25 CPU/64MiB/16PIDs、5s/64KiB。校验标签/资源 ID/layout manifest 和无活跃 writer；role=diagnostic，清理只自身。helper 输出非私密 manifest identity/layout 和 counts；Fleet 将其与 SQL 预期 namespace/FleetID/node/DaemonID 比较后才绑定 ref/DataVolume/current observation，不信任 helper 自报任意身份/时间，不把自身 manifest 当 expected identity。Delete 前再校验静止状态/proof，unknown/nonempty 保留 data。canonical TestOfflineReportsWrongMountNeverZero/TestOfflineHelperHasNoCredentialsOrNetwork/TestOfflineReportsUnknownPreservesData 在 offline_reports_test.go；Engine HTTP mock 验 create/wait/log bounds/cleanup，无 Docker。

动态节点 `NodeHostConfig(spec model.Spec,linuxHostGateway bool) container.HostConfig` 明确 Linux ExtraHosts=[host.docker.internal:host-gateway]，不假定 Compose 继承；仅节点 network，无 PG network/凭证，resource tests 包含回归。

**Dependencies:** Tasks 1–4、7。**Deliverable:** SDK fake HTTP 能验证资源安全配置，实际 Docker 未授权时不执行。

**Files:** Create `server/internal/fleet/docker/provider.go`、`bootstrap.go`、`inspect.go`、`provider_test.go`、`inspect_test.go`；Modify [Go dependencies](<../../../server/go.mod>) 和对应 sum。

**Interfaces:** `docker.New(engine Engine,cfg model.Config) *Provider`；Engine 是本任务对实际 SDK 的窄接口，方法限 create/inspect/start/stop/remove、volumes、network、copy 固定 bootstrap tar、固定 health 执行。Provider 满足 `model.Provider`；不向业务 HTTP 暴露 Engine/exec。


SDK窄adapter构造函数为 `NewEngine(client *client.Client) Engine`，返回实现下述接口的SDK adapter；其精确契约（放新文件 `server/internal/fleet/docker/engine.go`，同目录 `engine_test.go`；只此adapter调用所选并锁定的SDK）：

```go
type Resource struct { ID,Name,Role string; Labels map[string]string }
type Inspection struct {
    ID,State,StartedAt string
    Labels map[string]string
    HealthJSON []byte
}
type Engine interface {
    Find(context.Context,map[string]string) ([]Resource,error)
    Inspect(context.Context,string) (Inspection,error)
    EnsureNetwork(context.Context,Resource) error
    EnsureVolume(context.Context,Resource) error
    Create(context.Context,*container.Config,*container.HostConfig,string,string) (string,error)
    InstallBootstrap(context.Context,[]Resource,[]byte) error
    Start(context.Context,string) error
    Stop(context.Context,string) error
    Remove(context.Context,string) error
    RemoveVolume(context.Context,Resource) error
    FixedHealth(context.Context,string) ([]byte,error)
}
```

Create最后两参数为networkname/containername；InstallBootstrap只能接收本node已校验自有卷和固定bootstrap tar；FixedHealth只执行fleet-node health，無callercommand参数。Inspection.HealthJSON映射SDKhealth输出，ObservedAt由Fleet当前clock写，绝不接受任意futuretimestamp。volume名在CreateIntent持久化DataVolume/SecretsVolume，重试检查名字+全部labels+SQLowner。网络仅按namespace创建，删节点不删其他node仍使用的网络。

- [ ] **Step 1:** mock Docker HTTP Transport 检查 create Config，测试纯 resource builder：

```go
func TestNodeHostConfigIsRestricted(t *testing.T) {
    cfg := NodeHostConfig(model.Spec{CPUs:2,MemoryBytes:4<<30,Pids:256,MaxRuns:1},true)
    if cfg.Privileged || cfg.NetworkMode == "host" || cfg.PidMode == "host" {
        t.Fatal("unsafe container isolation")
    }
    if cfg.Memory != 4<<30 || len(cfg.PortBindings) != 0 { t.Fatal("resource/port mismatch") }
}
```

`NodeHostConfig(model.Spec,bool) container.HostConfig` 本任务定义；SDK package 路径按所选 SDK 版本固定，不能混用新/旧 module imports。断言 nonroot user、socket 不在 mounts、restart=no、labels 包含 namespace/node/role、secrets 只读。
- [ ] **Step 2:** `go test ./internal/fleet/docker -run 'TestNodeHostConfig|TestOwnership|TestInspect' -count=1`；预期缺 builder/错误默认配置 FAIL，不访问 socket。
- [ ] **Step 3:** 固定 image/tag/network/volumes 从 cfg + Node UUID 生成。Ensure 先 inspect labels/container identity，重复相同 node 不重复 create；一致才 adopt。Delete 在每次资源移除前校验 Store ID 与全套 labels；遇其他 namespace 返回 Forbidden，缺失自有资源视幂等成功。bootstrap 通过 fixed init container/tar 写 volumes，secret 不放 Config.Env。健康用固定 `fleet-node health` 命令，任意 cmd/body 禁止进入 exec；parse json 校验 DaemonID 和 start epoch。

```go
func Owns(labels map[string]string,namespace,fleetID,nodeID,role string) bool {
    return labels["multica.fleet.fleet_id"] == fleetID &&
        labels["multica.fleet.namespace"] == namespace &&
        labels["multica.fleet.node"] == nodeID && labels["multica.fleet.role"] == role
}
```

同时要匹配实际 resource ID，不能只凭标签删除。每个 Engine 调用独立 timeout，retry 前 inspect，不直接重跑 create。
- [ ] **Step 4:** fake Engine tests PASS，覆盖 timeout-after-create、foreign resource、already stopped/removed、unknown health、resource labels 不全。Docker SDK 版本在 go.mod 精确锁定；不得新增 shell docker 管理节点。
- [ ] **Step 5:** 提交 `feat(fleet): manage isolated nodes through Docker Engine`。

### Task 9: 节点 bootstrap、固定 Claude 镜像与假 CLI

**审查补充（本任务所有权与 canonical tests）：** 新增 Modify: `server/internal/daemon/health.go`、`server/internal/daemon/health_test.go`、`server/internal/daemon/terminal_report_queue.go`、`server/internal/daemon/terminal_report_queue_test.go`；Create `server/internal/daemon/report_queue_stats.go`、`server/internal/daemon/report_queue_stats_test.go`、`server/cmd/fleet-node/report_stats.go`、`server/cmd/fleet-node/report_stats_test.go`。`daemon.ReportQueueStats{Known bool; Pending,Failed int}` JSON known/pending/failed；`daemon.ScanReportQueueStats(workspacesRoot string)(ReportQueueStats,error)` 只读复用 terminalReportDirectoryStats/otherNamespaceStats 的 traversal 规则，所有 durable namespace+failed，不初始化 store、不创建目录/不 replay。扫描错误 Known=false；零只在已验证根中缺 report 子目录。

HealthResponse 新 ReportQueueStats 字段 JSON report_queue_stats（非 omitempty），不改变旧 health HTTP200/status/counters。canonical TestReportQueueStatsAllNamespacesAndFailed/TestReportQueueStatsReadErrorUnknown 在 scanner tests；TestHealthReportStatsErrorPreservesLivenessAndLegacyCounters 在 health_test.go。ReadObservation 只消费新 stats，旧/畸形 stats 默认 ReportStatsKnown=false，fleet-node health_test.go 拥有 decoding 回归。

run/health/report-stats 均验证同一 manifest/Task 1 ValidateLayoutManifest，run 显式 --workspaces-root /data/workspaces；`ReadOfflineReportStats() (daemon.ReportQueueStats,error)` 仅固定路径，无 secret/profile/path 参数。canonical TestOfflineReportStatsMatchesOnlineLayout/TestOfflineMissingManifestIsUnknown 覆盖错 mount、missing/symlink 根、只读扫描不启动 Daemon/Claude。bootstrap 写 manifest 后才健康。registry 步骤仍待独立许可。

**Dependencies:** Tasks 1、3、5、8。**Deliverable:** nonroot 镜像仅含 Claude，可用 fake CLI 测试 config/health；不调用真实模型。

**Files:** Create `server/cmd/fleet-node/main.go`、`bootstrap.go`、`bootstrap_test.go`、`health.go`、`health_test.go`、`docker/runtime/Dockerfile`、`docker/runtime/claude-version.txt`、`docker/runtime/Dockerfile.test`、`server/internal/fleet/integration/testdata/claude`。

**Interfaces:** `BootstrapFiles(home,secretDir string) (cli.CLIConfig,error)`；`ReadObservation(client *http.Client,healthURL string) (model.Observation,error)`；固定 CLI 子命令 `fleet-node run|health|bootstrap|report-stats`。使用现有 `cli.SaveCLIConfigForProfile(cfg, "")` 写默认profile，Token/ServerURL存CLIConfig；固定DaemonID保留在bootstrap并用--daemon-id传入，设置HOME为节点data volume。fake CLI 的版本检查和 stream-json 输出必须匹配现有 Claude backend，不把所有 stdout 当作聊天结果。

- [ ] **Step 1:** TempDir 写测试专用 secret 文件，证明仅文件注入配置：

```go
func TestBootstrapDoesNotNeedMulticaTokenEnv(t *testing.T) {
    home, secrets := t.TempDir(), t.TempDir()
    t.Setenv("MULTICA_TOKEN","")
    raw := []byte("{\"node_token\":\"mcn_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\",\"server_url\":\"http://api.test\",\"daemon_id\":\"00000000-0000-4000-8000-000000000003\",\"api_key\":\"unit-test-only\"}")
    if err := os.WriteFile(filepath.Join(secrets,"bootstrap.json"),raw,0600); err != nil { t.Fatal(err) }
    cfg, err := BootstrapFiles(home,secrets)
    if err != nil || cfg.Token != "mcn_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" { t.Fatalf("bootstrap error=%v",err) }
}
```

仅测试写这些 marker secrets；production 必须验证真实 mcn 格式/Daemon UUID。增加 owner/profile mismatch、现有 node data 保留、file mode、logs redacted。
- [ ] **Step 2:** `go test ./cmd/fleet-node -count=1`，预期 config 初始化/health decode FAIL。
- [ ] **Step 3:** 固定 go helper 读文件、写 CLIConfig，进程内部设置 `ANTHROPIC_API_KEY`、可选 `ANTHROPIC_BASE_URL`、default model，然后 `syscall.Exec` 现有 multica daemon foreground，flags `--no-auto-update --no-auto-reload --max-concurrent-tasks 1`，并以独立argv对传 `--daemon-id` 与Bootstrap.DaemonID（UUID公开身份，不传Token）。HTTP health 默认容器 loopback 19514；使用现有 `daemon.HealthResponse` 字段映射 ActiveTaskCount/Agents/Workspaces；维护 counts 只取新增 ReportQueueStats（known/pending/failed），不从旧默认零计数推断安全，Ready 判 Status=running、claude 已注册及 RuntimeCount>0。不要编造 ready_for_task_claims JSON 字段。

镜像代码关键结构：

```dockerfile
FROM golang:1.26.6 AS builder
WORKDIR /src
COPY server/ ./
RUN CGO_ENABLED=0 go build -o /out/multica ./cmd/multica && CGO_ENABLED=0 go build -o /out/fleet-node ./cmd/fleet-node
FROM node:22-bookworm-slim
ARG CLAUDE_CODE_VERSION
RUN test -n "$CLAUDE_CODE_VERSION" && npm install -g "@anthropic-ai/claude-code@$CLAUDE_CODE_VERSION"
RUN useradd --uid 10001 --create-home runtime
COPY --from=builder /out/ /usr/local/bin/
USER 10001:10001
ENV HOME=/data/home
ENTRYPOINT ["fleet-node","run"]
HEALTHCHECK --interval=5s --timeout=5s CMD ["fleet-node","health"]
```

当前go.mod为1.26.6，镜像builder固定同版本；实施时如Go版本变更，按同一提交更新两处，不能降级；Linux image 同时支持 amd64/arm64。增加 git、CA、必要 CLI 运行依赖，不安装其他 Agent。Task 的版本锁步骤是 `npm view @anthropic-ai/claude-code version` 只查 registry，不执行用户 CLI/账户，将返回的精确版本写入 claude-version.txt；build 只消费该文件，不接受 latest。真实 npm CLI 安装只发生在显式 image build，不进入默认测试。Dockerfile.test 改用 testdata 假 claude，绝不安装真实包。
- [ ] **Step 4:** 全 bootstrap/health 测试 PASS，假 CLI backend 专项 PASS。镜像 build 属于 Task 14 opt-in；未获 Docker 许可只检查 Dockerfile/config builder，不声称镜像运行通过。
- [ ] **Step 5:** 提交 `feat(fleet): bootstrap persistent Claude-only runtime images`。

### Task 10: Worker、恢复协调与独立 Fleet 程序

**审查补充（本任务所有权与 canonical tests）：** 消费 Task 4 Diagnose DTO/Provider seam + Task 7 ReviewOperation route/client，不另造许可。未批准 op Tick 在 tx 外 ReviewOperation(owner,ref)，重用原 op ID/generation/action，只 CAS 写；unknown 保留屏障，不凭超时推进。RecordObservation 持久化 ReportStatsKnown/offline/layout/DataVolume，拒绝 future time、旧 epoch/错误卷；offline 不发布 running/Ready。canonical TestRecoveryReviewUsesSameOperation/TestRecoveryUnknownOfflinePreservesBarrier 于 reconciler_test.go；delete 恢复需原 op approved 且 proof/current identity 匹配。

**Dependencies:** Tasks 1–9。**Deliverable:** 程序组合、async create、approved maintenance、crash recovery；默认 fake Engine 可验证。

**Files:** Create `server/internal/fleet/reconciler.go`、`reconciler_test.go`、`server/internal/fleet/store/recovery.go`、`recovery_test.go`、`server/cmd/fleet/main.go`、`main_test.go`、`docker/fleet/Dockerfile`；Modify service/http、fleet.sql。

**Interfaces:** `fleet.NewReconciler(repo *store.Store,p model.Provider,cfg model.Config) *Reconciler`；`Reconciler.Tick(ctx) error`、`Run(ctx) error`；`Store.ListRecoverable(ctx) ([]model.Operation,error)`、`Store.RecordObservation(ctx context.Context,nodeID pgtype.UUID,generation int64,observation model.Observation) error`、`FinishDelete(ctx,operationID,generation) error`。main 组装 DB/Schema check、Provider、Service/Reconciler；仅 readyz 检测，不自行跑 migrate。

- [ ] **Step 1:** fake Provider 的 Action log 测试：重复 Tick 不重复 Ensure、unapproved delete 从不调用 Delete、delete after crash 恢复同 op、unknown Docker 不擦 SQL、健康 Ready 只在正确 epoch。纯 guard：

```go
func TestOperationRequiresApproval(t *testing.T) {
    op := model.Operation{Action:model.Delete,Phase:"prepared",Approved:false}
    if CanApplyOperation(op) { t.Fatal("unapproved delete cannot execute") }
    op.Approved = true
    if !CanApplyOperation(op) { t.Fatal("approved delete must be recoverable") }
}
```

`CanApplyOperation(model.Operation) bool` 在 reconciler.go 定义：create/start 不要求 maintenance approval，stop/reboot/delete 必须 approved 且代次匹配。
- [ ] **Step 2:** `go test ./internal/fleet ./internal/fleet/store ./cmd/fleet -run 'TestOperation|TestRecovery|TestFleetMain' -count=1`；预期组合/恢复规则 FAIL。
- [ ] **Step 3:** operation CAS 领取 → tx 外 Engine 动作 → 短 tx 记录结果。容器健康过期30秒，失败仍保存 error/barrier。默认周期5s、重试5次、初始化5min；永久配置/归属错误停止重试，显式 retry 复用原 op。先 revoke/准入关闭，再删除 resources，再 SQL tombstone/offline Runtime；不删除 Agent/Issue。startup reconcile namespace-owned资源，foreign标签拒绝，Docker/DB故障 failclosed，新Docker动作只有DB已记录授权才开始。

```go
func CanApplyOperation(op model.Operation) bool {
    if op.Phase != "queued" && op.Phase != "prepared" && op.Phase != "applying" { return false }
    switch op.Action {
    case model.Create, model.Start: return true
    case model.Stop, model.Reboot, model.Delete: return op.Approved
    default: return false
    }
}
```

main 信号取消 Run，停止接受新管理操作，不意外删除节点。Fleet 启动不建新数据库/迁移/volume；pool budget 与默认 API pool一起控制。固定诊断/review HTTP 不得在 tx 内调用；恢复检查使用已有 Task7 Maintainer协议。
- [ ] **Step 4:** tests PASS，`go test -race ./internal/fleet/... ./internal/fleetguard/... -count=1`；构建 `go build ./cmd/fleet ./cmd/fleet-node`。核对 HTTP 仅 loopback映射，服务密钥不进 logs，sqlite dependency不存在。
- [ ] **Step 5:** 提交 `feat(fleet): reconcile durable Docker node operations`。


**异常身份不重建：** create仅在没有已确认container身份时Ensure；若已记录containerID的容器被外部删除，标记实例丢失，不创建替代容器。Create失败撤销Token，只清理由该op新建且归属明确的卷，不删已有卷。Worker使用自己的context，浏览器断开不取消已接受意图。服务secret只在API/Fleet，不进node；NodeToken不进入模型env/argv/prompt，保留既有每任务mat_配置路径。

### Task 11: core 能力、响应和生命周期 mutations

**审查补充（本任务所有权与 canonical tests）：** 新增 Create `packages/core/runtimes/managed-fleet-runtime.ts`、`packages/core/runtimes/managed-fleet-runtime.test.ts`。产出 `getManagedFleetNodeID(runtime:AgentRuntime):string|null`，runtime metadata zod 校验，缺失/畸形 null；canonical DOM-free tests 首行 node 环境。Task 12 只消费。浏览器五 action 每意图固定 key、retry 不生成新 key；client.test.ts 表驱动验证 Idempotency-Key create/start/stop/reboot/delete，不发 owner/service key。wire snake_case 在 client 转 camelCase。

**Dependencies:** Task4（最终对接在Task10后）。
**Files:** Modify [client.ts](<../../../packages/core/api/client.ts#L2014-L2055>)、[client.test.ts](<../../../packages/core/api/client.test.ts#L1523-L1580>)、[schemas.ts](<../../../packages/core/api/schemas.ts#L1643-L1677>)、[schemas.test.ts](<../../../packages/core/api/schemas.test.ts>)、[cloud-runtime.ts](<../../../packages/core/runtimes/cloud-runtime.ts>)；Create `packages/core/runtimes/cloud-runtime.test.ts`、`cloud-runtime-capabilities.ts`、`cloud-runtime-capabilities.test.ts`。

**Interfaces — Consumes:** Task4 DTO/routes。**Produces:**

```ts
export type NodeAction = "create"|"start"|"stop"|"reboot"|"delete";
export interface CloudRuntimeCapabilities {
  provider:"docker"|"cloud"|"unknown";
  operations:NodeAction[];
  specs:Array<{id:string;cpus:number;memoryBytes:number;pids:number}>;
  persistentStorage:boolean;
  diskQuotaSupported:boolean;
}
export interface CreateDockerNodeRequest {name?:string;spec:string}
```

API方法 `getCloudRuntimeCapabilities():Promise<CloudRuntimeCapabilities>`；`createCloudRuntimeNode(data:CreateCloudRuntimeNodeRequest|CreateDockerNodeRequest,idempotencyKey?:string):Promise<CloudRuntimeNode>`；`startCloudRuntimeNode(instanceId:string,key:string)`、stop/reboot同签名，返回Promise<CloudRuntimeNode>；delete保留Promise<void>、增加可选key。`cloudRuntimeCapabilityOptions(wsId:string)` 及 `useStartCloudRuntimeNode(wsId:string)`、stop/reboot同模式。旧EC2 request保留，不新增内部shim。

- [ ] **Step 1:** DOM-free tests首行node环境；错误能力/响应：

```ts
it("unknown provider disables operations", () => {
  const caps = parseWithFallback({provider:"future",operations:["delete"]},
    CloudRuntimeCapabilitiesSchema,EMPTY_CLOUD_RUNTIME_CAPABILITIES,
    {endpoint:"GET /api/cloud-runtime/"});
  expect(caps.provider).toBe("unknown");
  expect(caps.operations).toEqual([]);
});
```

另测hosted有效字段不变、malformedcreate显式失败、retry同key、不同意图不同key、不发servicekey。
- [ ] **Step 2:** `pnpm --filter @multica/core test -- api/client.test.ts api/schemas.test.ts runtimes/cloud-runtime-capabilities.test.ts runtimes/cloud-runtime.test.ts`；预期schema/method缺失FAIL。
- [ ] **Step 3:** zod校验numericlimits/optionaldiagnostics，unknownprovider清空operations；criticalcreate/action解析EMPTY_NODE须throw，list安全emptyfallback。实现：

```ts
export const EMPTY_CLOUD_RUNTIME_CAPABILITIES: CloudRuntimeCapabilities = {
  provider:"unknown",operations:[],specs:[],persistentStorage:false,diskQuotaSupported:false,
};
export function supportsNodeAction(caps:CloudRuntimeCapabilities,action:NodeAction):boolean {
  return caps.provider !== "unknown" && caps.operations.includes(action);
}
```

Lifecycle body `{instance_id}`，Mutation onSettled invalidate节点/Runtimequery，无optimisticremove；account节点key accountscope，wsId只给Runtime投影失效。core无process.env/storage/UI；Node新增provider/ready/op/error为optionaldefaults。schema边界生成受管metadata类型，不cast。
- [ ] **Step 4:** 定向tests PASS、`pnpm --filter @multica/core typecheck`；valid hosted旧tests继续PASS，malformedcreation转显式失败。
- [ ] **Step 5:** scoped commit `feat(fleet): add compatible client capabilities and node actions`。

### Task 12: Web/Desktop 共享节点 UI

**审查补充（本任务所有权与 canonical tests）：** 仅消费 Task 11 managed-fleet-runtime.ts helper，不修改 core source；组件 tests 验 managed delete 导向节点管理而非 runtime delete/unbind。Task 7 后端保护权威，无模型选择器，使用提供默认模型。

**Dependencies:** Tasks5、7、11。
**Files:** Modify [cloud-runtime-dialog](<../../../packages/views/runtimes/components/cloud-runtime-dialog.tsx>)、[runtimes-page](<../../../packages/views/runtimes/components/runtimes-page.tsx>)、[delete-runtime-dialog](<../../../packages/views/runtimes/components/delete-runtime-dialog.tsx>)、[runtime-list:548](<../../../packages/views/runtimes/components/runtime-list.tsx#L548>) 和 [row-menu tests](<../../../packages/views/runtimes/components/runtime-row-menu.test.tsx>)、[Web route](<../../../apps/web/app/%5BworkspaceSlug%5D/%28dashboard%29/runtimes/page.tsx>)、[Desktop wrapper](<../../../apps/desktop/src/renderer/src/components/desktop-runtimes-page.tsx>)；Create `packages/views/runtimes/components/cloud-node-actions.tsx`、`cloud-node-actions.test.tsx`、`cloud-runtime-dialog.test.tsx`，修改existingdelete-runtime test。

同步 [en](<../../../packages/views/locales/en/runtimes.json>)、[zh-Hans](<../../../packages/views/locales/zh-Hans/runtimes.json>)、[fr](<../../../packages/views/locales/fr/runtimes.json>)、[ja](<../../../packages/views/locales/ja/runtimes.json>)、[ko](<../../../packages/views/locales/ko/runtimes.json>)。先读 [英文约定](<../../../apps/docs/content/docs/developers/conventions.mdx>)、[中文约定](<../../../apps/docs/content/docs/developers/conventions.zh.mdx>)、[Button](<../../../packages/ui/docs/button.md>)、[Dialog](<../../../packages/ui/docs/dialog.md>)。

**Interfaces — Consumes:** Task11 caps/node/hooks。**Produces:** `CloudNodeActions({node,capabilities,wsId,onDeleted})`，props类型CloudRuntimeNode/CloudRuntimeCapabilities/string/()=>void；消费 Task 11 产出的 core `getManagedFleetNodeID(runtime:AgentRuntime):string|null`，不在本任务创建 core helper。平台只注入navigation/plumbing。

- [ ] **Step 1:** QueryClient/i18n沿用views测试setup、API mock在core/api；namedregression：

```tsx
it("requires confirmation before deletion", async () => {
  const user = userEvent.setup();
  render(<CloudNodeActions node={testNode} capabilities={testCaps} wsId="ws-test" onDeleted={vi.fn()} />);
  await user.click(screen.getByRole("button",{name:"Delete node"}));
  expect(screen.getByRole("alertdialog")).toBeInTheDocument();
  expect(api.deleteCloudRuntimeNode).not.toHaveBeenCalled();
});
```

testNode和testCaps在本文件顶部用Task4完整DTO/Task11capsschemaparse声明；testCaps为docker、local-small规格、全部5actions、persistenttrue/diskquotafalse。再测busy错误不remove/navigate、managedordinarydelete不开直接删除接口。
- [ ] **Step 2:** `pnpm --filter @multica/views test -- runtimes/components/cloud-runtime-dialog.test.tsx runtimes/components/cloud-node-actions.test.tsx runtimes/components/delete-runtime-dialog.test.tsx`；预期component/wiring缺失FAIL。
- [ ] **Step 3:** Docker仅名称/spec，Cloud旧字段保留；servercaps驱动两平台：

```tsx
const caps = useQuery(cloudRuntimeCapabilityOptions(wsId));
const local = caps.data?.provider === "docker" && supportsNodeAction(caps.data,"create");
const showCloudEntry = cloudRuntimeEnabled || local;
```

action英文Create node/Start node/Stop node/Restart node/Delete node，中译遵循节点/运行时/智能体。delete确认卷/会话/目录不可恢复；busy/unknown/profilemissing给恢复指引，不声称key已认证。sameintentkey局部draft持有直到成功，重复按钮pending禁用。受管普通delete/restart导节点管理，后端仍拒绝绕过。长名/hover/accessibility用已有primitives，无newstore/mobile改动。
- [ ] **Step 4:** tests、views typecheck、Web/Desktop窄typecheck PASS；react-best-practices审查多TSX，5种翻译key一致，views不mock框架routing。
- [ ] **Step 5:** scoped commit `feat(fleet): manage Docker nodes in shared runtime views`。

### Task 13: 当前 checkout 可选 Fleet 环境组件

**审查补充（本任务所有权与 canonical tests）：** baseline docker-compose.yml name=multica/service=postgres、内部 alias postgres/5432、只发布 loopback。prepare 以授权后的 Engine inspection/managed manifest 校验既有容器 ID/network ID/aliases/port 和 DB 来源，传 external network 名（例如经确认的 multica_default，不凭名字猜）。Fleet-only DB URL 保留 worktree DB/username/password/query options、替换 host/port，API URL 不变。找不到匹配来源（含 native PG）配置失败，不第二 DB/公开监听。Compose fleet 加 external shared PG network 及节点 network；动态节点 Task 8 只进节点 network。fleet-env.test.sh canonical TestSharedPGNetworkURLPreservesDatabaseAndOptions/TestLinuxLoopbackPGIsNotGateway/TestNodesDoNotJoinPGNetwork，仅 stub inspect fixtures；dynamic ExtraHosts 归 Task 8，Compose extra_hosts 只影响 Fleet。

**Dependencies:** Tasks5、7–10。
**Files:** Create `docker-compose.fleet.yml`、`scripts/fleet-env.sh`、`fleet-env.test.sh`、`fleet-config.example.json`；Modify [dev-env.sh](<../../../scripts/dev-env.sh#L42-L43>)、[dev-env.test.sh](<../../../scripts/dev-env.test.sh>)、[Makefile](<../../../Makefile#L157-L176>)、[env模板](<../../../.env.example>)、[gitignore](<../../../.gitignore>)、[AGENTS](<../../../AGENTS.md>)。

**Interfaces — Consumes:** Task7维护API、Task10readyz/cmd。**Produces:** `fleet-env.sh prepare|up|status|quiesce|down|destroy`，dev-env传绝对repo/env/registryID/namespace/port。prepare 用 URL parser 仅将 Fleet DB URL 的 host/port 替换为已验证的共享 PG Docker 网络内部 alias/port，dbname/凭证/query options 不变，API URL 不变；privatefiles600。quiesce全部nodes屏障，busy/unknown在关API前中止；destroy失败保留DB/registry用于恢复。

- [ ] **Step 1:** existingdev-envfakePATHpattern写hermetic测试，stubdocker/http只记动作；assertdefault仍api/web，fleet可选，URL保留当前DBname，busy不shutdownAPI，资源失败不dropDB。实际testURL：

```sh
export DATABASE_URL='postgres://test:test@localhost:15432/worktree_test'
bash scripts/fleet-env.test.sh
```

测试完整privateprofile/image/owner/namespace在TempDir；不能引入production认证bypass。
- [ ] **Step 2:** `bash scripts/fleet-env.test.sh`、`bash scripts/dev-env.test.sh`；unknowncomponent/cleanup次序FAIL，无真实Engine。
- [ ] **Step 3:** migrate_database先于API/Fleet；Compose仅fleet、無DB服务/DB卷，登记实际allocatedport。合法片段：

```yaml
services:
  fleet:
    build:
      context: .
      dockerfile: docker/fleet/Dockerfile
    ports:
      - "127.0.0.1:${MULTICA_LOCAL_FLEET_PORT:?Fleet port required}:8080"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - "${MULTICA_FLEET_CONFIG_DIR:?Private config required}:/run/multica-fleet:ro"
    extra_hosts:
      - "host.docker.internal:host-gateway"
    networks:
      - shared-pg
      - fleet-nodes
    restart: unless-stopped
networks:
  shared-pg:
    external: true
    name: "${MULTICA_SHARED_PG_NETWORK:?Verified existing PG network required}"
  fleet-nodes:
    external: true
    name: "${MULTICA_FLEET_NODE_NETWORK:?Namespace node network required}"
```

node restart=no、Fleetpool有界。node API 连通检查兼容 Desktop/Linux gateway，Fleet DB 仅走验证过的共享 PG internal network；不自动扩大 API 公网监听。down先quiesce再停止，volumes保留；destroy确认、labels+SQLowner验证、先清Docker后DB/registry，故障中止，禁止globalprune。namespace/FleetID registry持久化，同checkout重启不变、不同worktree隔离；同步AGENTS新流程而非事故历史。
- [ ] **Step 4:** hermetictests、`bash -n scripts/fleet-env.sh scripts/dev-env.sh` PASS；授权Docker时 `make up C=api,web,fleet`、`make status`、down再up验证DB/卷恢复。未授权准确报告未运行。
- [ ] **Step 5:** scoped commit `feat(dev): add optional managed Docker Fleet environment`。

### Task 14: opt-in Docker/fake Claude 与浏览器全链路

**审查补充（本任务所有权与 canonical tests）：** cloud-runtime.spec.ts 增五 action 浏览器意图/重试 key 捕获与 API→Fleet 同 key trace，TestApiClient 用同 key。docker_recovery_test.go 验 stopped/missing + other namespace failed report 拒删卷、错 mount unknown、zero proof 后同 op 清理、Linux shared PG internal URL 与动态 API ExtraHosts。复用 Task 7/8/9 canonical policy 单测，只验证组合/named regressions。Docker/build/setup 待明确授权，SKIP 非 green。

**Dependencies:** Tasks1–13。
**Files:** Create `server/internal/fleet/integration/docker_test.go`、`docker_recovery_test.go`、`e2e/cloud-runtime.spec.ts`；Modify [TestApiClient](<../../../e2e/fixtures.ts>)、[Playwright](<../../../playwright.config.ts>)；新fleet-dockerproject只在Docker gate启用，不在默认suite自动启动Fleet。

**Interfaces — Consumes:** Task9假镜像、Task10service、Task13managedenv。**Produces:** `RoundTripFakeNode(ctx context.Context,t *testing.T) error`（本任务integration测试同包），真实Engine+testutil自有fixture+原taskHTTP链路，不执行用户CLI。TestApiClient新 `createFleetNode(spec:string,name:string):Promise<CloudRuntimeNode>`、`listFleetNodes():Promise<CloudRuntimeNode[]>`、`deleteFleetNode(instanceId:string):Promise<void>`、`createFleetAgent(runtimeId:string,name:string):Promise<{id:string}>`、`createFleetChat(agentId:string):Promise<{id:string}>`、`sendFleetChat(sessionId:string,content:string):Promise<{task_id:string}>`；privateauthedFetch+schema+现有workspaceheader，不旁路auth。公开getter `getFleetWorkspaceSlug():string`，已有workspaceSlug为private。

- [ ] **Step 1:** tag/envgate必须先于Engine/executable/accountlookup：

```go
//go:build dockerintegration

package integration

func TestDockerNodeRoundTrip(t *testing.T) {
    if os.Getenv("MULTICA_RUN_DOCKER_INTEGRATION") != "1" { t.Skip("Docker opt-in required") }
    ctx,cancel := context.WithTimeout(context.Background(),5*time.Minute)
    defer cancel()
    if err := RoundTripFakeNode(ctx,t); err != nil { t.Fatal(err) }
}
```

- [ ] **Step 2:** 不设gate时SKIP且无socket访问；授权后 `make env-exec ARGS='-- bash -c "cd server && MULTICA_RUN_DOCKER_INTEGRATION=1 go test -tags=dockerintegration ./internal/fleet/integration -run TestDockerNodeRoundTrip -count=1 -v"'`，最初protocol/helper未实现FAIL；skip不算green。
- [ ] **Step 3:** fakeClaude实现--version/currentstream-json、marker回复和可控block。RoundTripFakeNode具体sequence：APIcreate→注册/ready→POST /api/agents绑定Runtime→POST /api/chat/sessions→POST消息→pollcompleted/messages→stop/start原身份/卷→Fleet重启→维护批准边界crash→delete/tokeninvalid。报告pending用fakeHTTP控制，channels/poll等状态不用sleep猜测。GoDB细节用testutil；browser业务资源由TestApiClient setup/cleanup。实现getter：

```ts
getFleetWorkspaceSlug():string {
  if (!this.workspaceSlug) throw new Error("Test API client workspace is not initialized");
  return this.workspaceSlug;
}
```

browser firstcase：

```ts
test("Docker node becomes ready", async ({page,apiClient}) => {
  test.skip(process.env.MULTICA_RUN_DOCKER_INTEGRATION !== "1");
  const node = await apiClient.createFleetNode("local-small","E2E Docker node");
  await expect.poll(async () => {
    const nodes = await apiClient.listFleetNodes();
    return nodes.find(item => item.id === node.id)?.ready;
  },{timeout:300000}).toBe(true);
  await page.goto("/" + apiClient.getFleetWorkspaceSlug() + "/runtimes");
  await expect(page.getByText("E2E Docker node",{exact:true})).toBeVisible();
});
```

本Task在fixtures基于现有login/workspace登录逻辑暴露apiClient。另case从节点ClaudeRuntimeID建Agent/session，经浏览器输入发送固定prompt，观察fake回复/执行日志，APIpoll不能替代UI结果验证。cover跨owner403、busy停止拒绝、unknown能力入口隐藏、delete确认及卷清理。所有资源通过typedschema读，节点metadata不cast。
- [ ] **Step 4:** 授权后GoDockertests与 `make env-exec ARGS='-- pnpm exec playwright test e2e/cloud-runtime.spec.ts --project=fleet-docker'` PASS；保留截图/trace/脱敏IDs。只清本testlabels+SQLowner资源，无prune；未授权报告未运行。
- [ ] **Step 5:** scoped commit `test(fleet): verify Docker execution and recovery end to end`。

### Task 15: 文档、回归与交付门

**审查补充（本任务所有权与 canonical tests）：** 文档说明 online/offline known gate、wrong-mount 不等于空、同 op 重审、PG internal network 与 API gateway 不同用途、setup pending 非 red；记录命令/跳过原因。不改成 Aurora/AWS 部署，不声称未执行 registry/Docker/DB 已验收。

**Dependencies:** Tasks1–14。
**Files:** Create `apps/docs/content/docs/developers/local-docker-fleet.mdx`、`local-docker-fleet.zh.mdx`；Modify [CONTRIBUTING](<../../../CONTRIBUTING.md>)、[开发导航](<../../../apps/docs/content/docs/developers/meta.json>)、[中文导航](<../../../apps/docs/content/docs/developers/meta.zh.json>)；其余locale遵循既有fallback不丢已有项。

**Interfaces — Consumes:** 每任务的配置/操作/测试契约。**Produces:** 用户能独立配置privateprofiles、create/绑定/执行/维护/恢复的说明和验收记录。

- [ ] **Step 1:** 新文档linkcheck先FAIL；新增defaulttests无真实executablelookup，参照已有 [CLI清单](<../../../scripts/agent-cli-command-names.txt>)（Claude已在清单，无需新增其他Agent）；Task14defaultgate禁止socket。
- [ ] **Step 2:** 说明版本锁、owner/privateprofile、gatewaydiagnostic、卷永久删除、queued/deferred取消、failedreport恢复、socket风险，不保证volumequota/物理wipe/生产强隔离。最小命令：

```sh
make up C=api,web,fleet
make status
make down
```

真实smoke是可选且额外授权：若新增 `server/pkg/agent/claude_fleet_integration_test.go`，名称锁定 `TestClaudeFleetRealAgentSmoke`，首动作现有 `requireRealAgentSmoke(t)`，后才读取显式容器testprofile/HTTP业务sequence，不查用户home/OAuth/本机CLI。真实账户不作default依赖。
- [ ] **Step 3:** 实施阶段完整回归：

```sh
make sqlc
make test
pnpm typecheck
pnpm lint
pnpm test
bash scripts/dev-env.test.sh
bash scripts/fleet-env.test.sh
git diff --check
```

再跑 `make check`；获Docker许可后跑Task14specifictests/currentURL。真实smoke仅新test实现且授权后：

```sh
(cd server && MULTICA_RUN_REAL_AGENT_SMOKE=1 go test -tags=agentintegration ./pkg/agent -run '^TestClaudeFleetRealAgentSmoke$' -count=1 -v)
```

“no tests to run”不是成功；参照 [已有gate](<../../../server/pkg/agent/real_agent_smoke_integration_test.go>)。
- [ ] **Step 4:** Spec验收11项对应证据，noFK/concurrentindex、secretmarker扫描、sameDB/noSQLite、labels/namespace/owner、no cachedclaimpermit、API/Fleetcrash、Web/Desktop/5locale、再生成sqlc无新diff。未跑项写原因/命令，不声称全通过。
- [ ] **Step 5:** scoped commit `docs(fleet): document local Docker runtime setup and recovery`；交付actualURL/commit/test证据、跳过真实smoke/剩余风险，不宣称Mobile/生产强隔离。

## Spec Coverage 自审表

| 设计要求 | 任务 | 关键证据 |
| --- | --- | --- |
| 独立服务/仅Claude/原协议 | 1、5、9–10、14 | onlyClaude注册，无新Runtimeenum |
| SaaS兼容/独立Billing | 4–5、11 | validhosted响应/seatpolicy回归 |
| 本地配置/限额/能力 | 1、3、5、11、13 | mixedconfig拒绝/owner限额并发 |
| auth/owner/幂等/禁exec | 3–5、11、14 | 越权/同键冲突/未知字段 |
| PG/迁移/秘密卷 | 2–3、8–9、13 | 独立并发indexmigration/hashonly |
| create→runtime→task→结果 | 5–6、9–10、14 | fake实际执行/回报/UI日志 |
| barrier/capacity/delete准入 | 6–7、10、12、14 | 真实事务竞态/tx外I/O |
| 网络/安全/资源归属 | 5、8–9、13–14 | gatewaydiagnostic/全部标签 |
| crash/unknown不猜测 | 7–8、10、13–14 | unapproved不执行/同op恢复 |
| managedenv/Web/Desktop | 11–13、15 | downbusy不关API/destroy先资源 |
| defaultfake/real授权 | 每任务、14–15 | Docker/real各自gate |
| docs/boundaries/sqlc | 2、5–7、13、15 | sqlc无新diff/AGENTS同步 |

## 执行交接

开发已经授权，controller 审阅本次准备报告后按 Task 1–15 顺序执行 red→green→review，checkbox 跟踪，M1/M2/M3 分别验收。本次准备不提交；后续提交也须遵守 controller 指令。

1. **Subagent-Driven（推荐）**：subagent-driven-development，每task fresh subagent、两阶段审查；不是Agent Teams。
2. **Inline Execution**：executing-plans，当前会话按批实施，在阶段门暂停审查。

已选择顺序 SDD；不再等待开发授权或配置模型选择器。执行前检查隔离 worktree 与 managed environment。环境建立、Docker、registry 和真实模型账户仍单独等待明确安全授权。
