# 统一 Cloud Runtime 与 Aurora skills 执行层设计

日期：2026-10-06

状态：2026-10-07 深度审查修订版（r2），书面规格待用户审阅。统一镜像与运行时的方向已确认；执行环境与隔离实证门尚未关闭，不能标为已证明可实施。

初稿基线：`feat/local-docker-cloud-runtime`，`92650fc80`。审查基线：`1bb9b9043`；本次不修改并行进行的 Fleet 代码。问题、证据与处置见[深度审查记录](2026-10-07-unified-cloud-runtime-aurora-review.md)。本文是架构规格，尚不是包含逐文件任务和测试命令的实施计划。

## 1. 目标与已确认边界

本地 Docker Cloud Runtime 直接承载 Aurora 的完整 skills 执行链路。交付标准是用户从 Aurora 应用准备运行时、提交生成到查看作品及结算结果的完整流程，不以容器启动、HTTP 201 或单独的 broker 测试代替。

用户已确认：

1. **一个发布镜像、同一个 digest、一套 Cloud Runtime。** 不保留普通版与 Aurora 版两个最终构建 target，不保留独立 Aurora 节点镜像或第二套运行时。
2. `docker/runtime/Dockerfile` 是唯一节点镜像构建入口，包含 daemon、Fleet 节点入口、Claude、MCP broker、13 个可用 skill 的完整依赖、加固后的 vendor、Chromium、FFmpeg 和出网代理程序。
3. Fleet 是唯一 Docker 控制面。每个工作区的专用节点、实际执行运行时和 Aurora 智能体绑定由同一套身份与生命周期管理。
4. 注册产生个人工作区、或显式新建工作区时，异步准备运行时。准备失败可重试；就绪前禁止生成、不预留积分。
5. Aurora 的业务 API、审核、作品回传和积分结算保留；执行交由 Cloud Runtime，应用不直连 Docker 或 Fleet 私有接口。
6. 若使用 egress 代理容器，它与执行节点使用**完全相同的镜像引用和 digest**，只以固定代理入口启动。它不注册运行时、不领取任务、不挂载工作区数据或业务凭据。
7. 同一镜像包含通用工具后，收紧实际执行权限，不再以“镜像里没有 Git”作为隔离保证。必要隔离不能生效时禁止执行，不能静默降级。

一个运行时指统一的实现、注册、任务领取及生命周期模型；多个工作区仍各有隔离的节点实例。一个镜像指节点与代理的唯一发布产物，不要求把 API、PostgreSQL 或高权限 Fleet 控制器也装进节点镜像。

多架构发布使用一个 OCI image index digest，配置和发布入口只出现该引用。各架构由 OCI 解析出的子 manifest 属于同一发布产物，必须记录用于复核，不得借多架构机制发布普通版和 Aurora 版两个功能变体。同一宿主机上的节点与代理还必须解析为相同的架构 manifest。

## 2. 现状、依据与取代关系

只读核对得到以下事实：

- Fleet 已接管 Aurora 节点；旧 `server/internal/aurorafleet` 已退役。本设计继续使用既有 Fleet，不另建控制器。
- `server/internal/aurora/agents.go` 仍创建 `provider=aurora_managed` 的载体，注册流程会将它绑定到真实 daemon；它不是始终脱离执行节点的占位记录，但仍依赖专用身份与生命周期。
- `CreateAuroraGeneration` 仍在生成请求中准备节点；`GenerationComposer` 的提交条件不含运行时就绪检查。
- Aurora Runtime 查询只有 stale-time，没有持续追踪节点准备状态；运行时页面没有准备、唤醒和恢复操作。
- 普通节点 Dockerfile 含 Git；Aurora Dockerfile 含完整 broker/runtime/vendor 树并要求无 Git。现有 AppArmor 广泛允许执行系统和 `/opt` 目录，不能直接用于合并后的工具集合。
- 现有验收记录将 API 生成→作品→结算及浏览器链路列为未运行。该记录不作为本设计新版本的通过证据。

相关文档：

- [本地 Docker Cloud Runtime 设计](2026-10-04-local-docker-cloud-runtime-design.md)
- [本地 Docker Cloud Runtime Implementation Plan](../plans/2026-10-04-local-docker-cloud-runtime.md)
- [Aurora Cloud Runtime 对接实现计划](../plans/2026-10-06-aurora-cloud-runtime-integration.md)
- [已有对接验收记录](../plans/2026-10-06-aurora-cloud-runtime-acceptance.md)
- [Aurora skills 执行层计划](../plans/2026-09-25-aurora-sandbox-skill-runtime.md)

本规格批准后，取代上述文档中双节点镜像、独立 `aurora_managed` 生命周期、生成时首次准备、Runtime 只读展示和无 Git 镜像隔离等相冲突的设计。旧验收记录保留为历史证据。下一份实施计划明确标注这些取代关系，不把旧任务的完成状态当作新设计已实施。

非目标：新增模型 CLI、扩充 skill 目录、Slide/deck 功能、SaaS 计费改造、移动端改造、多机调度、AWS 生产部署、自动扩缩容、宿主机目录任意挂载。保留当前 Claude 执行面，不能通过统一镜像放宽 Aurora 为通用代码执行产品。

## 3. 组件与唯一责任

| 组件 | 责任 | 不承担的责任 |
| --- | --- | --- |
| Aurora API / 业务服务 | 工作区授权、skill 输入、审核、权益、生成、作品、积分 | Docker 生命周期、第二套运行时注册 |
| Cloud Runtime 服务与 Fleet Store | 工作区绑定、准备意图、节点身份、操作幂等、状态投影 | 模型判断、作品业务结算 |
| Fleet Reconciler / Docker Provider | 镜像与隔离预检、容器/卷/网络、启动恢复、维护和销毁 | 重新定义业务任务队列 |
| 统一 daemon | 注册、心跳、任务领取、执行策略校验、子进程监督、结果回传 | 根据提示词修改权限或部署配置 |
| Aurora MCP broker | 执行已审核 skill 工具、provider 调用、产物暂存 | 任意 shell、动态安装、任意凭据读取 |
| egress 代理 | 固定入口、目标允许列表、出网限制 | 运行时注册、任务执行、业务密钥保管 |
| core / views / apps/aurora | API 契约与 Query、共享业务组件、应用路由接线 | 在 Zustand 复制服务器状态 |

统一运行链路：工作区准备意图 → Fleet 节点 → 统一 daemon → 受限 Claude 与 broker → 暂存/审核/作品回传 → 既有完成与结算服务。

普通任务与 Aurora 任务使用同一套运行时实现；新受管节点统一为工作区作用域，执行策略是服务器持有的任务约束，不是两种镜像、两套 bootstrap 或第二个 daemon。Aurora 自动准备的工作区节点只接收该工作区获准的 Aurora 任务；通用任务仍能在同一镜像上按其授权策略运行。策略不能在忙碌节点上原地放宽。已有跨工作区的手工节点不自动拆分或迁移：升级前须列出其工作区映射，经原资源管理员确定迁移范围；新引导不继承旧 account-wide token 的权限。

## 4. 单一镜像与固定布局

### 4.1 构建与发布

`docker/runtime/Dockerfile` 允许多个中间构建 stage，但只有一个可发布的最终运行 stage。合并 Go 构建、锁定的 Claude/Node 依赖、vendor 校验与补丁、媒体工具和代理程序；删除 `deploy/aurora-sandbox/Dockerfile` 与 `Dockerfile.egress` 的独立构建入口。

保留 `deploy/aurora-sandbox/runtime`、vendor、锁文件和安全策略作为已有源目录，本次不为目录命名迁移扩大变更。固定 broker 路径可继续指向 `/opt/aurora/runtime/deploy/aurora-sandbox/runtime/src/server.mjs`，它表示载荷位置，不表示第二种运行时。

镜像内只安装一份锁定版本的 Claude。`/usr/local/bin/claude` 是统一的可执行入口，普通执行与 Aurora 执行共同使用；删除第二套 Claude 版本来源和安装过程。入口必须解析到镜像内只读可信文件，不能由工作区 PATH 或用户文件覆盖。

镜像默认入口保持 `fleet-node run`；代理角色由 Fleet 使用独立的 `/usr/local/bin/aurora-egress-proxy` 入口启动。MCP 客户端使用独立的 `/usr/local/libexec/multica-mcp-client`，不复用 `fleet-node` 子命令。AppArmor 按可执行文件路径授权，不能靠同一二进制的 argv 区分管理权限与模型权限；这些小入口只实现各自功能，不是不同运行时或镜像。客户端不能指定 entrypoint、command、image、挂载、环境变量或脚本。代理不允许执行 `fleet-node`、daemon 或其他管理入口。

### 4.2 镜像契约

- 非 root；镜像内程序和依赖 root-owned、对执行用户不可写；运行根文件系统只读。
- `/data` 保存节点身份与持久状态；任务输入、输出和临时文件按任务隔离；`/secrets` 保存注册输入；provider 凭据只使用固定只读路径。
- 节点不挂载 Docker socket，无 privileged、host network/PID 或业务端口发布；资源上限沿用 Fleet 规格。
- Git 等通用任务需要的工具可存在；运行时不动态下载安装 skills 或替换镜像中的程序。构建工具和无运行用途的包管理器不进入最终镜像。
- 统一内容校验同时覆盖 Fleet 入口、Claude、broker、13 个可用 skill 的运行依赖、vendor 哈希、代理、共享库与固定布局。
- 一次构建、一次供应链扫描、一次签名/证明及一个发布 image index。漏洞策略仍生效；不能因镜像合并豁免现有供应链阻断。

配置只有一个权威 `image`。删除独立 sandbox/proxy 镜像配置，节点和代理从该字段取值；部署切换时迁移现有配置，不增加旧字段回退链。容器接管检查同时验证引用、实际 image ID、角色、命名空间、卷及隔离策略。

## 5. 运行时身份、数据与注册

### 5.1 唯一绑定

以既有 `fleet_nodes` 的 `workspace_id`、`runtime_id`、`daemon_id` 和节点 ID 为绑定权威，`agent_runtime` 保存同一个实际执行身份。已绑定的运行时记录从准备到重启保持 ID 稳定；未完成注册的记录只能是不可用状态。

新增约束确保每个 Fleet namespace 中每个工作区最多一个未终止的自动受管节点，并防止同一实际运行时被两个活动节点绑定。手工通用节点不自动纳入该关系；迁移发现多重绑定时拒绝自动选择并报告冲突。

系统智能体的 `runtime_id`、领取载体和前端显示的执行目标必须一致。不再额外创建 `aurora_managed` 载体，也不维护“实际 runtime + Aurora 别名 runtime”的双重领取集合。新节点统一为 `provider=claude`、`runtime_mode=local`，`metadata.managed_by=local_fleet`、`fleet_node_id` 由服务器设置；Aurora 系统智能体同步采用该运行时模式。修改 `installManagedEnrollment`、claim 集合、SQL provider/mode 谓词和身份保护，不能只改显示名称。兼容保持在公开响应边界，不增加新的 Runtime mode 或内部别名载体。

### 5.2 持久化准备意图

工作区创建和准备意图在同一 PostgreSQL 事务提交。复用 Fleet 节点/操作表表达意图，不新建数据库、队列系统或 Aurora 专用节点状态机。创建事务不调用 Docker 或 Fleet HTTP；共享的领域服务只写事务内数据，后台协调器消费已提交意图。

准备意图持有稳定 workspace/node/runtime/daemon ID。规格、允许任务类别及配置引用从管理员规则解析；配置缺失仍能保存意图并呈现不可用原因。API 与 Fleet 必须对同一可信配置修订达成一致后才能创建资源。

具体持久模型为：新增 `prepare` 操作类型，`fleet_nodes.status=waiting`、`ready=false` 表示尚未准入的逻辑绑定，operation 的 `phase=waiting_config|waiting_capacity|queued|running|succeeded|failed` 表示进度。`waiting` 节点没有物理资源，不能被现有 create worker 自动领取。通过配置及容量检查后在短事务内取得配额、固定镜像/策略修订并推进到既有创建阶段。唯一绑定覆盖 waiting 节点；容量只计算已批准准备及实际占用资源的节点。`next_attempt_at` 沿用操作表；失败重试、删除和 namespace destroy 都必须识别 waiting 状态。

工作区准备的资源 owner 固定为创建工作区的 owner；普通成员点击重试只作为触发者，不替换资源 owner 或凭据 profile。所有权转移须暂停准入并由新旧权限边界显式处理；owner 离开或失效不能悄悄将资源记到请求成员名下。工作区删除事务先给所有 waiting/活动意图设持久化 tombstone；协调器创建资源前和回写前重新核验，避免工作区删除后产生孤儿节点。

新工作区创建只保证业务记录和准备意图成功，不等待模型、拉取镜像或容器健康。Docker 故障不会回滚已成功创建的工作区；数据库意图写入失败则整个工作区事务回滚。自动准备仅在显式启用本地 Cloud Runtime 的部署生效；未启用的自托管/SaaS 部署保持现有工作区创建行为，Aurora 返回未配置状态。

注册沿用一次性短期注册凭据兑换受限 daemon 身份的机制，将实现移入统一 Cloud Runtime 边界。凭据绑定 node/workspace/runtime/daemon 和操作代次；消费、绑定、撤销旧输入及新身份记录在同一事务完成。只存哈希，明文不进 SQL、日志或模型上下文。注册代码不得按 Aurora/普通任务分成两条长期维护的引导分支。

token 的公开旧格式仅按已有 API 兼容要求处理；新节点内部统一使用一套注册协议。旧节点先排空后迁移，不靠新旧 token 双写维持两套运行时。

### 5.3 注册持久化、续期与响应丢失

现有 `bootstrapManaged` 只将兑换后的 daemon token 放在进程内，当前 token 有八小时有效期；新设计必须补齐以下协议，不能假设已有重启恢复：

- 首次注册经统一 `/api/daemon/managed/enroll` 接口兑换一次性输入。验证后，将 daemon credential、有效期、node/runtime/daemon ID、注册代次原子写入 `/data/identity/session.json`（0700 目录、0600 文件、拒绝 symlink、fsync 文件及目录）。此文件只对 daemon 可读。持久化失败不能开始领取任务。
- 重启优先读取持久身份，向服务器核验代次、绑定及撤销状态；不能再次消费原一次性 enrollment。节点卷损坏/丢失也不能推断成新的工作区或 daemon。
- 增加 `POST /api/daemon/managed/renew` 与 `POST /api/daemon/managed/renew/ack`。剩余有效期 30 分钟时停止新领取并启动轮换：daemon 用密码学随机源生成新 token，将新旧身份及稳定轮换 ID 持久化到 pending 文件，再以有效旧 credential 上传新 token 哈希。服务端只为当前绑定 CAS 建立一个 pending 轮换，同 ID/同哈希重放返回同结果，不同输入拒绝。daemon 收到确认后先原子替换并 fsync 本地 session，再以新 credential 发送 ack；服务端在 ack 事务内激活新 credential 并撤销旧 credential，随后客户端清理 pending。不能先撤销旧 credential 再保存新明文。
- ack 响应丢失时用已持久的新 credential 重放同轮换；重启发现 pending 时先恢复这一协议，不再次生成 token。pending credential 只接受绑定的轮换操作；旧 credential 在原有效期内只允许心跳、原任务报告和轮换，不允许新领取。已 ack 的新 credential 才恢复正常权限。轮换超时不能延长旧 token 的八小时上限，过期后仅允许尚有效的新 pending credential 完成原 ack。认证、HTTP 和 WS 均检查这些状态及服务端有效期，不能只更新正向缓存。每个持久化/提交/响应边界均需中断恢复测试。
- 轮换完成后撤销旧 token、清认证缓存和旧 WS 的领取权；若超时或两份 credential 均不可用，保持暂停，走下述重新注册流程，不延长无效凭据。
- enrollment 已消费但响应丢失、本地持久化结果未知或身份过期时：Fleet 首先关闭领取，核查精确容器/操作归属与待回传，撤销该注册代次全部 daemon credential 并确认旧进程停止，再重新交付新代次 enrollment。逻辑 node/runtime ID 保留；未知资源/未知活跃执行状态拒绝重建。停止旧进程并不证明外部 provider 未执行，相关业务结果按 §7 恢复。

所有控制端点只接受服务器绑定的节点身份；不允许请求体替换 workspace/runtime/daemon ID。控制代次、Docker start epoch、业务 task attempt 分别校验，不能将任何一个数字当作另外两者的证明。

### 5.4 幂等与恢复

- 工作区准备请求使用同一活动 operation；重复点击、并发请求和网络重试返回相同操作身份。
- 确认失败后的显式重试递增操作代次，复用逻辑节点和运行时身份；旧代次回调不得使新代次变为就绪。
- 沿用 namespace、owner/capacity、node 等既有锁顺序与 `fleetguard` 事务屏障。补充工作区唯一绑定锁时所有写入入口使用一致顺序。
- 数据库事务不跨 Docker/网络 I/O。资源创建结果未知时先检查原资源身份；不因超时创建第二份资源。
- 注册明文交接丢失或消费结果不确定时，先查原操作及凭据状态，按既有期限和撤销规则恢复；不猜成功、不盲目重发可复用密钥。
- 保留当前默认资源、有限重试、初始化期限和健康过期规则；准备流程不绕过每 owner 的节点容量限制。容量不足显示阻塞原因，释放容量后可重试。只有通过容量准入的活动准备操作才占用节点配额，尚未获准的意图不能制造资源或无限占满配额。

## 6. 状态、接口与应用行为

### 6.1 两层状态

Fleet 的目标状态、观察状态和操作阶段仍是事实来源。应用投影为 `unconfigured`、`provisioning`、`online`、`failed`、`stopped`、`offline`、`maintenance`、`deleting`；不另写一列由前端或 Aurora 独立维护的生命周期。

`online` 表示基础运行时可以接收其允许类别的任务。skill 是否可运行还需能力检查，避免某个 provider 缺失使所有无关 skill 都不可用。对指定 skill，准入必须同时满足：

1. 工作区成员权限及服务器确定的执行策略有效。
2. 节点和 daemon 心跳新鲜，注册绑定一致，无维护、删除或 namespace 屏障。
3. 统一镜像与要求的隔离策略通过检查。
4. skill 已在目录开放、broker/tool/依赖契约匹配，所需 provider 配置存在且格式有效。

配置检查不偷偷发起收费 provider 调用，也不承诺第三方服务始终可用。真实服务故障走运行失败与结算恢复流程。

### 6.2 API

| 接口 | 行为 |
| --- | --- |
| `GET /api/aurora/runtime` | 保留现有路径及字段；通过统一 Cloud Runtime 服务读取工作区投影，始终只读 |
| `POST /api/aurora/runtime/prepare` | 新增；确保已有工作区准备、重试确认失败或唤醒明确停止的节点，返回 202 和稳定操作引用 |
| `POST /api/aurora/generations` | 保留业务接口；只向已就绪且允许目标 skill 的运行时准入，不再负责首次创建节点 |

准备响应包含 `workspaceId`、`nodeId`、`runtimeId`、`operationId`、`state`、`ready`；已就绪时返回 200 当前投影，不创建操作。请求体不接收镜像、凭据、执行策略或运行时 ID。

运行时投影保留现有 `workspaceId/node/runtimeId/state`，增量提供 `ready`、`allowedActions`、`skillReadiness` 和公开错误码。`skillReadiness` 按 skill ID 返回 `ready` 与可公开的阻塞原因；不暴露密钥值、私密路径、完整内部错误或宿主机信息。`operationId` 是进度追踪用稳定 ID；准备代次变化通过 `operationGeneration` 显示，旧代次状态不能覆盖新代次。

`ready` 由服务器计算，布尔值始终存在；`node/runtimeId/operationId` 在尚未存在时为 null；`allowedActions` 是当前调用者可做的动作数组，缺失或未知动作一律不显示。无逻辑绑定投影为 unconfigured，通过原因区分部署未配置和已有工作区尚未准备；有绑定时状态优先级固定为 deleting → maintenance → failed → stopped → provisioning（含 waiting）→ offline → online。waiting_config/waiting_capacity 必须返回具体阻塞原因，不能让用户误以为 Docker 已开始创建。后台协调器每 5 秒处理可重试意图，获准创建后沿用 5 分钟初始化窗口和最多 5 次有界自动重试；等待配置/容量不启动该窗口，状态不明不消费重试次数去创建新资源。

准备/唤醒/失败重试需要经验证的人类工作区成员身份，资源配置由服务端固定；节点的 stop/delete、配置变更继续要求原管理权限。成员不能借准备接口扩大规格或恢复被管理员置于维护/删除状态的节点。所有请求重新验证成员身份，不能信任前端缓存。

公开错误码区分 `runtime_unconfigured`、`runtime_preparing`、`runtime_offline`、`runtime_busy`、`runtime_capacity_exceeded`、`runtime_policy_unavailable`、`skill_unavailable`、`provider_unconfigured`。生成接口继续保留现有 `aurora_runtime_unavailable` 的响应兼容，可增加 `reasonCode`；不删除旧客户端依赖的字段。

普通错误使用已有 401/403/409/503 语义：身份/权限错误、维护或操作冲突、基础设施或依赖不可用各自区分。未知或读取失败的状态绝不推断为就绪。离线但原容器结果不明时，准备接口只能触发原身份的诊断恢复，不创建替代节点。

### 6.3 Aurora 应用

- 注册/工作区创建完成后正常进入应用，显示准备状态；用户可浏览目录、编辑草稿和准备附件。
- 已有工作区无绑定时提供显式“准备运行时”；状态查询和页面挂载不能隐式创建 Docker 资源。
- 准备失败显示脱敏原因与“重试准备”；明确停止显示“唤醒运行时”；状态不明显示恢复状态，不能显示虚假的已完成。
- 生成按钮组合校验原有输入/附件条件、运行时 `ready` 和目标 skill 的 `ready`。表单保留输入，运行时变化不清空草稿。
- 运行时准备期间每 3 秒更新，前台就绪期间每 10 秒更新；轮询是本次必须实现的更新机制，不假定已有 Runtime WebSocket 事件；后续若接入已定义事件，只触发同一 Query 失效。窗口恢复焦点立即重查，后台标签停止周期轮询；离开工作区取消该视图订阅，不复用其他工作区的数据。
- 恢复操作等待服务器接受后更新操作引用，再追踪状态；不乐观标为就绪。提交生成响应不明时保留现有不可重复提交保护，引导核对历史记录。
- core 放 DTO、schema、请求、Query 和 mutation；views 放业务组件；apps/aurora 保留路由与应用接线。新增响应走 zod/parseWithFallback，未知枚举保守降级。
- 中文称“运行时”，skill 保持英文；帮助仅说明准备限制、恢复操作和费用后果，同步全部受影响的翻译与无障碍名称。

## 7. 生成准入、作品与积分

准备和恢复不创建 generation、不消费月度生成额度、不预留积分。当前生成实现先提交 generation、再单独预留积分、再入队、最后关联 task，不能满足崩溃原子性。本次须抽取接受同一 `qtx` 的内部操作，在一个事务中完成最终准入、generation、积分 deduction、task、附件关联和 generation→task 链接；保留原账本与队列，不保留这些步骤的多事务拼接。

审核、附件可读性、月度额度初始化及无副作用的配置解析放在事务前；最终成员/绑定/容量/额度检查在事务内重做。事务先沿既有 `runFleetTx` 获取 namespace/node/业务所有者屏障，再取得 Aurora 用户额度锁及余额行锁，最后写新 generation/task/ledger；其他触及这些锁的路径必须遵守同一顺序，不能在持锁事务中调用自行开事务的 `Credit.Reserve` 或 `EnqueueQuickCreateTask`。提交后才发送 WebSocket/唤醒提示；提示丢失由现有队列扫描恢复。

新客户端在一次明确提交时持久化 `Idempotency-Key`（用户、工作区作用域），网络重试复用该键。新增唯一键和请求指纹绑定 skill、prompt、按原顺序的 attachment IDs；同键同请求返回同一 generation，不同请求为 409。旧客户端不带键仍保持原请求兼容，由服务器为该请求生成唯一 ID，但不宣称其跨请求自动重试幂等。未就绪请求不落 generation/ledger，不消耗该业务提交键。

不得把前端 `ready` 或一次事务外检查当作最终授权。准入与维护切换必须共享既有节点屏障，防止“检查就绪后节点进入删除、任务却继续入队”的竞态。节点在准入提交后仍可能掉线，该情况由任务恢复处理，不通过持有数据库锁等待整个执行规避。

生成绑定实际 `runtime_id`、节点身份和受信任的 skill 上下文。沿用当前任务队列、task token、附件暂存、manifest、审核及 `ReportTaskArtifacts`/完成结算机制，不增加节点专用业务协议或第二套积分账本。

运行中断时保留原 generation/task。提供方请求、产物导入、完成报告、扣款或退款按现有业务身份去重；收到重复回调只产生一次资产写入和一次账务效果。节点恢复只重放原任务允许的恢复步骤，不重新生成用户请求。

超时、心跳丢失或回调暂时不可读都不能单独证明 provider 未执行。原 `settleAuroraOnFailed` 会对失败任务退款，`aurora_provider_run` 的 ambiguous 分支也以失败退款为既有语义；本次明确改变这些入口的恢复判断，不能写成直接复用即可。

新增统一的 Aurora 结果协调服务，读取已有 generation/task/provider-run/asset/ledger 事实，每 30 秒有界扫描并持久化修复状态，不依赖已消失 daemon 再次回调。业务状态与 `recovery.state=none|reconciling|needs_review` 分离，旧客户端的 generation status 不改为未知枚举。恢复规则：

| 可验证事实 | 动作 |
| --- | --- |
| 未创建任何 provider run、任务已被可信屏障停止且无待回传 | 原 generation 确认失败，幂等退款 |
| 存在 external ID，provider 支持查状态 | 仅查询原 ID，不再 create；结果导入与审核完成后结算 |
| provider 明确失败/取消且无可交付资产 | 沿既有账本幂等退款并记录终态 |
| provider creating/ambiguous 且没有 external ID | 禁止自动 resubmit/退款/扣第二次款；进入 needs_review |
| 已有合规资产、task 完成但 generation 未终态 | 重放原完成结算 |
| task 已终态、退款或终态更新曾失败 | 协调器重试同一账本幂等键，不重新执行任务 |

明确可查询的恢复最多自动观察 15 分钟，超过后转 needs_review 并展示原因；这只是升级人工处理，不是判定失败。平台运维通过受服务认证的固定 `reconcile` 操作提交 provider/资产证据，追加审计记录后推进同一个 generation；不能任意改余额。无法取得确定证据时保留未决状态，并明确对用户展示预留金额，不能声称系统保证最终自动收敛。

有未决外部结果且旧执行进程未确认停止时维持节点屏障；确认本地无活跃进程/待回传后可释放执行并发，但保留业务恢复与预留。`CountActiveGenerations` 必须区分活跃执行和 needs_review，避免一个人工处理项永久占满用户并发额度；月度已发生的生成计数不因此清零。迟到回调核对原 task attempt、租约/注册代次和身份，先作为证据进入协调服务，不能复活已终态任务或跨代次覆盖结果。

## 8. 同一镜像的执行隔离

### 8.1 安全模型

受信任部分包括 Fleet、daemon 的固定启动器、只读 broker 实现、签名镜像及管理员配置。提示词、附件、模型工具参数、模型输出和 provider 返回内容均不可信。隔离目标包括阻止模型取得任意执行面、跨工作区访问、读取其他 provider 凭据和绕过出网规则；不宣称 Docker 可抵御宿主机管理员或所有内核漏洞。

### 8.2 进程边界

daemon 分别启动 broker 与 Claude，清理各自环境，监督其生命周期。broker 不再由 Claude 直接以拥有 provider 文件权限的通用 Node 命令启动。

保留 broker 的 MCP 工具协议；增加固定本地桥接：daemon 监督 broker 的 stdio，并通过任务专用 Unix socket 提供 MCP 连接，Claude 的 MCP 配置只启动独立的 `/usr/local/libexec/multica-mcp-client`。该二进制没有管理、启动 daemon 或启动代理的子命令；禁止改回 `fleet-node broker-client`。

桥接只转发当前任务的 MCP 流量，不接受命令、文件路径、provider 地址或任意目标 socket。socket 路径绑定 node/task/attempt，由 daemon 创建。0600 只是基础 DAC，进程同 UID 时不构成任务隔离：还必须验证 peer credential、可用的 AppArmor peer label、daemon 持有的活动 attempt 会话，并在文件规则中只允许当前任务 socket。每个 attempt 仅一个有效客户端；断连重连需关闭旧连接。JSON-RPC 消息设置 1 MiB 单帧上限、最多 8 个未完成请求和有界发送缓冲；超限拒绝该连接，不能杀死 daemon。保留 MCP initialize、notification、request ID 与 cancellation 语义，任务取消先停止接收新工具调用，再关闭桥接并持久化结果。

| 进程 | 允许范围 | 必须禁止 |
| --- | --- | --- |
| daemon / 固定监督器 | 注册、节点持久状态、任务领取、创建子进程、报告 | 向模型转交 Fleet/daemon 凭据 |
| Claude | 当前任务上下文、自己的会话目录、获准的模型连接、固定 MCP 客户端 | provider 文件、节点身份文件、Git/shell/任意解释器、其他任务目录、启动 broker 服务端 |
| broker | 固定可信代码、当前任务附件/输出、所需 provider 文件、固定适配器 | 用户命令、动态脚本、任意路径读取、其他任务、daemon 凭据 |
| 渲染/转码子进程 | 固定程序、必需只读资源、当前任务输入输出 | provider 密钥、节点身份、非必需网络、执行可写目录内容 |
| egress 代理 | 代理程序、公开允许列表和 CA | 节点注册、任务目录、provider 凭据、shell/其他程序 |

Claude 的模型认证输入可由受信任启动器按现有契约提供；不因此允许 Claude 读取 broker 的图片、视频等 provider 文件。子进程环境不继承其他角色的认证变量；进程内存和 `/proc` 等旁路读取纳入拒绝测试。

操作系统权限采用受加载验证的 AppArmor 进程策略与 seccomp，并保留只读根、capability drop、no-new-privileges、限制挂载及内部网络。收紧旧目录级执行通配，按固定入口和实际运行依赖授权。[Linux no_new_privs 文档](https://docs.kernel.org/userspace-api/no_new_privs.html) 指出该标志可能影响 LSM 在 exec 时收紧策略；因此父子进程必须在目标内核上验证实际 label 与实际拒绝行为，不能将配置中存在 profile 名称当作证明。切换失败不能退回继承宽权限，不能关闭 no-new-privileges 换取通过。

AppArmor 不能仅靠允许通用 Node 路径约束脚本参数。Claude 不具备 Node/loader/管理二进制执行权限；可信 broker 的 Node 进程来自 daemon 固定参数，移除 `NODE_OPTIONS`、`LD_PRELOAD`、`LD_LIBRARY_PATH` 等可注入变量。模型设置只读取管理员生成的最小配置，禁用工作区 hooks、插件及额外 MCP 配置发现。broker 可读数据不能变成动态 import/eval；恶意路径、符号链接、解释器加载可写文件与动态 loader 绕行均是拒绝测试。

当前 Chromium 发行包入口可能是 shell wrapper，HyperFrames 也会使用内部 loopback 静态服务。因此可行性门必须解析真实 Chromium/Claude/HyperFrames 入口及必要 helper；为它们提供固定可信启动路径，不因 wrapper 失败就开放任意 shell。允许渲染器访问当前任务固定内容与自己的 loopback 服务，禁止访问 daemon health/control 和 provider 代理；Chromium `--no-sandbox` 不算浏览器隔离证明，其外层进程策略必须覆盖实际渲染进程树。

broker 和渲染器不得将用户输入作为命令、脚本或可执行模块；解释器只能加载只读可信实现。仅设置 `noexec`、修改 PATH 或工具 denylist 不构成该保证。Claude 只能使用服务端确定的 `mcp__aurora__<tool>`，未知 skill/provider/工具在进程启动前被拒绝。

### 8.3 平台能力与网络

Fleet 必须探测实际 Docker Engine 的隔离能力并验证策略已加载，平台检查依据 [Docker AppArmor 文档](https://docs.docker.com/engine/security/apparmor/)。AppArmor `unconfined`、策略缺失、固定程序无法进入预期进程策略均使 Aurora 能力不可用，返回 `runtime_policy_unavailable`。

本版不增加未经验证的弱隔离后备方案。2026-10-07 对当前环境只读执行 `docker info --format '{{json .SecurityOptions}} {{.OperatingSystem}} {{.Architecture}} {{.ServerVersion}}'`，返回 `["name=seccomp,profile=builtin","name=cgroupns"] Docker Desktop aarch64 29.8.0`，未报告 AppArmor。按本规格当前 Docker Desktop 不满足执行前置条件，不能把它列为已支持平台。

完整执行的候选目标为能加载所需 AppArmor 策略的 Linux Docker Engine；可以位于 Mac 上的独立 Linux VM，但这属于需用户确定的执行环境变更，不是本评审已经完成的环境切换。在该 Linux 环境内 co-locate 受管 API/Fleet，并验证共享数据库身份、API↔Fleet loopback 契约、容器回调及私密文件的 guest 路径；不能只把 Docker socket 指向另一台引擎并沿用 Mac bind 路径。若用户要求当前 Docker Desktop 原地运行，则隔离设计必须另行修订并实证，当前规格整体仍不能标为可执行完成。评审不安装 VM、修改 Docker 配置或开启远程 socket。

节点只在工作区内部网络，代理是唯一受控出网路径。代理负责精确 CONNECT 目标、解析地址及私有/元数据地址规则；HTTPS 隧道内的路径、请求体和重定向由 broker/provider transport 验证，不能宣称 CONNECT 代理看得到 TLS 内部 HTTP。API 回调只开放该部署必需的固定目标。节点内部网络不是进程间网络隔离：Claude/broker/渲染器到代理、daemon loopback 及任务 socket 的不同权限必须实际验证。运行时或模型不能改允许列表。代理虽使用全量镜像，进程策略仍只允许代理程序所需访问。

节点同一时间最多执行一个任务。任务结束先停止全部子进程、关闭 broker/socket、持久化必要回传，再清理任务输入输出；无法确认清理完成时不接收下一任务。禁止以清空共享目录替代按任务身份清理。

## 9. 测试与可复核验收

### 9.1 同一个被测发布产物

确定性闭环运行真正的统一镜像、daemon、Claude CLI 和 broker。测试通过受控模型/provider HTTP 服务返回稳定响应和媒体素材，不在镜像中加入 fake daemon、fake Claude、测试专用 skill 或安全绕过开关，不发布 fake 节点镜像。此层不是默认测试：除 Docker 集成门控外，执行真实 CLI 的用例也必须具备 `agentintegration`、`MULTICA_RUN_REAL_AGENT_SMOKE=1` 和明确授权；只能读取测试生成的假凭据及受控 endpoint，不能自动探测宿主机账号。默认单元/组件测试继续使用测试创建的替身，不启动镜像中的真实 CLI。

现有 egress 明确拒绝 provider 解析到 RFC1918、loopback、文档和 benchmark IP；把固定 provider 域名 DNS 指向本地 fixture 不可行。完整闭环的测试服务必须提供操作者控制的、通过生产 `IsPublicIP` 的固定可路由 IP，以既有 `egress_pins` 将获准 provider host:443 指向它，不改变生产 IP 拒绝逻辑。服务只使用假凭据和合成素材，限制测试来源及临时会话，不接收用户数据；测试 CA 通过固定只读文件及严格允许的 Node/Claude 信任配置注入，不使用 `NODE_TLS_REJECT_UNAUTHORIZED=0`。证书覆盖所模拟的 provider 主机；API 的产物导入客户端也必须接入同一受控返回 URL/信任链，否则只证明 broker 半程。

此 fixture 服务及合法 IP 是完整端到端测试的显式环境依赖，当前尚未建立；不能把它写成现成能力或偷偷使用真正 provider。缺少此环境时默认适配器单测可用内存替身运行，但真实镜像闭环保持未运行。测试信任材料与假凭据仅经部署文件注入，不存在测试专用节点镜像、镜像内 fake 模式或生产放宽开关。模型 SSE/tool-use、provider create/poll、媒体下载和 API 产物导入须先各有契约 fixture，再组合为 13-route 闭环。

### 9.2 分层覆盖

| 层次 | 必须覆盖 |
| --- | --- |
| 领域/数据库 | 工作区与意图原子性、waiting 不占资源、唯一绑定、重复准备、操作代次、owner 变更及工作区删除、容量阻塞、注册消费/响应丢失、凭据持久化/续期、维护/领取竞态、跨工作区拒绝 |
| API/schema/组件 | 所有状态及错误降级、权限与错误码、GET 只读、准备操作幂等、生成就绪门禁、草稿保留、切换工作区、所有支持的翻译 |
| 镜像/安全 | 唯一产物、节点与代理相同 digest/实际 image ID、固定 Claude/broker 路径、依赖/vendor 校验、非 root/只读、进程角色权限、无密钥泄漏 |
| 真实 Docker + 受控服务 | 新工作区→准备→注册→就绪→13 个可用 skill 的工具调用与产物→审核→作品入库→结算/退款；执行真实 daemon/Claude/broker |
| 故障矩阵 | 在 generation/reserve/task/link 每一步注入事务故障并验证全回滚；提交成功响应丢失的幂等重试；Docker 创建结果未知、协调器重启、注册交接丢失、凭据到期、provider ambiguous、结算后进程崩溃、待回传及删除/恢复竞争 |
| 浏览器 | 注册或新建工作区、准备中不能生成、失败重试、就绪生成、进度更新、作品预览/下载、余额变化、离线恢复、刷新及工作区切换 |

13 个可用 skill 分别验证自身输入约束、工具路由和真实适配器输出契约；未开放的目录项不能被计作可运行。并非只验证 13 个名字存在。

负向隔离测试实际尝试 Git/shell/Node 任意脚本执行、读取 broker/daemon 凭据、跨任务/工作区文件与 socket、读取进程环境、直接出网、代理绕过及可写脚本执行。允许的 Claude、broker、Chromium、FFmpeg 正向路径也必须通过，不能通过禁止全部执行制造假安全通过。

### 9.3 真实服务与证据

受控服务闭环证明工程集成；真实 Claude/model/provider 烟测证明外部服务可用，二者分开记录。真实调用沿用 `agentintegration`、`MULTICA_RUN_REAL_AGENT_SMOKE=1` 和 owner 明确授权，不从本次设计确认推导调用或消费授权。

每次验收记录代码 HEAD、唯一 index digest、实际架构 manifest/image ID、Docker Engine/平台、策略版本和加载结果、受管 API URL、workspace/node/runtime/task/generation 关联、命令与结果、资产及账本断言、浏览器 trace/screenshot。不得记录密钥或 token。

`PASS`、`FAIL`、`SKIP`、身份未核验必须分别表达。完整交付要求支持环境中的真实 Docker/受控服务闭环和浏览器通过；真实 provider 未授权时明确保留未验证状态，不声称所有线上服务已经验证。

## 10. 迁移、切换和恢复

这是对当前本地 Fleet 的受控升级，不支持双运行时长期并行或内部双写。实施计划按以下顺序组织，不能先拆除旧执行链路再准备新镜像：

1. 完成隔离可行性门：在统一候选镜像上证明 Claude/broker 分离、进程策略切换、代理同镜像运行及允许/拒绝矩阵。候选是同一最终目标的构建版本，不是第二种发布镜像。未通过则停止切换并如实报告设计实现阻塞。
2. 完成统一镜像构建、内容与供应链门，以及统一身份/准备/应用改造的确定性检查；所有迁移编号以实施时仓库占用情况为准。
3. 切换目标 namespace 前检查实际镜像、节点归属、数据库身份、活跃任务、待回传和未完成 provider/账务结果，生成可审阅的迁移映射。多重运行时或资源归属不明时拒绝自动合并。
4. 通过既有屏障停止新准入，让旧任务和回传收尾；保留 API 处理结果直到确认静止。不强杀执行、不因超时销毁未知资源。
5. 在保持维护屏障下应用 additive 数据变更；新增迁移日志存储必须先于新控制程序启动，旧 API 仅在明确兼容 additive schema 时继续处理收尾，不能绕过版本门。每节点持久记录 `inventory_verified → admission_closed → drained → old_resources_stopped → binding_converted → new_resources_ready → admitted`，只有对应事实已证明才推进。日志保留旧/新镜像、精确容器/卷/网络、runtime/agent/token 映射及完成证明；不得先覆盖旧资源身份再尝试清理。
6. 经原资源所有权检查关闭旧节点与代理并记录 old_resources_stopped，随后转换绑定再启动新资源。优先保留原 `runtime_id`，转换 provider/mode/metadata 并更新智能体关系；核查 `(workspace_id, owner_id, runtime_id, system_key)` 系统智能体唯一约束，不能通过重新 seed 产生重复智能体。历史 task/generation/账本不改写。`aurora_sandbox_node` 停止参与新生命周期，确认 token、reaper、heartbeat、workspace delete 及 SQL 引用迁移后退役，不能作为隐藏的第二事实来源。配置唯一镜像引用并以统一引导启动；保留逻辑绑定和所需持久数据。原物理资源 ID 不得伪装成新容器身份，替换使用明确的维护/迁移操作。
7. 新节点完成隔离、注册、健康与能力检查后，才解除准入屏障；运行受控生成与浏览器验收。
8. 删除旧 Dockerfile、独立发布任务、Aurora 专用 bootstrap/生命周期入口及失效配置，更新旧计划链接、镜像验证器、操作文档和受影响的 AGENTS 规则。

切换失败保留维护屏障、原操作、映射与数据，按最后已证明的检查点向前恢复。Fleet 与 API 共用数据库，不能为回滚一个 namespace 恢复整库旧快照并覆盖其他工作区的新任务/账本；默认恢复只针对迁移日志证明归属的绑定与资源。逆向迁移只允许在解除准入前且新节点没有任何业务写入的条件下，按已审查的逐行补偿执行。整库恢复是需要全库停写与独立授权的灾难恢复操作，不是本计划的回滚步骤。恢复前不允许新旧节点同时领取任务。

所有 DDL 遵循仓库规则：无外键、无级联；每个索引单独使用 `CREATE [UNIQUE] INDEX CONCURRENTLY` 迁移；条件对象变更幂等；SQL 修改后 `make sqlc`。既有 managed environment、数据库物理归属及 Fleet namespace 销毁规则不因本设计改变。

## 11. 实施边界与交付物

代码职责集中于 `server/internal/fleet`、`cloudruntime`、`fleetguard`、`daemon`、Aurora 业务接缝及相关注册/工作区 handler；不把产品逻辑塞进 Docker Provider，也不在 Aurora 新建调度器。

主要交付物：唯一 Dockerfile/发布链、统一身份与注册、事务性准备/恢复、运行时投影和准备 API、Aurora 就绪及恢复 UI、进程/网络隔离策略、13-route 与浏览器证据、一次性迁移和操作文档。

实施验证按改动范围运行窄检查，再执行根前端检查、Go 检查及相应 Docker/浏览器门。文档设计阶段只运行链接/引用和 diff 检查，不运行产品测试、构建镜像、启动环境或调用 provider。

## 12. 实施依赖与阻塞门

按一条主计划和以下有依赖的工作包实施，禁止以独立完成的小测试替代最终闭环：

| 工作包 | 前置条件 | 可判定交付 |
| --- | --- | --- |
| G0 平台与隔离原型 | 执行环境经用户确定 | 实际加载策略、NNP 下进程 label、独立 MCP 客户端、Chromium/HyperFrames 正向及越权负向矩阵；不通过禁止进入发布切换 |
| G1 唯一镜像与受控服务契约 | G0 | 单发布产物、两架构内容/安全/供应链验证，受控服务的固定公网 IP、TLS、SSE/provider/media 合同实证 |
| G2 统一注册/身份 | G0；使用 G1 候选镜像验证 | 单次注册、响应丢失、8 小时到期前轮换、重启持久化、claim/WS/HTTP 代次屏障 |
| G3 准备/生成/结果事务 | G2 | waiting 意图、owner/capacity/delete 规则、原子生成提交、幂等请求、独立结算协调及 needs_review 运维闭环 |
| G4 应用接线 | G3 API/schema 合同 | 准备/恢复/就绪门禁、Query 更新、旧响应兼容、多语言与浏览器交互 |
| G5 迁移与最终验收 | G1–G4 | 每检查点中断恢复、同一镜像 13-route、browser、真实服务已运行或明确未授权的分项证据 |

截至 2026-10-07，G0 目标平台尚未落实且当前 Docker Desktop 不满足 AppArmor 前置条件；G1 受控公网 fixture 尚未建立。其余修订是可测试的实现要求，均未通过代码或引擎实验验证。本规格不保证第三方 API、无 external ID 的不确定请求或缺失隔离能力的主机会自动成功。

本规格的书面批准是下一步编写实施计划的前提。完成规格不等于开始实施，不把历史计划的旧授权或旧验收状态当作新镜像已经通过验证。
