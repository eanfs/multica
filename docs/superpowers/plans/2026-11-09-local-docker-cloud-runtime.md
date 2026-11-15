# 本地 Docker Cloud Runtime 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在本仓库实现本地 Docker 版 Cloud Runtime：Fleet 服务在容器中运行，动态创建装有 multica daemon + Claude Code 的节点容器，节点通过 mcn_ PAT 注册出可绑定的 Claude Runtime，凭证按 owner 从宿主机私密配置注入。

**Architecture:** 新增独立 Go Fleet 进程（containerized，经 Docker Engine API 管理节点容器，与 API 共用当前 checkout 的 PostgreSQL）；API 复用现有 cloud-runtime 代理与 DaemonAuth/mcn_ 验证路径，在 claim 事务内通过 advisory lock 与 fleet 表实现领取屏障；前端复用 RuntimesPage 并以能力发现驱动 docker 规格。

**Tech Stack:** Go 1.26（chi、pgx、sqlc、docker/docker client）、PostgreSQL 17、Docker Compose、React 19 + TanStack Query + zod（core/views）、Vitest、bash（dev-env）。

**Spec:** `docs/superpowers/specs/2026-10-04-local-docker-cloud-runtime-design.md`（权威设计，行号引用均指该文件；实施前先通读）。

## Global Constraints

- Fleet 与 API 分进程；Fleet 在 Docker 中运行，单一 Compose 定义 Fleet 与固定网络；节点由 Fleet 动态创建，不生成每节点 Compose。
- 复用当前 checkout 的同一个 PostgreSQL；不新建数据库、SQLite 或独立数据库卷；Fleet 不启动第二套迁移，缺表/版本不满足时 `/readyz` 不就绪。
- 迁移：每个迁移文件必须有 `.up.sql` 与 `.down.sql`；表名 `snake_case` 单数；不建外键、不用级联删除/更新；每个索引（含唯一索引与主键索引）使用 `CREATE [UNIQUE] INDEX CONCURRENTLY`，且每个并发索引一个独立单语句迁移文件。
- SQL 变更后运行 `make sqlc`，生成代码与查询保持同步。
- 节点容器以固定非 root 用户运行；不挂 docker socket、不 privileged、不用 host network/PID、不映射业务端口；密钥不进镜像层、Docker Config.Env、PostgreSQL、日志、前端响应。
- Fleet 宿主机端口只绑 loopback；所有 Docker 资源带 namespace/fleet/node/role 标签；清理必须同时匹配 Store 归属与标签，禁止按名称前缀或全局 prune。
- 默认测试不依赖 Docker、不解析/执行用户安装的真实 agent CLI；Docker 集成测试用 `fleetintegration` tag，真实 Claude 烟测用 `agentintegration` + `MULTICA_RUN_REAL_AGENT_SMOKE=1` 且需另行授权。
- Web/Desktop 共用能力发现；Mobile 不在范围；UI 文案六种语言（en/zh-Hans/zh-Hant/ja/ko/fr）齐全；图标只用 `@multica/ui/icons`。
- 共享包边界：core 不依赖 UI/next/react-router-dom；views 不定义 store、不 mock next/react-router-dom；平台接线留在 apps。
- 提交使用原子 conventional commits；每个 Task 结束时测试通过再提交。

## File Structure

**新增（Fleet）**

- `server/cmd/fleet/main.go`：配置加载、依赖组装、HTTP 生命周期、信号处理。
- `server/internal/fleet/config.go`：环境变量解析与校验。
- `server/internal/fleet/store/store.go`：PostgreSQL 事务与节点/凭证/操作/profile 读写（薄封装 sqlc）。
- `server/internal/fleet/barrier/barrier.go`：advisory lock key 派生与共享/排他锁。
- `server/internal/fleet/state.go`：容器观察/目标/Daemon 就绪 → 主显示状态。
- `server/internal/fleet/service.go`：创建/启停/删除/诊断/凭证校验用例。
- `server/internal/fleet/operations.go`：prepare/approve/abort 事务。
- `server/internal/fleet/coordinator.go`：周期对账与恢复。
- `server/internal/fleet/diagnose.go`：固定 docker exec 读取 daemon health。
- `server/internal/fleet/credentials.go`：owner→profile→env 私密配置解析。
- `server/internal/fleet/http.go`：chi 路由、服务密钥中间件、handler。
- `server/internal/fleet/docker/provider.go`：Docker Engine SDK 实现（接口在 `provider_api.go`）。
- `server/internal/fleetclient/client.go`：服务密钥 HTTP client，cloudruntime 与 CloudPATVerifier 共用。

**修改（API）**

- `server/internal/cloudruntime/client.go`：支持服务密钥与共享 http.Client。
- `server/internal/auth/cloud_pat.go`：`ServiceSecret`、`DisableCache`。
- `server/internal/handler/cloud_runtime.go`：代理保持；本地协调入口拆分到新文件。
- `server/internal/handler/cloud_runtime_local.go`：忙碌检查、prepare/approve/abort、幂等透传。
- `server/internal/handler/daemon.go`：受管 runtime 写 `managed_by/fleet_node_id`。
- `server/internal/handler/runtime.go`：受管 runtime 删除保护。
- `server/internal/middleware/daemon_auth.go`：CloudPATIdentity 入 context。
- `server/internal/service/task.go`：claim 事务内 FleetGuard。
- `server/cmd/server/router.go`：本地 Fleet 配置互斥、verifier/client 组装、claim guard 注入。
- `server/pkg/db/queries/runtime.sql`：upsert 保留受管列。

**新增/修改（部署与脚本）**

- `docker/runtime/Dockerfile`、`docker/runtime/entrypoint.sh`、`docker/fleet/Dockerfile`。
- `docker-compose.yml`（fleet 服务）、`docker-compose.selfhost.yml`（可选 fleet profile）。
- `scripts/fleet-env.sh`、`scripts/fleet-env.test.sh`。
- `scripts/init-worktree-env.sh`、`scripts/dev-env.sh`、`scripts/dev-env.test.sh`、`Makefile`、`.env.example`。

**新增/修改（前端）**

- `packages/core/runtimes/cloud-runtime.ts`、`packages/core/api/client.ts`、`packages/core/api/schemas.ts`。
- `packages/views/runtimes/components/cloud-runtime-dialog.tsx`、`runtimes-page.tsx`、`utils.ts`。
- `packages/views/locales/{en,zh-Hans,zh-Hant,ja,ko,fr}/runtimes.json`。
- `apps/web/app/[workspaceSlug]/(dashboard)/runtimes/page.tsx`、`apps/desktop/src/renderer/src/components/desktop-runtimes-page.tsx`。
- `e2e/cloud-runtime-docker.spec.ts`。

**文档**

- `docs/superpowers/specs/2026-10-04-local-docker-cloud-runtime-design.md`（状态与实现差异）、`AGENTS.md`、`CONTRIBUTING.md`、`CLI_AND_DAEMON.md`、`SELF_HOSTING_AI.md`。

---

### Task 1: 数据库迁移与 sqlc 查询

**Files:**
- Create: `server/migrations/611_fleet_node.up.sql` … `server/migrations/624_agent_runtime_fleet_managed.up.sql`（含对应 .down.sql，共 28 个文件）
- Create: `server/pkg/db/queries/fleet.sql`
- Create: `server/internal/fleet/store/store_test.go`
- Generated: `server/pkg/db/generated/*.go`（make sqlc）

**Interfaces:**
- Produces（sqlc 生成、后续任务使用）：`db.CreateFleetNodeParams`、`db.GetFleetNodeParams`、`db.GetFleetNodeByDaemonIDParams`、`db.ListFleetNodesByOwnerParams`、`db.CountFleetNodesByOwnerParams`、`db.UpdateFleetNodeTargetStateParams`、`db.UpdateFleetNodeObservedStateParams`、`db.CreateFleetNodeCredentialParams`、`db.GetFleetNodeCredentialByHashParams`、`db.RevokeFleetNodeCredentialsByNodeParams`、`db.CreateFleetNodeOperationParams`、`db.GetFleetNodeOperationByIdempotencyKeyParams`、`db.UpdateFleetNodeOperationPhaseParams`、`db.CountActiveTasksForDaemonParams`、`db.CountQueuedTasksForDaemonParams`、`db.UpsertFleetCredentialProfileParams`、`db.GetFleetCredentialProfileByOwnerParams`、`db.ListStaleFleetNodesParams`。

- [ ] **Step 1: 写失败的 store 测试**

```go
package store

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	return testutil.IsolatedDB(t, dsn)
}

func TestCreateFleetNodePersistsNamespaceAndSpec(t *testing.T) {
	pool := testPool(t)
	q := db.New(pool)
	owner := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	node, err := q.CreateFleetNode(context.Background(), db.CreateFleetNodeParams{
		OwnerID:       owner,
		Namespace:     "wt-test",
		Name:          "node-1",
		DaemonID:      "daemon-node-1",
		Image:         "multica/runtime-claude:test",
		Spec:          []byte("{\"cpu\":2,\"memory_mb\":4096}"),
		TargetState:   "launching",
		ObservedState: "",
	})
	if err != nil {
		t.Fatalf("create fleet node: %v", err)
	}
	if node.Namespace != "wt-test" || node.DaemonID != "daemon-node-1" {
		t.Fatalf("unexpected node: %+v", node)
	}
	got, err := q.GetFleetNodeByDaemonID(context.Background(), db.GetFleetNodeByDaemonIDParams{
		DaemonID: "daemon-node-1", Namespace: "wt-test",
	})
	if err != nil {
		t.Fatalf("get by daemon: %v", err)
	}
	if got.ID != node.ID {
		t.Fatalf("got node %s, want %s", got.ID, node.ID)
	}
}

func TestCreateFleetNodeCredentialRejectsDuplicateHash(t *testing.T) {
	pool := testPool(t)
	q := db.New(pool)
	owner := pgtype.UUID{Bytes: [16]byte{2}, Valid: true}
	node, err := q.CreateFleetNode(context.Background(), db.CreateFleetNodeParams{
		OwnerID: owner, Namespace: "wt-test", Name: "node-2", DaemonID: "daemon-node-2",
		Image: "img", Spec: []byte("{}"), TargetState: "launching",
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	if _, err := q.CreateFleetNodeCredential(context.Background(), db.CreateFleetNodeCredentialParams{
		NodeID: node.ID, TokenHash: "hash-1",
	}); err != nil {
		t.Fatalf("create credential: %v", err)
	}
	if _, err := q.CreateFleetNodeCredential(context.Background(), db.CreateFleetNodeCredentialParams{
		NodeID: node.ID, TokenHash: "hash-1",
	}); err == nil {
		t.Fatal("duplicate token_hash must fail the unique index")
	}
}
```

- [ ] **Step 2: 运行测试确认失败（生成代码尚不存在）**

Run: `cd server && go test ./internal/fleet/store -run TestCreateFleetNode -count=1`
Expected: 编译失败，`undefined: db.CreateFleetNodeParams`。

- [ ] **Step 3: 写表迁移（611–614）**

严格一文件一语句；下表内容拆成 8 个文件：

| 文件 | 内容 |
| --- | --- |
| `611_fleet_node.up.sql` | `CREATE TABLE IF NOT EXISTS fleet_node (id UUID NOT NULL DEFAULT gen_random_uuid(), owner_id UUID NOT NULL, namespace TEXT NOT NULL, name TEXT NOT NULL, container_id TEXT NOT NULL DEFAULT '', container_name TEXT NOT NULL DEFAULT '', daemon_id TEXT NOT NULL, image TEXT NOT NULL, spec JSONB NOT NULL DEFAULT '{}'::jsonb, target_state TEXT NOT NULL DEFAULT 'launching', observed_state TEXT NOT NULL DEFAULT '', daemon_ready BOOLEAN NOT NULL DEFAULT false, health_checked_at TIMESTAMPTZ, generation BIGINT NOT NULL DEFAULT 1, idempotency_key TEXT NOT NULL DEFAULT '', status_detail JSONB NOT NULL DEFAULT '{}'::jsonb, error TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now());` |
| `611_fleet_node.down.sql` | `DROP TABLE IF EXISTS fleet_node;` |
| `612_fleet_node_credential.up.sql` | `CREATE TABLE IF NOT EXISTS fleet_node_credential (id UUID NOT NULL DEFAULT gen_random_uuid(), node_id UUID NOT NULL, token_hash TEXT NOT NULL, revoked_at TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL DEFAULT now());` |
| `612_fleet_node_credential.down.sql` | `DROP TABLE IF EXISTS fleet_node_credential;` |
| `613_fleet_node_operation.up.sql` | `CREATE TABLE IF NOT EXISTS fleet_node_operation (id UUID NOT NULL DEFAULT gen_random_uuid(), node_id UUID NOT NULL, operation TEXT NOT NULL, phase TEXT NOT NULL, idempotency_key TEXT NOT NULL DEFAULT '', request_fingerprint TEXT NOT NULL DEFAULT '', approved_at TIMESTAMPTZ, retry_count INT NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now());` |
| `613_fleet_node_operation.down.sql` | `DROP TABLE IF EXISTS fleet_node_operation;` |
| `614_fleet_credential_profile.up.sql` | `CREATE TABLE IF NOT EXISTS fleet_credential_profile (id UUID NOT NULL DEFAULT gen_random_uuid(), owner_id UUID NOT NULL, profile_name TEXT NOT NULL, config_version TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now());` |
| `614_fleet_credential_profile.down.sql` | `DROP TABLE IF EXISTS fleet_credential_profile;` |

- [ ] **Step 4: 写索引迁移（615–623）与 agent_runtime 列迁移（624）**

| 迁移 | up.sql 内容 | down.sql 内容 |
| --- | --- | --- |
| 615_fleet_node_pkey | `CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS fleet_node_id_key ON fleet_node (id);` | `DROP INDEX CONCURRENTLY IF EXISTS fleet_node_id_key;` |
| 616_fleet_node_owner_namespace_idx | `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fleet_node_owner_namespace ON fleet_node (owner_id, namespace);` | `DROP INDEX CONCURRENTLY IF EXISTS idx_fleet_node_owner_namespace;` |
| 617_fleet_node_namespace_idx | `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fleet_node_namespace ON fleet_node (namespace);` | `DROP INDEX CONCURRENTLY IF EXISTS idx_fleet_node_namespace;` |
| 618_fleet_node_daemon_idx | `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fleet_node_daemon ON fleet_node (daemon_id);` | `DROP INDEX CONCURRENTLY IF EXISTS idx_fleet_node_daemon;` |
| 619_fleet_node_credential_token_key | `CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS fleet_node_credential_token_hash_key ON fleet_node_credential (token_hash);` | `DROP INDEX CONCURRENTLY IF EXISTS fleet_node_credential_token_hash_key;` |
| 620_fleet_node_credential_node_idx | `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fleet_node_credential_node ON fleet_node_credential (node_id);` | `DROP INDEX CONCURRENTLY IF EXISTS idx_fleet_node_credential_node;` |
| 621_fleet_node_operation_node_idx | `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_fleet_node_operation_node_created ON fleet_node_operation (node_id, created_at DESC);` | `DROP INDEX CONCURRENTLY IF EXISTS idx_fleet_node_operation_node_created;` |
| 622_fleet_node_operation_idem_key | `CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS fleet_node_operation_idempotency_key_key ON fleet_node_operation (idempotency_key) WHERE idempotency_key <> '';` | `DROP INDEX CONCURRENTLY IF EXISTS fleet_node_operation_idempotency_key_key;` |
| 623_fleet_credential_profile_owner_key | `CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS fleet_credential_profile_owner_key ON fleet_credential_profile (owner_id);` | `DROP INDEX CONCURRENTLY IF EXISTS fleet_credential_profile_owner_key;` |
| 624_agent_runtime_fleet_managed | `ALTER TABLE agent_runtime ADD COLUMN IF NOT EXISTS managed_by TEXT NOT NULL DEFAULT '';` + `ALTER TABLE agent_runtime ADD COLUMN IF NOT EXISTS fleet_node_id UUID;` | `ALTER TABLE agent_runtime DROP COLUMN IF EXISTS managed_by;` + `ALTER TABLE agent_runtime DROP COLUMN IF EXISTS fleet_node_id;` |

- [ ] **Step 5: 写 fleet.sql 查询**

```sql
-- name: CreateFleetNode :one
INSERT INTO fleet_node (owner_id, namespace, name, container_id, container_name, daemon_id, image, spec, target_state, observed_state, idempotency_key)
VALUES (@owner_id, @namespace, @name, @container_id, @container_name, @daemon_id, @image, @spec, @target_state, @observed_state, @idempotency_key)
RETURNING *;

-- name: GetFleetNode :one
SELECT * FROM fleet_node WHERE id = @id AND namespace = @namespace;

-- name: GetFleetNodeByDaemonID :one
SELECT * FROM fleet_node WHERE daemon_id = @daemon_id AND namespace = @namespace;

-- name: ListFleetNodesByOwner :many
SELECT * FROM fleet_node
WHERE owner_id = @owner_id AND namespace = @namespace
ORDER BY created_at DESC
LIMIT @limit_count OFFSET @offset_count;

-- name: CountFleetNodesByOwner :one
SELECT count(*) FROM fleet_node WHERE owner_id = @owner_id AND namespace = @namespace;

-- name: UpdateFleetNodeContainer :exec
UPDATE fleet_node
SET container_id = @container_id, container_name = @container_name, updated_at = now()
WHERE id = @id AND namespace = @namespace;

-- name: UpdateFleetNodeTargetState :exec
UPDATE fleet_node
SET target_state = @target_state, generation = generation + 1, error = @error, updated_at = now()
WHERE id = @id AND namespace = @namespace;

-- name: UpdateFleetNodeObservedState :exec
UPDATE fleet_node
SET observed_state = @observed_state, daemon_ready = @daemon_ready,
    health_checked_at = now(), status_detail = @status_detail, error = @error, updated_at = now()
WHERE id = @id AND namespace = @namespace;

-- name: CreateFleetNodeCredential :one
INSERT INTO fleet_node_credential (node_id, token_hash)
VALUES (@node_id, @token_hash)
RETURNING *;

-- name: GetFleetNodeCredentialByHash :one
SELECT c.id, c.node_id, c.token_hash, c.revoked_at, c.created_at,
       n.owner_id, n.namespace, n.container_id, n.target_state, n.observed_state
FROM fleet_node_credential c
JOIN fleet_node n ON n.id = c.node_id
WHERE c.token_hash = @token_hash AND c.revoked_at IS NULL;

-- name: RevokeFleetNodeCredentialsByNode :exec
UPDATE fleet_node_credential SET revoked_at = now()
WHERE node_id = @node_id AND revoked_at IS NULL;

-- name: CreateFleetNodeOperation :one
INSERT INTO fleet_node_operation (node_id, operation, phase, idempotency_key, request_fingerprint)
VALUES (@node_id, @operation, @phase, @idempotency_key, @request_fingerprint)
RETURNING *;

-- name: GetFleetNodeOperation :one
SELECT * FROM fleet_node_operation WHERE id = @id;

-- name: GetFleetNodeOperationByIdempotencyKey :one
SELECT * FROM fleet_node_operation
WHERE idempotency_key = @idempotency_key AND idempotency_key <> '';

-- name: UpdateFleetNodeOperationPhase :exec
UPDATE fleet_node_operation
SET phase = @phase, approved_at = @approved_at, retry_count = @retry_count, error = @error, updated_at = now()
WHERE id = @id;

-- name: ListPendingFleetNodeOperations :many
SELECT * FROM fleet_node_operation
WHERE phase IN ('prepared', 'approved', 'executing')
ORDER BY created_at ASC;

-- name: CountActiveTasksForDaemon :one
SELECT count(*) FROM agent_task_queue q
JOIN agent a ON a.id = q.agent_id
JOIN agent_runtime r ON r.id = a.runtime_id
WHERE r.workspace_id = @workspace_id
  AND r.daemon_id = @daemon_id
  AND q.status IN ('dispatched', 'running', 'waiting_local_directory');

-- name: CountQueuedTasksForDaemon :one
SELECT count(*) FROM agent_task_queue q
JOIN agent a ON a.id = q.agent_id
JOIN agent_runtime r ON r.id = a.runtime_id
WHERE r.workspace_id = @workspace_id
  AND r.daemon_id = @daemon_id
  AND q.status IN ('queued', 'deferred');

-- name: GetFleetCredentialProfileByOwner :one
SELECT * FROM fleet_credential_profile WHERE owner_id = @owner_id;

-- name: UpsertFleetCredentialProfile :one
INSERT INTO fleet_credential_profile (owner_id, profile_name, config_version)
VALUES (@owner_id, @profile_name, @config_version)
ON CONFLICT (owner_id) DO UPDATE
SET profile_name = EXCLUDED.profile_name, config_version = EXCLUDED.config_version, updated_at = now()
RETURNING *;

-- name: ListStaleFleetNodes :many
SELECT * FROM fleet_node
WHERE namespace = @namespace AND target_state IN ('running', 'starting', 'stopping', 'rebooting')
  AND (health_checked_at IS NULL OR health_checked_at < now() - interval '30 seconds');
```

- [ ] **Step 6: 运行 make sqlc 并检查生成**

Run: `make sqlc`
Expected: 生成成功，`server/pkg/db/generated/fleet.sql.go` 出现；`git status` 只显示预期文件。

- [ ] **Step 7: 运行迁移与测试**

Run: `make worktree-env`（若尚未生成 env）然后 `cd server && go run ./cmd/migrate up`
Run: `cd server && go test ./internal/fleet/store -run 'TestCreateFleetNode' -count=1 -v`
Expected: PASS（数据库不可达时 testutil 会跳过，需显式 `DATABASE_URL`）。

- [ ] **Step 8: 提交**

```bash
git add server/migrations server/pkg/db/queries/fleet.sql server/pkg/db/generated server/internal/fleet/store/store_test.go
git commit -m "feat(fleet): add fleet node schema and sqlc queries"
```

---

### Task 2: Advisory Lock 屏障

**Files:**
- Create: `server/internal/fleet/barrier/barrier.go`
- Test: `server/internal/fleet/barrier/barrier_test.go`

**Interfaces:**
- Produces:
  - `func Key(namespace, nodeID string) int64`
  - `func Keys(namespace string, nodeIDs []string) []int64`（排序去重）
  - `func AcquireShared(ctx context.Context, tx pgx.Tx, keys ...int64) error`
  - `func AcquireExclusive(ctx context.Context, tx pgx.Tx, keys ...int64) error`

- [ ] **Step 1: 写失败的测试**

```go
package barrier

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	return testutil.IsolatedDB(t, dsn)
}

func TestKeyIsStableAndNamespaceScoped(t *testing.T) {
	a := Key("wt-1", "node-1")
	if a != Key("wt-1", "node-1") {
		t.Fatal("key must be stable")
	}
	if a == Key("wt-2", "node-1") || a == Key("wt-1", "node-2") {
		t.Fatal("key must include namespace and node id")
	}
}

func TestKeysDedupesAndSorts(t *testing.T) {
	got := Keys("wt", []string{"b", "a", "b"})
	want := []int64{Key("wt", "a"), Key("wt", "b")}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSharedLocksDoNotConflict(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tx1, err := pool.Begin(ctx)
	if err != nil { t.Fatal(err) }
	defer tx1.Rollback(ctx)
	if err := AcquireShared(ctx, tx1, Key("wt", "n")); err != nil { t.Fatal(err) }

	tx2, err := pool.Begin(ctx)
	if err != nil { t.Fatal(err) }
	defer tx2.Rollback(ctx)
	lockCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := AcquireShared(lockCtx, tx2, Key("wt", "n")); err != nil {
		t.Fatalf("shared lock must not block shared lock: %v", err)
	}
}

func TestExclusiveLockBlocksUntilHolderReleases(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tx1, err := pool.Begin(ctx)
	if err != nil { t.Fatal(err) }
	if err := AcquireExclusive(ctx, tx1, Key("wt", "n")); err != nil { t.Fatal(err) }

	tx2, err := pool.Begin(ctx)
	if err != nil { t.Fatal(err) }
	defer tx2.Rollback(ctx)
	lockCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if err := AcquireShared(lockCtx, tx2, Key("wt", "n")); err == nil {
		t.Fatal("shared lock must block while exclusive holder exists")
	}
	if err := tx1.Rollback(ctx); err != nil { t.Fatal(err) }
	if err := AcquireShared(ctx, tx2, Key("wt", "n")); err != nil {
		t.Fatalf("lock after release: %v", err)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd server && go test ./internal/fleet/barrier -count=1`
Expected: 编译失败（`undefined: Key`）。

- [ ] **Step 3: 实现 barrier**

```go
package barrier

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
)

// Key derives the 64-bit advisory-lock key every API/Fleet process must share
// for a node: the namespace keeps worktrees isolated, the node id keeps nodes
// independent.
func Key(namespace, nodeID string) int64 {
	sum := sha256.Sum256([]byte("fleet:" + namespace + ":" + nodeID))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

func Keys(namespace string, nodeIDs []string) []int64 {
	seen := make(map[int64]struct{}, len(nodeIDs))
	out := make([]int64, 0, len(nodeIDs))
	for _, id := range nodeIDs {
		k := Key(namespace, id)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// AcquireShared locks every key for read-only coordination. Take keys in the
// order Keys returns (sorted) so concurrent batch claims cannot deadlock.
func AcquireShared(ctx context.Context, tx pgx.Tx, keys ...int64) error {
	for _, k := range keys {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock_shared($1)", k); err != nil {
			return fmt.Errorf("acquire shared fleet lock %d: %w", k, err)
		}
	}
	return nil
}

func AcquireExclusive(ctx context.Context, tx pgx.Tx, keys ...int64) error {
	sorted := append([]int64(nil), keys...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	for _, k := range sorted {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", k); err != nil {
			return fmt.Errorf("acquire exclusive fleet lock %d: %w", k, err)
		}
	}
	return nil
}
```

- [ ] **Step 4: 运行测试通过并提交**

Run: `cd server && go test ./internal/fleet/barrier -count=1 -v`
Expected: PASS。
```bash
git add server/internal/fleet/barrier
git commit -m "feat(fleet): add advisory-lock barrier for node coordination"
```

---

### Task 3: Fleet 配置加载

**Files:**
- Create: `server/internal/fleet/config.go`
- Test: `server/internal/fleet/config_test.go`

**Interfaces:**
- Produces:
  - `type Config struct { Listen, Namespace, SecretFile, DockerHost, NodeNetwork, RuntimeImage, APIURL, DatabaseURL, CredentialsFile string; MaxNodesPerOwner int; Specs []Spec }`
  - `type Spec struct { ID string; CPU float64; MemoryMB int64; PidsLimit int64; MaxConcurrent int }`
  - `func Load(getenv func(string) string) (Config, error)`
  - `func (c Config) Secret() (string, error)`

- [ ] **Step 1: 写失败测试**

```go
package fleet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envFrom(values map[string]string) func(string) string {
	return func(k string) string { return values[k] }
}

func validEnv(t *testing.T) map[string]string {
	t.Helper()
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	creds := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(creds, []byte("{\"users\":{},\"profiles\":{}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"MULTICA_FLEET_LISTEN":           "127.0.0.1:8091",
		"MULTICA_FLEET_NAMESPACE":        "wt-test",
		"MULTICA_FLEET_SECRET_FILE":      secret,
		"MULTICA_FLEET_DOCKER_HOST":      "unix:///var/run/docker.sock",
		"MULTICA_FLEET_NODE_NETWORK":     "multica-fleet-wt-test",
		"MULTICA_FLEET_RUNTIME_IMAGE":    "multica/runtime-claude:test",
		"MULTICA_FLEET_API_URL":          "http://host.docker.internal:8080",
		"DATABASE_URL":                   "postgres://u:p@postgres:5432/db?sslmode=disable",
		"MULTICA_FLEET_CREDENTIALS_FILE": creds,
	}
}

func TestLoadAcceptsMinimalValidEnvironment(t *testing.T) {
	cfg, err := Load(envFrom(validEnv(t)))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:8091" || cfg.MaxNodesPerOwner != 2 || len(cfg.Specs) == 0 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	secret, err := cfg.Secret()
	if err != nil || secret != "s3cret" {
		t.Fatalf("secret = %q, err = %v", secret, err)
	}
}

func TestLoadRejectsMissingAndMalformedValues(t *testing.T) {
	cases := []struct {
		name string
		mut  func(map[string]string)
		want string
	}{
		{"missing namespace", func(m map[string]string) { delete(m, "MULTICA_FLEET_NAMESPACE") }, "MULTICA_FLEET_NAMESPACE"},
		{"bad listen", func(m map[string]string) { m["MULTICA_FLEET_LISTEN"] = "0.0.0.0:8091" }, "loopback"},
		{"missing secret file", func(m map[string]string) { m["MULTICA_FLEET_SECRET_FILE"] = "/nonexistent" }, "secret"},
		{"missing database", func(m map[string]string) { delete(m, "DATABASE_URL") }, "DATABASE_URL"},
		{"bad owner limit", func(m map[string]string) { m["MULTICA_FLEET_MAX_NODES_PER_OWNER"] = "0" }, "MAX_NODES"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := validEnv(t)
			tc.mut(env)
			_, err := Load(envFrom(env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want substring %q", err, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd server && go test ./internal/fleet -run TestLoad -count=1`
Expected: 编译失败（`undefined: Load`）。

- [ ] **Step 3: 实现 config.go**

```go
package fleet

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

type Spec struct {
	ID            string  `json:"id"`
	CPU           float64 `json:"cpu"`
	MemoryMB      int64   `json:"memory_mb"`
	PidsLimit     int64   `json:"pids_limit"`
	MaxConcurrent int     `json:"max_concurrent"`
}

type Config struct {
	Listen           string
	Namespace        string
	SecretFile       string
	DockerHost       string
	NodeNetwork      string
	RuntimeImage     string
	APIURL           string
	DatabaseURL      string
	CredentialsFile  string
	MaxNodesPerOwner int
	Specs            []Spec
}

func Load(getenv func(string) string) (Config, error) {
	get := func(key string) string { return strings.TrimSpace(getenv(key)) }

	cfg := Config{
		Listen:           get("MULTICA_FLEET_LISTEN"),
		Namespace:        get("MULTICA_FLEET_NAMESPACE"),
		SecretFile:       get("MULTICA_FLEET_SECRET_FILE"),
		DockerHost:       get("MULTICA_FLEET_DOCKER_HOST"),
		NodeNetwork:      get("MULTICA_FLEET_NODE_NETWORK"),
		RuntimeImage:     get("MULTICA_FLEET_RUNTIME_IMAGE"),
		APIURL:           get("MULTICA_FLEET_API_URL"),
		DatabaseURL:      get("DATABASE_URL"),
		CredentialsFile:  get("MULTICA_FLEET_CREDENTIALS_FILE"),
		MaxNodesPerOwner: 2,
		Specs:            []Spec{{ID: "standard", CPU: 2, MemoryMB: 4096, PidsLimit: 256, MaxConcurrent: 1}},
	}
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:8091"
	}
	for key, val := range map[string]string{
		"MULTICA_FLEET_NAMESPACE":        cfg.Namespace,
		"MULTICA_FLEET_SECRET_FILE":      cfg.SecretFile,
		"MULTICA_FLEET_DOCKER_HOST":      cfg.DockerHost,
		"MULTICA_FLEET_NODE_NETWORK":     cfg.NodeNetwork,
		"MULTICA_FLEET_RUNTIME_IMAGE":    cfg.RuntimeImage,
		"MULTICA_FLEET_API_URL":          cfg.APIURL,
		"DATABASE_URL":                   cfg.DatabaseURL,
		"MULTICA_FLEET_CREDENTIALS_FILE": cfg.CredentialsFile,
	} {
		if val == "" {
			return Config{}, fmt.Errorf("%s is required", key)
		}
	}
	host, _, err := net.SplitHostPort(cfg.Listen)
	if err != nil || (host != "127.0.0.1" && host != "localhost" && host != "::1") {
		return Config{}, fmt.Errorf("MULTICA_FLEET_LISTEN must bind loopback, got %q", cfg.Listen)
	}
	if raw := get("MULTICA_FLEET_MAX_NODES_PER_OWNER"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("MULTICA_FLEET_MAX_NODES_PER_OWNER must be a positive integer")
		}
		cfg.MaxNodesPerOwner = n
	}
	if _, err := os.Stat(cfg.SecretFile); err != nil {
		return Config{}, fmt.Errorf("fleet secret file is unreadable: %w", err)
	}
	return cfg, nil
}

func (c Config) Secret() (string, error) {
	data, err := os.ReadFile(c.SecretFile)
	if err != nil {
		return "", fmt.Errorf("read fleet secret: %w", err)
	}
	secret := strings.TrimSpace(string(data))
	if secret == "" {
		return "", fmt.Errorf("fleet secret file is empty")
	}
	return secret, nil
}
```

- [ ] **Step 4: 运行测试通过并提交**

Run: `cd server && go test ./internal/fleet -run TestLoad -count=1 -v`
Expected: PASS。
```bash
git add server/internal/fleet/config.go server/internal/fleet/config_test.go
git commit -m "feat(fleet): load and validate fleet configuration"
```

---

### Task 4: Fleet Store（PostgreSQL 事务层）

**Files:**
- Create: `server/internal/fleet/store/store.go`
- Modify/Test: `server/internal/fleet/store/store_test.go`（追加）

**Interfaces:**
- Consumes: Task 1 的 `db.*` 生成函数。
- Produces:
  - `var ErrLimitReached = errors.New("fleet node limit reached")`
  - `type Store struct { pool *pgxpool.Pool }`
  - `func New(pool *pgxpool.Pool) *Store`
  - `func (s *Store) Queries() *db.Queries`
  - `func (s *Store) WithinTx(ctx context.Context, fn func(q *db.Queries) error) error`
  - `func (s *Store) ReserveNode(ctx context.Context, p db.CreateFleetNodeParams, maxPerOwner int) (db.FleetNode, error)`
  - `func (s *Store) RevokeCredentials(ctx context.Context, nodeID pgtype.UUID) error`
  - `func (s *Store) ListStaleNodes(ctx context.Context, namespace string) ([]db.FleetNode, error)`

- [ ] **Step 1: 追加失败的测试**

```go
func TestReserveNodeEnforcesPerOwnerLimitUnderConcurrency(t *testing.T) {
	pool := testPool(t)
	owner := pgtype.UUID{Bytes: [16]byte{9}, Valid: true}
	s := New(pool)
	makeParams := func(name string) db.CreateFleetNodeParams {
		return db.CreateFleetNodeParams{
			OwnerID: owner, Namespace: "wt-limit", Name: name, DaemonID: "daemon-" + name,
			Image: "img", Spec: []byte("{}"), TargetState: "launching",
		}
	}

	errs := make(chan error, 2)
	for _, name := range []string{"a", "b"} {
		go func(name string) {
			_, err := s.ReserveNode(context.Background(), makeParams(name), 1)
			errs <- err
		}(name)
	}
	var ok, limited int
	for i := 0; i < 2; i++ {
		switch err := <-errs; {
		case err == nil:
			ok++
		case errors.Is(err, ErrLimitReached):
			limited++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || limited != 1 {
		t.Fatalf("ok=%d limited=%d, want 1/1", ok, limited)
	}
}

func TestStoreQueriesAreNamespaceScoped(t *testing.T) {
	pool := testPool(t)
	s := New(pool)
	owner := pgtype.UUID{Bytes: [16]byte{10}, Valid: true}
	node, err := s.Queries().CreateFleetNode(context.Background(), db.CreateFleetNodeParams{
		OwnerID: owner, Namespace: "wt-a", Name: "n", DaemonID: "daemon-a",
		Image: "img", Spec: []byte("{}"), TargetState: "launching",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Queries().GetFleetNode(context.Background(), db.GetFleetNodeParams{
		ID: node.ID, Namespace: "wt-b",
	}); err == nil {
		t.Fatal("cross-namespace read must miss")
	}
}

func TestRevokeCredentialsMakesHashUnresolvable(t *testing.T) {
	pool := testPool(t)
	s := New(pool)
	owner := pgtype.UUID{Bytes: [16]byte{11}, Valid: true}
	node, err := s.Queries().CreateFleetNode(context.Background(), db.CreateFleetNodeParams{
		OwnerID: owner, Namespace: "wt-c", Name: "n", DaemonID: "daemon-c",
		Image: "img", Spec: []byte("{}"), TargetState: "running",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Queries().CreateFleetNodeCredential(context.Background(), db.CreateFleetNodeCredentialParams{
		NodeID: node.ID, TokenHash: "h-revoke",
	}); err != nil {
		t.Fatalf("create credential: %v", err)
	}
	if err := s.RevokeCredentials(context.Background(), node.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	_, err = s.Queries().GetFleetNodeCredentialByHash(context.Background(), db.GetFleetNodeCredentialByHashParams{
		TokenHash: "h-revoke",
	})
	if err == nil {
		t.Fatal("revoked credential must not resolve")
	}
}
```

测试文件需要补充 import：`"errors"`、`"github.com/jackc/pgx/v5/pgtype"`、`db "github.com/multica-ai/multica/server/pkg/db/generated"`。

- [ ] **Step 2: 运行确认失败**

Run: `cd server && go test ./internal/fleet/store -run 'TestReserveNode|TestStoreQueries|TestRevokeCredentials' -count=1`
Expected: 编译失败（`undefined: New`）。

- [ ] **Step 3: 实现 store.go**

```go
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/fleet/barrier"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var ErrLimitReached = errors.New("fleet node limit reached")

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Queries() *db.Queries { return db.New(s.pool) }

func (s *Store) WithinTx(ctx context.Context, fn func(q *db.Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin fleet tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(db.New(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReserveNode serializes creations per owner with a transaction-scoped advisory
// lock, so two concurrent requests cannot both pass the limit check.
func (s *Store) ReserveNode(ctx context.Context, p db.CreateFleetNodeParams, maxPerOwner int) (db.FleetNode, error) {
	var out db.FleetNode
	err := s.WithinTx(ctx, func(q *db.Queries) error {
		ownerKey := pgtype.Int8{}
		_ = ownerKey
		if _, err := q.CountFleetNodesByOwner(ctx, db.CountFleetNodesByOwnerParams{
			OwnerID: p.OwnerID, Namespace: p.Namespace,
		}); err != nil {
			return err
		}
		// Count is taken after the lock below; keep the order explicit.
		if _, err := q.CountFleetNodesByOwner(ctx, db.CountFleetNodesByOwnerParams{
			OwnerID: p.OwnerID, Namespace: p.Namespace,
		}); err != nil {
			return err
		}
		return nil
	})
	_ = err
	return out, nil
}

func (s *Store) RevokeCredentials(ctx context.Context, nodeID pgtype.UUID) error {
	return s.Queries().RevokeFleetNodeCredentialsByNode(ctx, db.RevokeFleetNodeCredentialsByNodeParams{NodeID: nodeID})
}

func (s *Store) ListStaleNodes(ctx context.Context, namespace string) ([]db.FleetNode, error) {
	return s.Queries().ListStaleFleetNodes(ctx, db.ListStaleFleetNodesParams{Namespace: namespace})
}

var _ = barrier.Key
```

上面的 `ReserveNode` 骨架故意不完整，按下面完整实现替换（不要保留临时变量）：

```go
func (s *Store) ReserveNode(ctx context.Context, p db.CreateFleetNodeParams, maxPerOwner int) (db.FleetNode, error) {
	var out db.FleetNode
	err := s.WithinTx(ctx, func(q *db.Queries) error {
		// Owner-scoped lock: serialize concurrent creations for the same owner
		// without blocking other owners or other namespaces.
		if _, err := q.RawExec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))",
			"fleet-owner:"+p.Namespace+":"+UUIDText(p.OwnerID)); err != nil {
			return err
		}
		count, err := q.CountFleetNodesByOwner(ctx, db.CountFleetNodesByOwnerParams{
			OwnerID: p.OwnerID, Namespace: p.Namespace,
		})
		if err != nil {
			return err
		}
		if count >= int64(maxPerOwner) {
			return ErrLimitReached
		}
		out, err = q.CreateFleetNode(ctx, p)
		return err
	})
	return out, err
}
```

sqlc 不生成 `RawExec`；改为在 store 里直接用 `s.pool` 执行锁再进入 sqlc 事务：

```go
func (s *Store) ReserveNode(ctx context.Context, p db.CreateFleetNodeParams, maxPerOwner int) (db.FleetNode, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return db.FleetNode{}, fmt.Errorf("begin reserve: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))",
		"fleet-owner:"+p.Namespace+":"+UUIDText(p.OwnerID)); err != nil {
		return db.FleetNode{}, fmt.Errorf("lock owner: %w", err)
	}
	q := db.New(tx)
	count, err := q.CountFleetNodesByOwner(ctx, db.CountFleetNodesByOwnerParams{
		OwnerID: p.OwnerID, Namespace: p.Namespace,
	})
	if err != nil {
		return db.FleetNode{}, err
	}
	if count >= int64(maxPerOwner) {
		return db.FleetNode{}, ErrLimitReached
	}
	node, err := q.CreateFleetNode(ctx, p)
	if err != nil {
		return db.FleetNode{}, err
	}
	return node, tx.Commit(ctx)
}

func UUIDText(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	b := id.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
```

- [ ] **Step 4: 运行测试通过并提交**

Run: `cd server && go test ./internal/fleet/store -count=1 -v`
Expected: PASS（含 Task 1 的两个测试）。
```bash
git add server/internal/fleet/store
git commit -m "feat(fleet): add transactional store with owner-scoped reservation"
```

---

### Task 5: 状态映射

**Files:**
- Create: `server/internal/fleet/state.go`
- Test: `server/internal/fleet/state_test.go`

**Interfaces:**
- Produces:
  - `const (StateLaunching = "launching"; StateStarting = "starting"; StateRunning = "running"; StateStopping = "stopping"; StateStopped = "stopped"; StateRebooting = "rebooting"; StateTerminating = "terminating"; StateTerminated = "terminated"; StateFailed = "failed"; StateInstanceMissing = "instance_missing")`
  - `type Observed struct { ContainerExists bool; ContainerState string; Healthy bool; DaemonReady bool; HealthFresh bool }`
  - `func DeriveDisplayState(target string, obs Observed) string`

- [ ] **Step 1: 写失败测试**

```go
package fleet

import "testing"

func TestDeriveDisplayState(t *testing.T) {
	running := Observed{ContainerExists: true, ContainerState: "running", Healthy: true, DaemonReady: true, HealthFresh: true}
	cases := []struct {
		name   string
		target string
		obs    Observed
		want   string
	}{
		{"launching stays launching", StateLaunching, Observed{}, StateLaunching},
		{"running and ready", StateRunning, running, StateRunning},
		{"running but daemon not ready", StateRunning, Observed{ContainerExists: true, ContainerState: "running", Healthy: true}, StateStarting},
		{"running but health stale", StateRunning, Observed{ContainerExists: true, ContainerState: "running", Healthy: true, DaemonReady: true}, StateStarting},
		{"running but container missing", StateRunning, Observed{}, StateInstanceMissing},
		{"running but exited", StateRunning, Observed{ContainerExists: true, ContainerState: "exited", Healthy: false}, StateStopped},
		{"stopping", StateStopping, running, StateStopping},
		{"stopped", StateStopped, Observed{ContainerExists: true, ContainerState: "exited"}, StateStopped},
		{"rebooting", StateRebooting, running, StateRebooting},
		{"terminating", StateTerminating, running, StateTerminating},
		{"terminated", StateTerminated, Observed{}, StateTerminated},
		{"failed", StateFailed, running, StateFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeriveDisplayState(tc.target, tc.obs); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `cd server && go test ./internal/fleet -run TestDeriveDisplayState -count=1`
Expected: 编译失败（`undefined: DeriveDisplayState`）。

- [ ] **Step 3: 实现 state.go**

```go
package fleet

const (
	StateLaunching      = "launching"
	StateStarting       = "starting"
	StateRunning        = "running"
	StateStopping       = "stopping"
	StateStopped        = "stopped"
	StateRebooting      = "rebooting"
	StateTerminating    = "terminating"
	StateTerminated     = "terminated"
	StateFailed         = "failed"
	StateInstanceMissing = "instance_missing"
)

type Observed struct {
	ContainerExists bool
	ContainerState  string
	Healthy         bool
	DaemonReady     bool
	HealthFresh     bool
}

// DeriveDisplayState separates the user-visible state from the four signals
// kept in the store (target state, container observation, daemon readiness,
// health freshness). A running target that lost its container or health must
// never read as executable.
func DeriveDisplayState(target string, obs Observed) string {
	switch target {
	case StateLaunching:
		return StateLaunching
	case StateTerminated:
		return StateTerminated
	case StateFailed:
		return StateFailed
	case StateStopping:
		return StateStopping
	case StateRebooting:
		return StateRebooting
	case StateTerminating:
		return StateTerminating
	case StateStopped:
		return StateStopped
	case StateStarting:
		if !obs.ContainerExists {
			return StateInstanceMissing
		}
		if obs.ContainerState == "running" && obs.Healthy && obs.DaemonReady && obs.HealthFresh {
			return StateRunning
		}
		return StateStarting
	case StateRunning:
		if !obs.ContainerExists {
			return StateInstanceMissing
		}
		if obs.ContainerState == "exited" || obs.ContainerState == "dead" {
			return StateStopped
		}
		if obs.ContainerState == "running" && obs.Healthy && obs.DaemonReady && obs.HealthFresh {
			return StateRunning
		}
		return StateStarting
	default:
		return StateFailed
	}
}
```

- [ ] **Step 4: 运行测试通过并提交**

Run: `cd server && go test ./internal/fleet -run TestDeriveDisplayState -count=1 -v`
Expected: PASS。
```bash
git add server/internal/fleet/state.go server/internal/fleet/state_test.go
git commit -m "feat(fleet): derive display state from observed signals"
```

---

### Task 6: 凭证 profile 解析

**Files:**
- Create: `server/internal/fleet/credentials.go`
- Test: `server/internal/fleet/credentials_test.go`

**Interfaces:**
- Produces:
  - `type Profile struct { Env map[string]string }`
  - `type Credentials struct { users map[string]string; profiles map[string]Profile }`
  - `func LoadCredentials(path string) (*Credentials, error)`
  - `func (c *Credentials) Resolve(ownerID string) (Profile, error)`
  - `var ErrProfileMissing = errors.New("claude credential profile missing")`
  - `var ErrProfileInvalid = errors.New("claude credential profile invalid")`

- [ ] **Step 1: 写失败测试**

```go
package fleet

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeCredentials(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveUsesOwnerMapping(t *testing.T) {
	path := writeCredentials(t, "{\"users\":{\"owner-1\":\"work\"},\"profiles\":{\"work\":{\"env\":{\"ANTHROPIC_API_KEY\":\"sk-test\"}}}}")
	creds, err := LoadCredentials(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	profile, err := creds.Resolve("owner-1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if profile.Env["ANTHROPIC_API_KEY"] != "sk-test" {
		t.Fatalf("unexpected env: %+v", profile.Env)
	}
}

func TestResolveRejectsMissingOwnerAndUnknownProfile(t *testing.T) {
	creds, err := LoadCredentials(writeCredentials(t, "{\"users\":{},\"profiles\":{}}"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := creds.Resolve("owner-1"); !errors.Is(err, ErrProfileMissing) {
		t.Fatalf("err = %v, want ErrProfileMissing", err)
	}

	creds, err = LoadCredentials(writeCredentials(t, "{\"users\":{\"owner-1\":\"ghost\"},\"profiles\":{}}"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := creds.Resolve("owner-1"); !errors.Is(err, ErrProfileInvalid) {
		t.Fatalf("err = %v, want ErrProfileInvalid", err)
	}
}

func TestLoadCredentialsRejectsInjectedOrUnknownEnv(t *testing.T) {
	cases := []string{
		"{\"users\":{\"o\":\"p\"},\"profiles\":{\"p\":{\"env\":{\"MULTICA_TOKEN\":\"x\"}}}}",
		"{\"users\":{\"o\":\"p\"},\"profiles\":{\"p\":{\"env\":{\"ANTHROPIC_API_KEY\":\"a\\nb\"}}}}",
	}
	for _, body := range cases {
		if _, err := LoadCredentials(writeCredentials(t, body)); err == nil {
			t.Fatalf("expected rejection for %s", body)
		}
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `cd server && go test ./internal/fleet -run TestResolve -count=1`
Expected: 编译失败（`undefined: LoadCredentials`）。

- [ ] **Step 3: 实现 credentials.go**

```go
package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

var (
	ErrProfileMissing = errors.New("claude credential profile missing")
	ErrProfileInvalid = errors.New("claude credential profile invalid")
)

type Profile struct {
	Env map[string]string `json:"env"`
}

type credentialsFile struct {
	Users    map[string]string  `json:"users"`
	Profiles map[string]Profile `json:"profiles"`
}

type Credentials struct {
	users    map[string]string
	profiles map[string]Profile
}

// allowedModelEnv is the only environment the node entrypoint may export into
// the daemon process. Anything else (MULTICA_*, PATH, HOME) is rejected so a
// credentials file cannot smuggle platform credentials into the model process.
var allowedModelEnv = map[string]bool{
	"ANTHROPIC_API_KEY":     true,
	"ANTHROPIC_AUTH_TOKEN":  true,
	"ANTHROPIC_BASE_URL":    true,
	"ANTHROPIC_MODEL":       true,
	"CLAUDE_CODE_USE_BEDROCK": true,
	"API_TIMEOUT_MS":        true,
}

func LoadCredentials(path string) (*Credentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read fleet credentials: %w", err)
	}
	var f credentialsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse fleet credentials: %w", err)
	}
	for name, profile := range f.Profiles {
		for key, value := range profile.Env {
			if !allowedModelEnv[key] {
				return nil, fmt.Errorf("credential profile %q: env %q is not an allowed model variable", name, key)
			}
			if strings.ContainsAny(value, "\r\n\x00") {
				return nil, fmt.Errorf("credential profile %q: env %q contains a newline or NUL", name, key)
			}
			if len(value) > 8192 {
				return nil, fmt.Errorf("credential profile %q: env %q is too long", name, key)
			}
		}
	}
	return &Credentials{users: f.Users, profiles: f.Profiles}, nil
}

func (c *Credentials) Resolve(ownerID string) (Profile, error) {
	name, ok := c.users[ownerID]
	if !ok || name == "" {
		return Profile{}, ErrProfileMissing
	}
	profile, ok := c.profiles[name]
	if !ok {
		return Profile{}, ErrProfileInvalid
	}
	return profile, nil
}
```

- [ ] **Step 4: 运行测试通过并提交**

Run: `cd server && go test ./internal/fleet -run 'TestResolve|TestLoadCredentials' -count=1 -v`
Expected: PASS。
```bash
git add server/internal/fleet/credentials.go server/internal/fleet/credentials_test.go
git commit -m "feat(fleet): resolve owner credential profiles with env allowlist"
```

---

### Task 7: Docker Provider 接口与容器规格

**Files:**
- Create: `server/internal/fleet/docker/provider_api.go`
- Create: `server/internal/fleet/docker/provider.go`
- Test: `server/internal/fleet/docker/provider_test.go`

**Interfaces:**
- Produces:
  - `type NodeSpec struct { NodeID, Namespace, Name, Image string; Env []string; DataVolume, SecretsVolume, Network string; MemoryMB int64; CPU float64; PidsLimit int64; Labels map[string]string }`
  - `func BuildNodeSpec(nodeID, namespace, name, image, dataVolume, secretsVolume, network string, memoryMB int64, cpu float64, pids int64) NodeSpec`
  - `type Provider interface { Ping(ctx context.Context) error; EnsureNetwork(ctx context.Context, name string, labels map[string]string) error; EnsureVolume(ctx context.Context, name string, labels map[string]string) error; CreateContainer(ctx context.Context, spec NodeSpec) (string, error); WriteSecrets(ctx context.Context, containerID string, files map[string][]byte) error; Start(ctx context.Context, containerID string) error; Stop(ctx context.Context, containerID string, timeout time.Duration) error; Restart(ctx context.Context, containerID string, timeout time.Duration) error; Remove(ctx context.Context, containerID string, removeVolumes bool) error; RemoveVolume(ctx context.Context, name string) error; Inspect(ctx context.Context, containerID string) (ObservedContainer, error); ListByLabels(ctx context.Context, labels map[string]string) ([]string, error); ExecHealth(ctx context.Context, containerID string) ([]byte, error) }`
  - `type ObservedContainer struct { ID, State string; Healthy bool }`
  - `func ParseHealthReport(body []byte) (HealthReport, error)`（Task 9 使用）
  - `type HealthReport struct { Status string; ActiveTaskCount, RunningTaskCount, ResourceWaitTaskCount, PendingTerminalReportCount int; Agents []string }`

- [ ] **Step 1: 写失败的测试**

```go
package docker

import (
	"strings"
	"testing"
)

func TestBuildNodeSpecSetsLabelsResourcesAndNoSecrets(t *testing.T) {
	spec := BuildNodeSpec("node-1", "wt-a", "n1", "img:test", "vol-data", "vol-secrets", "net-a", 4096, 2, 256)
	if spec.Labels["multica.fleet.namespace"] != "wt-a" || spec.Labels["multica.fleet.node"] != "node-1" {
		t.Fatalf("missing labels: %+v", spec.Labels)
	}
	if spec.MemoryMB != 4096 || spec.CPU != 2 || spec.PidsLimit != 256 {
		t.Fatalf("resources: %+v", spec)
	}
	for _, env := range spec.Env {
		if strings.Contains(env, "TOKEN") || strings.Contains(env, "API_KEY") {
			t.Fatalf("secrets must not be in Config.Env: %s", env)
		}
	}
}

func TestBuildNodeSpecRejectsForbiddenEnv(t *testing.T) {
	cases := [][]string{
		{"MULTICA_TOKEN=x"},
		{"ANTHROPIC_API_KEY=sk-x"},
		{"PATH=/evil"},
	}
	for _, env := range cases {
		if err := validateNodeEnv(env); err == nil {
			t.Fatalf("expected rejection for %v", env)
		}
	}
	if err := validateNodeEnv([]string{"MULTICA_SERVER_URL=http://host.docker.internal:8080"}); err != nil {
		t.Fatalf("platform url must be allowed: %v", err)
	}
}

func TestParseHealthReport(t *testing.T) {
	body := []byte(`{"status":"running","active_task_count":1,"running_task_count":1,"resource_wait_task_count":0,"pending_terminal_report_count":2,"agents":["claude"]}`)
	report, err := ParseHealthReport(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if report.Status != "running" || report.ActiveTaskCount != 1 || report.PendingTerminalReportCount != 2 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if len(report.Agents) != 1 || report.Agents[0] != "claude" {
		t.Fatalf("agents: %+v", report.Agents)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `cd server && go test ./internal/fleet/docker -count=1`
Expected: 编译失败（`undefined: BuildNodeSpec`）。

- [ ] **Step 3: 实现 provider_api.go**

```go
package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type ObservedContainer struct {
	ID      string
	State   string
	Healthy bool
}

type HealthReport struct {
	Status                     string   `json:"status"`
	ActiveTaskCount            int      `json:"active_task_count"`
	RunningTaskCount           int      `json:"running_task_count"`
	ResourceWaitTaskCount      int      `json:"resource_wait_task_count"`
	PendingTerminalReportCount int      `json:"pending_terminal_report_count"`
	Agents                     []string `json:"agents"`
}

func ParseHealthReport(body []byte) (HealthReport, error) {
	var report HealthReport
	if err := json.Unmarshal(body, &report); err != nil {
		return HealthReport{}, fmt.Errorf("parse daemon health: %w", err)
	}
	return report, nil
}

type NodeSpec struct {
	NodeID        string
	Namespace     string
	Name          string
	Image         string
	Env           []string
	DataVolume    string
	SecretsVolume string
	Network       string
	MemoryMB      int64
	CPU           float64
	PidsLimit     int64
	Labels        map[string]string
}

func BuildNodeSpec(nodeID, namespace, name, image, dataVolume, secretsVolume, network string, memoryMB int64, cpu float64, pids int64) NodeSpec {
	labels := map[string]string{
		"multica.fleet.namespace": namespace,
		"multica.fleet.node":      nodeID,
		"multica.fleet.role":      "node",
	}
	return NodeSpec{
		NodeID: nodeID, Namespace: namespace, Name: name, Image: image,
		DataVolume: dataVolume, SecretsVolume: secretsVolume, Network: network,
		MemoryMB: memoryMB, CPU: cpu, PidsLimit: pids,
		Labels: labels,
	}
}

// validateNodeEnv rejects any variable that could carry a secret or mutate the
// platform from inside the node. The allowed set is intentionally explicit.
func validateNodeEnv(env []string) error {
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			return fmt.Errorf("invalid env entry %q", entry)
		}
		switch {
		case key == "MULTICA_SERVER_URL", key == "MULTICA_DAEMON_ID", key == "MULTICA_DAEMON_DEVICE_NAME",
			key == "MULTICA_WORKSPACES_ROOT", key == "MULTICA_DAEMON_AUTO_UPDATE", key == "MULTICA_DAEMON_AUTO_RELOAD",
			key == "HOME":
		default:
			return fmt.Errorf("env %q is not allowed in the node container", key)
		}
	}
	return nil
}

type Provider interface {
	Ping(ctx context.Context) error
	InspectImage(ctx context.Context, image string) error
	EnsureNetwork(ctx context.Context, name string, labels map[string]string) error
	EnsureVolume(ctx context.Context, name string, labels map[string]string) error
	CreateContainer(ctx context.Context, spec NodeSpec) (string, error)
	WriteSecrets(ctx context.Context, containerID string, files map[string][]byte) error
	Start(ctx context.Context, containerID string) error
	Stop(ctx context.Context, containerID string, timeout time.Duration) error
	Restart(ctx context.Context, containerID string, timeout time.Duration) error
	Remove(ctx context.Context, containerID string, removeVolumes bool) error
	RemoveVolume(ctx context.Context, name string) error
	Inspect(ctx context.Context, containerID string) (ObservedContainer, error)
	ListByLabels(ctx context.Context, labels map[string]string) ([]string, error)
	ExecHealth(ctx context.Context, containerID string) ([]byte, error)
}
```

- [ ] **Step 4: 实现 SDK provider（provider.go）**

```go
package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/api/types"
	"github.com/docker/go-connections/nat"
)

type SDK struct {
	cli     *client.Client
	user    string
	healthCmd []string
}

func NewSDK(host string) (*SDK, error) {
	cli, err := client.NewClientWithOpts(client.WithHost(host), client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &SDK{cli: cli, user: "1000:1000", healthCmd: []string{"curl", "-fsS", "http://127.0.0.1:19514/health"}}, nil
}

func (s *SDK) Ping(ctx context.Context) error {
	_, err := s.cli.Ping(ctx)
	return err
}

func (s *SDK) InspectImage(ctx context.Context, image string) error {
	_, err := s.cli.ImageInspect(ctx, image)
	return err
}

func (s *SDK) EnsureNetwork(ctx context.Context, name string, labels map[string]string) error {
	_, err := s.cli.NetworkCreate(ctx, name, network.CreateOptions{Driver: "bridge", Labels: labels})
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("create network: %w", err)
	}
	return nil
}

func (s *SDK) EnsureVolume(ctx context.Context, name string, labels map[string]string) error {
	_, err := s.cli.VolumeCreate(ctx, volume.CreateOptions{Name: name, Labels: labels})
	return err
}

func (s *SDK) CreateContainer(ctx context.Context, spec NodeSpec) (string, error) {
	if err := validateNodeEnv(spec.Env); err != nil {
		return "", err
	}
	resp, err := s.cli.ContainerCreate(ctx,
		&container.Config{
			Image: spec.Image,
			Env:   spec.Env,
			User:  s.user,
			Labels: spec.Labels,
			Healthcheck: &container.HealthConfig{
				Test:     append([]string{"CMD"}, s.healthCmd...),
				Interval: 10 * time.Second, Timeout: 5 * time.Second, Retries: 3, StartPeriod: 120 * time.Second,
			},
		},
		&container.HostConfig{
			RestartPolicy: container.RestartPolicy{Name: "no"},
			NetworkMode:   container.NetworkMode(spec.Network),
			Resources: container.Resources{
				Memory: spec.MemoryMB * 1024 * 1024, NanoCPUs: int64(spec.CPU * 1e9), PidsLimit: &spec.PidsLimit,
			},
			Mounts: []container.Mount{
				{Type: "volume", Source: spec.DataVolume, Target: "/home/multica"},
				{Type: "volume", Source: spec.SecretsVolume, Target: "/run/secrets/runtime", ReadOnly: true},
			},
		},
		nil, nil, spec.Name)
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}
	return resp.ID, nil
}

func (s *SDK) WriteSecrets(ctx context.Context, containerID string, files map[string][]byte) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		hdr := &tar.Header{Name: "run/secrets/runtime/" + name, Mode: 0o600, Size: int64(len(body))}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write(body); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return s.cli.CopyToContainer(ctx, containerID, "/", &buf, container.CopyToContainerOptions{})
}

func (s *SDK) Start(ctx context.Context, containerID string) error {
	return s.cli.ContainerStart(ctx, containerID, container.StartOptions{})
}

func (s *SDK) Stop(ctx context.Context, containerID string, timeout time.Duration) error {
	return s.cli.ContainerStop(ctx, containerID, container.StopOptions{Timeout: ptrInt(int(timeout.Seconds()))})
}

func (s *SDK) Restart(ctx context.Context, containerID string, timeout time.Duration) error {
	return s.cli.ContainerRestart(ctx, containerID, container.StopOptions{Timeout: ptrInt(int(timeout.Seconds()))})
}

func (s *SDK) Remove(ctx context.Context, containerID string, removeVolumes bool) error {
	return s.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true, RemoveVolumes: removeVolumes})
}

func (s *SDK) RemoveVolume(ctx context.Context, name string) error {
	return s.cli.VolumeRemove(ctx, name, true)
}

func (s *SDK) Inspect(ctx context.Context, containerID string) (ObservedContainer, error) {
	info, err := s.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return ObservedContainer{}, err
	}
	healthy := info.State != nil && info.State.Health != nil && info.State.Health.Status == "healthy"
	state := ""
	if info.State != nil {
		state = info.State.Status
	}
	return ObservedContainer{ID: info.ID, State: state, Healthy: healthy}, nil
}

func (s *SDK) ListByLabels(ctx context.Context, labels map[string]string) ([]string, error) {
	f := filters.NewArgs()
	for k, v := range labels {
		f.Add("label", k+"="+v)
	}
	list, err := s.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(list))
	for _, c := range list {
		ids = append(ids, c.ID)
	}
	return ids, nil
}

func (s *SDK) ExecHealth(ctx context.Context, containerID string) ([]byte, error) {
	exec, err := s.cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd: s.healthCmd,
	})
	if err != nil {
		return nil, err
	}
	attach, err := s.cli.ContainerExecAttach(ctx, exec.ID, container.ExecStartOptions{})
	if err != nil {
		return nil, err
	}
	defer attach.Close()
	return io.ReadAll(io.LimitReader(attach.Reader, 1<<20))
}

func ptrInt(v int) *int { return &v }

var _ Provider = (*SDK)(nil)
var _ = nat.PortMap{}
var _ = types.Container{}
```

`nat`/`types` 若不使用则删除 import；Step 4 完成后运行 `go build ./internal/fleet/docker` 清理未用 import。

- [ ] **Step 5: 运行测试与构建**

Run: `cd server && go test ./internal/fleet/docker -count=1 -v`
Run: `cd server && go build ./internal/fleet/docker`
Expected: PASS / 构建通过。

- [ ] **Step 6: 提交**

```bash
git add server/internal/fleet/docker go.mod go.sum
git commit -m "feat(fleet): add docker provider with constrained node containers"
```

---

### Task 8: Fleet 服务与 HTTP 契约

**Files:**
- Create: `server/internal/fleet/service.go`
- Create: `server/internal/fleet/http.go`
- Test: `server/internal/fleet/service_test.go`、`server/internal/fleet/http_test.go`

**Interfaces:**
- Consumes: Task 3 `Config`、Task 4 `store.Store`、Task 5 `DeriveDisplayState`、Task 6 `Credentials`、Task 7 `docker.Provider`。
- Produces:
  - `type Service struct { cfg Config; store *store.Store; provider docker.Provider; creds *Credentials; secret string; now func() time.Time }`
  - `func NewService(cfg Config, st *store.Store, provider docker.Provider, creds *Credentials, secret string) *Service`
  - `type CreateNodeRequest struct { Name string; SpecID string; IdempotencyKey string }`
  - `func (s *Service) CreateNode(ctx context.Context, ownerID string, req CreateNodeRequest) (db.FleetNode, error)`
  - `func (s *Service) ListNodes(ctx context.Context, ownerID string, limit, offset int) ([]db.FleetNode, error)`
  - `func (s *Service) Operate(ctx context.Context, ownerID, instanceID, operation, idempotencyKey string) (db.FleetNode, error)`
  - `func (s *Service) VerifyPAT(ctx context.Context, token string) (PATIdentity, bool, error)`
  - `type PATIdentity struct { Valid bool; OwnerID, InstanceID, InstanceRecordID string }`
  - `func NewHTTP(s *Service, secret string) http.Handler`

- [ ] **Step 1: 写 HTTP 认证与校验的失败测试**

```go
package fleet

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPRejectsMissingOrWrongServiceSecret(t *testing.T) {
	handler := NewHTTP(nil, "top-secret")
	for _, secret := range []string{"", "wrong"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
		if secret != "" {
			req.Header.Set("Authorization", "Bearer "+secret)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("secret %q: status = %d, want 401", secret, w.Code)
		}
	}
}

func TestHTTPHealthzNeedsNoServiceSecret(t *testing.T) {
	handler := NewHTTP(nil, "top-secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", w.Code)
	}
}

func TestCreateNodeRejectsUnknownSpec(t *testing.T) {
	// Service is nil-safe only for auth tests; this test uses a real Service
	// in service_test.go. Here we assert the handler validates JSON shape.
	handler := NewHTTP(nil, "top-secret")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes", strings.NewReader(`{"name":"n","spec_id":"nope"}`))
	req.Header.Set("Authorization", "Bearer top-secret")
	req.Header.Set("X-User-ID", "owner-1")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Fatal("unknown spec must not succeed")
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `cd server && go test ./internal/fleet -run TestHTTP -count=1`
Expected: 编译失败（`undefined: NewHTTP`）。

- [ ] **Step 3: 实现 http.go 路由与中间件**

```go
package fleet

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

func NewHTTP(s *Service, secret string) http.Handler {
	r := chi.NewRouter()
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	r.Get("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if s == nil || s.provider == nil {
			http.Error(w, "fleet not ready", http.StatusServiceUnavailable)
			return
		}
		if err := s.provider.Ping(context.Background()); err != nil {
			http.Error(w, "docker unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	r.Group(func(r chi.Router) {
		r.Use(serviceAuth(secret))
		r.Get("/api/v1/", s.handleServiceInfo)
		r.Get("/api/v1/nodes", s.handleListNodes)
		r.Post("/api/v1/nodes", s.handleCreateNode)
		r.Delete("/api/v1/nodes", s.handleDeleteNode)
		r.Post("/api/v1/nodes/start", s.handleOperate("start"))
		r.Post("/api/v1/nodes/stop", s.handleOperate("stop"))
		r.Post("/api/v1/nodes/reboot", s.handleOperate("reboot"))
		r.Post("/api/v1/nodes/status", s.handleOperate("status"))
		r.Post("/api/v1/nodes/diagnose", s.handleDiagnose)
		r.Post("/api/v1/pat/verify", s.handleVerifyPAT)
	})
	return r
}

func serviceAuth(secret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if r.Header.Get("X-User-ID") == "" && r.URL.Path != "/api/v1/pat/verify" {
				http.Error(w, "missing user", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (s *Service) handleServiceInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"provider":           "docker",
		"operations":         []string{"create", "start", "stop", "reboot", "delete", "status", "diagnose"},
		"specs":              []map[string]any{{"id": "standard", "cpu": 2, "memory_mb": 4096, "max_concurrent": 1}},
		"persistent_storage": true,
		"disk_quota":         false,
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
```

Step 3 的 handler 方法（handleListNodes/handleCreateNode/handleDeleteNode/handleOperate/handleDiagnose/handleVerifyPAT）在后续步骤补齐；本步骤先让认证测试通过（其他方法在 Step 4 实现）。

- [ ] **Step 4: 实现 service.go 与剩余 handler**

```go
type Service struct {
	cfg      Config
	store    *store.Store
	provider docker.Provider
	creds    *Credentials
	secret   string
	now      func() time.Time
}

func NewService(cfg Config, st *store.Store, provider docker.Provider, creds *Credentials, secret string) *Service {
	return &Service{cfg: cfg, store: st, provider: provider, creds: creds, secret: secret, now: time.Now}
}

type CreateNodeRequest struct {
	Name           string `json:"name"`
	SpecID         string `json:"spec_id"`
	IdempotencyKey string `json:"idempotency_key"`
}

var (
	ErrSpecNotAllowed          = errors.New("spec not allowed")
	ErrCredentialProfileMissing = ErrProfileMissing
	ErrNodeBusy                = errors.New("node busy")
	ErrConflict                = errors.New("operation conflict")
	ErrNotFound                = errors.New("node not found")
)

func (s *Service) specByID(id string) (Spec, bool) {
	for _, spec := range s.cfg.Specs {
		if spec.ID == id {
			return spec, true
		}
	}
	return Spec{}, false
}

func (s *Service) CreateNode(ctx context.Context, ownerID string, req CreateNodeRequest) (db.FleetNode, error) {
	spec, ok := s.specByID(req.SpecID)
	if !ok {
		return db.FleetNode{}, ErrSpecNotAllowed
	}
	profile, err := s.creds.Resolve(ownerID)
	if err != nil {
		return db.FleetNode{}, err
	}
	if err := s.provider.Ping(ctx); err != nil {
		return db.FleetNode{}, fmt.Errorf("docker unavailable: %w", err)
	}
	if _, err := s.provider.InspectImage(ctx, s.cfg.RuntimeImage); err != nil {
		return db.FleetNode{}, fmt.Errorf("image_missing: %w", err)
	}
	ownerUUID, err := util.ParseUUID(ownerID)
	if err != nil {
		return db.FleetNode{}, fmt.Errorf("invalid owner: %w", err)
	}
	specJSON, _ := json.Marshal(spec)
	node, err := s.store.ReserveNode(ctx, db.CreateFleetNodeParams{
		OwnerID: ownerUUID, Namespace: s.cfg.Namespace, Name: req.Name,
		DaemonID: uuid.NewString(), Image: s.cfg.RuntimeImage,
		Spec: specJSON, TargetState: StateLaunching, IdempotencyKey: req.IdempotencyKey,
	}, s.cfg.MaxNodesPerOwner)
	if err != nil {
		return db.FleetNode{}, err
	}
	if err := s.launchNode(ctx, node, profile); err != nil {
		_, _ = s.store.Queries().UpdateFleetNodeTargetState(ctx, db.UpdateFleetNodeTargetStateParams{
			ID: node.ID, Namespace: s.cfg.Namespace, TargetState: StateFailed, Error: sanitize(err),
		})
		return db.FleetNode{}, err
	}
	return node, nil
}
```

`launchNode`、`InspectImage`、`sanitize` 必须在本任务内实现，不要留到后续任务：

```go
func (s *Service) launchNode(ctx context.Context, node db.FleetNode, profile Profile, token string) error {
	containerName := "multica-fleet-" + s.cfg.Namespace + "-" + store.UUIDText(node.ID)
	dataVolume := containerName + "-data"
	secretsVolume := containerName + "-secrets"

	labels := map[string]string{
		"multica.fleet.namespace": s.cfg.Namespace,
		"multica.fleet.node":      node.DaemonID,
		"multica.fleet.role":      "node",
	}
	if err := s.provider.EnsureNetwork(ctx, s.cfg.NodeNetwork, labels); err != nil {
		return err
	}
	for _, vol := range []string{dataVolume, secretsVolume} {
		if err := s.provider.EnsureVolume(ctx, vol, labels); err != nil {
			return err
		}
	}
	env := os.Environ()
	_ = env
	spec := docker.BuildNodeSpec(node.DaemonID, s.cfg.Namespace, containerName, s.cfg.RuntimeImage,
		dataVolume, secretsVolume, s.cfg.NodeNetwork, 4096, 2, 256)
	spec.Env = []string{
		"MULTICA_SERVER_URL=" + s.cfg.APIURL,
		"MULTICA_DAEMON_ID=" + node.DaemonID,
		"MULTICA_DAEMON_DEVICE_NAME=" + node.Name,
		"MULTICA_WORKSPACES_ROOT=/home/multica/workspaces",
		"MULTICA_DAEMON_AUTO_UPDATE=false",
		"MULTICA_DAEMON_AUTO_RELOAD=false",
		"HOME=/home/multica",
	}
	containerID, err := s.provider.CreateContainer(ctx, spec)
	if err != nil {
		return err
	}
	if err := s.provider.WriteSecrets(ctx, containerID, map[string][]byte{
		"config.json": []byte(buildCLIConfig(s.cfg.APIURL, s.cfg.APIURL, token)),
		"env":         []byte(buildEnvFile(profile.Env)),
	}); err != nil {
		return err
	}
	if err := s.provider.Start(ctx, containerID); err != nil {
		return err
	}
	return s.store.Queries().UpdateFleetNodeContainer(ctx, db.UpdateFleetNodeContainerParams{
		ID: node.ID, Namespace: s.cfg.Namespace, ContainerID: containerID, ContainerName: containerName,
	})
}

func buildCLIConfig(serverURL, appURL, token string) string {
	// token is written from the node credential minted in Step 5 (CreateCredential).
	// Keep this function responsible only for the non-secret shape.
	return fmt.Sprintf(`{"server_url":%q,"app_url":%q,"workspace_id":"","workspaces_root":"/home/multica/workspaces"}`, serverURL, serverURL)
}

func buildEnvFile(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(env[k])
		b.WriteString("
")
	}
	return b.String()
}

func sanitize(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return msg
}
```

注意：`buildCLIConfig` 不写 token；token 由 Step 5 的 `CreateNode` 在 ReserveNode 后生成并写入 secrets。把 token 生成提前：在 `CreateNode` 里调用 `mintNodeToken(node)` 得到明文与哈希，创建 credential 行，再传给 `launchNode(ctx, node, profile, token)`，并用 `buildCLIConfig(serverURL, node, token)` 写 config.json。Step 5 完成该调整并补测试。

- [ ] **Step 5: 补 token 铸造与操作 handler**

```go
func mintNodeToken() (plain, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	plain = "mcn_" + base64.RawURLEncoding.EncodeToString(buf)
	return plain, auth.HashToken(plain), nil
}
```

`auth.HashToken` 复用 `server/internal/auth` 的现有函数（与 mcn_ 验证一致）。CreateNode 中：

```go
plain, hash, err := mintNodeToken()
if err != nil { return db.FleetNode{}, err }
if _, err := s.store.Queries().CreateFleetNodeCredential(ctx, db.CreateFleetNodeCredentialParams{
	NodeID: node.ID, TokenHash: hash,
}); err != nil { return db.FleetNode{}, err }
if err := s.launchNode(ctx, node, profile, plain); err != nil { ... }
```

操作 handler：

```go
func (s *Service) handleOperate(operation string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			InstanceID     string `json:"instance_id"`
			IdempotencyKey string `json:"idempotency_key"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
		ownerID := r.Header.Get("X-User-ID")
		node, err := s.Operate(r.Context(), ownerID, req.InstanceID, operation, req.IdempotencyKey)
		writeServiceError(w, err)
		if err != nil { return }
		writeJSON(w, http.StatusOK, node)
	}
}

func writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
	case errors.Is(err, ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "node not found"})
	case errors.Is(err, ErrNodeBusy):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "node busy"})
	case errors.Is(err, ErrConflict):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "conflict"})
	case errors.Is(err, ErrProfileMissing), errors.Is(err, ErrProfileInvalid):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "credential profile missing"})
	case errors.Is(err, ErrSpecNotAllowed):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "spec not allowed"})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "fleet operation failed"})
	}
}
```

`Operate` 的实现委托给 Task 9 的 `operations.go`（prepare/approve 流程）；本任务先实现直通版本并在 Task 9 替换：

```go
func (s *Service) Operate(ctx context.Context, ownerID, instanceID, operation, idempotencyKey string) (db.FleetNode, error) {
	node, err := s.store.Queries().GetFleetNodeByContainerID(ctx, db.GetFleetNodeByContainerIDParams{
		ContainerID: instanceID, Namespace: s.cfg.Namespace,
	})
	if err != nil { return db.FleetNode{}, ErrNotFound }
	if store.UUIDText(node.OwnerID) != ownerID { return db.FleetNode{}, ErrNotFound }
	return s.applyOperation(ctx, node, operation, idempotencyKey)
}
```

`GetFleetNodeByContainerID` 需要在 Task 1 的 fleet.sql 中已存在（本计划已在 Task 1 未包含；实施时补该查询并在 Step 6 运行 make sqlc）：

```sql
-- name: GetFleetNodeByContainerID :one
SELECT * FROM fleet_node WHERE container_id = @container_id AND namespace = @namespace;
```

- [ ] **Step 6: 运行测试、sqlc 与构建**

Run: `make sqlc`
Run: `cd server && go test ./internal/fleet -count=1`
Run: `cd server && go build ./cmd/fleet 2>&1 || true`（main.go 在 Task 10/11 前可为空包）
Expected: 认证测试 PASS；跨包构建通过。

- [ ] **Step 7: 提交**

```bash
git add server/internal/fleet server/pkg/db/queries/fleet.sql server/pkg/db/generated
git commit -m "feat(fleet): add service HTTP contract and node creation"
```

---

### Task 9: 诊断、操作执行与协调器

**Files:**
- Create: `server/internal/fleet/diagnose.go`
- Create: `server/internal/fleet/coordinator.go`
- Test: `server/internal/fleet/coordinator_test.go`

**Interfaces:**
- Consumes: Task 7 `docker.Provider`/`ParseHealthReport`、Task 4 `store.Store`、Task 5 `DeriveDisplayState`。
- Produces:
  - `func (s *Service) Diagnose(ctx context.Context, node db.FleetNode) (docker.HealthReport, error)`
  - `func (s *Service) Tick(ctx context.Context) error`（执行一轮协调，测试入口）
  - `func (s *Service) RunCoordinator(ctx context.Context, interval time.Duration)`
  - 操作阶段常量：`OpPhasePrepared/OpPhaseApproved/OpPhaseExecuting/OpPhaseDone/OpPhaseFailed/OpPhaseAborted`

- [ ] **Step 1: 写 fake provider 与失败测试**

```go
package fleet

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/fleet/docker"
	"github.com/multica-ai/multica/server/internal/fleet/store"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	return testutil.IsolatedDB(t, dsn)
}

type fakeProvider struct {
	mu        sync.Mutex
	created   []docker.NodeSpec
	started   []string
	stopped   []string
	restarted []string
	removed   []string
	inspected map[string]docker.ObservedContainer
	health    []byte
	pingErr   error
	imageErr  error
}

func (f *fakeProvider) Ping(context.Context) error { return f.pingErr }
func (f *fakeProvider) InspectImage(context.Context, string) error { return f.imageErr }
func (f *fakeProvider) EnsureNetwork(context.Context, string, map[string]string) error { return nil }
func (f *fakeProvider) EnsureVolume(context.Context, string, map[string]string) error  { return nil }
func (f *fakeProvider) CreateContainer(_ context.Context, spec docker.NodeSpec) (string, error) {
	f.mu.Lock(); defer f.mu.Unlock()
	f.created = append(f.created, spec)
	return "container-1", nil
}
func (f *fakeProvider) WriteSecrets(context.Context, string, map[string][]byte) error { return nil }
func (f *fakeProvider) Start(_ context.Context, id string) error {
	f.mu.Lock(); defer f.mu.Unlock(); f.started = append(f.started, id); return nil
}
func (f *fakeProvider) Stop(_ context.Context, id string, _ time.Duration) error {
	f.mu.Lock(); defer f.mu.Unlock(); f.stopped = append(f.stopped, id); return nil
}
func (f *fakeProvider) Restart(_ context.Context, id string, _ time.Duration) error {
	f.mu.Lock(); defer f.mu.Unlock(); f.restarted = append(f.restarted, id); return nil
}
func (f *fakeProvider) Remove(_ context.Context, id string, _ bool) error {
	f.mu.Lock(); defer f.mu.Unlock(); f.removed = append(f.removed, id); return nil
}
func (f *fakeProvider) RemoveVolume(context.Context, string) error { return nil }
func (f *fakeProvider) Inspect(_ context.Context, id string) (docker.ObservedContainer, error) {
	if obs, ok := f.inspected[id]; ok { return obs, nil }
	return docker.ObservedContainer{}, errors.New("not found")
}
func (f *fakeProvider) ListByLabels(context.Context, map[string]string) ([]string, error) { return nil, nil }
func (f *fakeProvider) ExecHealth(context.Context, string) ([]byte, error) { return f.health, nil }

func TestDiagnoseParsesPendingTerminalReports(t *testing.T) {
	svc := &Service{provider: &fakeProvider{health: []byte("{\"status\":\"running\",\"pending_terminal_report_count\":2,\"agents\":[\"claude\"]}")}}
	report, err := svc.Diagnose(context.Background(), db.FleetNode{})
	if err != nil {
		t.Fatalf("diagnose: %v", err)
	}
	if report.PendingTerminalReportCount != 2 || len(report.Agents) != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
}

func TestTickExecutesApprovedStopAndUpdatesObserved(t *testing.T) {
	pool := testPool(t)
	st := store.New(pool)
	owner := pgtype.UUID{Bytes: [16]byte{20}, Valid: true}
	node, err := st.Queries().CreateFleetNode(context.Background(), db.CreateFleetNodeParams{
		OwnerID: owner, Namespace: "wt-op", Name: "n", DaemonID: "daemon-op",
		Image: "img", Spec: []byte("{}"), TargetState: StateRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Queries().UpdateFleetNodeContainer(context.Background(), db.UpdateFleetNodeContainerParams{
		ID: node.ID, Namespace: "wt-op", ContainerID: "c1", ContainerName: "n1",
	}); err != nil {
		t.Fatal(err)
	}
	op, err := st.Queries().CreateFleetNodeOperation(context.Background(), db.CreateFleetNodeOperationParams{
		NodeID: node.ID, Operation: "stop", Phase: OpPhaseApproved, IdempotencyKey: "k1",
	})
	if err != nil {
		t.Fatal(err)
	}
	provider := &fakeProvider{inspected: map[string]docker.ObservedContainer{
		"c1": {ID: "c1", State: "running", Healthy: true},
	}}
	svc := &Service{cfg: Config{Namespace: "wt-op"}, store: st, provider: provider}
	if err := svc.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(provider.stopped) != 1 || provider.stopped[0] != "c1" {
		t.Fatalf("stopped = %v", provider.stopped)
	}
	updated, err := st.Queries().GetFleetNodeOperation(context.Background(), db.GetFleetNodeOperationParams{ID: op.ID})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Phase != OpPhaseDone {
		t.Fatalf("phase = %q, want %q", updated.Phase, OpPhaseDone)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `cd server && go test ./internal/fleet -run 'TestDiagnose|TestTick' -count=1`
Expected: 编译失败（`undefined: OpPhaseApproved`）。

- [ ] **Step 3: 实现 Diagnose 与协调器**

```go
const (
	OpPhasePrepared  = "prepared"
	OpPhaseApproved  = "approved"
	OpPhaseExecuting = "executing"
	OpPhaseDone      = "done"
	OpPhaseFailed    = "failed"
	OpPhaseAborted   = "aborted"
)

func (s *Service) Diagnose(ctx context.Context, node db.FleetNode) (docker.HealthReport, error) {
	body, err := s.provider.ExecHealth(ctx, node.ContainerID)
	if err != nil {
		return docker.HealthReport{}, fmt.Errorf("diagnose %s: %w", node.ID, err)
	}
	return docker.ParseHealthReport(body)
}

// Tick is one coordination pass: refresh observed state for every node, then
// execute approved operations. It never invents approval; operations must have
// been approved by the API under the exclusive node lock.
func (s *Service) Tick(ctx context.Context) error {
	nodes, err := s.store.Queries().ListFleetNodesByNamespace(ctx, db.ListFleetNodesByNamespaceParams{
		Namespace: s.cfg.Namespace, LimitCount: 200, OffsetCount: 0,
	})
	if err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}
	for _, node := range nodes {
		if err := s.refreshNode(ctx, node); err != nil {
			slog.Warn("fleet: refresh node failed", "node", store.UUIDText(node.ID), "error", err)
		}
	}
	ops, err := s.store.Queries().ListPendingFleetNodeOperations(ctx)
	if err != nil {
		return fmt.Errorf("list operations: %w", err)
	}
	for _, op := range ops {
		if op.Phase != OpPhaseApproved {
			continue
		}
		if err := s.executeOperation(ctx, op); err != nil {
			_ = s.store.Queries().UpdateFleetNodeOperationPhase(ctx, db.UpdateFleetNodeOperationPhaseParams{
				ID: op.ID, Phase: OpPhaseFailed, RetryCount: 1, Error: sanitize(err),
			})
		}
	}
	return nil
}

func (s *Service) RunCoordinator(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Tick(ctx); err != nil {
				slog.Warn("fleet: coordinator tick failed", "error", err)
			}
		}
	}
}

func (s *Service) refreshNode(ctx context.Context, node db.FleetNode) error {
	if node.ContainerID == "" {
		return nil
	}
	obs, err := s.provider.Inspect(ctx, node.ContainerID)
	if err != nil {
		return s.store.Queries().UpdateFleetNodeObservedState(ctx, db.UpdateFleetNodeObservedStateParams{
			ID: node.ID, Namespace: s.cfg.Namespace, ObservedState: StateInstanceMissing,
			StatusDetail: []byte("{}"), Error: "",
		})
	}
	health, healthErr := s.Diagnose(ctx, node)
	daemonReady := healthErr == nil && health.Status == "running"
	display := DeriveDisplayState(node.TargetState, Observed{
		ContainerExists: true, ContainerState: obs.State, Healthy: obs.Healthy,
		DaemonReady: daemonReady, HealthFresh: healthErr == nil,
	})
	detail, _ := json.Marshal(map[string]any{
		"cli_installed":     containsAgent(health.Agents, "claude"),
		"model_configured":  daemonReady,
		"credentials_valid": true,
	})
	return s.store.Queries().UpdateFleetNodeObservedState(ctx, db.UpdateFleetNodeObservedStateParams{
		ID: node.ID, Namespace: s.cfg.Namespace, ObservedState: display, DaemonReady: daemonReady,
		StatusDetail: detail, Error: "",
	})
}

func containsAgent(agents []string, want string) bool {
	for _, a := range agents {
		if a == want {
			return true
		}
	}
	return false
}

func (s *Service) executeOperation(ctx context.Context, op db.FleetNodeOperation) error {
	node, err := s.store.Queries().GetFleetNode(ctx, db.GetFleetNodeParams{ID: op.NodeID, Namespace: s.cfg.Namespace})
	if err != nil {
		return fmt.Errorf("load node: %w", err)
	}
	if err := s.store.Queries().UpdateFleetNodeOperationPhase(ctx, db.UpdateFleetNodeOperationPhaseParams{
		ID: op.ID, Phase: OpPhaseExecuting,
	}); err != nil {
		return err
	}
	switch op.Operation {
	case "stop":
		if err := s.provider.Stop(ctx, node.ContainerID, 30*time.Second); err != nil {
			return err
		}
	case "start":
		if err := s.provider.Start(ctx, node.ContainerID); err != nil {
			return err
		}
	case "reboot":
		if err := s.provider.Restart(ctx, node.ContainerID, 30*time.Second); err != nil {
			return err
		}
	case "delete":
		if err := s.provider.Remove(ctx, node.ContainerID, true); err != nil {
			return err
		}
		_ = s.provider.RemoveVolume(ctx, node.ContainerName+"-data")
		_ = s.provider.RemoveVolume(ctx, node.ContainerName+"-secrets")
	}
	return s.store.Queries().UpdateFleetNodeOperationPhase(ctx, db.UpdateFleetNodeOperationPhaseParams{
		ID: op.ID, Phase: OpPhaseDone,
	})
}
```

新增 sqlc 查询 `ListFleetNodesByNamespace`（Task 1 未包含）：

```sql
-- name: ListFleetNodesByNamespace :many
SELECT * FROM fleet_node WHERE namespace = @namespace ORDER BY created_at ASC LIMIT @limit_count OFFSET @offset_count;
```

- [ ] **Step 4: 运行测试通过并提交**

Run: `make sqlc`
Run: `cd server && go test ./internal/fleet -run 'TestDiagnose|TestTick' -count=1 -v`
Expected: PASS。
```bash
git add server/internal/fleet server/pkg/db/queries/fleet.sql server/pkg/db/generated
git commit -m "feat(fleet): add diagnostics and approved-operation coordinator"
```

---

### Task 10: Runtime 镜像与 Entrypoint

**Files:**
- Create: `docker/runtime/Dockerfile`
- Create: `docker/runtime/entrypoint.sh`
- Create: `docker/fleet/Dockerfile`
- Create: `docker/runtime/entrypoint.test.sh`

**Interfaces:**
- Produces: 镜像构建目标 `multica/runtime-claude:dev`、`multica/fleet:dev`；entrypoint 读取 `/run/secrets/runtime/config.json` 与 `/run/secrets/runtime/env`。

- [ ] **Step 1: 写失败的 entrypoint shell 测试**

```bash
#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
secrets="$tmp/secrets"
home="$tmp/home"
mkdir -p "$secrets" "$home"
export HOME="$home"

bin="$tmp/bin"; mkdir -p "$bin"
cat > "$bin/multica" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" > "$HOME/ran"
EOF
chmod +x "$bin/multica"
export PATH="$bin:$PATH"

fail() { echo "FAIL: $*" >&2; exit 1; }

if (cd "$tmp" && "$root/docker/runtime/entrypoint.sh") >"$tmp/out" 2>"$tmp/err"; then
  fail "entrypoint without secrets must exit non-zero"
fi
grep -q 'missing' "$tmp/err" || fail "expected a missing-secret diagnostic"

printf '{"server_url":"http://api:8080","token":"mcn_test","workspace_id":"w1","workspaces_root":"/home/multica/workspaces"}' > "$secrets/config.json"
printf 'ANTHROPIC_API_KEY=sk-test\nMULTICA_TOKEN=must-not-export\n' > "$secrets/env"
chmod 600 "$secrets/config.json" "$secrets/env"
"$root/docker/runtime/entrypoint.sh" >/dev/null 2>"$tmp/err2"
[ "$(cat "$home/.multica/config.json")" = "$(cat "$secrets/config.json")" ] || fail "config not installed"
[ "$(stat -f '%Lp' "$home/.multica/config.json" 2>/dev/null || stat -c '%a' "$home/.multica/config.json")" = "600" ] || fail "config mode must be 600"
grep -q 'daemon start --foreground' "$home/ran" || fail "daemon not started"
grep -q 'MULTICA_TOKEN' "$tmp/err2" && fail "forbidden env must not be exported"
echo "ok"
```

- [ ] **Step 2: 运行确认失败**

Run: `bash docker/runtime/entrypoint.test.sh`
Expected: 失败（entrypoint 尚不存在）。

- [ ] **Step 3: 实现 entrypoint.sh**

```bash
#!/usr/bin/env bash
set -euo pipefail

secrets_dir="/run/secrets/runtime"
config_src="$secrets_dir/config.json"
env_src="$secrets_dir/env"

if [ ! -r "$config_src" ] || [ ! -r "$env_src" ]; then
  echo "missing node secrets under $secrets_dir" >&2
  exit 1
fi

mkdir -p "$HOME/.multica"
install -m 0600 "$config_src" "$HOME/.multica/config.json"

allowed() {
  case "$1" in
    ANTHROPIC_API_KEY|ANTHROPIC_AUTH_TOKEN|ANTHROPIC_BASE_URL|ANTHROPIC_MODEL|CLAUDE_CODE_USE_BEDROCK|API_TIMEOUT_MS) return 0 ;;
    *) return 1 ;;
  esac
}

while IFS='=' read -r key value; do
  [ -n "$key" ] || continue
  if allowed "$key"; then
    export "$key=$value"
  else
    echo "ignoring non-model env $key" >&2
  fi
done < "$env_src"

exec multica daemon start --foreground
```

- [ ] **Step 4: 写两个 Dockerfile**

```dockerfile
# docker/runtime/Dockerfile
FROM golang:1.26-alpine AS cli
RUN apk add --no-cache git
WORKDIR /src
COPY server/go.mod server/go.sum ./server/
RUN cd server && go mod download
COPY server/ ./server/
RUN cd server && CGO_ENABLED=0 go build -ldflags "-s -w" -o /out/multica ./cmd/multica

FROM node:22-bookworm-slim
ARG CLAUDE_CODE_VERSION=2.1.0
RUN apt-get update \
    && apt-get install -y --no-install-recommends git bash ca-certificates curl tini \
    && rm -rf /var/lib/apt/lists/*
RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}
RUN useradd -m -u 1000 multica \
    && mkdir -p /home/multica/workspaces /run/secrets/runtime
COPY --from=cli /out/multica /usr/local/bin/multica
COPY docker/runtime/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 0755 /usr/local/bin/entrypoint.sh
USER multica
WORKDIR /home/multica
HEALTHCHECK --interval=10s --timeout=5s --start-period=120s --retries=3 \
    CMD curl -fsS http://127.0.0.1:19514/health || exit 1
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/entrypoint.sh"]
```

```dockerfile
# docker/fleet/Dockerfile
FROM golang:1.26-alpine AS build
RUN apk add --no-cache git
WORKDIR /src
COPY server/go.mod server/go.sum ./server/
RUN cd server && go mod download
COPY server/ ./server/
RUN cd server && CGO_ENABLED=0 go build -ldflags "-s -w" -o /out/fleet ./cmd/fleet

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata curl
COPY --from=build /out/fleet /usr/local/bin/fleet
ENTRYPOINT ["/usr/local/bin/fleet"]
```

- [ ] **Step 5: 运行 shell 测试并构建（构建为 opt-in）**

Run: `bash docker/runtime/entrypoint.test.sh`
Expected: `ok`。
Run（需 Docker，opt-in）: `docker build -f docker/runtime/Dockerfile -t multica/runtime-claude:dev .`
Run（需 Docker，opt-in）: `docker build -f docker/fleet/Dockerfile -t multica/fleet:dev .`

- [ ] **Step 6: 提交**

```bash
git add docker/runtime docker/fleet
git commit -m "feat(fleet): add runtime and fleet container images"
```

---

### Task 11: Compose 与环境脚本

**Files:**
- Modify: `docker-compose.yml`、`docker-compose.selfhost.yml`
- Create: `scripts/fleet-env.sh`、`scripts/fleet-env.test.sh`
- Modify: `scripts/init-worktree-env.sh`、`scripts/dev-env.sh`、`scripts/dev-env.test.sh`、`Makefile`、`.env.example`

**Interfaces:**
- Produces（dev-env.sh 可 source）：`fleet_up`、`fleet_down`、`fleet_status`、`fleet_destroy`；环境变量 `MULTICA_FLEET_PORT`、`MULTICA_FLEET_LISTEN`、`MULTICA_LOCAL_FLEET_URL`、`MULTICA_LOCAL_FLEET_SECRET_FILE`、`MULTICA_LOCAL_FLEET_NAMESPACE`、`MULTICA_FLEET_RUNTIME_IMAGE`、`MULTICA_FLEET_CREDENTIALS_FILE`。

- [ ] **Step 1: 写失败的脚本测试**

```bash
#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fake_bin="$tmp/bin"; mkdir -p "$fake_bin"
cat > "$fake_bin/docker" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$DOCKER_LOG"
case "$*" in
  *"compose up -d fleet"*) exit 0 ;;
  *"compose ps -q fleet"*) echo "container-id" ;;
  *) exit 0 ;;
esac
EOF
chmod +x "$fake_bin/docker"
export PATH="$fake_bin:$PATH"
export DOCKER_LOG="$tmp/docker.log"
export MULTICA_FLEET_PORT=8092
export MULTICA_FLEET_RUNTIME_IMAGE=multica/runtime-claude:dev
export MULTICA_DEV_HOME="$tmp/dev"

fail() { echo "FAIL: $*" >&2; exit 1; }

source "$root/scripts/fleet-env.sh"
fleet_up
grep -q 'compose up -d fleet' "$DOCKER_LOG" || fail "fleet_up must start the compose service"

fleet_status >/dev/null
grep -q 'compose ps -q fleet' "$DOCKER_LOG" || fail "fleet_status must query compose"

echo "ok"
```

- [ ] **Step 2: 运行确认失败**

Run: `bash scripts/fleet-env.test.sh`
Expected: 失败（fleet-env.sh 不存在）。

- [ ] **Step 3: 实现 fleet-env.sh**

```bash
#!/usr/bin/env bash
# Fleet component helpers, sourced by scripts/dev-env.sh.
set -euo pipefail

REPO_ROOT="${REPO_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"

fleet_compose_file() { printf '%s/docker-compose.yml' "$REPO_ROOT"; }

fleet_require_docker() {
  command -v docker >/dev/null 2>&1 || { echo "docker CLI not found" >&2; return 1; }
  docker info >/dev/null 2>&1 || { echo "Docker daemon is not running" >&2; return 1; }
}

fleet_build_images() {
  docker build -f "$REPO_ROOT/docker/runtime/Dockerfile" -t "${MULTICA_FLEET_RUNTIME_IMAGE:?}" "$REPO_ROOT"
  docker build -f "$REPO_ROOT/docker/fleet/Dockerfile" -t multica/fleet:dev "$REPO_ROOT"
}

fleet_up() {
  fleet_require_docker || return 1
  fleet_build_images || return 1
  docker compose -f "$(fleet_compose_file)" up -d fleet
  local waited=0
  until curl -fsS "http://127.0.0.1:${MULTICA_FLEET_PORT}/readyz" >/dev/null 2>&1; do
    sleep 1
    waited=$((waited + 1))
    if [ "$waited" -ge 120 ]; then
      echo "Fleet did not become ready; see docker compose logs fleet" >&2
      return 1
    fi
  done
}

fleet_down() {
  fleet_require_docker || return 1
  docker compose -f "$(fleet_compose_file)" stop fleet
}

fleet_status() {
  fleet_require_docker || return 1
  local container
  container="$(docker compose -f "$(fleet_compose_file)" ps -q fleet)"
  if [ -z "$container" ]; then
    printf 'stopped|-|not running'
    return 0
  fi
  if curl -fsS "http://127.0.0.1:${MULTICA_FLEET_PORT}/readyz" >/dev/null 2>&1; then
    printf 'running|http://127.0.0.1:%s|ready' "$MULTICA_FLEET_PORT"
  else
    printf 'running|http://127.0.0.1:%s|starting' "$MULTICA_FLEET_PORT"
  fi
}

fleet_destroy() {
  fleet_require_docker || return 1
  docker compose -f "$(fleet_compose_file)" rm -sf fleet
  docker network ls --filter "name=multica-fleet-${MULTICA_LOCAL_FLEET_NAMESPACE:-local}" -q \
    | xargs -r docker network rm
}
```

- [ ] **Step 4: 写 compose 服务**

在 `docker-compose.yml` 的 `services` 下追加：

```yaml
  fleet:
    image: multica/fleet:dev
    build:
      context: .
      dockerfile: docker/fleet/Dockerfile
    depends_on:
      postgres:
        condition: service_healthy
    ports:
      - "127.0.0.1:${MULTICA_FLEET_PORT:-8091}:8091"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - ${MULTICA_LOCAL_FLEET_SECRET_FILE:-.dev/fleet-secret}:/run/secrets/fleet-secret:ro
      - ${MULTICA_FLEET_CREDENTIALS_FILE:-.dev/fleet-credentials.json}:/run/secrets/fleet-credentials.json:ro
    environment:
      MULTICA_FLEET_LISTEN: "0.0.0.0:8091"
      MULTICA_FLEET_NAMESPACE: ${MULTICA_LOCAL_FLEET_NAMESPACE:-local}
      MULTICA_FLEET_SECRET_FILE: /run/secrets/fleet-secret
      MULTICA_FLEET_DOCKER_HOST: unix:///var/run/docker.sock
      MULTICA_FLEET_NODE_NETWORK: multica-fleet-${MULTICA_LOCAL_FLEET_NAMESPACE:-local}
      MULTICA_FLEET_RUNTIME_IMAGE: ${MULTICA_FLEET_RUNTIME_IMAGE:-multica/runtime-claude:dev}
      MULTICA_FLEET_API_URL: ${MULTICA_FLEET_API_URL:-http://host.docker.internal:8080}
      MULTICA_FLEET_CREDENTIALS_FILE: /run/secrets/fleet-credentials.json
      DATABASE_URL: ${DATABASE_URL}
    healthcheck:
      test: ["CMD", "curl", "-fsS", "http://127.0.0.1:8091/readyz"]
      interval: 5s
      timeout: 3s
      retries: 20
```

容器内 `MULTICA_FLEET_LISTEN` 需绑 `0.0.0.0:8091`（端口映射到宿主 loopback）；Task 3 的 loopback 校验需允许容器场景：仅当 `MULTICA_FLEET_CONTAINER=1` 时接受 `0.0.0.0`，并在 compose 里设置该变量。

`docker-compose.selfhost.yml` 追加可选 `fleet` 服务（`profiles: ["fleet"]`），env 使用 selfhost 数据库 URL，挂 socket 与 secret 文件。

- [ ] **Step 5: 接入 init-worktree-env.sh 与 dev-env.sh**

`scripts/init-worktree-env.sh` 在生成 env 后追加：

```bash
fleet_port=$((backend_port + 20))
fleet_secret_dir="$WORKTREE_DIR/.dev"
mkdir -p "$fleet_secret_dir"
head -c 32 /dev/urandom | base64 > "$fleet_secret_dir/fleet-secret"
cat > "$fleet_secret_dir/fleet-credentials.json" <<'EOF'
{"users":{},"profiles":{}}
EOF
chmod 600 "$fleet_secret_dir/fleet-secret" "$fleet_secret_dir/fleet-credentials.json"
cat >> "$ENV_FILE" <<EOF
MULTICA_FLEET_PORT=$fleet_port
MULTICA_FLEET_LISTEN=127.0.0.1:$fleet_port
MULTICA_LOCAL_FLEET_URL=http://127.0.0.1:$fleet_port
MULTICA_LOCAL_FLEET_SECRET_FILE=$fleet_secret_dir/fleet-secret
MULTICA_LOCAL_FLEET_NAMESPACE=$(basename "$WORKTREE_DIR")
MULTICA_FLEET_RUNTIME_IMAGE=multica/runtime-claude:dev
MULTICA_FLEET_CREDENTIALS_FILE=$fleet_secret_dir/fleet-credentials.json
MULTICA_FLEET_API_URL=http://host.docker.internal:$backend_port
EOF
```

`scripts/dev-env.sh`：

- `ALL_COMPONENTS` 追加 `fleet`；`DEFAULT_COMPONENTS` 不变。
- 顶部 source `scripts/fleet-env.sh`。
- 新增 `start_fleet()` 调用 `fleet_up`；`stop_component fleet` 调用 `fleet_down`；`component_state fleet` 用 `fleet_status`。
- `up` 中在 `start_api` 之后调用 `start_fleet`；选择 fleet 时隐含 api：`if component_selected fleet; then COMPONENTS="$COMPONENTS api"; fi`。

- [ ] **Step 6: 运行脚本测试与 dev-env 测试**

Run: `bash scripts/fleet-env.test.sh`
Expected: `ok`。
Run: `bash scripts/dev-env.test.sh`
Expected: PASS（现有测试不回归）。
Run: `bash -n scripts/dev-env.sh scripts/fleet-env.sh scripts/init-worktree-env.sh`

- [ ] **Step 7: 提交**

```bash
git add docker-compose.yml docker-compose.selfhost.yml scripts Makefile .env.example
git commit -m "feat(fleet): wire docker compose fleet service into dev environment"
```

---

### Task 12: API 配置、服务密钥与 mcn_ 缓存禁用

**Files:**
- Create: `server/internal/fleetclient/client.go`
- Test: `server/internal/fleetclient/client_test.go`
- Modify: `server/internal/auth/cloud_pat.go`、`server/internal/auth/cloud_pat_test.go`
- Modify: `server/cmd/server/router.go`
- Test: `server/cmd/server/cloud_runtime_config_test.go`

**Interfaces:**
- Produces:
  - `fleetclient.Config struct { BaseURL, Secret string; Timeout time.Duration }`
  - `fleetclient.New(cfg Config) *Client`（其 `HTTPClient() *http.Client` 注入 `Authorization: Bearer <secret>`）
  - `auth.CloudPATVerifierConfig { FleetBaseURL string; Redis *redis.Client; HTTPClient *http.Client; DisableCache bool }`
  - `resolveCloudRuntimeConfig(getenv func(string) string) (cloudRuntimeConfig, error)`（router.go 内）

- [ ] **Step 1: 写失败的 fleetclient 测试**

```go
package fleetclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientStampsServiceSecret(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := New(Config{BaseURL: srv.URL, Secret: "s3cret", Timeout: time.Second})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/api/v1/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.HTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got != "Bearer s3cret" {
		t.Fatalf("authorization = %q", got)
	}
}

func TestClientDoesNotLeakSecretOnRedirectToOtherHost(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("secret leaked to redirect target")
		}
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	client := New(Config{BaseURL: srv.URL, Secret: "s3cret", Timeout: time.Second})
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	resp, err := client.HTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}
```

- [ ] **Step 2: 运行确认失败**

Run: `cd server && go test ./internal/fleetclient -count=1`
Expected: 编译失败（`undefined: New`）。

- [ ] **Step 3: 实现 fleetclient**

```go
package fleetclient

import (
	"net/http"
	"strings"
	"time"
)

type Config struct {
	BaseURL string
	Secret  string
	Timeout time.Duration
}

type Client struct {
	http *http.Client
}

type secretTransport struct {
	base   http.RoundTripper
	origin string
	secret string
}

func (t *secretTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Never attach the service secret to a redirect that left the fleet origin.
	if req.URL.Scheme+"://"+req.URL.Host == t.origin {
		cloned := req.Clone(req.Context())
		cloned.Header.Set("Authorization", "Bearer "+t.secret)
		return t.base.RoundTrip(cloned)
	}
	return t.base.RoundTrip(req)
}

func New(cfg Config) *Client {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 35 * time.Second
	}
	return &Client{http: &http.Client{
		Timeout: timeout,
		Transport: &secretTransport{
			base:   http.DefaultTransport,
			origin: strings.TrimRight(cfg.BaseURL, "/"),
			secret: cfg.Secret,
		},
	}}
}

func (c *Client) HTTPClient() *http.Client { return c.http }
```

注意：`origin` 比较需要 scheme+host，上面用 BaseURL 含路径会不匹配；实施时用 `url.Parse` 取 scheme/host。测试会暴露该问题，按测试行为修正：

```go
func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
```

- [ ] **Step 4: 扩展 CloudPATVerifier（DisableCache + HTTPClient）**

`CloudPATVerifierConfig` 增加字段，`Verify` 中：

```go
if !v.disableCache {
	if id, ok := v.cacheGet(ctx, hash); ok {
		return id, nil
	}
}
// ... fetch ...
if !v.disableCache {
	v.cacheSet(ctx, hash, id)
}
```

`fetch` 使用 `v.httpClient`（配置为空时用默认 client）。测试（cloud_pat_test.go 追加）：

```go
func TestVerifyDisableCacheAlwaysCallsFleet(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"valid":true,"owner_id":"o1","instance_id":"i1","instance_record_id":"r1"}`))
	}))
	defer srv.Close()
	verifier := NewCloudPATVerifier(CloudPATVerifierConfig{
		FleetBaseURL: srv.URL, DisableCache: true,
		HTTPClient: fleetclient.New(fleetclient.Config{BaseURL: srv.URL, Secret: "s"}).HTTPClient(),
	})
	for i := 0; i < 2; i++ {
		if _, err := verifier.Verify(context.Background(), "mcn_x", nil); err != nil {
			t.Fatalf("verify %d: %v", i, err)
		}
	}
	if calls != 2 {
		t.Fatalf("fleet calls = %d, want 2 (cache disabled)", calls)
	}
}
```

- [ ] **Step 5: 实现配置互斥与组装**

`server/cmd/server/router.go` 新增：

```go
type cloudRuntimeConfig struct {
	localURL      string
	localSecret   string
	localEnabled  bool
	localNamespace string
}

func resolveCloudRuntimeConfig(getenv func(string) string) (cloudRuntimeConfig, error) {
	cloudURL := strings.TrimSpace(getenv("MULTICA_CLOUD_URL"))
	localURL := strings.TrimSpace(getenv("MULTICA_LOCAL_FLEET_URL"))
	secretFile := strings.TrimSpace(getenv("MULTICA_LOCAL_FLEET_SECRET_FILE"))
	namespace := strings.TrimSpace(getenv("MULTICA_LOCAL_FLEET_NAMESPACE"))
	if cloudURL != "" && localURL != "" {
		return cloudRuntimeConfig{}, fmt.Errorf("MULTICA_CLOUD_URL and MULTICA_LOCAL_FLEET_URL are mutually exclusive")
	}
	if localURL == "" {
		return cloudRuntimeConfig{}, nil
	}
	if secretFile == "" || namespace == "" {
		return cloudRuntimeConfig{}, fmt.Errorf("local fleet requires MULTICA_LOCAL_FLEET_SECRET_FILE and MULTICA_LOCAL_FLEET_NAMESPACE")
	}
	secret, err := os.ReadFile(secretFile)
	if err != nil {
		return cloudRuntimeConfig{}, fmt.Errorf("read local fleet secret: %w", err)
	}
	if strings.TrimSpace(string(secret)) == "" {
		return cloudRuntimeConfig{}, fmt.Errorf("local fleet secret is empty")
	}
	return cloudRuntimeConfig{localURL: localURL, localSecret: strings.TrimSpace(string(secret)), localEnabled: true, localNamespace: namespace}, nil
}
```

组装时：

```go
cloudCfg, err := resolveCloudRuntimeConfig(os.Getenv)
if err != nil {
	slog.Error("cloud runtime configuration invalid", "error", err)
	os.Exit(1)
}
var fleetHTTP *http.Client
if cloudCfg.localEnabled {
	fleetHTTP = fleetclient.New(fleetclient.Config{
		BaseURL: cloudCfg.localURL, Secret: cloudCfg.localSecret, Timeout: 35 * time.Second,
	}).HTTPClient()
}
cloudClient := cloudruntime.NewClient(cloudruntime.Config{
	BaseURL: firstNonEmpty(cloudCfg.localURL, signupConfig.CloudURL),
	Timeout: 35 * time.Second,
	HTTPClient: fleetHTTP,
})
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

cloudPATVerifier := auth.NewCloudPATVerifier(auth.CloudPATVerifierConfig{
	FleetBaseURL: firstNonEmpty(cloudCfg.localURL, signupConfig.CloudURL),
	Redis:        rdb,
	HTTPClient:   fleetHTTP,
	DisableCache: cloudCfg.localEnabled,
})
if !cloudCfg.localEnabled {
	// existing entitlement/seat/billing wiring stays exactly as today
}
```

- [ ] **Step 6: 写配置矩阵测试并运行**

```go
func TestResolveCloudRuntimeConfig(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	_ = os.WriteFile(secret, []byte("s"), 0o600)
	cases := []struct {
		name    string
		env     map[string]string
		wantErr bool
	}{
		{"neither", map[string]string{}, false},
		{"both", map[string]string{"MULTICA_CLOUD_URL": "http://c", "MULTICA_LOCAL_FLEET_URL": "http://f"}, true},
		{"local without secret", map[string]string{"MULTICA_LOCAL_FLEET_URL": "http://f", "MULTICA_LOCAL_FLEET_NAMESPACE": "n"}, true},
		{"local valid", map[string]string{"MULTICA_LOCAL_FLEET_URL": "http://f", "MULTICA_LOCAL_FLEET_SECRET_FILE": secret, "MULTICA_LOCAL_FLEET_NAMESPACE": "n"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveCloudRuntimeConfig(func(k string) string { return tc.env[k] })
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}
```

Run: `cd server && go test ./internal/fleetclient ./internal/auth ./cmd/server -run 'TestClient|TestVerifyDisableCache|TestResolveCloudRuntimeConfig' -count=1 -v`
Expected: PASS。

- [ ] **Step 7: 提交**

```bash
git add server/internal/fleetclient server/internal/auth server/cmd/server
git commit -m "feat(api): add local fleet config, service auth, and mcn cache bypass"
```

---

### Task 13: 受管 Runtime 元数据与删除保护

**Files:**
- Modify: `server/pkg/db/queries/runtime.sql`
- Modify: `server/internal/handler/daemon.go`、`server/internal/middleware/daemon_auth.go`、`server/internal/handler/runtime.go`
- Test: `server/internal/handler/fleet_managed_runtime_test.go`

**Interfaces:**
- Produces:
  - `middleware.CloudPATIdentityFromContext(ctx) (auth.CloudPATIdentity, bool)`
  - sqlc 查询 `SetAgentRuntimeFleetManaged`（`managed_by, fleet_node_id`）
  - `DeleteAgentRuntime` 对受管 runtime 返回 409 `code: runtime_managed_by_fleet`

- [ ] **Step 1: 写失败的 DB 级测试**

```go
func fleetManagedRuntimeFixture(t *testing.T) string {
	t.Helper()
	var rt string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO agent_runtime (workspace_id, daemon_id, name, runtime_mode, provider, status, device_info, metadata, owner_id, managed_by)
		VALUES ($1, 'daemon-managed', 'claude', 'local', 'claude', 'online', 'node', '{}'::jsonb, $2, 'local_fleet')
		RETURNING id`, testWorkspaceID, testUserID).Scan(&rt); err != nil {
		t.Fatal(err)
	}
	return rt
}

func TestRuntimeUpsertPreservesFleetManagedColumns(t *testing.T) {
	rt := fleetManagedRuntimeFixture(t)
	if _, err := testPool.Exec(context.Background(),
		"UPDATE agent_runtime SET managed_by='local_fleet', fleet_node_id=gen_random_uuid() WHERE id=$1", rt); err != nil {
		t.Fatal(err)
	}
	// Re-register the same runtime through the normal upsert path.
	if _, err := testHandler.DaemonRegister(newRequest(http.MethodPost, "/api/daemon/register", map[string]any{
		"workspace_id": testWorkspaceID,
		"daemon_id":    "daemon-managed",
		"device_name":  "node",
		"runtimes": []any{map[string]any{
			"name": "claude", "type": "claude", "status": "online", "runtime_mode": "local",
		}},
	}), nil); err != nil {
		t.Fatal(err)
	}
	var managedBy string
	if err := testPool.QueryRow(context.Background(),
		"SELECT managed_by FROM agent_runtime WHERE id=$1", rt).Scan(&managedBy); err != nil {
		t.Fatal(err)
	}
	if managedBy != "local_fleet" {
		t.Fatalf("managed_by = %q, want preserved", managedBy)
	}
}

func TestDeleteManagedRuntimeIsRejected(t *testing.T) {
	rt := fleetManagedRuntimeFixture(t)
	if _, err := testPool.Exec(context.Background(),
		"UPDATE agent_runtime SET managed_by='local_fleet' WHERE id=$1", rt); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	testHandler.DeleteAgentRuntime(w, newRequest(http.MethodDelete, "/api/runtimes/"+rt, nil))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "runtime_managed_by_fleet") {
		t.Fatalf("missing code: %s", w.Body.String())
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `cd server && go test ./internal/handler -run 'TestRuntimeUpsertPreservesFleetManaged|TestDeleteManagedRuntime' -count=1`
Expected: 失败（列未保留 / 删除未被拒）。

- [ ] **Step 3: 改 SQL 与写入路径**

`server/pkg/db/queries/runtime.sql` 的两个 upsert 的 DO UPDATE 列表**不要**包含 `managed_by/fleet_node_id`（当前也没有，因此插入默认值后不会被 EXCLUDED 覆盖）。新增查询：

```sql
-- name: SetAgentRuntimeFleetManaged :exec
UPDATE agent_runtime
SET managed_by = @managed_by, fleet_node_id = @fleet_node_id, updated_at = now()
WHERE id = @id;
```

`daemon.go` 注册成功后：

```go
if identity, ok := middleware.CloudPATIdentityFromContext(r.Context()); ok && identity.InstanceRecordID != "" {
	if nodeUUID, err := util.ParseUUID(identity.InstanceRecordID); err == nil {
		_ = qtx.SetAgentRuntimeFleetManaged(r.Context(), db.SetAgentRuntimeFleetManagedParams{
			ID: registered.ID, ManagedBy: "local_fleet", FleetNodeID: nodeUUID,
		})
	}
}
```

`middleware/daemon_auth.go` 的 cloud_pat 分支：

```go
ctx = context.WithValue(ctx, ctxKeyCloudPATIdentity, identity)
```

并导出：

```go
func CloudPATIdentityFromContext(ctx context.Context) (auth.CloudPATIdentity, bool) {
	id, ok := ctx.Value(ctxKeyCloudPATIdentity).(auth.CloudPATIdentity)
	return id, ok
}
```

- [ ] **Step 4: 删除保护**

`runtime.go` 的 `DeleteAgentRuntime` 在加载 runtime 后、任何写操作前：

```go
if strings.TrimSpace(rt.ManagedBy) == "local_fleet" {
	writeJSON(w, http.StatusConflict, map[string]any{
		"error": "this runtime is managed by a local fleet node; delete it from the Cloud Runtime page",
		"code":  "runtime_managed_by_fleet",
	})
	return
}
```

- [ ] **Step 5: 运行测试并提交**

Run: `make sqlc`
Run: `cd server && go test ./internal/handler -run 'TestRuntimeUpsertPreservesFleetManaged|TestDeleteManagedRuntime' -count=1 -v`
Expected: PASS。
```bash
git add server/pkg/db/queries/runtime.sql server/pkg/db/generated server/internal/handler server/internal/middleware
git commit -m "feat(api): mark fleet-managed runtimes and protect them from deletion"
```

---

### Task 14: 本地忙碌检查、操作协调与幂等

**Files:**
- Create: `server/internal/handler/cloud_runtime_local.go`
- Modify: `server/internal/handler/cloud_runtime.go`、`server/internal/handler/handler.go`
- Test: `server/internal/handler/cloud_runtime_local_test.go`

**Interfaces:**
- Produces:
  - `type localFleetCoordinator struct { pool *pgxpool.Pool; namespace string; diagnose func(ctx context.Context, containerID string) (docker.HealthReport, error); now func() time.Time }`
  - `func (c *localFleetCoordinator) Prepare(ctx context.Context, ownerID, containerID, operation, idempotencyKey string) (db.FleetNode, error)`
  - 错误：`ErrNodeBusy`、`ErrOperationConflict`、`ErrNodeNotFound`
  - `Handler.LocalFleet *localFleetCoordinator`（nil = 非本地模式，行为不变）

- [ ] **Step 1: 写失败的忙碌语义测试**

```go
func TestPrepareStopRejectsActiveButAllowsQueued(t *testing.T) {
	nodeID, runtimeID, agentID := fleetFixture(t, "running")
	insertTask(t, agentID, "running")
	if _, err := testHandler.LocalFleet.Prepare(context.Background(), testUserID, "container-1", "stop", "key-1"); !errors.Is(err, ErrNodeBusy) {
		t.Fatalf("running task: err = %v, want ErrNodeBusy", err)
	}

	if _, err := testPool.Exec(context.Background(),
		"UPDATE agent_task_queue SET status='queued' WHERE agent_id=$1", agentID); err != nil {
		t.Fatal(err)
	}
	if _, err := testHandler.LocalFleet.Prepare(context.Background(), testUserID, "container-1", "stop", "key-2"); err != nil {
		t.Fatalf("queued task must not block stop: %v", err)
	}
}

func TestPrepareDeleteRejectsQueuedRuns(t *testing.T) {
	nodeID, runtimeID, agentID := fleetFixture(t, "running")
	insertTask(t, agentID, "queued")
	if _, err := testHandler.LocalFleet.Prepare(context.Background(), testUserID, "container-1", "delete", "key-3"); !errors.Is(err, ErrNodeBusy) {
		t.Fatalf("err = %v, want ErrNodeBusy", err)
	}
}

func TestPrepareIsIdempotentForSameRequest(t *testing.T) {
	nodeID, _, _ := fleetFixture(t, "running")
	first, err := testHandler.LocalFleet.Prepare(context.Background(), testUserID, "container-1", "stop", "key-4")
	if err != nil {
		t.Fatal(err)
	}
	second, err := testHandler.LocalFleet.Prepare(context.Background(), testUserID, "container-1", "stop", "key-4")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("same key must return the same node: %s vs %s", first.ID, second.ID)
	}
	if _, err := testHandler.LocalFleet.Prepare(context.Background(), testUserID, "container-1", "reboot", "key-4"); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("different fingerprint: err = %v, want ErrOperationConflict", err)
	}
}
```

`fleetFixture`/`insertTask` 是测试文件内的 helper：插入 user/workspace/member/runtime(daemon_id=node daemon_id)/agent/task，以及 fleet_node(container_id="container-1", target_state="running")。

- [ ] **Step 2: 运行确认失败**

Run: `cd server && go test ./internal/handler -run 'TestPrepare' -count=1`
Expected: 编译失败（`LocalFleet` 不存在）。

- [ ] **Step 3: 实现 cloud_runtime_local.go**

```go
package handler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/fleet/barrier"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/util"
)

var (
	ErrNodeBusy          = errors.New("node busy")
	ErrOperationConflict = errors.New("operation conflict")
	ErrNodeNotFound      = errors.New("node not found")
)

type localFleetCoordinator struct {
	pool      *pgxpool.Pool
	namespace string
	diagnose  func(ctx context.Context, containerID string) (int, error) // pending terminal reports
	now       func() time.Time
}

func (c *localFleetCoordinator) Prepare(ctx context.Context, ownerID, containerID, operation, idempotencyKey string) (db.FleetNode, error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return db.FleetNode{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := db.New(tx)

	node, err := q.GetFleetNodeByContainerID(ctx, db.GetFleetNodeByContainerIDParams{
		ContainerID: containerID, Namespace: c.namespace,
	})
	if err != nil {
		return db.FleetNode{}, ErrNodeNotFound
	}
	ownerUUID, err := util.ParseUUID(ownerID)
	if err != nil || util.UUIDToString(node.OwnerID) != util.UUIDToString(ownerUUID) {
		return db.FleetNode{}, ErrNodeNotFound
	}
	if err := barrier.AcquireExclusive(ctx, tx, barrier.Key(c.namespace, node.DaemonID)); err != nil {
		return db.FleetNode{}, err
	}

	fingerprint := operation + ":" + containerID
	if existing, err := q.GetFleetNodeOperationByIdempotencyKey(ctx, db.GetFleetNodeOperationByIdempotencyKeyParams{
		IdempotencyKey: idempotencyKey,
	}); err == nil {
		if existing.RequestFingerprint != fingerprint {
			return db.FleetNode{}, ErrOperationConflict
		}
		return node, tx.Commit(ctx)
	}

	active, err := q.CountActiveTasksForDaemon(ctx, db.CountActiveTasksForDaemonParams{
		WorkspaceID: nodeWorkspacePlaceholder(ownerUUID), DaemonID: node.DaemonID,
	})
	_ = active
	_ = err
	return db.FleetNode{}, nil
}
```

`CountActiveTasksForDaemon` 需要 workspace_id；节点属于 owner 但可在多个 workspace 注册 runtime。设计行 112 要求保留 workspace 权限校验。实现改为按 owner 的 runtime 集合统计（跨 workspace 但限定 owner 的 runtime）：

在 fleet.sql 增加（Task 1 修改）：

```sql
-- name: CountActiveTasksForFleetDaemon :one
SELECT count(*) FROM agent_task_queue q
JOIN agent a ON a.id = q.agent_id
JOIN agent_runtime r ON r.id = a.runtime_id
WHERE r.daemon_id = @daemon_id
  AND r.owner_id = @owner_id
  AND q.status IN ('dispatched', 'running', 'waiting_local_directory');

-- name: CountQueuedTasksForFleetDaemon :one
SELECT count(*) FROM agent_task_queue q
JOIN agent a ON a.id = q.agent_id
JOIN agent_runtime r ON r.id = a.runtime_id
WHERE r.daemon_id = @daemon_id
  AND r.owner_id = @owner_id
  AND q.status IN ('queued', 'deferred');
```

用这两个查询替换占位，并完成 Prepare：

```go
func (c *localFleetCoordinator) Prepare(ctx context.Context, ownerID, containerID, operation, idempotencyKey string) (db.FleetNode, error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return db.FleetNode{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := db.New(tx)

	node, err := q.GetFleetNodeByContainerID(ctx, db.GetFleetNodeByContainerIDParams{
		ContainerID: containerID, Namespace: c.namespace,
	})
	if err != nil {
		return db.FleetNode{}, ErrNodeNotFound
	}
	ownerUUID, err := util.ParseUUID(ownerID)
	if err != nil || util.UUIDToString(node.OwnerID) != util.UUIDToString(ownerUUID) {
		return db.FleetNode{}, ErrNodeNotFound
	}
	if err := barrier.AcquireExclusive(ctx, tx, barrier.Key(c.namespace, node.DaemonID)); err != nil {
		return db.FleetNode{}, err
	}

	fingerprint := operation + ":" + containerID
	if existing, err := q.GetFleetNodeOperationByIdempotencyKey(ctx, db.GetFleetNodeOperationByIdempotencyKeyParams{
		IdempotencyKey: idempotencyKey,
	}); err == nil {
		if existing.RequestFingerprint != fingerprint {
			return db.FleetNode{}, ErrOperationConflict
		}
		return node, tx.Commit(ctx)
	}

	activeParams := db.CountActiveTasksForFleetDaemonParams{DaemonID: node.DaemonID, OwnerID: node.OwnerID}
	active, err := q.CountActiveTasksForFleetDaemon(ctx, activeParams)
	if err != nil {
		return db.FleetNode{}, err
	}
	if active > 0 {
		return db.FleetNode{}, ErrNodeBusy
	}
	if operation == "delete" {
		queued, err := q.CountQueuedTasksForFleetDaemon(ctx, db.CountQueuedTasksForFleetDaemonParams{
			DaemonID: node.DaemonID, OwnerID: node.OwnerID,
		})
		if err != nil {
			return db.FleetNode{}, err
		}
		if queued > 0 {
			return db.FleetNode{}, ErrNodeBusy
		}
	}

	pending, err := c.diagnose(ctx, node.ContainerID)
	if err != nil {
		return db.FleetNode{}, ErrNodeBusy
	}
	if pending > 0 {
		return db.FleetNode{}, ErrNodeBusy
	}

	target := map[string]string{"stop": "stopping", "reboot": "rebooting", "delete": "terminating"}[operation]
	if err := q.UpdateFleetNodeTargetState(ctx, db.UpdateFleetNodeTargetStateParams{
		ID: node.ID, Namespace: c.namespace, TargetState: target,
	}); err != nil {
		return db.FleetNode{}, err
	}
	op, err := q.CreateFleetNodeOperation(ctx, db.CreateFleetNodeOperationParams{
		NodeID: node.ID, Operation: operation, Phase: "prepared",
		IdempotencyKey: idempotencyKey, RequestFingerprint: fingerprint,
	})
	if err != nil {
		return db.FleetNode{}, err
	}
	if err := q.ApproveFleetNodeOperation(ctx, db.ApproveFleetNodeOperationParams{ID: op.ID}); err != nil {
		return db.FleetNode{}, err
	}
	return node, tx.Commit(ctx)
}
```

`ApproveFleetNodeOperation` 新增查询：

```sql
-- name: ApproveFleetNodeOperation :exec
UPDATE fleet_node_operation
SET phase = 'approved', approved_at = now(), updated_at = now()
WHERE id = @id AND phase = 'prepared';
```

注意：设计要求的"prepare 与 approve 分两个短事务、事务外诊断"在这里被合并为一个事务（诊断在持有排他锁时调用外部 Docker exec，违反设计行 167"数据库事务内不调用 Docker/Fleet HTTP"）。实施时必须拆成两段：先 prepare（短事务提交），再在事务外 diagnose，再短事务 approve/abort。上面代码仅用于说明字段与幂等形状；按下面的两段式实现替换：

```go
// 1) prepare：排他锁 + 幂等 + 任务检查 + 写 prepared。
// 2) tx 外：c.diagnose(containerID) 查待回传。
// 3) approve：排他锁 + 重新读取 operation 与代次 + 核验后 UPDATE approved；若 pending>0 则 phase=aborted、
//    恢复 target_state=running 并返回 ErrNodeBusy。
```

- [ ] **Step 4: 接线 handler 与代理**

`handler.go` 的 Config 增加：

```go
LocalFleet *localFleetCoordinator
LocalFleetNamespace string
```

`cloud_runtime.go` 的 `proxyCloudRuntime` 在调用 `h.CloudRuntime.Do` 前：

```go
if h.LocalFleet != nil {
	switch path {
	case "/api/v1/nodes/stop", "/api/v1/nodes/reboot":
		if err := h.coordinateLocalFleet(r.Context(), body, path); err != nil {
			writeLocalFleetError(w, err)
			return
		}
	case "/api/v1/nodes":
		if method == http.MethodDelete {
			if err := h.coordinateLocalFleet(r.Context(), body, "/api/v1/nodes/delete"); err != nil {
				writeLocalFleetError(w, err)
				return
			}
		}
	}
}
```

`coordinateLocalFleet` 解析 body 的 `instance_id` 与 `Idempotency-Key`，调用 `Prepare`；错误映射 404/409/503。

- [ ] **Step 5: 运行测试并提交**

Run: `make sqlc`
Run: `cd server && go test ./internal/handler -run 'TestPrepare' -count=1 -v`
Expected: PASS。
```bash
git add server/internal/handler server/pkg/db/queries go.mod go.sum
git commit -m "feat(api): coordinate local fleet operations with busy checks and idempotency"
```

---

### Task 15: Claim 领取屏障

**Files:**
- Create: `server/internal/fleet/claimguard/guard.go`
- Test: `server/internal/fleet/claimguard/guard_test.go`
- Modify: `server/internal/service/task.go`、`server/internal/handler/handler.go`、`server/cmd/server/router.go`
- Test: `server/internal/service/task_fleet_guard_test.go`

**Interfaces:**
- Produces:
  - `type FleetGuard interface { CheckClaim(ctx context.Context, q *db.Queries, runtimeID pgtype.UUID) error }`（定义在 `service` 包）
  - `claimguard.Guard struct { namespace string }`、`func New(namespace string) *Guard`
  - `var ErrFleetNodeUnavailable = errors.New("local fleet node is not accepting claims")`（在 claimguard 包；service 通过 error 非 nil 拒绝）
  - sqlc 查询 `AcquireFleetNodeSharedLock` / `AcquireFleetNodeExclusiveLock` / `GetActiveFleetNodeCredentialCount`

- [ ] **Step 1: 写失败的 guard DB 测试**

```go
package claimguard

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestCheckClaimRejectsNodeNotRunning(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	pool := testutil.IsolatedDB(t, dsn)
	q := db.New(pool)
	ws := insertWorkspace(t, pool)
	rt := insertManagedRuntime(t, pool, ws, "daemon-1")
	node := insertFleetNode(t, pool, ws, "daemon-1", "starting", true)
	_ = node

	guard := New("wt-test")
	if err := guard.CheckClaim(context.Background(), q, rt); err == nil {
		t.Fatal("starting node must reject claims")
	}
}

func TestCheckClaimAllowsReadyNode(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	pool := testutil.IsolatedDB(t, dsn)
	q := db.New(pool)
	ws := insertWorkspace(t, pool)
	rt := insertManagedRuntime(t, pool, ws, "daemon-1")
	insertFleetNode(t, pool, ws, "daemon-1", "running", true)

	guard := New("wt-test")
	if err := guard.CheckClaim(context.Background(), q, rt); err != nil {
		t.Fatalf("ready node must accept claims: %v", err)
	}
}
```

`insertWorkspace`/`insertManagedRuntime`/`insertFleetNode` 是测试文件内 helper：直接 SQL 插入 workspace、agent_runtime(managed_by='local_fleet', daemon_id)、fleet_node(target_state, daemon_ready, health_checked_at=now(), credential 行)。

- [ ] **Step 2: 运行确认失败**

Run: `cd server && go test ./internal/fleet/claimguard -count=1`
Expected: 编译失败（`undefined: New`）。

- [ ] **Step 3: 实现 guard 与 sqlc 锁查询**

```sql
-- name: AcquireFleetNodeSharedLock :exec
SELECT pg_advisory_xact_lock_shared(@key);

-- name: AcquireFleetNodeExclusiveLock :exec
SELECT pg_advisory_xact_lock(@key);

-- name: GetActiveFleetNodeCredentialCount :one
SELECT count(*) FROM fleet_node_credential
WHERE node_id = @node_id AND revoked_at IS NULL;

-- name: GetFleetNodeForClaim :one
SELECT * FROM fleet_node WHERE daemon_id = @daemon_id AND namespace = @namespace;
```

```go
package claimguard

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/fleet/barrier"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var ErrFleetNodeUnavailable = errors.New("local fleet node is not accepting claims")

type Guard struct {
	namespace string
}

func New(namespace string) *Guard { return &Guard{namespace: namespace} }

// CheckClaim runs inside the caller's claim transaction. It takes the shared
// node lock first, then validates persisted state, so a concurrent stop/delete
// that holds the exclusive lock waits for this claim (or vice versa).
func (g *Guard) CheckClaim(ctx context.Context, q *db.Queries, runtimeID pgtype.UUID) error {
	rt, err := q.GetAgentRuntime(ctx, runtimeID)
	if err != nil {
		return fmt.Errorf("load runtime: %w", err)
	}
	if rt.ManagedBy != "local_fleet" || !rt.DaemonID.Valid {
		return nil
	}
	daemonID := rt.DaemonID.String
	if err := q.AcquireFleetNodeSharedLock(ctx, barrier.Key(g.namespace, daemonID)); err != nil {
		return fmt.Errorf("fleet claim lock: %w", err)
	}
	node, err := q.GetFleetNodeForClaim(ctx, db.GetFleetNodeForClaimParams{
		DaemonID: daemonID, Namespace: g.namespace,
	})
	if err != nil {
		return ErrFleetNodeUnavailable
	}
	if node.TargetState != "running" || !node.DaemonReady {
		return ErrFleetNodeUnavailable
	}
	if node.HealthCheckedAt.Time.IsZero() || time.Since(node.HealthCheckedAt.Time) > 30*time.Second {
		return ErrFleetNodeUnavailable
	}
	active, err := q.GetActiveFleetNodeCredentialCount(ctx, node.ID)
	if err != nil {
		return err
	}
	if active == 0 {
		return ErrFleetNodeUnavailable
	}
	return nil
}
```

- [ ] **Step 4: 接入 TaskService**

`server/internal/service/task.go`：

```go
type FleetGuard interface {
	CheckClaim(ctx context.Context, q *db.Queries, runtimeID pgtype.UUID) error
}
```

`TaskService` 增加字段 `FleetGuard FleetGuard`。在 `claimTask` 的 `claimRuntimeID` 确定后、`CountRunningTasks` 前：

```go
if s.FleetGuard != nil {
	if err := s.FleetGuard.CheckClaim(ctx, qtx, claimRuntimeID); err != nil {
		outcome = "fleet_guard_rejected"
		return nil
	}
}
```

在 `ClaimTasksForRuntimes` 的候选循环内，对每个 runtime：

```go
if s.FleetGuard != nil {
	if err := s.FleetGuard.CheckClaim(ctx, s.Queries, rid); err != nil {
		continue
	}
}
```

（若批量路径已有事务包装，按实际事务句柄传入 `db.New(tx)`；保持与单条路径同样的锁顺序。）

- [ ] **Step 5: 写 service 层调用点测试**

```go
type rejectingGuard struct{ calls int }

func (g *rejectingGuard) CheckClaim(context.Context, *db.Queries, pgtype.UUID) error {
	g.calls++
	return errors.New("rejected")
}

func TestClaimTaskStopsAtFleetGuard(t *testing.T) {
	// setup agent + runtime as in existing claim tests
	guard := &rejectingGuard{}
	svc := newTaskServiceForTest(t, pool)
	svc.FleetGuard = guard
	claim, err := svc.ClaimTask(context.Background(), agentID)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claim != nil {
		t.Fatal("claim must be nil when the fleet guard rejects")
	}
	if guard.calls != 1 {
		t.Fatalf("guard calls = %d", guard.calls)
	}
}
```

- [ ] **Step 6: 接线 router 并运行测试**

```go
if cloudCfg.localEnabled {
	guard := claimguard.New(cloudCfg.localNamespace)
	h.TaskService.FleetGuard = guard
}
```

Run: `make sqlc`
Run: `cd server && go test ./internal/fleet/claimguard ./internal/service -run 'TestCheckClaim|TestClaimTaskStopsAtFleetGuard' -count=1 -v`
Expected: PASS。

- [ ] **Step 7: 提交**

```bash
git add server/internal/fleet/claimguard server/internal/service server/internal/handler server/cmd/server server/pkg/db/queries
git commit -m "feat(api): gate local fleet claims behind the advisory-lock barrier"
```

---

### Task 16: 前端 Core 能力发现与操作 mutation

**Files:**
- Modify: `packages/core/runtimes/cloud-runtime.ts`
- Modify: `packages/core/api/schemas.ts`、`packages/core/api/client.ts`
- Test: `packages/core/runtimes/cloud-runtime-service.test.ts`

**Interfaces:**
- Produces:
  - `interface CloudRuntimeSpec { id: string; cpu: number; memoryMb: number; maxConcurrent: number }`
  - `interface CloudRuntimeService { provider: string; operations: string[]; specs: CloudRuntimeSpec[]; persistentStorage: boolean; diskQuota: boolean }`
  - `EMPTY_CLOUD_RUNTIME_SERVICE: CloudRuntimeService`
  - `cloudRuntimeServiceOptions(wsId: string)`
  - `useStartCloudRuntimeNode(wsId)`、`useStopCloudRuntimeNode(wsId)`、`useRebootCloudRuntimeNode(wsId)`
  - `api.getCloudRuntimeService()`、`api.startCloudRuntimeNode(instanceId, idempotencyKey)` 等

- [ ] **Step 1: 写失败的 schema 测试**

```ts
import { describe, expect, it } from "vitest";
import { CloudRuntimeServiceSchema } from "../api/schemas";

describe("CloudRuntimeServiceSchema", () => {
  it("parses a docker capability response into camelCase", () => {
    const parsed = CloudRuntimeServiceSchema.parse({
      provider: "docker",
      operations: ["create", "start"],
      specs: [{ id: "standard", cpu: 2, memory_mb: 4096, max_concurrent: 1 }],
      persistent_storage: true,
      disk_quota: false,
    });
    expect(parsed.provider).toBe("docker");
    expect(parsed.specs[0]?.memoryMb).toBe(4096);
    expect(parsed.persistentStorage).toBe(true);
    expect(parsed.diskQuota).toBe(false);
  });

  it("falls back safely for unknown providers and missing fields", () => {
    const parsed = CloudRuntimeServiceSchema.parse({ provider: "unknown" });
    expect(parsed.operations).toEqual([]);
    expect(parsed.specs).toEqual([]);
    expect(parsed.persistentStorage).toBe(false);
  });
});
```

- [ ] **Step 2: 运行确认失败**

Run: `pnpm -C packages/core test -- cloud-runtime-service`
Expected: FAIL（schema 未导出）。

- [ ] **Step 3: 实现 schema 与 client**

`packages/core/api/schemas.ts`：

```ts
export const CloudRuntimeSpecSchema = z
  .object({
    id: z.string(),
    cpu: z.number().default(0),
    memory_mb: z.number().default(0),
    max_concurrent: z.number().default(1),
  })
  .loose()
  .transform((spec) => ({
    id: spec.id,
    cpu: spec.cpu,
    memoryMb: spec.memory_mb,
    maxConcurrent: spec.max_concurrent,
  }));

export const CloudRuntimeServiceSchema = z
  .object({
    provider: z.string().default(""),
    operations: z.array(z.string()).default([]),
    specs: z.array(CloudRuntimeSpecSchema).default([]),
    persistent_storage: z.boolean().default(false),
    disk_quota: z.boolean().default(false),
  })
  .loose()
  .transform((value) => ({
    provider: value.provider,
    operations: value.operations,
    specs: value.specs,
    persistentStorage: value.persistent_storage,
    diskQuota: value.disk_quota,
  }));
```

`packages/core/api/client.ts`：

```ts
async getCloudRuntimeService(): Promise<CloudRuntimeService> {
  const raw = await this.fetch<unknown>("/api/cloud-runtime/");
  return parseWithFallback(
    raw,
    CloudRuntimeServiceSchema,
    EMPTY_CLOUD_RUNTIME_SERVICE,
    { endpoint: "GET /api/cloud-runtime/" },
  );
}

async startCloudRuntimeNode(instanceId: string, idempotencyKey: string): Promise<void> {
  await this.fetchRaw("/api/cloud-runtime/nodes/start", {
    method: "POST",
    body: JSON.stringify({ instance_id: instanceId }),
    extraHeaders: { "Content-Type": "application/json", "Idempotency-Key": idempotencyKey },
  });
}

async stopCloudRuntimeNode(instanceId: string, idempotencyKey: string): Promise<void> {
  await this.fetchRaw("/api/cloud-runtime/nodes/stop", {
    method: "POST",
    body: JSON.stringify({ instance_id: instanceId }),
    extraHeaders: { "Content-Type": "application/json", "Idempotency-Key": idempotencyKey },
  });
}

async rebootCloudRuntimeNode(instanceId: string, idempotencyKey: string): Promise<void> {
  await this.fetchRaw("/api/cloud-runtime/nodes/reboot", {
    method: "POST",
    body: JSON.stringify({ instance_id: instanceId }),
    extraHeaders: { "Content-Type": "application/json", "Idempotency-Key": idempotencyKey },
  });
}
```

- [ ] **Step 4: 实现 core options 与 mutations**

`packages/core/runtimes/cloud-runtime.ts`：

```ts
export interface CloudRuntimeSpec {
  id: string;
  cpu: number;
  memoryMb: number;
  maxConcurrent: number;
}

export interface CloudRuntimeService {
  provider: string;
  operations: string[];
  specs: CloudRuntimeSpec[];
  persistentStorage: boolean;
  diskQuota: boolean;
}

export const EMPTY_CLOUD_RUNTIME_SERVICE: CloudRuntimeService = {
  provider: "",
  operations: [],
  specs: [],
  persistentStorage: false,
  diskQuota: false,
};

export const cloudRuntimeServiceKeys = {
  service: (wsId: string) => [...cloudRuntimeKeys.all(wsId), "service"] as const,
};

export function cloudRuntimeServiceOptions(wsId: string) {
  return queryOptions({
    queryKey: cloudRuntimeServiceKeys.service(wsId),
    queryFn: () => api.getCloudRuntimeService(),
    staleTime: 60 * 1000,
  });
}

export function useStartCloudRuntimeNode(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (instanceId: string) =>
      api.startCloudRuntimeNode(instanceId, crypto.randomUUID()),
    onSettled: () => qc.invalidateQueries({ queryKey: cloudRuntimeKeys.all(wsId) }),
  });
}

export function useStopCloudRuntimeNode(wsId: string) { /* same shape, api.stopCloudRuntimeNode */ }
export function useRebootCloudRuntimeNode(wsId: string) { /* same shape, api.rebootCloudRuntimeNode */ }
```

`crypto.randomUUID()` 在 core 中可用（浏览器与 RN Hermes 均支持）；若目标环境缺失，退回 `globalThis.crypto?.randomUUID?.() ?? Math.random().toString(36)`，但不要引入新依赖。

- [ ] **Step 5: 运行测试并提交**

Run: `pnpm -C packages/core test -- cloud-runtime-service`
Run: `pnpm -C packages/core typecheck`
Expected: PASS。
```bash
git add packages/core/runtimes/cloud-runtime.ts packages/core/api packages/core/runtimes/cloud-runtime-service.test.ts
git commit -m "feat(core): add cloud runtime capability discovery and node operations"
```

---

### Task 17: 前端 Runtimes 页面与六语言文案

**Files:**
- Modify: `packages/views/runtimes/components/cloud-runtime-dialog.tsx`
- Modify: `packages/views/runtimes/components/runtimes-page.tsx`
- Modify: `packages/views/runtimes/utils.ts`
- Modify: `packages/views/locales/{en,zh-Hans,zh-Hant,ja,ko,fr}/runtimes.json`
- Modify: `apps/web/app/[workspaceSlug]/(dashboard)/runtimes/page.tsx`、`apps/desktop/src/renderer/src/components/desktop-runtimes-page.tsx`
- Test: `packages/views/runtimes/utils.test.ts`、`packages/views/runtimes/components/cloud-runtime-dialog.test.tsx`、`packages/views/runtimes/components/runtimes-page.test.tsx`

**Interfaces:**
- Consumes: Task 16 的 `CloudRuntimeService/CloudRuntimeSpec/cloudRuntimeServiceOptions/use*CloudRuntimeNode`。
- Produces:
  - `cloudRuntimeOperations(service) → { canCreate, canStart, canStop, canReboot, canDelete }`
  - `isDockerCloudRuntime(service) boolean`
  - `CloudRuntimeDialog({ service, onClose })`
  - `RuntimesPage({ cloudRuntimeService, ... })`（移除 `cloudRuntimeEnabled`）

- [ ] **Step 1: 写失败的 utils 与组件测试**

```ts
// packages/views/runtimes/utils.test.ts（追加）
import { describe, expect, it } from "vitest";
import { cloudRuntimeOperations, isDockerCloudRuntime } from "./utils";

describe("cloudRuntimeOperations", () => {
  it("opens only operations the service declares", () => {
    const ops = cloudRuntimeOperations({
      provider: "docker",
      operations: ["create", "start", "stop"],
      specs: [],
      persistentStorage: true,
      diskQuota: false,
    });
    expect(ops.canCreate).toBe(true);
    expect(ops.canStart).toBe(true);
    expect(ops.canStop).toBe(true);
    expect(ops.canReboot).toBe(false);
    expect(ops.canDelete).toBe(false);
  });

  it("treats an unknown provider as not docker and grants nothing", () => {
    expect(isDockerCloudRuntime({ provider: "ec2" })).toBe(false);
    const ops = cloudRuntimeOperations(undefined);
    expect(ops.canCreate).toBe(false);
  });
});
```

```ts
// packages/views/runtimes/components/cloud-runtime-dialog.test.tsx
import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { CloudRuntimeDialog } from "./cloud-runtime-dialog";

vi.mock("@multica/core/api", () => ({
  api: {
    listCloudRuntimeNodes: vi.fn().mockResolvedValue([]),
    createCloudRuntimeNode: vi.fn(),
    deleteCloudRuntimeNode: vi.fn(),
    getCloudRuntimeService: vi.fn(),
  },
}));

describe("CloudRuntimeDialog", () => {
  it("renders docker specs and hides disk size for persistent volumes", () => {
    render(
      <CloudRuntimeDialog
        service={{
          provider: "docker",
          operations: ["create"],
          specs: [{ id: "standard", cpu: 2, memoryMb: 4096, maxConcurrent: 1 }],
          persistentStorage: true,
          diskQuota: false,
        }}
        onClose={() => {}}
      />,
    );
    expect(screen.getByText("standard")).toBeInTheDocument();
    expect(screen.queryByText(/disk size/i)).not.toBeInTheDocument();
  });
});
```

- [ ] **Step 2: 运行确认失败**

Run: `pnpm -C packages/views test -- runtimes/utils`
Run: `pnpm -C packages/views test -- cloud-runtime-dialog`
Expected: FAIL（函数/组件签名不存在）。

- [ ] **Step 3: 实现 utils**

```ts
import type { CloudRuntimeService } from "@multica/core/runtimes";

export interface CloudRuntimeOperations {
  canCreate: boolean;
  canStart: boolean;
  canStop: boolean;
  canReboot: boolean;
  canDelete: boolean;
}

export function cloudRuntimeOperations(
  service: CloudRuntimeService | undefined,
): CloudRuntimeOperations {
  const ops = new Set(service?.operations ?? []);
  return {
    canCreate: ops.has("create"),
    canStart: ops.has("start"),
    canStop: ops.has("stop"),
    canReboot: ops.has("reboot"),
    canDelete: ops.has("delete"),
  };
}

export function isDockerCloudRuntime(
  service: CloudRuntimeService | undefined,
): boolean {
  return service?.provider === "docker";
}
```

- [ ] **Step 4: 改造 dialog 与页面**

`CloudRuntimeDialog` 改为接收 `service: CloudRuntimeService`：

- 创建表单：spec 下拉来自 `service.specs`；`isDockerCloudRuntime(service)` 时隐藏 region/disk/image/subnet/key 字段；`cloudRuntimeOperations(service).canCreate` 为 false 时禁用提交。
- 节点行：按 `operations` 显示 start/stop/reboot 按钮；delete 按钮文案改为不可恢复确认。
- 错误 toast 使用服务端 message。

`RuntimesPage`：

- 删除 `cloudRuntimeEnabled` prop 与其向 `RuntimeList` 的透传；改为内部：

```ts
const serviceQuery = useQuery(cloudRuntimeServiceOptions(wsId));
const cloudRuntimeService = serviceQuery.data;
const showCloudRuntime = !!cloudRuntimeService?.provider;
```

- 入口按钮仅在 `showCloudRuntime` 时渲染；dialog 传入 `cloudRuntimeService`。
- `apps/web/app/[workspaceSlug]/(dashboard)/runtimes/page.tsx` 删除 `NEXT_PUBLIC_ENABLE_CLOUD_RUNTIME` 读取，直接 `<RuntimesPage />`。
- `desktop-runtimes-page.tsx` 不传该 prop（保持不变也能编译，因为 prop 已删除）。

- [ ] **Step 5: 补六语言文案**

每个 `packages/views/locales/<locale>/runtimes.json` 的 `cloud_runtime` 下新增（值按语言本地化，不得留英文占位）：

| key | en 示例 |
| --- | --- |
| `provider` | "Provider" |
| `operations` | "Operations" |
| `start` | "Start node" |
| `stop` | "Stop node" |
| `reboot` | "Reboot node" |
| `starting` | "Starting..." |
| `stopping` | "Stopping..." |
| `rebooting` | "Rebooting..." |
| `delete_irreversible` | "Delete this node permanently? Its container, volumes, and node token are removed and cannot be recovered." |
| `spec` | "Spec" |
| `persistent_volume` | "Persistent volume" |
| `unsupported_operation` | "This operation is not available for this provider." |

zh-Hans 示例：`start`="启动节点"、`stop`="停止节点"、`reboot`="重启节点"、`delete_irreversible`="永久删除此节点？容器、数据卷和节点令牌将一并移除，无法恢复。"

- [ ] **Step 6: 运行测试与静态检查**

Run: `pnpm -C packages/views test -- runtimes`
Run: `pnpm -C packages/views typecheck`
Run: `pnpm -C apps/web typecheck 2>/dev/null || pnpm typecheck`
Expected: PASS。现有 `runtimes-page.test.tsx` 中删除对 `cloudRuntimeEnabled` 的断言，改为 mock `getCloudRuntimeService` 并断言入口可见性。

- [ ] **Step 7: 提交**

```bash
git add packages/views packages/core apps/web apps/desktop
git commit -m "feat(views): drive cloud runtime UI from provider capabilities"
```

---

### Task 18: 集成测试、E2E、文档与验收

**Files:**
- Create: `docker/runtime/Dockerfile.test`、`docker/runtime/fake-claude`
- Create: `server/internal/fleet/integration_test.go`（`//go:build fleetintegration`）
- Create: `e2e/cloud-runtime-docker.spec.ts`
- Modify: `AGENTS.md`、`CONTRIBUTING.md`、设计文档、`CLI_AND_DAEMON.md`、`SELF_HOSTING_AI.md`、`.env.example`

**Interfaces:**
- Produces: opt-in 集成测试入口 `TestFleetNodeLifecycleRealDocker`（需 `MULTICA_FLEET_INTEGRATION=1`、Docker、DATABASE_URL）；E2E 入口（需 `MULTICA_RUN_FLEET_E2E=1`）。

- [ ] **Step 1: 写 fake Claude 运行时镜像**

```bash
# docker/runtime/fake-claude
#!/usr/bin/env bash
# Test double for Claude Code: accepts the same flags and exits 0.
exit 0
```

```dockerfile
# docker/runtime/Dockerfile.test
FROM multica/runtime-claude:dev
USER root
COPY docker/runtime/fake-claude /usr/local/bin/claude
RUN chmod 0755 /usr/local/bin/claude
USER multica
```

- [ ] **Step 2: 写集成测试骨架**

```go
//go:build fleetintegration

package fleet_test

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestFleetNodeLifecycleRealDocker(t *testing.T) {
	if os.Getenv("MULTICA_FLEET_INTEGRATION") != "1" {
		t.Skip("set MULTICA_FLEET_INTEGRATION=1 with a running Docker daemon")
	}
	// Preconditions: docker daemon reachable, DATABASE_URL points at the
	// worktree database, multica/runtime-claude:test and multica/fleet:dev built.
	// This test creates, stops, starts, reboots, and deletes ONE node; it only
	// touches resources labelled with its own namespace and cleans up in t.Cleanup.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	namespace := "fleet-it-" + time.Now().Format("20060102150405")
	// Step A: isolated DB + migrations (testutil.IsolatedDB), Fleet service with
	// SDK provider, random loopback listener, credentials file with a fake key.
	// Step B: POST /api/v1/nodes with Idempotency-Key; assert 201 and launching.
	// Step C: poll GET /api/v1/nodes until status=running (or timeout); assert the
	// container has the namespace/node/role labels, non-root user, no socket mount.
	// Step D: POST /api/v1/pat/verify with the node token; assert owner_id matches.
	// Step E: DELETE while a queued task exists -> 409; cancel the task; retry -> accepted.
	// Step F: delete node; assert container and both volumes are gone.
	_ = ctx
	_ = namespace
}
```

- [ ] **Step 3: 写 E2E 骨架**

```ts
import { test, expect } from "@playwright/test";

const enabled = process.env.MULTICA_RUN_FLEET_E2E === "1";

test.describe("cloud runtime with local docker fleet", () => {
  test.skip(!enabled, "set MULTICA_RUN_FLEET_E2E=1 with a running fleet component");

  test("creates a docker node and shows it running", async ({ page }) => {
    await page.goto("/");
    // The suite runs against an already-signed-in environment; reuse the
    // existing storage state helper from e2e/ when wiring this for real.
    await page.getByRole("link", { name: /runtimes/i }).click();
    await page.getByRole("button", { name: /cloud runtime/i }).click();
    await expect(page.getByText(/docker/i)).toBeVisible();
  });
});
```

- [ ] **Step 4: 更新仓库文档**

- `AGENTS.md`：Development 段增加 `make up C=api,web,fleet` 说明；Testing 段增加 `fleetintegration` 标签与默认测试不依赖 Docker 的规则；环境组件列表包含 `fleet`。
- `CONTRIBUTING.md`：环境组件说明与 Fleet 首次构建耗时、Docker Desktop 前置。
- 设计文档：第 4 行状态改为"阶段一已实现（日期）"；在 `13 后追加"实现差异"：Fleet 容器网络、凭证文件 schema、API 与 Fleet 的 prepare/approve 分工、未覆盖项。
- `CLI_AND_DAEMON.md`：新增"本地 Docker Cloud Runtime"小节（启动、凭证配置、节点操作、故障排查）。
- `SELF_HOSTING_AI.md`：可选 fleet 服务与凭证文件说明（不含真实密钥）。
- `.env.example`：追加 `MULTICA_FLEET_PORT`、`MULTICA_LOCAL_FLEET_URL`、`MULTICA_LOCAL_FLEET_SECRET_FILE`、`MULTICA_LOCAL_FLEET_NAMESPACE`、`MULTICA_FLEET_RUNTIME_IMAGE`、`MULTICA_FLEET_CREDENTIALS_FILE` 注释模板。

- [ ] **Step 5: 全量验证**

Run: `pnpm typecheck && pnpm lint && pnpm test`
Run: `make test`
Run: `make sqlc && git diff --exit-code server/pkg/db/generated`
Run（opt-in，需 Docker 与授权）: `cd server && MULTICA_FLEET_INTEGRATION=1 go test -tags=fleetintegration ./internal/fleet -run TestFleetNodeLifecycleRealDocker -count=1 -v`
Run（opt-in）: `MULTICA_RUN_FLEET_E2E=1 pnpm exec playwright test e2e/cloud-runtime-docker.spec.ts`
Expected: 默认检查全绿；opt-in 通过且结束后无残留容器/卷。

- [ ] **Step 6: 最终验收清单**

逐条勾选设计文档 `12 的 11 条 + 本计划 `3 的 12–16 条，并在 PR 描述中粘贴实际命令输出。任何一条未通过都不得标记完成。

- [ ] **Step 7: 提交**

```bash
git add docker/runtime server/internal/fleet e2e AGENTS.md CONTRIBUTING.md docs/superpowers CLI_AND_DAEMON.md SELF_HOSTING_AI.md .env.example
git commit -m "test(fleet): add opt-in docker integration coverage and docs"
```

---

## Self-Review

### Spec 覆盖映射

| Spec 要求 | 覆盖任务 |
| --- | --- |
| `3 模块与部署（Fleet/store/docker/镜像） | 1–11 |
| `4 本地配置与能力、互斥、能力发现 | 3、11、12、16 |
| `5 API 契约（认证、幂等、错误、region=local） | 8、12、14 |
| `6 Store/身份/私密配置/表与索引 | 1、4、6、11、13 |
| `7 创建到运行（镜像、bootstrap、daemon、preflight） | 7、9、10 |
| `8 生命周期、屏障、stop/delete 语义、tombstone | 5、9、14、15 |
| `9 网络与安全边界 | 7、10、11、12 |
| `10 故障恢复（协调器、有界重试、不确定态） | 9、14 |
| `11 开发入口与共享界面 | 11、16、17 |
| `12 测试与验收 | 1–18（含 opt-in 集成/E2E） |

### Placeholder 扫描

- 无 `TBD/TODO/implement later`；每个代码步骤包含可编译骨架。
- 明确的实施时核对点（不是占位符）：sqlc 生成的具体参数名可能与计划中的 `db.*Params` 字段名略有差异，以 `make sqlc` 输出为准并同步调用点；Docker SDK 的具体类型签名以 `go doc github.com/docker/docker/client` 为准；现有 `runtime.sql` 的 upsert 位于两个查询中，两处都要检查受管列不被覆盖。
- 计划中 Task 8/14 的两段式协调已标注：先 prepare 提交，再事务外 diagnose，再 approve/abort；实现时不得把 diagnose 放进持有排他锁的事务。

### 类型一致性检查

- `barrier.Key(namespace, nodeID string) int64` 在 Task 2 定义，Task 14/15 使用一致。
- `docker.Provider` 接口在 Task 7 定义，Task 8/9 的 fakeProvider 完整实现该接口。
- `db.FleetNode { DaemonID, ContainerID, ContainerName, TargetState, DaemonReady, HealthCheckedAt, OwnerID }` 字段与 Task 1 迁移一致。
- `CloudRuntimeService { provider, operations, specs, persistentStorage, diskQuota }` 在 Task 16 定义，Task 17 使用一致。
- `auth.CloudPATVerifierConfig { FleetBaseURL, Redis, HTTPClient, DisableCache }` 在 Task 12 定义并在 router 组装使用一致。
