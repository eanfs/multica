# 统一 Cloud Runtime 重构计划（2026-11-03 复核版）

状态：待评审。本文档已按 2026-11-03 的 `origin/main` 与 sync PR #277 复核；此前 2026-10-28 版基于的旧事实（QCA 未合入、存在 `aurorafleet`）已废弃。

## 0. 基线核查结论（2026-11-03）

- `origin` = `eanfs/multica`，是**主仓库**，不是 fork；默认 PR/push 均指向它，`upstream` 只是上游源。
- `origin/main`（`764724e9f`）已经包含 QCA：`server/internal/qoderruntime`、`server/internal/handler/qoder_*`、`cloud_agent_connection`（迁移 `586`–`588`）、`packages/core/runtimes/qoder*`、runtimes 页 cloud services。**不再需要“把 QCA 移植进主仓库”**。
- Aurora 已收敛到**单一 Docker 控制面**：`server/internal/fleet` + `fleetguard`；server 侧通过 `server/internal/aurora/fleet_provisioner.go` 供给节点。`server/internal/aurorafleet` 已删除；`aurora/sandbox_manager.go`、`sandbox_enrollment.go`、`sandbox_reaper.go` 仍保留并驱动该 provisioner。
- 待合并：**sync PR #277**（`sync/upstream-20261103`，upstream 186 commits）。合并后基线再前进，QCA 将新增 attachments/quickcreate 能力与 runtime-wide concurrency；迁移 `592`–`607` 随之进入主仓库。
- 迁移规则（`AGENTS.md`）：新 Aurora fork-only 迁移使用保留段 **5000+**；通用新迁移紧接当前最大号（当前 `607`，即 `608+`）；**不得重命名已应用迁移**（`schema_migrations` 以完整文件名为键）。
- 结论：重构对象是 **QCA 与 Aurora 两套 server 侧连接/调度逻辑**，不是分支合并或迁移重编号。

## 1. 决策与范围

- 基线：`origin/main`；若 sync PR #277 先合并，则从合并后的 main 起。
- 范围：逆向工程 QCA（主仓库当前版本）形成行为契约；建立统一 `Provider` 接口 + `Supervisor` 调度层；Aurora（`aurora_managed`）与 QCA（`qoder_cloud`）迁移为 provider，共用连接表、journal、租约、凭证与状态；删除两套重复调度。
- 流程：先更新本 Plan PR 供评审，批准后实施。
- 不在范围：`server/internal/fleet` 控制面、daemon 任务协议、Aurora 产品 UI/计费/创作管线、Qoder API 客户端重写、SaaS Billing/entitlement、mobile；**不合并** Fleet 控制面与 Aurora provider 的节点实现。

## 2. 目标与成功标准

**目标**：一份 `CloudRuntimeProvider` 接口 + 一份 `Supervisor` 调度层，Aurora 与 QCA 作为两个 provider 共用 `cloud_runtime_connection`、journal、租约、凭证、状态与可观测性。

**成功标准（可验证）**：

1. Plan PR 先合并：仅文档，`git diff --check` 通过，不含代码/迁移/配置变更。
2. `server/internal/cloudruntime` 中存在唯一 `Provider` 接口、`Supervisor`、`Connection` store；全仓 grep 不再有 `RunQoderConnections`、`runQoderSupervisor`、`SandboxReaper.Sweep` 独立循环、`newWorkspaceSandboxManager`/`newSandboxReaper` 装配。
3. Aurora 的 `Ensure/Reaper/Enrollment` 合并为一个 `aurora_managed` provider（基于 `fleet_provisioner`）；QCA supervisor/bridge 适配为 `qoder_cloud` provider；两者共用 `cloud_runtime_connection`。
4. QCA 行为不回归：`qoderruntime` 现有 bridge/并发/续跑/事件/取消测试全绿；workspace 端点与前端 schema 不变；sync 后新增的 attachments/quickcreate 行为在迁移中保留。
5. Aurora 全链路不回归：generation 创建 → `fleet_provisioner` 供给 → enrollment → daemon claim → 执行 → 结算；现有 sandbox/enrollment/reaper 测试迁移后等价通过。
6. 迁移号唯一、`make sqlc` 无 diff、`go build ./...` 通过；`pnpm typecheck/lint/test`（非 mobile）通过；runtimes 页无回归。
7. 删除旧实现后 `git grep` 无残留引用；Fleet 控制面与 Aurora provider 边界不变。

## 3. Phase 1：契约基线与逆向（QCA 已在 main）

- 基于主仓库当前 `qoderruntime`/`handler/qoder_*` 重写 `docs/cloud-runtime/qca-reverse-engineering.md`：端点、状态机、事件映射、幂等/恢复、凭证、单主/revision/租约语义，并覆盖 sync PR #277 带来的 attachments/quickcreate。
- 以现有 `qoderruntime` 测试为行为金标准，新增 provider 注入契约测试（LoadState/SaveState/BeforeStep/OnReady）。
- 梳理 Aurora 调度边界：`fleet_provisioner`（节点供给）、`sandbox_manager`（Ensure）、`enrollment`（mse_→mdt_）、`reaper`（生命周期/结算）的职责与调用点。

## 4. Phase 2：统一接口与调度层

**包结构**（扩展 `server/internal/cloudruntime/`，保留 `client.go`、`fleet_internal.go`）：`provider.go`、`supervisor.go`、`store.go`、`registry.go`、`lease.go`、`connection.go`。

**接口契约**：

```go
type Provider interface {
    Name() string                              // "qoder_cloud" | "aurora_managed"
    ValidateConfig(config []byte) error
    Run(ctx context.Context, rt *Runtime) error // 单连接驱动，可重启、可恢复
}
type Runtime struct {
    WorkspaceID        pgtype.UUID
    ConnectionRevision int64
    Config             []byte
    LoadState          func(context.Context) ([]byte, error)
    SaveState          func(context.Context, []byte) error
    BeforeStep         func(context.Context) (release func(), error)
    OnReady            func(context.Context) error
    OnObservation      func(context.Context, Observation) error
    Wake               <-chan struct{}
    Now                func() time.Time
    Log                *slog.Logger
}
type Observation struct { Status, LastError string; Detail map[string]any }
```

**调度层语义**（复用 QCA 已验证机制）：

- 单主：`pg_try_advisory_lock(71243,8191)`；每 enabled 连接一个 runner；`revision` 变化取消重启；失败指数退避 10s→160s，封顶 5min；5s 监管循环。
- 租约：`BeforeStep` 原子写 `busy_until=now()+45s` 并校验 owner/revision；释放清空。
- 状态：`status`（starting/running/error/paused/stopped）、`last_error`、`revision`、`observation jsonb`；provider 只上报非密 inventory。
- 凭证：统一续期/撤销 `daemon_token`；provider config 统一 AES-256-GCM。
- 触发：`Supervisor.Wake(ws, provider)`（Aurora generation）；QCA 忽略唤醒。
- 清理：停 provider → 请求远端取消 → 删连接 → 解绑 agent。

**数据模型**：

- `cloud_agent_connection` 重命名 `cloud_runtime_connection`（保留列），新增 `observation jsonb NOT NULL DEFAULT '{}'`。
- 新迁移：`608_cloud_runtime_connection_rename`、`609_cloud_runtime_connection_observation`（通用段；不触碰已应用迁移）。
- 更新 `pkg/db/queries`、sqlc、QCA handler/前端 schema 表名。

**验收**：框架单测覆盖单主、revision 重启、租约互斥、退避、加解密、owner 校验、清理顺序。

## 5. Phase 3：QCA 迁移为 provider

- 新增 `server/internal/qoderruntime/provider.go`：实现 `Provider`，把 `Runtime` 四设施注入 `Bridge.Config`，`Run` 调用 `Bridge.Run`；standalone `cmd/qoder-runtime` 继续复用同一 `Bridge`。
- `handler/qoder_connection.go`：删除 `RunQoderConnections`、`runQoderSupervisor`、`managedQoderRun`、`acquireQoderStep`、`qoderReady`、`qoderCredentialAllowed` 的调度/续期/租约逻辑；保留 settings、check/save/stop/resume/disconnect，改调 `cloudruntime.Store` 与 `Supervisor`。
- `handler/qoder_cleanup.go`、`qoder_disconnect.go`：改为通用 store/supervisor 接口。
- 注册：`cmd/server` 装配 `registry.Register(qoderruntime.NewProvider())`，避免循环 import。
- 测试：现有 bridge/concurrency/credentials 测试保持；新增 provider 注入测试；handler 测试迁到通用 supervisor 测试。

## 6. Phase 4：Aurora 迁移为 provider

- 新增 `server/internal/aurora/provider.go`（import `cloudruntime`）：
  - `Name()="aurora_managed"`；`ValidateConfig` 校验 image digest/policy（Fleet URL、service key 仍从 server env/file 读取，不进 DB）。
  - `Run`：`select { Wake, ticker }` → ①`reapPass`（现 `SandboxReaper.Sweep`：starting 2min、online idle 15min、8h 硬寿命、drain 30min、失败任务结算）→ ②`observe`（节点/任务状态写 `Observation`）→ ③`ensurePass`（复用 `SandboxManager.arm` + `SandboxEnrollmentService.Issue` + `FleetProvisioner.EnsureWorkspaceNode`）。
  - `Ensure(ctx, ws)`：handler 同步加速路径，只做 arm+enroll+ensure，保留 fail-closed；与 reap 共享 workspace advisory lock 与一次性 enrollment。
- `CreateAuroraGeneration`：`supervisor.Wake(ws, "aurora_managed")` + 同步 `provider.Ensure`；失败保持现有 503/错误码。
- 连接行：`EnsureSystemAgents` 幂等创建 `cloud_runtime_connection`（provider=`aurora_managed`，owner=workspace owner，config=非密 image/policy）；Fleet 未配置时 status=error、fail-closed。
- `aurora_sandbox_node` 保留为 provider 明细表。
- 删除：`sandbox_manager.go` 独立调度路径、`sandbox_reaper.go` 独立 sweeper、`cmd/server/main.go` 的 `newWorkspaceSandboxManager`/`newSandboxReaper` 装配；保留 enrollment、`fleet_provisioner`、daemon 执行、任务结算。
- 测试：现有 sandbox/enrollment/reaper 测试迁移为 provider 测试；新增 Wake→Ensure→reap 集成测试（fake provisioner）。

## 7. Phase 5：前端统一、删除与可观测

- `packages/core/runtimes/display.ts`：`aurora_managed` 显示名；`runtimes-page.tsx`：把 `aurora_managed` 从 machines 排除，cloud services 分组显示只读状态行（状态/节点数/最后错误，无连接表单）；`provider-logo.tsx` 复用 Cloud 图标。
- QCA 连接卡/详情页保持；schema 表名变化不影响 API 响应。
- 删除残留符号；`git grep` 校验。
- 文档：更新 `server/cmd/qoder-runtime/README.md`；新增 `docs/cloud-runtime/README.md`（框架边界、provider 接入指南、迁移号规则）。

## 8. 边界条件与失败模式

- 单主崩溃/切换：锁释放 → 新主恢复 journal；租约 + 幂等防重复远端调用。
- revision 并发（save/stop 与 runner）：租约冲突可重试；save/stop 拒绝 active 任务。
- 凭证缺失：`MULTICA_VCS_SECRET_KEY` 缺失 → QCA 503、Aurora autoprovision 禁用（fail-closed）。
- enrollment 过期/重放：5min 一次性 token，统一 401；starting 2min 未消费 → reaper 失败并结算。
- 节点/容器泄漏：Fleet 控制面按 label 清理孤儿资源（保留）。
- QCA 响应丢失/提交模糊：发送意图先落盘、不重发、创建响应丢失不重建（保持）。
- workspace 删除/owner 降权：停 runner → 取消远端 → 删连接/节点 → 解绑 agent。
- 迁移：只新增 `608+`/`5000+`；不重命名已应用迁移；不处理历史 `129+` 号段冲突（由既有 grandfather 测试覆盖）。

## 9. 测试与验收

- Go：`go build ./...`、`go vet`、`make sqlc`（无 diff）、`go test ./internal/cloudruntime/... ./internal/qoderruntime/... ./internal/aurora/... ./internal/fleet/... ./internal/handler/... ./cmd/server/...`。
- 迁移：`go test ./internal/migrations ./cmd/migrate`；干净库 `./cmd/migrate up`；迁移号 lint（含 5000+ 段规则）。
- 前端：`pnpm typecheck`、`pnpm lint`、`pnpm test`（非 mobile）。
- E2E：runtimes 页（QCA 连接流、Aurora cloud 行）、Aurora generation 全链路、QCA issue 执行链路。
- 验收门：第 2 节 7 条满足；旧调度符号 grep 为空；QCA 测试语义不变。

## 10. 实施顺序与 PR 拆分

| PR | 内容 | 依赖 |
| --- | --- | --- |
| Plan | 本计划 + 两份调研（已复核） | — |
| 1 | QCA 逆向工程文档 + 契约测试基线 | sync #277 / Plan |
| 2 | 统一框架 + `cloud_runtime_connection` 迁移 608–609 + 单测 | 1 |
| 3 | QCA provider 迁移 + supervisor 删除 | 2 |
| 4 | Aurora provider 迁移 + 旧调度删除 | 2 |
| 5 | 前端 cloud service 行 + 文档 + 清理 | 3,4 |

PR 3 与 4 可在 2 完成后并行（`handler.go`/`router.go`/`cmd/server/main.go` 由 Lead 串行合并，其余文件域分离）。

## 11. 假设与风险

**假设**：sync PR #277 先于或同时于重构落地；standalone QCA bridge 保留；Fleet 控制面与 daemon 协议不变；Aurora 产品 UI/计费不变；不新增外部依赖；不重命名已应用迁移。

**风险与缓解**：

- sync PR #277 未合并前，QCA attachments/quickcreate 尚不在 main → 契约文档标注两者差异，迁移在 #277 合并后开始。
- 统一 supervisor 改变 QCA 时序 → 现有 `qoderruntime` 测试为金标准，provider 适配层单独测试。
- Aurora 同步 Ensure 与后台 reap 竞态 → 共享 workspace advisory lock + 一次性 enrollment + 行锁；集成测试覆盖并发。
- 迁移号段误用 → 所有新迁移显式标注段位并跑 `TestMigrationNumericPrefixesAreUnique`/`TestAuroraMigrationsUseTheReservedRange`。
- 前端 cloud service 分组回归 → runtimes 页测试先行，`aurora_managed` 从 machines 排除。
