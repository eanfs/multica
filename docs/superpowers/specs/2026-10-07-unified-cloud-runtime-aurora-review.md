# 统一 Cloud Runtime 与 Aurora 设计深度审查

日期：2026-10-07

审查对象：[统一 Cloud Runtime 与 Aurora skills 执行层设计](2026-10-06-unified-cloud-runtime-aurora-design.md)，初稿提交 `1a4a0138e`。本报告对应已修订的 r2。代码审查起点为 `1bb9b9043`；收尾时工作区 HEAD 已由并行工作推进到 `9fa8e1990`，相关注册、业务事务、egress 与进程入口证据重新核对仍成立。本次只修改两份文档。

此处的“并行工作”仅记录审查期间仓库已有其他开发提交，不表示存在第二套 Fleet 或第二个运行时，也不限制后续修改 Fleet。规格与本报告已随 [PR #188](https://github.com/eanfs/multica/pull/188) 合并；以下结论与环境观测是审查时的记录，不是该 PR 的整体交付状态或后续部署的实时状态。统一镜像方案仍须独立完成规格 §12 的实施与验收门。

## 结论

**初稿不应直接作为完整实施依据。** 发现 10 项会影响落地、故障恢复或验收真实性的缺口；r2 已补充对应设计和验收要求，但文档修订不代表实现问题已经修复。

唯一镜像、唯一发布 digest、Fleet 单一控制面与工作区独立节点的方向可以保留。当前不能给出“已保证可运行”的结论：本机 Docker Engine 未报告 AppArmor；新的进程权限边界尚未做原型；完整受控端到端所需测试服务尚未建立。下一步首先关闭 r2 的 G0 平台与隔离门，再按 G1–G5 实施和验收。

P1 表示未补齐会阻塞目标流程或破坏权限、恢复、账务约束；P2 表示职责或验收表述错误，会误导实施和通过判断。这里评审的是设计缺口，并非将所有问题报告为当前线上事故。

## 发现与修订

### R1 · P1：当前宿主环境无法满足新隔离前置条件

初稿 §8 将 AppArmor 作为必要条件，但没有核实用户正在使用的 Engine。只读查询实际返回：

```text
["name=seccomp,profile=builtin","name=cgroupns"] Docker Desktop aarch64 29.8.0
```

按初稿的 fail-closed 要求，这台 Engine 上 Aurora 将一直不可执行。另见 [local_fleet.go](../../../server/cmd/server/local_fleet.go)：API 对 Fleet URL 要求 loopback；把 Docker 指向远端或 Linux VM 也不会自动修复控制面地址与 bind mount 路径。

**处置：** r2 §8.3、§12 明确当前环境不在已支持集合中；将具备 AppArmor 的 Linux Engine 列为候选，要求 API/Fleet 位置、数据库身份和 guest 私密路径一起验证。是否接受 Linux VM/Engine 已向用户询问，尚未得到选择；没有安装或切换环境。若必须原地使用当前 Docker Desktop，则需要重新设计隔离实现。

依据：[Docker AppArmor 官方文档](https://docs.docker.com/engine/security/apparmor/)。通过条件是实际 profile 加载和拒绝测试，不是配置字段非空。

### R2 · P1：多功能入口和同 UID socket 不能证明模型权限隔离

初稿 §8 用 `fleet-node broker-client` 作为模型可执行入口。但 [fleet-node/main.go](../../../server/cmd/fleet-node/main.go) 同一个二进制还负责 bootstrap、run、health 和报告；仅按文件路径授权会把管理入口一起交给该进程。0600 socket 也不能区分同 UID 的不同运行。现有 [AppArmor profile](../../../deploy/aurora-sandbox/multica-aurora-sandbox.apparmor) 含广泛的系统和 `/opt` 执行许可，直接沿用会扩大合并镜像的执行面。

**处置：** r2 §4.1、§8.2 使用同一镜像内的独立单功能 MCP 客户端和代理入口；增加 peer/attempt 校验、消息边界、环境与配置发现限制、凭据与进程内存拒绝测试。固定 Node 入口不能成为加载任意脚本的授权。

仍需实证：no-new-privileges 下的实际进程 label 转换；Chromium wrapper/helper 与 HyperFrames loopback 的正常执行；模型与渲染器无法接触 daemon、其他运行或 provider 密钥。[Linux 官方文档](https://docs.kernel.org/userspace-api/no_new_privs.html) 明确提示该标志与 LSM 收紧权限的相互作用，不能凭配置推定支持。G0 必须同时通过允许与拒绝矩阵。

### R3 · P1：一次性注册没有闭合重启、到期与响应丢失

[managed.go](../../../server/internal/daemon/managed.go) 的 `bootstrapManaged` 每次读取一次性 enrollment，兑换结果只通过 `SetToken` 放入进程；[sandbox_enrollment.go](../../../server/internal/aurora/sandbox_enrollment.go) 定义 enrollment 五分钟、daemon token 八小时有效。初稿 §5 没有持久化与轮换协议，无法保证重启恢复或长期运行。服务端只持有 token 哈希，消费成功但响应丢失不能简单重返原明文。

**处置：** r2 §5.3 明确 session 原子保存、先持久化再领取、renew/pending/ack 顺序、认证与 WS 撤销、崩溃重放。新 token 必须先落盘，才能 ack 撤销旧 token。无法恢复时须核查旧执行、撤销原代次并停止精确旧进程后重新注册，保留逻辑身份。每个写盘、提交、响应边界都需故障注入；目前协议尚未实现。

### R4 · P1：生成、积分与入队分步提交留下崩溃窗口

[handler/aurora.go](../../../server/internal/handler/aurora.go) 依次调用 `createAuroraGenerationWithEntitlement`、`Credit.Reserve`、`EnqueueQuickCreateTask`、`UpdateAuroraGenerationTask`。其中账本 [credit.go](../../../server/internal/aurora/credit.go) 和队列 [task.go](../../../server/internal/service/task.go) 各自拥有事务。进程在这些提交之间退出时，可能留下无任务的 generation 或已预留但未关联的运行；复用原调用顺序不能实现初稿 §7 的完整准入保证。

**处置：** r2 §7 要求复用业务规则、抽取同一 `qtx` 下的写入原语，明确 Fleet/用户额度/余额锁顺序；将 generation、ledger、task、附件和关联原子提交。新增请求幂等键及指纹处理“已提交但响应丢失”，保留旧客户端无键请求兼容。验收逐步注入错误并证明全回滚及重放只有一个账务效果。

### R5 · P1：不确定 provider 结果不能直接落入既有失败退款

初稿要求结果不明时保持恢复，但 [aurora_provider_run.sql](../../../server/pkg/db/queries/aurora_provider_run.sql) 将 ambiguous 冻结，[aurora_completion.go](../../../server/internal/service/aurora_completion.go) 的 `settleAuroraOnFailed` 对失败运行执行退款。只依赖 daemon 回报也无法修复 daemon 已消失、结算更新失败的情形。无 external ID 的请求无法证明 provider 没收到，更不能自动重试创建。

**处置：** r2 §7 增加独立结果协调服务；按可验证事实查询原 ID、导入资产、结算或退款；无证据时进入 needs_review，保留原预留并提供受认证、可审计的处理路径。明确本地执行并发与业务未决状态分离，迟到回报作为原 attempt 证据处理。十五分钟只是升级处理期限，不是自动退款期限；没有承诺未知外部结果一定自动收敛。

### R6 · P1：把 provider 域名指向本地 fixture 会被生产 egress 拒绝

[auroraegress/policy.go](../../../server/internal/auroraegress/policy.go) 的 `NewPolicy`、`buildPins` 与 `IsPublicIP` 拒绝 provider 指向私有、loopback、文档及 benchmark 地址。初稿 §9 的本地 HTTPS fixture 不能直接穿过该策略。绕过它获得的通过也无法证明被测发布镜像的真实隔离。

**处置：** r2 §9.1 明确完整闭环依赖操作者控制的合法可路由公网 fixture、现有 egress_pins、受控 TLS 信任和假凭据；必须覆盖模型 SSE/tool-use、provider create/poll、下载以及 API 产物导入。不得新增 fake 节点镜像或放宽生产 IP 策略。这个环境尚未建立；它是待交付的 G1 依赖，不是现成测试能力。执行真正 CLI 仍遵守仓库独立门控和授权要求。

### R7 · P1：配置缺失时的准备意图与删除/owner 行为没有落地模型

[fleet.sql](../../../server/pkg/db/queries/fleet.sql) 的 `InsertFleetAuroraNode` 要求批准后的 image/spec，当前创建阶段可以进入 worker。初稿 §5 一方面要求工作区事务总能保存准备意图，另一方面只说复用节点/操作表，没有明确未配置和容量不足时如何避免非法资源创建、占满配额，或在工作区已删除后继续准备。

**处置：** r2 §5.2、§5.4 明确 prepare、waiting 与 operation phase，等待不创建资源/不占准入配额，通过配置和容量检查后才固定执行规格。增加工作区删除 tombstone 及创建前后核查；资源 owner 不能由点重试的成员替换，owner 转移先暂停并显式处理。普通创建窗口不覆盖无限等待配置的时间。

### R8 · P1：共享数据库整库回滚和先覆盖绑定会损害其他工作区

初稿 §10 以快照恢复作为回滚出口，却没有限定共享数据库的并发写入；回退整个数据库可能撤回其他工作区已完成的运行及账务。迁移若先更新容器和运行时映射，再清理旧资源，也会失去精确归属依据，无法安全重放。

**处置：** r2 §10 引入逐节点持久检查点，先屏障、排空和停止旧资源，再转换绑定和启动新资源；迁移日志保留原资源身份。默认向前恢复，解除准入前且无新业务写入才允许经审查的逐行补偿。整库恢复从普通回滚中移除。须验证系统智能体唯一约束、旧 token/reaper/heartbeat/delete 引用退役及每个检查点中断，不只测试成功迁移。

### R9 · P2：CONNECT 代理不能检查 TLS 内的 HTTP 重定向

[auroraegress/proxy.go](../../../server/internal/auroraegress/proxy.go) 的 `handleConnect` 建立 raw tunnel；HTTP 重定向拒绝实际由 [transport.mjs](../../../deploy/aurora-sandbox/runtime/src/transport.mjs) 的 providerFetch 执行。初稿把这些保证集中归给 egress，会导致只测试代理但遗漏适配器校验。

**处置：** r2 §8.3 区分代理的 host/IP 责任和 broker 的请求/重定向责任，并要求验证同容器不同进程的网络权限；内部 Docker bridge 不等于 Claude、broker、渲染器之间已经隔离。

### R10 · P1：身份统一会触及真实领取和保护逻辑，不能只更新 provider 字段

[managed.go](../../../server/internal/daemon/managed.go) 的 `installManagedEnrollment` 硬性要求 `aurora_managed/cloud`；[fleetguard/claim.go](../../../server/internal/fleetguard/claim.go) 用 `managed_by`/`fleet_node_id` 判断是否应用受管屏障；[aurora_agents.sql](../../../server/pkg/db/queries/aurora_agents.sql) 仍选择专用 provider/mode。初稿虽要求统一身份，却没有明确统一后的内部字段与这些门的迁移方式。

**处置：** r2 §3、§5.1、§10 明确单一 provider/mode 与服务器写入的受管 metadata，并列出注册、claim、SQL 选择和系统智能体关系的同步修改。现有 account-wide 手工节点须先清点映射，不能默认为工作区节点。响应兼容仅留在 API 边界，不保留内部别名载体或两条长期 bootstrap。

## 需求到验收的对应关系

| 用户约束 | r2 章节 | 通过证据 |
| --- | --- | --- |
| 一个镜像、一个 digest | §4、§9.3 | 唯一发布 index；节点/代理配置相同，实际架构 manifest 与 image ID 相同 |
| 一个 Cloud Runtime | §3、§5、§10 | 唯一逻辑身份、注册和领取集合；旧专用生命周期退役；不能双重领取 |
| 与真实 skills 执行层对接 | §8、§9 | 发布镜像内真正 daemon/Claude/broker；13-route 输入到产物合同完整；允许与越权矩阵 |
| 工作区创建时异步准备 | §5.2、§6 | 两个创建入口、事务故障、重复消费、配置/容量等待、删除竞争 |
| 就绪前不生成、不预留 | §6、§7 | UI 禁用和服务端最终准入；事务内屏障与零 generation/ledger 写入 |
| 应用可恢复且可看作品 | §6.3、§7、§9 | 浏览器 trace、作品预览/下载、余额断言、原任务恢复与 needs_review |
| 真实 provider 未授权不调用 | §9.1、§9.3 | 测试假凭据隔离；真实服务分项记录 SKIP，不能当 PASS |

## 已执行检查与未执行验证

已执行：CodeGraph 定位及代码只读核对；Docker Engine security options 只读查询；官方 Docker/Linux 文档核对；修订文档的本地链接存在性、占位符及 `git diff --check` 检查。

未执行：产品代码修改、镜像构建、容器启动、AppArmor 原型、数据库迁移、Go/前端测试、浏览器验证、真实 CLI/model/provider 调用。以上未执行项没有被记为通过。

书面设计的缺口已转成明确实施要求。平台选择和 G0/G1 实证仍是开放门：不能用文档评审代替运行证据，也不能把本次审查通过扩大为环境切换或真实 provider 调用授权。
