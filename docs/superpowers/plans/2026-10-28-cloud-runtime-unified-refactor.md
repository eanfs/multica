# 统一 Cloud Runtime 重构计划（Plan PR 先行）

状态：待评审。本文档已按 2026-10-28 的 `origin/main` 实测基线修正；早期基于本地 main（缺少 Aurora）的判断已废弃。

## 0. 基线核查结论（2026-10-28）

- `origin` = `eanfs/multica`，是**主仓库**，不是 fork；默认 PR/push 均指向它，`upstream` 只是上游源。
- `origin/main`（`ea9bff852`）已经包含 Aurora 产品线：`apps/aurora`、`server/internal/aurora`（`sandbox_manager`/`sandbox_enrollment`/`sandbox_reaper`/`settlement`）、`server/internal/aurorafleet`（`auth`/`client`/`policy`/`reconcile`/`docker`）、`server/internal/fleet` + `fleetguard`（本地 Docker 执行节点）、`server/internal/cloudruntime`（`client` + `fleet_internal`）。迁移最大号为 `582`。
- **QCA 不在 `origin/main`**：Qoder Cloud Agents（`server/internal/qoderruntime`、`handler/qoder_*`、`cloud_agent_connection`、前端 qoder 组件）位于 v0.7 上游线（本地 main / tag `v0.7.0`），迁移号为 `586`–`588`。
- `origin/feature/agency-aurora-ark-fleet` 仅比 `origin/main` 多 1 个提交（`22f6b08ed`），已基本合入；不再作为重构基线。
- 结论：**基线 = `origin/main`；QCA 需要移植进主仓库**，而不是把 agency 分支 merge 进 main。

## 1. 决策与范围（已确认）

- 基线：`origin/main` 新建重构分支；先把 QCA 从 v0.7 线移植到主仓库，再统一。
- 范围：逆向工程 QCA 形成行为契约；以它为参考建立统一 `Provider` 接口 + `Supervisor` 调度层；Aurora（`aurora_managed`）与 QCA（`qoder_cloud`）都迁移为 provider，共用连接表、journal、租约、凭证与状态；删除两套重复调度。
- 流程：先创建只含文档的 **Plan PR** 供评审，批准后实施。
- 不在范围：Aurora 产品 UI/计费/创作管线、Qoder API 客户端重写、daemon 任务协议、SaaS Billing/entitlement、mobile；**不合并** `server/internal/fleet`（本地执行节点）与 `aurorafleet`（Aurora sandbox），两者职责不同，只统一 server 侧连接与调度框架。

## 2. 目标与成功标准

**目标**：一份 `CloudRuntimeProvider` 接口 + 一份 `Supervisor` 调度层，Aurora 与 QCA 作为两个 provider 共用 `cloud_runtime_connection`、journal、租约、凭证、状态与可观测性。

**成功标准（可验证）**：

1. Plan PR 先合并：仅 3 个文档，`git diff --check` 通过，不含代码/迁移/配置变更。
2. QCA 已移植进主仓库且行为不变：`server/internal/qoderruntime` 的 bridge/并发/续跑/事件/取消测试全绿；workspace 端点与前端 schema 不变；迁移号唯一（`583`–`585`）。
3. `server/internal/cloudruntime` 中存在唯一 `Provider` 接口、`Supervisor`、`Connection` store；全仓 grep 不再有 `RunQoderConnections`、`runQoderSupervisor`、`SandboxReaper.Sweep` 独立循环、`newWorkspaceSandboxManager`/`newSandboxReaper` 装配。
4. Aurora 的 `Ensure/Reaper/Enrollment` 合并为一个 `aurora_managed` provider；QCA supervisor/bridge 适配为 `qoder_cloud` provider；两者共用 `cloud_runtime_connection`。
5. Aurora 全链路不回归：generation 创建 → 节点供给 → enrollment → daemon claim → 执行 → 结算；现有 sandbox/enrollment/reaper 测试迁移后等价通过。
6. 迁移号唯一、`make sqlc` 无 diff、`go build ./...` 通过；`pnpm typecheck/lint/test`（非 mobile）通过；runtimes 页无回归。
7. 删除旧实现后 `git grep` 无残留引用；`fleet`/`aurorafleet` 边界不变。

## 3. Phase 0（先行）：Plan PR（仅文档）

**分支**：从 `origin/main` 新建 `docs/cloud-runtime-unified-plan`，只提交：

- `docs/superpowers/plans/2026-10-28-cloud-runtime-unified-refactor.md`（本计划）
- `docs/superpowers/research/qca-v070.md`
- `docs/superpowers/research/aurora-cloud-runtime.md`

**排除**：`2026-10-04-local-docker-cloud-runtime.md`、`docs/superpowers/specs/`、`dsh-session-*` 等既有未跟踪内容，禁止 `git add .`。

**步骤**：独立 worktree（避免污染用户工作区）→ 写 3 个文档 → `git diff --check` → commit `docs(cloud-runtime): add unified runtime refactor plan` → push `origin` → `gh pr create --repo eanfs/multica --base main`（使用 `.github/PULL_REQUEST_TEMPLATE.md`）。

**验证**：`git status` 只含 3 个文档；`git diff --check`；PR 打开。

## 4. Phase 1：把 QCA 移植到 `origin/main`

**创建**：worktree/分支 `refactor/cloud-runtime-unified`，从 `origin/main` 起。

**依赖核查（已完成）**：主仓库已具备 QCA 所需的 `auth.GenerateDaemonToken`、`DaemonTokenCache`、`secretbox`/`VCSSecretBox`、`openVCSSecret`/`sealVCSSecret`、`runtime_mode`、`ClaimTaskForRuntime`；缺 QCA 自身的代码与三对迁移。

**步骤**：

1. **代码移植**：优先 `git cherry-pick -x` QCA 提交（`0fe2ebf02` managed QCA、`ac61e5ed3` rename、`c9b6de856` docs、`f4250e429` available-without-encryption、`188971168` spelling；以及 standalone 早期提交）；若与主仓库分叉冲突过大，改为按文件移植并保留提交说明。
2. **迁移重编号**：`586_cloud_agent_connection` → `583_cloud_agent_connection`；`587_cloud_agent_connection_provider_index` → `584_cloud_agent_connection_provider_index`；`588_daemon_token_owner` → `585_daemon_token_owner`。不改 SQL；更新 `server/cmd/migrate/main.go` 中 `587` 的并发索引登记与 `server/cmd/qoder-runtime/README.md` 的迁移号说明；`583`–`585` 后续不再复用。
3. **前端移植**：`packages/core/runtimes/qoder*.ts`、`packages/views/runtimes/components/qoder-*`、agents 的 Qoder 选择/绑定组件、runtimes 页 machines/cloud services 分组；与主仓库现有 runtimes 页做冲突合并，保持既有 Aurora/Fleet 行为。
4. **装配**：`router.go` 的 workspace qoder 端点、`main.go` 的 `RunQoderConnections` 启动、`handler.go` 字段；按主仓库现有装配风格接入。
5. **验证**：`go build ./...`；`make sqlc` 无 diff；`go test ./internal/qoderruntime ./internal/handler -run 'Qoder|CloudRuntime'`；`pnpm typecheck/lint/test`；干净库 `./cmd/migrate up` 成功。

**风险**：cherry-pick 冲突、前端 runtimes 页合并、`daemon.go` 注册门禁与主仓库 `runtime_mode` 语义差异；冲突时按文件移植并在 PR 中记录。

## 5. Phase 2：逆向工程 QCA（设计契约）

**交付**：`docs/cloud-runtime/qca-reverse-engineering.md`，标注 `v0.7.0` 出处：

- 端点矩阵：Multica daemon 侧、workspace 侧、Qoder 侧。
- 状态机：connection、journal phase、task/issue 生命周期。
- 事件映射：`agent.message/tool_use/result`、`session.error`、`session.status_idle + end_turn`。
- 调度语义：单主 advisory lock(71243,8191)、每连接 runner、revision 驱动、`busy_until=45s` 租约、指数退避、5s 循环。
- 幂等/恢复：发送意图先落盘、模糊响应不重发、创建响应丢失不重建、`idempotency_key`、取消等待 idle/terminated。
- 凭证矩阵：`mdt_`（1h）、`mat_`（24h）、AES-256-GCM、`mcn_` 与 QCA 无关。
- **框架映射表**：每条行为落到 `Provider.Run` / `Runtime.LoadState` / `Runtime.BeforeStep` / `Supervisor` 的哪一项。

**测试基线**：QCA 现有测试为行为金标准；新增 provider 注入契约测试。

## 6. Phase 3：统一接口与调度层

**包结构**（扩展 `server/internal/cloudruntime/`，保留 `client.go`、`fleet_internal.go`）：

- `provider.go`、`supervisor.go`、`store.go`、`registry.go`、`lease.go`、`connection.go`。

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
- 凭证：统一续期/撤销 `daemon_token`（<30min 续期；停用/降权/删除立即撤销）；provider config 统一 AES-256-GCM。
- 触发：`Supervisor.Wake(ws, provider)`（Aurora generation）；QCA 忽略唤醒。
- 清理：停 provider → 请求远端取消 → 删连接 → 解绑 agent。

**数据模型**：

- `cloud_agent_connection` 重命名为 `cloud_runtime_connection`，保留列；新增 `observation jsonb NOT NULL DEFAULT '{}'`。
- 新迁移 `586_cloud_runtime_connection_rename`、`587_cloud_runtime_connection_observation`（紧接 QCA 的 583–585）。
- 更新 `pkg/db/queries`、sqlc、QCA handler/前端 schema 表名。

**验收**：框架单测覆盖单主、revision 重启、租约互斥、退避、加解密、owner 校验、清理顺序。

## 7. Phase 4：QCA 迁移为 provider

- 新增 `server/internal/qoderruntime/provider.go`：实现 `Provider`，把 `Runtime` 四设施注入 `Bridge.Config`，`Run` 调用 `Bridge.Run`；standalone `cmd/qoder-runtime` 继续复用同一 `Bridge`。
- `handler/qoder_connection.go`：删除 `RunQoderConnections`、`runQoderSupervisor`、`managedQoderRun`、`acquireQoderStep`、`qoderReady`、`qoderCredentialAllowed` 的调度/续期/租约逻辑；保留 settings、check/save/stop/resume/disconnect，改调 `cloudruntime.Store` 与 `Supervisor`。
- `handler/qoder_cleanup.go`、`qoder_disconnect.go`：改为通用 store/supervisor 接口。
- 注册：`cmd/server` 装配 `registry.Register(qoderruntime.NewProvider())`，避免循环 import。
- 测试：现有 bridge/concurrency/credentials 测试保持；新增 provider 注入测试；handler 测试迁到通用 supervisor 测试。

## 8. Phase 5：Aurora 迁移为 provider

**现状（`origin/main`）**：`aurora.SandboxManager.Ensure`（请求驱动供给）+ `SandboxEnrollmentService`（一次性 `mse_` → `mdt_`）+ `SandboxReaper.Sweep`（独立 sweeper）+ `aurorafleet.ControlClient`（独立控制面）；`server/internal/fleet` 是本地 Docker 执行节点，保持独立。

- 新增 `server/internal/aurora/provider.go`（import `cloudruntime`）：
  - `Name()="aurora_managed"`；`ValidateConfig` 校验 image digest/policy（fleet URL、control token file 仍从 server env/file 读取，不进 DB）。
  - `Run`：`select { Wake, ticker }` → ①`reapPass`（starting 2min、online idle 15min、8h 硬寿命、drain 30min、失败任务结算）→ ②`observe`（节点/任务状态写 `Observation`）→ ③`ensurePass`（复用 `SandboxManager.arm` + `SandboxEnrollmentService.Issue` + fleet `EnsureWorkspaceNode`）。
  - `Ensure(ctx, ws)`：handler 同步加速路径，只做 arm+enroll+ensure，保留 fail-closed；与 reap 共享 workspace advisory lock 与一次性 enrollment。
- `CreateAuroraGeneration`：`supervisor.Wake(ws, "aurora_managed")` + 同步 `provider.Ensure`；失败保持现有 503/错误码。
- 连接行：`EnsureSystemAgents` 幂等创建 `cloud_runtime_connection`（provider=`aurora_managed`，owner=workspace owner，config=非密 image/policy）；fleet 未配置时 status=error、fail-closed。
- `aurora_sandbox_node` 保留为 provider 明细表。
- 删除：`sandbox_manager.go` 独立调度路径、`sandbox_reaper.go` 独立 sweeper、`cmd/server/main.go` 的 `newWorkspaceSandboxManager`/`newSandboxReaper` 装配；保留 enrollment、fleet control、daemon 执行、任务结算。
- 测试：现有 sandbox/enrollment/reaper 测试迁移为 provider 测试；新增 Wake→Ensure→reap 集成测试（fake fleet）。

## 9. Phase 6：前端统一、删除与可观测

- `packages/core/runtimes/display.ts`：`aurora_managed` 显示名；`runtimes-page.tsx`：把 `aurora_managed` 从 machines 排除，cloud services 分组显示只读状态行（状态/节点数/最后错误，无连接表单）；`provider-logo.tsx` 复用 Cloud 图标。
- QCA 连接卡/详情页保持；schema 表名变化不影响 API 响应。
- 删除残留符号；`git grep` 校验。
- 文档：更新 `server/cmd/qoder-runtime/README.md` 迁移号/语义；新增 `docs/cloud-runtime/README.md`。

## 10. 边界条件与失败模式

- 单主崩溃/切换：锁释放 → 新主恢复 journal；租约 + 幂等防重复远端调用。
- revision 并发（save/stop 与 runner）：租约冲突可重试；save/stop 拒绝 active 任务。
- 凭证缺失：`MULTICA_VCS_SECRET_KEY` 缺失 → QCA 503、Aurora autoprovision 禁用（fail-closed）。
- enrollment 过期/重放：5min 一次性 token，统一 401；starting 2min 未消费 → reaper 失败并结算。
- 节点/容器泄漏：`aurorafleet.Reconcile` 按 label 清理孤儿资源。
- QCA 响应丢失/提交模糊：发送意图先落盘、不重发、创建响应丢失不重建。
- workspace 删除/owner 降权：停 runner → 取消远端 → 删连接/节点 → 解绑 agent。
- 迁移：只新增 583+，不回改主仓库历史 500–582 同号不同名问题（该债不在本计划范围）。

## 11. 测试与验收

- Go：`go build ./...`、`go vet`、`make sqlc`（无 diff）、`go test ./internal/cloudruntime/... ./internal/qoderruntime/... ./internal/aurora/... ./internal/aurorafleet/... ./internal/fleet/... ./internal/handler/... ./cmd/server/...`。
- 迁移：`go test ./internal/migrations ./cmd/migrate`；干净库 `./cmd/migrate up`；迁移号唯一 lint。
- 前端：`pnpm typecheck`、`pnpm lint`、`pnpm test`（非 mobile）。
- E2E：runtimes 页（QCA 连接流、Aurora cloud 行）、Aurora generation 全链路、QCA issue 执行链路。
- 验收门：第 2 节 7 条满足；旧调度符号 grep 为空；QCA 测试语义不变。

## 12. 实施顺序与 PR 拆分

| PR | 内容 | 依赖 |
| --- | --- | --- |
| Plan | 仅文档：本计划 + 两份调研；base `origin/main` | — |
| 1 | QCA 移植到主仓库 + 迁移重编号 583–585 + 构建/前端修复 | Plan |
| 2 | QCA 逆向工程文档 + 契约测试基线 | 1 |
| 3 | 统一框架 + `cloud_runtime_connection` 迁移 586–587 + 单测 | 1 |
| 4 | QCA provider 迁移 + supervisor 删除 | 3 |
| 5 | Aurora provider 迁移 + 旧调度删除 | 3 |
| 6 | 前端 cloud service 行 + 文档 + 清理 | 4,5 |

PR 4 与 5 可在 3 完成后并行（`handler.go`/`router.go`/`cmd/server/main.go` 由 Lead 串行合并，其余文件域分离）。

## 13. 假设与风险

**假设**：开发库可重建（或按清单处理 schema_migrations）；standalone QCA bridge 保留；`aurorafleet` 独立进程保留；Aurora 产品 UI/计费不变；不新增外部依赖；不回改主仓库历史迁移；Plan PR 在 `origin`（`eanfs/multica`）的 `main` 上评审。

**风险与缓解**：

- QCA 移植冲突（主仓库与上游分叉）→ 优先 cherry-pick；冲突大时按文件移植并在 PR 记录；先跑 Go 构建与 QCA 测试再动前端。
- 前端 runtimes 页合并 → 以主仓库现有 Aurora/Fleet 行为为准，QCA 云服务分组做增量；测试先行。
- 统一 supervisor 改变 QCA 时序 → 现有 `qoderruntime` 测试为金标准，provider 适配层单独测试。
- Aurora 同步 Ensure 与后台 reap 竞态 → 共享 workspace advisory lock + 一次性 enrollment + 行锁；集成测试覆盖并发。
- 迁移号长期分叉（主仓库 500–582 与上游同号不同名）→ 本计划只新增 583+；后续若合上游需独立重编号计划。
