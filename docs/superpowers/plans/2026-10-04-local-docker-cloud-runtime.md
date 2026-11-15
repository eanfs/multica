# 本地 Docker Cloud Runtime Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在当前仓库交付可运行的 Docker Fleet，接入现有本地 API/Web，节点仅运行 Claude Code，基础设施状态复用原 PostgreSQL 数据库。

**Architecture:** 独立 Go Fleet 用 Docker Engine API 管理容器和节点卷，API 与 Fleet 共享当前 checkout 数据库、sqlc 查询和事务级节点锁。复用既有 Daemon、Runtime、任务队列和回传协议；本地节点服务与 SaaS Billing/配额客户端分离。

**Tech Stack:** Go、Chi、pgx/v5、sqlc、PostgreSQL、Docker Engine SDK、Node.js/Claude Code、TanStack Query、zod、Vitest、Playwright。

**Spec:** [已批准设计](<../specs/2026-10-04-local-docker-cloud-runtime-design.md>)，以用户批准的 PostgreSQL 修订版为准。

## Global Constraints

- 文档修订阶段不执行代码、迁移、构建、启动或测试；此限制不适用于用户明确授权后的实施阶段。实施时先完成下方执行前置检查，再按任务运行所列命令。
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
    ObservedAt time.Time
}
type Provider interface {
    Ensure(context.Context, Node, Bootstrap) (Observation, error)
    Inspect(context.Context, Node) (Observation, error)
    Apply(context.Context, Node, Action) (Observation, error)
    Delete(context.Context, Node) error
}
```

`model` 的错误变量：`ErrBusy`、`ErrConflict`、`ErrForbidden`、`ErrUnavailable`、`ErrUnknownHealth`、`ErrProfileMissing`、`ErrInvalidRequest`。pure helpers：`CanClaim(Node, time.Time) bool`、`CanEnqueue(Node) bool`、`ValidateCreate(CreateRequest, Config) error`。`Provider` 的 context import 属于 types 文件；所有 snippets 在所属文件补正常 imports，不引入未定义 test 框架。

`store.New(pool *pgxpool.Pool, namespace string, opts ...store.Option) *store.Store`；`store.WithMaxNodes(limit int) store.Option`，默认2，main按Config.MaxNodes注入。；`Store.WithTx(ctx, func(*db.Queries) error) error`。sqlc 锁查询的参数统一 `Namespace string, NodeID pgtype.UUID`。必须先节点锁、再 capacity 锁、再 agent/task 行锁；普通节点不创建 Fleet 状态依赖。

### Task 1: 固定 domain、配置与安全判断

**Dependencies:** 无。**Deliverable:** 纯逻辑可测试、固定错误/状态契约。

**Files:** Create `server/internal/fleet/model/types.go`、`policy.go`、`config.go`、`policy_test.go`、`config_test.go`。

**Interfaces:** Produces 上述 types 和 helpers；`Config` 包含 `Namespace, FleetID, Image, APIURL string`、`Specs map[string]Spec`、`MaxNodes int`；`Spec{CPUs int, MemoryBytes int64, Pids int64, MaxRuns int}`；`LoadConfig(path string) (Config,error)` 从明确文件读且拒绝未知危险字段。私密 profile 与 public config 用不同结构，不将 Bootstrap json.Marshal 到日志。

- [ ] **Step 1:** 写状态矩阵，至少以下回归：

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
- [ ] **Step 2:** `make env-exec ARGS='-- bash -c "cd server && go test ./internal/fleet/model -run TestCanClaim -count=1"'`；预期 undefined helper 或断言 FAIL，记录原因。
- [ ] **Step 3:** 实现 pure 判断，初版：

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
- [ ] **Step 4:** 同一命令跑 `./internal/fleet/model` 全套，预期 PASS；表驱动测试负责枚举，界面不重复这些矩阵。
- [ ] **Step 5:** 检查 diff，提交 `feat(fleet): define local node configuration and policy`，只 stage 本任务文件。

### Task 2: 同库迁移、sqlc 与数据库测试 fixture

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

- [ ] **Step 1:** 在 store_test 用 fixture 检查 namespace/owner 隔离与重复 ID/幂等键，测试需要的核心形状：

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
- [ ] **Step 2:** `make env-exec ARGS='-- bash -c "cd server && go test ./internal/fleet/store -run TestFleetNodeOwnerIsolation -count=1"'`；迁移未有表/方法未有时 FAIL。
- [ ] **Step 3:** 先建四表，所有表含 `id uuid DEFAULT gen_random_uuid()`、`namespace text`、`owner_id uuid`、时间字段。字段完整取自 Spec 第6节；profile 只含引用，凭证只含 hash。锁 SQL：

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

- [ ] **Step 4:** `make sqlc`，`make env-exec ARGS='-- bash -c "cd server && go run ./cmd/migrate up && go test ./internal/fleet/store ./internal/testutil -count=1"'`。确认测试未因数据库不达被 skip；检查 pg_indexes 与无 FK。只在测试自有数据验证 down/up，不对现有业务库运行 migrate down。
- [ ] **Step 5:** 提交 `feat(fleet): persist node state in existing PostgreSQL`，明确 stage 本批 migrations、fleet.sql、生成代码、store 与 fixture。

### Task 3: 创建意图、幂等与节点凭证

**Dependencies:** Tasks 1–2。**Deliverable:** 无 Docker 的原子 create/list intent 和凭证生命周期。

**Files:** Create `server/internal/fleet/store/intents.go`、`credentials.go`、`intents_test.go`、`credentials_test.go`、`server/internal/fleet/credentials.go`、`credentials_test.go`；Modify `fleet.sql` 并再生成。

**Profile/限额连接：** Task2 fixture另增 `Fixture.FleetProfile(t TB,namespace string,over ...Cols) string`，默认owner引用test私密profile；下面TestCreateIntentReplays先 `f.FleetProfile(t,"test-replay")`。Task3 `Store.UpsertProfiles(ctx context.Context,profiles map[pgtype.UUID]string,version int64) error` 投影管理员owner→profile；Service先确认profile存在可读才调用CreateIntent。main注入WithMaxNodes(cfg.MaxNodes)，不是在浏览器设quota。

**Interfaces:** `Store.CreateIntent(ctx, ownerID, model.CreateRequest) (model.Node,model.Operation,bool,error)`，bool 为 replayed；`Store.VerifyNodeToken(ctx, token string) (model.Node,error)`；`Store.RevokeNodeToken(ctx,nodeID) error`。`fleet.LoadProfile(path string) (model.Bootstrap,error)` 读取显式文件；profile reference 由 owner 配置匹配，不能从请求任意指定文件。

- [ ] **Step 1:** 写 create twice same key 得同 node/op、不同 fingerprint 409、两个并发请求不突破限额、Token DB 无明文、revoke 即失效：

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

- [ ] **Step 2:** 跑 `go test ./internal/fleet/store -run 'TestCreateIntent|TestNodeToken' -count=1`，通过 make env-exec 的 server 工作目录；预期缺方法/断言 FAIL。
- [ ] **Step 3:** WithTx 内先所有者级 advisory lock，再限额/幂等查询、节点/op 插入。Token 使用 crypto/rand 32 bytes，`mcn_` + base64url；hash 用既有 auth.HashToken。私密 bootstrap 写入前崩溃，重试撤销旧 hash 并推进 credential generation，不能反推 hash。profile 解析 `DisallowUnknownFields`、权限和 owner 校验；只保留引用/版本进 SQL。

```go
func NewNodeToken() (token,hash string,err error) {
    raw := make([]byte,32)
    if _,err = rand.Read(raw); err != nil { return "","",err }
    token = "mcn_" + base64.RawURLEncoding.EncodeToString(raw)
    return token,auth.HashToken(token),nil
}
```

Store.MintNodeToken(ctx context.Context,nodeID pgtype.UUID) (token string,generation int64,err error) 调用上述NewNodeToken，再在事务中保存hash/generation；token 返回只供内部 bootstrap，不进入公开 DTO。
- [ ] **Step 4:** store/profile 全测试 PASS；扫描测试 DB、HTTP JSON、logger 捕获无 marker key/token。`make sqlc` 如查询改变。
- [ ] **Step 5:** 提交 `feat(fleet): add idempotent intents and node credentials`。

### Task 4: Fleet HTTP 契约和假 Provider 服务

**Dependencies:** Tasks 1–3。**Deliverable:** httptest 可验证全部公开 API、拒绝任意 exec；无需 Docker。

**Files:** Create `server/internal/fleet/service.go`、`http.go`、`http_test.go`、`service_test.go`、`fake_provider_test.go`。

**Interfaces:** `fleet.NewService(repo *store.Store, cfg model.Config, provider model.Provider) *Service`；`Service.Handler(secret []byte) http.Handler`；Provider 是上方契约。Service 不直接执行维护，stop/reboot/delete 只接受已批准 operation，未批准返回 Conflict。公开 DTO 的 `id,owner_id,instance_id,region,instance_type,image_id,subnet_id,name,status,tags,metadata,created_at,updated_at` 保留，额外 `provider,operation_id,ready,error_code` 为可选。

- [ ] **Step 1:** 写无服务密钥 401、伪造 X-User-ID、越权节点、spec/image/path 拒绝和 exec 未支持：

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
- [ ] **Step 2:** `go test ./internal/fleet -run 'TestService|TestHTTP' -count=1`；预期路由/认证未实现 FAIL。
- [ ] **Step 3:** Chi 固定路由；secret 用 constant-time 比较，body 上限 1 MiB，未知字段拒绝。注册 设计第5节的 routes；healthz 存活、readyz 需 DB/schema/Provider 检查。统一 map errors 到状态码；公开响应构造独立 DTO，绝不序列化 Bootstrap。fake Provider 四方法实现于本任务的测试文件并记录动作，不能调用用户 Docker。

```go
if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Fleet-Service-Key")), secret) != 1 {
    http.Error(w,"unauthorized",http.StatusUnauthorized)
    return
}
```

- [ ] **Step 4:** 完整 httptest 行为 PASS，明确 terminated/stopped/token revoked、分页、异常 JSON 与重复操作返回码；response 字段可被现有 core schema 解析。
- [ ] **Step 5:** 提交 `feat(fleet): expose authenticated local node APIs`。

### Task 5: API 本地组装、SaaS 隔离和可信节点身份

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

**Dependencies:** Tasks 1–6。**Deliverable:** fake Provider 测试证明 stop/reboot/delete 无检查后领取竞态，崩溃保持屏障。

**Files:** Create `server/internal/fleetguard/maintenance.go`、`maintenance_test.go`、`server/internal/fleet/store/maintenance.go`、`maintenance_test.go`、`server/internal/handler/local_fleet_operations.go`、`local_fleet_operations_test.go`；Modify `fleet.sql`、[Cloud handler](<../../../server/internal/handler/cloud_runtime.go>)。

**Interfaces:** `store.PrepareMaintenance(ctx,ownerID,nodeID,action,key) (model.Operation,error)`、`ApproveMaintenance(ctx,operationID,generation) error`、`AbortMaintenance(ctx,operationID,generation) error`。`fleetguard.Maintainer{Repo *store.Store, Diagnose func(context.Context,model.Node)(model.Observation,error)}`；`Maintainer.Request(ctx,ownerID,nodeID,action,key) (model.Operation,error)`。Diagnose 由 Task 4 的固定内部检查接口提供，测试用 function literal，无网络数据库事务混合。

- [ ] **Step 1:** active SQL row 拒绝、pending/failed reports 拒绝、未知 health 保留未批准屏障、删除 queued 拒绝、维护后 new enqueue 拒绝、两段之间 API crash 可重试：

```go
func TestReportsBlockMaintenance(t *testing.T) {
    obs := model.Observation{Status:"running",Ready:true,PendingReports:1}
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
    return o.ActiveRuns > 0 || o.PendingReports > 0 || o.FailedReports > 0
}
```

stop/reboot 不提前撤销 Token，保持结果回调；delete 批准后才 revoke，并在节点状态标记 terminating 防止新认证/准入。stopped/missing container 仍须 SQL 无活跃；running container 必须有可信本代次诊断，不能沿用上一轮重启的健康。API 重审只处理同一未批准 op，服务密钥认证且 namespace 过滤。
- [ ] **Step 4:** race suite PASS，断言诊断执行时 SQL tx 已释放；模拟 second tx/approval crash，Worker 不执行未批准 delete。普通 Runtime 删除 handler 对 managed nodes 返回冲突，不静默删除。
- [ ] **Step 5:** 提交 `feat(fleet): authorize safe node maintenance and deletion`。

### Task 8: Docker Engine Provider 与资源归属

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
    cfg := NodeHostConfig(model.Spec{CPUs:2,MemoryBytes:4<<30,Pids:256,MaxRuns:1})
    if cfg.Privileged || cfg.NetworkMode == "host" || cfg.PidMode == "host" {
        t.Fatal("unsafe container isolation")
    }
    if cfg.Memory != 4<<30 || len(cfg.PortBindings) != 0 { t.Fatal("resource/port mismatch") }
}
```

`NodeHostConfig(model.Spec) container.HostConfig` 本任务定义；SDK package 路径按所选 SDK 版本固定，不能混用新/旧 module imports。断言 nonroot user、socket 不在 mounts、restart=no、labels 包含 namespace/node/role、secrets 只读。
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

**Dependencies:** Tasks 1、3、5、8。**Deliverable:** nonroot 镜像仅含 Claude，可用 fake CLI 测试 config/health；不调用真实模型。

**Files:** Create `server/cmd/fleet-node/main.go`、`bootstrap.go`、`bootstrap_test.go`、`health.go`、`health_test.go`、`docker/runtime/Dockerfile`、`docker/runtime/claude-version.txt`、`docker/runtime/Dockerfile.test`、`server/internal/fleet/integration/testdata/claude`。

**Interfaces:** `BootstrapFiles(home,secretDir string) (cli.CLIConfig,error)`；`ReadObservation(client *http.Client,healthURL string) (model.Observation,error)`；固定 CLI 子命令 `fleet-node run|health|bootstrap`。使用现有 `cli.SaveCLIConfigForProfile(cfg, "")` 写默认profile，Token/ServerURL存CLIConfig；固定DaemonID保留在bootstrap并用--daemon-id传入，设置HOME为节点data volume。fake CLI 的版本检查和 stream-json 输出必须匹配现有 Claude backend，不把所有 stdout 当作聊天结果。

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
- [ ] **Step 3:** 固定 go helper 读文件、写 CLIConfig，进程内部设置 `ANTHROPIC_API_KEY`、可选 `ANTHROPIC_BASE_URL`、default model，然后 `syscall.Exec` 现有 multica daemon foreground，flags `--no-auto-update --no-auto-reload --max-concurrent-tasks 1`，并以独立argv对传 `--daemon-id` 与Bootstrap.DaemonID（UUID公开身份，不传Token）。HTTP health 默认容器 loopback 19514；使用现有 `daemon.HealthResponse` 字段映射 ActiveTaskCount/PendingTerminalReportCount/FailedTerminalReportCount/Agents/Workspaces，Ready 判 Status=running、claude 已注册及 RuntimeCount>0。不要编造 ready_for_task_claims JSON 字段。

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

**Dependencies:** Task4（最终对接在Task10后）。
**Files:** Modify [client.ts](<../../../packages/core/api/client.ts#L2014-L2055>)、[client.test.ts](<../../../packages/core/api/client.test.ts#L1523-L1580>)、[schemas.ts](<../../../packages/core/api/schemas.ts#L1643-L1677>)、[schemas.test.ts](<../../../packages/core/api/schemas.test.ts>)、[cloud-runtime.ts](<../../../packages/core/runtimes/cloud-runtime.ts>)；Create `packages/core/runtimes/cloud-runtime.test.ts`、`cloud-runtime-capabilities.ts`、`cloud-runtime-capabilities.test.ts`。

**Interfaces — Consumes:** Task4 DTO/routes。**Produces:**

```ts
export type NodeAction = "create"|"start"|"stop"|"reboot"|"delete";
export interface CloudRuntimeCapabilities {
  provider:"docker"|"cloud"|"unknown";
  operations:NodeAction[];
  specs:Array<{id:string;cpus:number;memory_bytes:number;pids:number}>;
  persistent_storage:boolean;
  disk_quota_supported:boolean;
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
  provider:"unknown",operations:[],specs:[],persistent_storage:false,disk_quota_supported:false,
};
export function supportsNodeAction(caps:CloudRuntimeCapabilities,action:NodeAction):boolean {
  return caps.provider !== "unknown" && caps.operations.includes(action);
}
```

Lifecycle body `{instance_id}`，Mutation onSettled invalidate节点/Runtimequery，无optimisticremove；account节点key accountscope，wsId只给Runtime投影失效。core无process.env/storage/UI；Node新增provider/ready/op/error为optionaldefaults。schema边界生成受管metadata类型，不cast。
- [ ] **Step 4:** 定向tests PASS、`pnpm --filter @multica/core typecheck`；valid hosted旧tests继续PASS，malformedcreation转显式失败。
- [ ] **Step 5:** scoped commit `feat(fleet): add compatible client capabilities and node actions`。

### Task 12: Web/Desktop 共享节点 UI

**Dependencies:** Tasks5、7、11。
**Files:** Modify [cloud-runtime-dialog](<../../../packages/views/runtimes/components/cloud-runtime-dialog.tsx>)、[runtimes-page](<../../../packages/views/runtimes/components/runtimes-page.tsx>)、[delete-runtime-dialog](<../../../packages/views/runtimes/components/delete-runtime-dialog.tsx>)、[runtime-list:548](<../../../packages/views/runtimes/components/runtime-list.tsx#L548>) 和 [row-menu tests](<../../../packages/views/runtimes/components/runtime-row-menu.test.tsx>)、[Web route](<../../../apps/web/app/%5BworkspaceSlug%5D/%28dashboard%29/runtimes/page.tsx>)、[Desktop wrapper](<../../../apps/desktop/src/renderer/src/components/desktop-runtimes-page.tsx>)；Create `packages/views/runtimes/components/cloud-node-actions.tsx`、`cloud-node-actions.test.tsx`、`cloud-runtime-dialog.test.tsx`，修改existingdelete-runtime test。

同步 [en](<../../../packages/views/locales/en/runtimes.json>)、[zh-Hans](<../../../packages/views/locales/zh-Hans/runtimes.json>)、[fr](<../../../packages/views/locales/fr/runtimes.json>)、[ja](<../../../packages/views/locales/ja/runtimes.json>)、[ko](<../../../packages/views/locales/ko/runtimes.json>)。先读 [英文约定](<../../../apps/docs/content/docs/developers/conventions.mdx>)、[中文约定](<../../../apps/docs/content/docs/developers/conventions.zh.mdx>)、[Button](<../../../packages/ui/docs/button.md>)、[Dialog](<../../../packages/ui/docs/dialog.md>)。

**Interfaces — Consumes:** Task11 caps/node/hooks。**Produces:** `CloudNodeActions({node,capabilities,wsId,onDeleted})`，props类型CloudRuntimeNode/CloudRuntimeCapabilities/string/()=>void；core `getManagedFleetNodeID(runtime:AgentRuntime):string|null` 使用schema验证metadata。平台只注入navigation/plumbing。

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

**Dependencies:** Tasks5、7–10。
**Files:** Create `docker-compose.fleet.yml`、`scripts/fleet-env.sh`、`fleet-env.test.sh`、`fleet-config.example.json`；Modify [dev-env.sh](<../../../scripts/dev-env.sh#L42-L43>)、[dev-env.test.sh](<../../../scripts/dev-env.test.sh>)、[Makefile](<../../../Makefile#L157-L176>)、[env模板](<../../../.env.example>)、[gitignore](<../../../.gitignore>)、[AGENTS](<../../../AGENTS.md>)。

**Interfaces — Consumes:** Task7维护API、Task10readyz/cmd。**Produces:** `fleet-env.sh prepare|up|status|quiesce|down|destroy`，dev-env传绝对repo/env/registryID/namespace/port。prepare用URLparser转换localhost至gateway，dbname/密码/port不变；privatefiles600。quiesce全部nodes屏障，busy/unknown在关API前中止；destroy失败保留DB/registry用于恢复。

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
    restart: unless-stopped
```

node restart=no、Fleetpool有界。nodeAPI/DB连通检查兼容Desktop/Linuxgateway但不自动加API公网监听。down先quiesce再停止，volumes保留；destroy确认、labels+SQLowner验证、先清Docker后DB/registry，故障中止，禁止globalprune。namespace/FleetID registry持久化，同checkout重启不变、不同worktree隔离；同步AGENTS新流程而非事故历史。
- [ ] **Step 4:** hermetictests、`bash -n scripts/fleet-env.sh scripts/dev-env.sh` PASS；授权Docker时 `make up C=api,web,fleet`、`make status`、down再up验证DB/卷恢复。未授权准确报告未运行。
- [ ] **Step 5:** scoped commit `feat(dev): add optional managed Docker Fleet environment`。

### Task 14: opt-in Docker/fake Claude 与浏览器全链路

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

本轮只保存审查计划，不启动Docker、不应用迁移、不读取真实keys。执行每task red→green→review→commit、checkbox跟踪，M1/M2/M3分别验收。

1. **Subagent-Driven（推荐）**：subagent-driven-development，每task fresh subagent、两阶段审查；不是Agent Teams。
2. **Inline Execution**：executing-plans，当前会话按批实施，在阶段门暂停审查。

等待用户选择；执行前检查隔离worktree与managedenv。设计/计划批准不授权使用真实模型账户。
