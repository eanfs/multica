# v0.10.0 — Colima 本地 Cloud Runtime 版本计划

日期：2026-11-09（2026-11-15 二次同步后复审）
基线：已合并 `origin/main`（`739756533`，merge `fd6958907`）与 `upstream/main`（`975aa7d0f`，307 个新提交，merge `8866ae98c`）；本分支相对 upstream/main 50 个提交
目标版本：**v0.10.0**。上游已于 2026-11-13 发布 `v0.9.0`（Projects/Inbox/bulk-edit 等，与 Fleet 无重叠），本版本顺延为 v0.10.0
迁移基线：Fleet 迁移已从 611–624 重编号为 **660–673**（上游占用 611–659），并按新规则注册 `concurrentIndexCleanups`（commit `44340c38a`）

## 1. 版本目标

在 macOS 上，用 **Colima** 提供 Docker，一条命令启动完整的本地 Cloud Runtime：

```bash
colima start --cpu 4 --memory 8 --disk 60
make up C=api,web,fleet
```

Fleet 控制面、PostgreSQL、以及装有 multica daemon + Claude Code 的节点容器全部运行在 Colima VM 的 Docker 中；API/Web 在宿主。用户可在 Runtimes 页面创建节点、配置 API Key/凭证、启停/重启/删除，并把 Claude Runtime 绑定给 Agent 执行任务。

## 2. 范围

### 2.1 In scope（v0.9.0 必须包含）

| 能力 | 状态 |
| --- | --- |
| Fleet 控制面（配置校验、store、advisory-lock 屏障、服务密钥、readyz 含 schema 检查） | 已完成 |
| Docker provider（volume seeding、非 root、无 socket/privileged 节点） | 已完成 |
| 节点生命周期 create/start/stop/reboot/delete + coordinator + CAS/幂等 | 已完成 |
| 节点内 daemon + Claude Code 镜像（`multica/runtime-claude:dev`）与 entrypoint | 已完成 |
| mcn_ 节点 PAT 验证（Fleet `/api/v1/pat/verify`，本地模式禁用缓存） | 已完成 |
| 受管 Runtime 元数据（`managed_by=local_fleet`）、删除保护、每节点并发 1 | 已完成 |
| 本地忙碌检查、两段式 prepare/approve、claim 屏障 | 已完成 |
| 能力发现驱动的 Runtimes UI（含幂等重试语义） | 已完成 |
| Colima 自动适配（context 检测、宿主地址派生、`MULTICA_FLEET_HOST_GATEWAY` 覆盖） | 已完成 |
| opt-in `fleetintegration` 真实 Docker 测试 | 已完成并在 Colima 上 PASS |
| 文档（AGENTS/CONTRIBUTING/CLI/SELF_HOSTING/设计 §14/环境变量） | 已完成 |
| Multipass recipe（`deploy/multipass/`） | 已完成（未实测，v0.10 验证） |

### 2.2 Out of scope（本版本不做）

- 每个 runtime 一个 Multipass/VM 的 VM-provider 架构。
- 多机/远程 Docker（仅本机 Colima / Docker Desktop / Linux daemon）。
- 真实模型调用的自动化烟测（需真实 key 与授权，放 v0.9.x 手动验证）。
- 已知限制的修复（见 §6，进 v0.9.1/v0.10）。

## 3. 已完成基线（本版本输入）

- 分支相对 main 48 个提交、131 个文件，覆盖后端/Fleet/数据库迁移 611–624/前端/部署/测试/文档。
- 已合并 `origin/main` `739756533`（2026-11-09），解决 3 处冲突：`runtimes-page.tsx`、`runtimes-page.test.tsx`、`server/cmd/server/router.go`。
- 已合并 `upstream/main` `975aa7d0f`（2026-11-15，+307 提交，含 v0.9.0/0.9.x 功能），仅 2 处冲突（`Makefile` help、`scripts/dev-env.sh` 的 CDP/seed_dev_workspace），全部双侧保留；合并后 `go build ./...`、`go vet ./internal/fleet`、`go test ./cmd/migrate ./internal/migrations`、`dev-env/fleet-env.test.sh`、views runtimes **370 tests** 全绿。
- **迁移重编号**：上游占用 611–659，Fleet 迁移改为 660–673（28 个文件 git mv），9 个并发索引按新 AGENTS 规则注册进 `concurrentIndexCleanups`。
- 真实 Colima 手动端到端已验证：`make up C=api,fleet` → Fleet ready；创建节点 → Claude Runtime online（2.1.0）；stop/start/delete 生命周期；节点容器与卷清理。
- `TestFleetNodeLifecycleRealDocker` 在 Colima 上 **PASS**（25.27s，隔离 DB + fake-Claude 镜像）。
- 脚本测试（`fleet-env.test.sh`、`dev-env.test.sh`、`selfhost-config.test.sh`）、Go build/vet、前端 typecheck/相关 vitest 已通过。

## 4. 验收标准（发布门禁）

全部满足才可打 tag：

1. **G1 一键启动**：全新 worktree 上 `make worktree-env && make up C=api,web,fleet` 在 Colima 下成功；`make status` 显示 api/web/fleet/database 正常；Fleet `/readyz` 200。
2. **G2 节点闭环**：UI 或 API 创建节点 → 状态 `running` → `/api/runtimes` 出现 `Claude (<node>)` online；stop→stopped、start→running、delete→terminated，容器/卷清理。
3. **G3 凭证与 API Key**：配置 `.dev/fleet-credentials.json` 后节点 daemon 可见 `model_configured`；未配置时创建被明确拒绝（`credential_profile_missing`）。
4. **G4 受管保护**：`managed_by=local_fleet` 的 Runtime 在所有删除入口被拦截；GC 不删除；SaaS 模式不受影响。
5. **G5 后端回归**：`make test`（PostgreSQL 可用）通过；`go test ./cmd/migrate ./internal/migrations` 通过（迁移注册一致性已绿）；Fleet 相关 DB 测试不再 skip。
6. **G6 前端回归**：`pnpm typecheck`、`pnpm lint`、`pnpm test` 通过；Runtimes 页面在能力发现/旧 SaaS 响应两种情况下行为正确。
7. **G7 真实 Docker 集成**：`MULTICA_FLEET_INTEGRATION=1 … go test -tags=fleetintegration ./internal/fleet` 在 Colima 上 PASS。
8. **G8 E2E**：`MULTICA_RUN_FLEET_E2E=1 pnpm exec playwright test e2e/cloud-runtime-docker.spec.ts` 至少一次绿（需 Web 启动；若环境缺浏览器，记录为发布前人工验收项）。
9. **G9 文档**：`make down` 只停控制面、`make destroy` 清理节点、Colima 地址派生、已知限制均与实现一致；无过时表述。
10. **G10 脚本回归**（上游新规则）：改动的 `.github/` 或 `scripts/` 需通过 `pnpm test:scripts` 与对应 `bash scripts/<name>.test.sh`。

## 5. 里程碑

| 里程碑 | 内容 | 退出条件 | 预计 |
| --- | --- | --- | --- |
| M0 代码冻结 | 分支 `dsh-5d4b724e-22c8` 不再加功能；合并最新 `origin/main` | 全分支审查通过、无 Critical/Important；合并冲突解决后编译/typecheck/定向测试通过 | 已完成 |
| M1 Colima 验证 | G1/G2/G3/G7；修复验证中发现的 Defect；合并后回归 | 上述门禁绿 | 已完成（G4/G5/G6/G8 待补） |
| M2 回归与文档 | G4/G5/G6/G8/G9；补 CHANGELOG 与 release notes | 所有门禁绿或书面豁免 | 1–2 天 |
| M3 发布 v0.10.0 | 合并到 main、CI 绿、打 tag `v0.10.0` | Release workflow 发布成功 | 0.5 天 |
| M4 版本后 | v0.10.1 修已知限制；v0.11 Multipass 实测/VM-provider 评估 | 各自计划 | 后续 |

## 6. 已知限制（v0.9.0 发布说明必须列出）

1. enqueue 侧未接共享节点锁（R22）：delete 批准后的窄竞态仍可能让新任务进入排队；已在设计 §14.6 记录。
2. prepared/state_uncertain 操作无自动回收器；需要人工重试/诊断。
3. `make down` 只停 Fleet 控制面，节点容器保留到 `make destroy`；无 claim 屏障。
4. 旧共享 `multica` Compose project 的 fleet 容器需手动 `docker rm -f` 一次。
5. launching 节点无“永不健康”超时；由 coordinator settle 为 stopped，但不会自动删除。
6. 旧 SaaS capability 响应若缺少 provider，Runtimes 入口隐藏（上游 payload 未在本仓库验证）。
7. create 幂等键唯一索引已由迁移 671 建立（`fleet_node_operation_idempotency_key_key`，部分索引 WHERE idempotency_key <> ''）；API 层幂等语义仍依赖操作行而非仅该索引。
8. Colima 与 Docker Desktop 通过 context 切换；切换后需要重新 `make up C=fleet` 让 Fleet 容器使用新的宿主地址。
9. 真实 Claude 任务执行需有效 key，且当前仅手动验证过 CLI 探测/Runtime 注册。

## 7. 发布清单

1. 开 PR 前再次合并最新 `origin/main` 与 `upstream/main`（本次已合并至 `975aa7d0f`；上游若再前进需重复合并），冲突解决后运行 `go build ./...`、`pnpm typecheck`、`pnpm test:scripts`、views/core 定向测试。
2. 迁移规则（上游新增）：推送迁移前 rebase 最新 main、编号取当前最大前缀+1（当前 673，下一个从 674 起）、并发索引必须在 `server/cmd/migrate/main.go` 注册；Fleet 迁移已按此整改。
3. 合并分支到 `main`（PR，按仓库模板；UI 改动需附截图/录屏）。
2. 运行 `make check`（Web/API）+ `make test`（PostgreSQL）+ fleetintegration。
3. 更新 `CHANGELOG`/release notes：新增 Colima/Docker Desktop/Linux 后端、本地 Fleet、节点生命周期、能力驱动 UI、已知限制。
4. 打 tag：`v0.10.0`（按 `.github/RELEASING.md`，tag push 触发 Release workflow；CI 跑 Go tests + govulncheck）。
5. 发布物：容器镜像 `multica/fleet`、`multica/runtime-claude`（CI 构建）；文档站点更新。
6. 发布后验证：在干净 macOS + Colima 环境按 README 走一遍 G1–G3。

## 8. 风险与缓解

| 风险 | 影响 | 缓解 |
| --- | --- | --- |
| Colima 版本/VM 资源不足 | 镜像构建或节点启动失败 | 文档要求 ≥4 CPU/8 GB；构建前 `docker info` 检查 |
| Docker context 在 Colima/Desktop 间切换 | Fleet 回调地址错误、节点不 ready | `fleet-env.sh` 每次按 context 派生；切换后重跑 `make up C=fleet` |
| VPN 劫持 192.168.64.0/24（Multipass）或 Colima 网段 | 节点/API 不可达 | 文档列已知 VPN；`MULTICA_FLEET_HOST_GATEWAY` 覆盖 |
| 密钥进入镜像/build context | 凭证泄漏 | `.dockerignore` 排除 `.dev/`/`.env*`；Fleet 仅通过 secrets 卷注入 |
| 首次镜像构建耗时 | 启动体验差 | 文档说明首次较慢；CI 预热镜像 |
| 旧 `.env` 的 host.docker.internal | 切换 Colima 后 API 地址错误 | bind_paths 每次重新派生；旧 env 自动修正 |
| 上游 main 快速前进，合并冲突 | 分支落后、冲突解决引入回归 | 小步合并；已两次合并（origin `739756533` / upstream `975aa7d0f`），每次合并后跑 build/typecheck/定向测试；PR 前重复该流程 |
| 上游占用迁移编号（本次 611–659 vs Fleet 611–624）| schema_migrations 版本冲突、迁移互相覆盖 | 已重编号 660–673 并注册索引清理；后续新增迁移从 674 起、PR 前 rebase |

## 9. 后续版本

- **v0.9.1**：修复 R22 enqueue 屏障、prepared 回收器、launching 超时、create 幂等唯一索引。
- **v0.10.0**：Multipass 实测（recipe + context + 集成测试）；评估“每个 runtime 一个 VM”的 Multipass provider。
- **v0.9.x 手动验证**：真实 Claude key 的任务执行烟测（需授权）。
