# 2026-10-06 Aurora Cloud Runtime 验收记录

> 本记录只写实际执行过的命令与观察到的事实。被跳过的检查标注为 SKIP 并给出原因，绝不记为通过。
> 计划：[Aurora Cloud Runtime 对接实现计划](2026-10-06-aurora-cloud-runtime-integration.md)。
> 设计：[本地 Docker Cloud Runtime 设计](../specs/2026-10-04-local-docker-cloud-runtime-design.md)。

## 范围与结论

- 分支：`feat/local-docker-cloud-runtime`。与 `main` 的 merge-base：`ef253c738`；Task 9 起始 HEAD：`a10a9d18e`。
- Tasks 1–8 已实施，并各自经过两阶段审查（ledger：`.superpowers/sdd/2026-10-06-aurora-cloud-runtime-integration/progress.md`）。
- 本地 Docker Fleet（`server/internal/fleet` + `server/cmd/fleet`）是本仓库唯一的 Docker 控制面；`server/internal/aurorafleet` 与 `server/cmd/aurora-fleet` 已删除。
- P0 的 `aurora_runtime_unavailable` 503 在**已配置**的本地 Fleet 上修复；真正未配置的部署仍返回 503。
- 真实 dockerd 上已证明 Aurora 节点能启动、出网走 egress sidecar、生命周期可重放（Task 7 real-engine 子测试）。
- **仍未运行**：API `generation→asset→settlement` 往返与浏览器 Aurora spec（需要 fake-capable 的双契约节点镜像与已配置 Aurora 的受管 API/Fleet）；真实 provider/Claude 烟测（未授权）。两者都按 SKIP 记录，不声称通过。

## 环境

| 项 | 值 |
| --- | --- |
| 主机 | macOS darwin/arm64，Docker Desktop（真实 Docker Engine） |
| Go | 宿主机 `go1.26.3`；模块工具链 `go1.26.6` |
| Node / pnpm | Node `v24.15.0`，pnpm workspace（`pnpm-workspace.yaml`） |
| managed environment 数据库 | `multica_multica_22`（`DATABASE_URL` 来自 `.env.worktree`） |
| 运行日期 | 2026-10-06 |
| 私密输入 | 服务密钥、provider 凭据文件都是 owner-readable 私有文件；本记录不含任何凭据值 |

## Tasks 1–8 证据（提交）

| 任务 | 提交 |
| --- | --- |
| Task 1 Fleet Aurora 执行 profile | `cd759da6e` |
| Task 3 workspace-node provision API 与 `mse_` 交接 | `2878609e0`、`db226bf8b`、`0efb229f1` |
| Task 4 Aurora 改走 Fleet、aurorafleet 退役 | `b0005d2a7`、`57b641893`、`bb86ce305` |
| Task 2 镜像双契约 | `c0af4357b`、`cc11ed135` |
| Task 5 隔离/出网对齐 | `0f748d58f`、`085eaa76d` |
| Task 6 MCP broker 注入 | `d4b511230`、`238f1868f` |
| Task 7 Docker gated 端到端 | `b719f929d`、`012206c7e`、`26a4a53ff`、`aec1a6eb5` |
| Task 8 `apps/aurora` Runtime 视图 | `2d205b47a`、`a10a9d18e` |

## P0：`aurora_runtime_unavailable` 503

启用条件（`server/cmd/server/main.go` 的 `newWorkspaceSandboxManager`）是三者同时存在：

- `MULTICA_LOCAL_FLEET_URL` — 回环 Fleet URL。
- `MULTICA_LOCAL_FLEET_SECRET_FILE` — owner-readable 私密服务密钥文件。
- `AURORA_SANDBOX_IMAGE` — 与 descriptor `image` 一致、以 `@sha256:` 固定的镜像引用。

任缺其一，`SandboxManager` 为 nil，`POST /api/aurora/generations` 仍在预留任何积分之前返回 `503 {"code":"aurora_runtime_unavailable"}`；catalog 与作品库端点不受影响。这是既有契约，未被改成静默成功或占位 runtime。

配置齐备时，`SandboxManager.Ensure` 在 per-workspace advisory lock 内签发一次性 `mse_`、写 `aurora_sandbox_node`（`state='starting'`），经 `FleetProvisioner`（生产实现是 `cloudruntime.Client` 调 Fleet 的 `PUT|GET|DELETE /internal/v1/workspace-nodes/{nodeID}`）落 Fleet 节点，并写入 `backend_node_id`。Fleet 不可达/被拒绝/密钥交接缺失仍返回 503，且节点被标记 failed，不留 live enrollment。

证据：`internal/fleet/integration` 的 `TestAuroraRoundTrip` 的 Docker gated 子测试在真实 dockerd 上通过；`internal/handler` 与 `cmd/server` 的 unit 覆盖 201/503 两条路径。API 往返本体的 SKIP 见下文。

## Fleet `aurora` profile 字段

Fleet 配置的 `aurora` 块（管理员持有的公开配置，见 `fleet-config.example.json`）：

| 字段 | 含义 |
| --- | --- |
| `server_url` | 容器可达的 Multica API origin；受管守护进程用它注册/回调。 |
| `proxy_image` | 以摘要固定的 egress sidecar 镜像，是沙箱唯一出网路径。 |
| `seccomp_profile` | 已部署 seccomp profile 的宿主机绝对路径。 |
| `apparmor_profile` | 已加载的 AppArmor profile 名称。 |
| `egress_hosts` | sidecar 额外接受的精确 `host:443` 允许列表。 |
| `anthropic_base_url` / `anthropic_model` | 可选的受管 Claude 端点覆盖；为空保留供应商默认值。 |
| `provider_secret_files` | 固定目标名到宿主机绝对文件的映射，只读挂到 `/run/secrets/<name>`。 |
| `readonly_rootfs` | 必须显式为 `true`。 |
| `uplink_network` | 只有 egress sidecar 加入的 operator 网络。 |

`Validate` 在任何 Docker 资源创建之前校验全部字段，失败即 fail-closed；普通 Claude-only profile 的字节不变。

## `mse_` 交接与崩溃语义

- 注册密钥格式 `^mse_[0-9a-f]{40}$`。Fleet 只把哈希写入 `fleet_node_credentials`，明文只在协调器进程内交接表，经只读 secrets 卷的固定路径 `/secrets/aurora-enrollment` 交给节点。
- 协调器崩溃即丢失明文，节点 bootstrap 失败；Aurora 侧在其 5 分钟 TTL 后重新 arm。不落库明文、不超时自动重试、不猜成功。
- 重新 arm 时，只要有合法 provision，交接表就被覆盖；被回收/失败的 create op 与节点在同一事务内以 generation 前推重置，因此重放不会把新 `mse_` 丢给旧 op。
- 注册密钥、`mcn_`、`mdt_` 都不进模型 env/argv/prompt；节点 SQL 只存哈希。

## 镜像双契约

同一个 digest-pinned 镜像同时满足 Aurora sandbox 契约（runtime tree、MCP broker、egress/AppArmor/seccomp 布局）与 Fleet 节点契约（`fleet-node` 位于固定路径、layout manifest、健康检查、`/data` + `/secrets` 布局）。任一契约不满足，镜像构建/内容校验即失败；`scripts/verify-aurora-sandbox-image.sh` 与 lock 校验器是权威门。Task 2 的 native arm64 校验、fake-pipeline smoke 通过；Linux/AppArmor 矩阵在 macOS 上诚实 SKIP（本机无 `apparmor_parser`），由 CI 的 Linux runner 负责。

## MCP broker 是唯一执行面

- Aurora 任务在 daemon 的 `runTask` aurora 分支注入 `McpConfig`，只有唯一一个 `mcpServers` 条目：`{command:"node", args:[<固定 broker entrypoint>]}`，env 恰为编译期 9 键允许列表。
- 输入由 daemon 在启动前暂存到 broker input root，mode-`0400` 的 context 与 task token 写在任务 workdir 下；缺失 context、输入、token 或 skill policy 时任务在启动前失败并退款。
- 允许工具以**限定名** `mcp__aurora__<tool>` 放进 `--allowedTools`（Claude Code 以 `mcp__<server>__<tool>` 识别 MCP 工具；裸 broker 方法名不会批准任何工具）。通用工具（Bash/Read/Write/WebFetch/Task 等）继续被 deny。
- 真实引擎证据：broker `server.mjs` 在 9 键 env 下启动并对外宣告 9 个 `mcp__aurora__*` 工具，与 Claude Code 校验器一致。

## seccomp 内联为 JSON

Aurora 节点不再把宿主机路径塞进 `HostConfig.SecurityOpt`（SDK 会把该值原样发给 dockerd，dockerd 按 inline JSON 解析，导致 `Decoding seccomp profile failed: invalid character '/'`，容器创建后无法启动）。Task 7 的 D2 修复：Fleet 读取并校验宿主机上的 seccomp 文件，在任何 Docker 变更之前把内容作为 compact JSON 内联进 `SecurityOpt`；`validateNodeInspection` 校验同一份内联 profile。修复前 `RealEngineNodeStarts` 在 `Start` 失败，修复后在真实 dockerd 上 `State.Running` 且 `State.Error` 为空。普通 Claude profile 的 `HostConfig` 字节不变。

## Task 9 回归门（实际命令与结果）

| 命令 | 结果 |
| --- | --- |
| `make sqlc` | PASS：`git status` 无 generated 变更（无新 diff）。 |
| `pnpm typecheck` | PASS（`TYPECHECK_OK`）。 |
| `pnpm lint` | PASS（`LINT_OK`）。 |
| `pnpm test` | PASS：turbo 9/9 成功（views 6145、core 2372、desktop 666、web 239、aurora 55、ui-lab 46、docs 16、nextjs 13）。 |
| `bash scripts/fleet-env.test.sh` | PASS：`Fleet behavioral cases: 32 PASS, 0 FAIL, 0 SKIP`。 |
| `bash scripts/dev-env.test.sh` | PASS：3 个具名用例 + registry 行为验证。 |
| `git diff --check` | PASS：exit 0。 |
| `make test` | 见下方「make test 结果」。 |

### make test 结果

**命令：** `make test`（即 `bash scripts/test-go.sh --race`，使用 managed environment 的 `DATABASE_URL`）。

**结果：FAIL**，但剩余红都可证明不是本分支 Fleet 工作引入，或与本分支无关的 flake：

1. `internal/handler TestVerifyCodeRechecksSignupInvitation/pending` — 在 merge-base `ef253c738` 上复现同样失败：Aurora 的 `ensurePersonalWorkspace` 为新账号创建个人工作区，测试用 `member` 总数断言 `0` 已过时。非环境问题（DEV 邮件可正常出码），非本分支引入。
2. `internal/migrations TestMigrationNumericPrefixesAreUnique` — 在 merge-base 上复现；Aurora 迁移（506/509/…/537）与上游 wakeup 迁移编号冲突，两组文件在 `origin/main` 上同时存在。
3. `internal/daemon/repocache TestCheckoutIdentityConcurrentTasksAndOverrides` — 第一次完整 `make test` 通过；第二次在并行 `-race` 下失败于 `stop process tree: operation not permitted`；单独重跑两次均 PASS。判为 macOS 进程树清理在并发压力下的 flake，未触碰该包。

除此之外全部 Go 包通过，包括本记录修复的 `cmd/fleet-env`（PASS）、`cmd/migrate`（PASS，含 `TestFleetInvalidConcurrentIndexRetry` 与 `TestConcurrentIndexCleanupsMatchTheirMigrations`）、`internal/fleet/operator`（PASS）、`internal/fleetguard`（PASS），以及 `internal/handler` 除上述 signup 子测试外全部通过。

## Task 9 范围内的分支红修复

| 红 | 处置 | 说明 |
| --- | --- | --- |
| `internal/handler TestWorkspaceDeletionManifestCoversPublicSchema` | 已修 | 把 5 张 Fleet 表纳入 workspace-deletion manifest：`fleet_credential_profiles`/`fleet_namespace_fences`/`fleet_node_credentials`/`fleet_node_operations` = `workspaceDeleteKeep`（namespace/owner/node 维度，无 `workspace_id`）；`fleet_nodes` = `workspaceDeleteSettle`（带 Aurora workspace 维度，其 Docker 生命周期归 Fleet 协调器，API 事务不删它无法清理容器的行）。不是测试豁免。 |
| `internal/handler TestMergeLegacyRuntime_KeepsOldRuntimeWhenFenceRefuses` | 已修 | `fleetguard.CheckRuntimeMerge` 在 runtime 行消失时改为返回新的 `ErrRuntimeMissing`（不是可重试的 `ErrBindingChanged`）；`mergeLegacyRuntimeTx` 把它映射回 `errRuntimeMergeFenced`，保留旧 runtime 与任务历史。 |
| `internal/handler TestVerifyCodeRechecksSignupInvitation/pending` | 诊断开放（非本分支） | 在 merge-base `ef253c738` 上同样失败（已复现），且与 Fleet 无关：Aurora 的 `ensurePersonalWorkspace` 会为新账号创建个人工作区，测试用 `member` 总数断言 `0` 已经过时。正确修法是把断言限定到 invited workspace，属于 auth 行为范围，不在本分支 Fleet 工作授权内。 |
| `internal/fleet/operator` 四个失败 | 已修 | 该包测试要求 operator 的 HOME-free 私有调用；`localDatabaseURI` 在 `HOME` 非空时 fail-closed。给测试包加 `TestMain` 取消 `HOME`（模拟真实 operator 子进程环境），保留原 `TestPrivateDatabaseRejectsHomeBeforeParser` 的显式 `t.Setenv` 拒绝断言。未放宽任何断言。 |
| `cmd/fleet-env TestPrivateCommandExplicitFakeAndSanitizedOutput` | 已修 | 同一 HOME 根因（`execute` 先走 `operator.Validate`）；同样加 HOME-free `TestMain`。 |
| `cmd/migrate TestFleetInvalidConcurrentIndexRetry` | 已修 | 测试原先要求每张 `*_fleet_*.up.sql` 都注册 index cleanup，只豁免 576。改为按 `concurrentIndexCleanups` 判定：注册的索引迁移必须有 hook，未注册的迁移必须没有 hook；索引有效性与数量按注册集合断言。全局不变量仍由 `TestEveryConcurrentUpBuildHasCleanup` 保证。 |
| `cmd/migrate TestConcurrentIndexCleanupsMatchTheirMigrations` | 已修 | `concurrentDownIndexCleanups` 错把 `584_fleet_nodes_workspace_index` 当作 down 重建索引登记；584 的 down 只做 `DROP INDEX CONCURRENTLY IF EXISTS`，不重建。删除该登记项。 |
| `internal/migrations TestMigrationNumericPrefixesAreUnique` | 诊断开放（非本分支） | 在 merge-base `ef253c738` 上同样失败；Aurora 迁移（506/509/…/537）与上游 wakeup 迁移编号冲突，且两组文件在 `origin/main` 上同时存在。重新编号是跨 30+ 个已应用迁移的大范围变更，不属于本分支 Fleet 工作授权。 |

## 未运行项（SKIP，非通过）

| 未运行项 | 原因 | 需要的环境 |
| --- | --- | --- |
| API `TestAuroraRoundTrip` 的 generation→asset→settlement 子测试 | 需要 fake-capable 双契约节点镜像（`MULTICA_AURORA_FAKE_PIPELINE_IMAGE`）；当前已审查的 daemon/broker 没有 fake pipeline 模式，选择 smoke fake 会要求改已审查的 daemon/broker。 | 一个用 fake pipeline 构建的 digest-pinned 双契约节点镜像 + 已配置 Aurora 的受管 API/Fleet。 |
| Playwright `fleet-docker` 项目的 `e2e/aurora-cloud-runtime.spec.ts` | 同一 fake-capable 镜像缺口；默认 suite 不含它。 | 同上，且 `MULTICA_RUN_DOCKER_INTEGRATION=1`。 |
| 真实 provider/Claude 烟测（`TestAuroraSandboxRealProviderSmoke` 等 7 个 Plan D 门控） | 需要 owner 明确授权与真实凭证；当前无凭证文件，全部记为 skipped。 | `agentintegration` + `MULTICA_RUN_REAL_AGENT_SMOKE=1` 及具名环境。 |
| Linux/AppArmor 安全验收矩阵（Task 2 的 `AURORA_DOCKER_SECURITY_COUNT=2`） | 本机是 macOS/Docker Desktop，无 `apparmor_parser`，不强制 AppArmor。 | AppArmor-enabled Linux runner（CI 负责）。 |

## 从 ledger 承接的 deferred minors

以下都是 ledger 中明确「deferred」的次要项，未合入本记录范围，保持可见：

- Task 3：`DeleteAuroraIntent` 对 foreign-owner delete 返回 204 而 GET 返回 403；`EnsureAuroraWorkspaceNode` 缺 `Delete` 的 nil-resp 保护；`http_aurora_test.go` 的子测试顺序依赖 map 遍历；节点在 bootstrap 前被删除时已登记的密钥仍留在交接表。
- Task 4：没有单个测试在一次断言里 post `/api/aurora/generations` 并检查三个环境变量下的 201 + `backend_node_id`；`newLocalFleetProvisioner` 两次读取密钥文件；spec 名 `sandbox` 必须与 Task 5 的 config 一致；缺失 `linux-acceptance.json` 不变量测试；docs/README 陈旧（本记录处理）；`AURORA_DOCKER_SECURITY_COUNT` 在 Task 2 前 inert；AURORA_PROXY_IMAGE/SECCOMP/EGRESS 环境变量文档已失效。
- Task 2：fixture base digest 未做 lock 校验；`checkFleetNodeContract` 漏 `-o 10001`/USER/WORKDIR；healthcheck 子串匹配偏松；`emit_skip`/`skip_count` 死代码；honest-skip 的 `docker info` 顺序；非 Linux skip 在 `docker info` 之后。
- Task 5：`EgressProxyArgs`/`EgressNetworkConnectArgs` 死代码且与 `egressProxySpec` 漂移；`ProviderSecretTargets` 是导出的可变 map；`inspectEnvironment` 的 `maxRuns==0` 早真跳过 aurora proxy key 要求；builder 未断言 readonly/fail-closed；空 `egress_hosts` 被接受；`TestEngineWorkspaceNetworkOwnershipRefusal` 的 ensure-foreign 子例在更早的 role/Owns 门就被拒（弱子测试）；`Ensure` 仍会为从不加入的 Aurora 节点创建 namespace bridge。
- Task 6：Go 侧附件扩展名/MIME/上限表手抄 `policy.mjs` 且没有漂移校验；某 generation lookup miss 会对没有 generation 行的 Aurora 任务无限重投；`cleanAuroraSandboxIO` 在每任务后删全局 `/workspace/{input,output}`（既有全局根设计；Aurora 节点同时最多 1 个任务）。
- Task 7：`DATABASE_URL` 被要求但未使用（伪 skip）；`t.Skipf` 后的不可达代码；测试内正则只部分验证工具身份；重复 helper；`NodeHostConfig` 收裸 seccompJSON 字符串；脆弱的 exec 输出子串匹配；Playwright 没有 trace/screenshot 保留。
- Task 8：节点状态是 mount-time 快照（无轮询/invalidate）；视图重新声明 `STATE_VISUAL` 而没消费 runtimes 词汇，且从不渲染映射后的公开 `errorCode`；恢复文案复述状态；重复的 provider 常量与恒为 nil 的 `operationId`。
- Task 6 修复前既有缺陷（已在本计划修复）：9 个 `aurora.*` 工具此前从未注册为 Claude 的 MCP server。
