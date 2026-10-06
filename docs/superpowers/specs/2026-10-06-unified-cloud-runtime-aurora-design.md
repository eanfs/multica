# 统一 Cloud Runtime 与 Aurora skills 执行层设计

日期：2026-10-06

状态：三节架构设计已由用户确认；本书面规格待用户审阅，尚未进入新实施计划或代码实施。

核对基线：`feat/local-docker-cloud-runtime`，`92650fc80`。工作区存在并行改动，本次只提交本文件。

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

普通任务与 Aurora 任务可以使用同一套运行时实现，但执行策略是服务器持有的任务约束，不是两种镜像、两套 bootstrap 或第二个 daemon。Aurora 自动准备的工作区节点只接收该工作区获准的 Aurora 任务；通用节点不能因镜像中存在 broker 就自动获得 Aurora 任务权限。策略不能在忙碌节点上原地放宽。

## 4. 单一镜像与固定布局

### 4.1 构建与发布

`docker/runtime/Dockerfile` 允许多个中间构建 stage，但只有一个可发布的最终运行 stage。合并 Go 构建、锁定的 Claude/Node 依赖、vendor 校验与补丁、媒体工具和代理程序；删除 `deploy/aurora-sandbox/Dockerfile` 与 `Dockerfile.egress` 的独立构建入口。

保留 `deploy/aurora-sandbox/runtime`、vendor、锁文件和安全策略作为已有源目录，本次不为目录命名迁移扩大变更。固定 broker 路径可继续指向 `/opt/aurora/runtime/deploy/aurora-sandbox/runtime/src/server.mjs`，它表示载荷位置，不表示第二种运行时。

镜像内只安装一份锁定版本的 Claude。`/usr/local/bin/claude` 是统一的可执行入口，普通执行与 Aurora 执行共同使用；删除第二套 Claude 版本来源和安装过程。入口必须解析到镜像内只读可信文件，不能由工作区 PATH 或用户文件覆盖。

镜像默认入口保持 `fleet-node run`；代理角色由 Fleet 使用固定的 `fleet-node proxy` 入口启动。客户端不能指定 entrypoint、command、image、挂载、环境变量或脚本。代理角色只能启动内置代理程序。代理容器中即使存在节点二进制，也不具备节点注册凭据和执行权限。

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

系统智能体的 `runtime_id`、领取载体和前端显示的执行目标必须一致。不再额外创建 `aurora_managed` 载体，也不维护“实际 runtime + Aurora 别名 runtime”的双重领取集合。普通 Cloud Runtime 的既有身份字段含义沿用，不新增 Runtime mode 枚举。

### 5.2 持久化准备意图

工作区创建和准备意图在同一 PostgreSQL 事务提交。复用 Fleet 节点/操作表表达意图，不新建数据库、队列系统或 Aurora 专用节点状态机。创建事务不调用 Docker 或 Fleet HTTP；共享的领域服务只写事务内数据，后台协调器消费已提交意图。

准备意图持有稳定 workspace/node/runtime/daemon ID。规格、允许任务类别及配置引用从管理员规则解析；配置缺失仍能保存意图并呈现不可用原因。API 与 Fleet 必须对同一可信配置修订达成一致后才能创建资源。

新工作区创建只保证业务记录和准备意图成功，不等待模型、拉取镜像或容器健康。Docker 故障不会回滚已成功创建的工作区；数据库意图写入失败则整个工作区事务回滚。自动准备仅在显式启用本地 Cloud Runtime 的部署生效；未启用的自托管/SaaS 部署保持现有工作区创建行为，Aurora 返回未配置状态。

注册沿用一次性短期注册凭据兑换受限 daemon 身份的机制，将实现移入统一 Cloud Runtime 边界。凭据绑定 node/workspace/runtime/daemon 和操作代次；消费、绑定、撤销旧输入及新身份记录在同一事务完成。只存哈希，明文不进 SQL、日志或模型上下文。注册代码不得按 Aurora/普通任务分成两条长期维护的引导分支。

token 的公开旧格式仅按已有 API 兼容要求处理；新节点内部统一使用一套注册协议。旧节点先排空后迁移，不靠新旧 token 双写维持两套运行时。

### 5.3 幂等与恢复

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

运行时投影保留现有 `workspaceId/node/runtimeId/state`，增量提供 `ready`、`allowedActions`、`skillReadiness` 和公开错误码。`skillReadiness` 按 skill ID 返回 `ready` 与可公开的阻塞原因；不暴露密钥值、私密路径、完整内部错误或宿主机信息。

准备/唤醒/失败重试需要经验证的人类工作区成员身份，资源配置由服务端固定；节点的 stop/delete、配置变更继续要求原管理权限。成员不能借准备接口扩大规格或恢复被管理员置于维护/删除状态的节点。所有请求重新验证成员身份，不能信任前端缓存。

公开错误码区分 `runtime_unconfigured`、`runtime_preparing`、`runtime_offline`、`runtime_busy`、`runtime_capacity_exceeded`、`runtime_policy_unavailable`、`skill_unavailable`、`provider_unconfigured`。生成接口继续保留现有 `aurora_runtime_unavailable` 的响应兼容，可增加 `reasonCode`；不删除旧客户端依赖的字段。

普通错误使用已有 401/403/409/503 语义：身份/权限错误、维护或操作冲突、基础设施或依赖不可用各自区分。未知或读取失败的状态绝不推断为就绪。离线但原容器结果不明时，准备接口只能触发原身份的诊断恢复，不创建替代节点。

### 6.3 Aurora 应用

- 注册/工作区创建完成后正常进入应用，显示准备状态；用户可浏览目录、编辑草稿和准备附件。
- 已有工作区无绑定时提供显式“准备运行时”；状态查询和页面挂载不能隐式创建 Docker 资源。
- 准备失败显示脱敏原因与“重试准备”；明确停止显示“唤醒运行时”；状态不明显示恢复状态，不能显示虚假的已完成。
- 生成按钮组合校验原有输入/附件条件、运行时 `ready` 和目标 skill 的 `ready`。表单保留输入，运行时变化不清空草稿。
- 运行时准备期间每 3 秒更新，前台就绪期间每 10 秒更新；对应 WebSocket 事件触发 Query 失效，窗口恢复焦点立即重查。离开工作区取消该视图订阅，不复用其他工作区的数据。
- 恢复操作等待服务器接受后更新操作引用，再追踪状态；不乐观标为就绪。提交生成响应不明时保留现有不可重复提交保护，引导核对历史记录。
- core 放 DTO、schema、请求、Query 和 mutation；views 放业务组件；apps/aurora 保留路由与应用接线。新增响应走 zod/parseWithFallback，未知枚举保守降级。
- 中文称“运行时”，skill 保持英文；帮助仅说明准备限制、恢复操作和费用后果，同步全部受影响的翻译与无障碍名称。

## 7. 生成准入、作品与积分

准备和恢复不创建 generation、不消费月度生成额度、不预留积分。生成请求先验证权限、输入、审核及目标 skill 的执行条件；在现有事务屏障内再次验证绑定/维护状态，再执行权益准入、生成记录及现有积分预留流程。

不得把前端 `ready` 或一次事务外检查当作最终授权。准入与维护切换必须共享既有节点屏障，防止“检查就绪后节点进入删除、任务却继续入队”的竞态。节点在准入提交后仍可能掉线，该情况由任务恢复处理，不通过持有数据库锁等待整个执行规避。

生成绑定实际 `runtime_id`、节点身份和受信任的 skill 上下文。沿用当前任务队列、task token、附件暂存、manifest、审核及 `ReportTaskArtifacts`/完成结算机制，不增加节点专用业务协议或第二套积分账本。

运行中断时保留原 generation/task。提供方请求、产物导入、完成报告、扣款或退款按现有业务身份去重；收到重复回调只产生一次资产写入和一次账务效果。节点恢复只重放原任务允许的恢复步骤，不重新生成用户请求。

超时、心跳丢失或回调暂时不可读都不能单独证明 provider 未执行。状态不确定时显示“恢复中”，保留原预留并进入现有确定性终态处理；确认失败后才按原规则退款。取消、审核拒绝及终态重放必须沿同一结算路径，不新增例外双写。

## 8. 同一镜像的执行隔离

### 8.1 安全模型

受信任部分包括 Fleet、daemon 的固定启动器、只读 broker 实现、签名镜像及管理员配置。提示词、附件、模型工具参数、模型输出和 provider 返回内容均不可信。隔离目标包括阻止模型取得任意执行面、跨工作区访问、读取其他 provider 凭据和绕过出网规则；不宣称 Docker 可抵御宿主机管理员或所有内核漏洞。

### 8.2 进程边界

daemon 分别启动 broker 与 Claude，清理各自环境，监督其生命周期。broker 不再由 Claude 直接以拥有 provider 文件权限的通用 Node 命令启动。

保留 broker 的 MCP 工具协议；增加固定本地桥接：daemon 监督 broker 的 stdio，并通过任务专用 Unix socket 提供 MCP 连接，Claude 的 MCP 配置只启动镜像内固定的 `fleet-node broker-client`。桥接只转发当前任务的 MCP 流量，不接受命令、文件路径、provider 地址或任意目标 socket。socket 位置由可信任务 ID 派生，权限为 0600；连接核对运行身份和活动任务，任务结束关闭并清理，不能连接另一任务。

| 进程 | 允许范围 | 必须禁止 |
| --- | --- | --- |
| daemon / 固定监督器 | 注册、节点持久状态、任务领取、创建子进程、报告 | 向模型转交 Fleet/daemon 凭据 |
| Claude | 当前任务上下文、自己的会话目录、获准的模型连接、固定 MCP 客户端 | provider 文件、节点身份文件、Git/shell/任意解释器、其他任务目录、启动 broker 服务端 |
| broker | 固定可信代码、当前任务附件/输出、所需 provider 文件、固定适配器 | 用户命令、动态脚本、任意路径读取、其他任务、daemon 凭据 |
| 渲染/转码子进程 | 固定程序、必需只读资源、当前任务输入输出 | provider 密钥、节点身份、非必需网络、执行可写目录内容 |
| egress 代理 | 代理程序、公开允许列表和 CA | 节点注册、任务目录、provider 凭据、shell/其他程序 |

Claude 的模型认证输入可由受信任启动器按现有契约提供；不因此允许 Claude 读取 broker 的图片、视频等 provider 文件。子进程环境不继承其他角色的认证变量；进程内存和 `/proc` 等旁路读取纳入拒绝测试。

操作系统权限采用受加载验证的 AppArmor 进程策略与 seccomp，并保留只读根、capability drop、no-new-privileges、限制挂载及内部网络。收紧旧目录级执行通配，按固定入口和实际运行依赖授权；父进程到子进程的策略切换必须在既有 no-new-privileges 下实测。切换失败不能退回继承宽权限。

broker 和渲染器不得将用户输入作为命令、脚本或可执行模块；解释器只能加载只读可信实现。仅设置 `noexec`、修改 PATH 或工具 denylist 不构成该保证。Claude 只能使用服务端确定的 `mcp__aurora__<tool>`，未知 skill/provider/工具在进程启动前被拒绝。

### 8.3 平台能力与网络

Fleet 必须探测实际 Docker Engine 的隔离能力并验证策略已加载。AppArmor `unconfined`、策略缺失、固定程序无法进入预期进程策略均使 Aurora 能力不可用，返回 `runtime_policy_unavailable`。

本版不增加未经验证的弱隔离后备方案。macOS Docker Desktop 的能力取决于其 Linux VM，不能按宿主机名称假定支持；若不能通过同样的探针，本机只能运行不依赖该隔离的开发检查，Aurora 执行保持禁用。完整执行验收必须使用通过能力预检的本地或明确配置的 Linux Docker Engine。这是平台支持条件，不是已验证本机可以运行的声明。

节点只在工作区内部网络，代理是唯一受控出网路径。代理对精确目标、重定向、DNS 解析结果及私有/元数据地址实施既有允许规则；API 回调只开放该部署必需的固定目标。运行时或模型不能改允许列表。代理虽使用全量镜像，进程策略仍只允许代理程序所需访问。

节点同一时间最多执行一个任务。任务结束先停止全部子进程、关闭 broker/socket、持久化必要回传，再清理任务输入输出；无法确认清理完成时不接收下一任务。禁止以清空共享目录替代按任务身份清理。

## 9. 测试与可复核验收

### 9.1 同一个被测发布产物

确定性闭环运行真正的统一镜像、daemon、Claude CLI 和 broker。测试通过受控模型/provider HTTP 服务返回稳定响应和媒体素材，不在镜像中加入 fake daemon、fake Claude、测试专用 skill 或安全绕过开关，不发布 fake 节点镜像。此层不是默认测试：除 Docker 集成门控外，执行真实 CLI 的用例也必须具备 `agentintegration`、`MULTICA_RUN_REAL_AGENT_SMOKE=1` 和明确授权；只能读取测试生成的假凭据及受控 endpoint，不能自动探测宿主机账号。默认单元/组件测试继续使用测试创建的替身，不启动镜像中的真实 CLI。

测试服务运行于测试工具进程，可由操作者配置模型 endpoint；固定 provider origin 可在隔离测试网络中由 DNS 和受控 HTTPS 服务解析，测试 CA 作为显式只读信任材料提供。不得关闭 TLS 验证，生产不增加用户可控 provider URL、代理转发例外或密钥回退。若测试链路无法完成，记录失败或未运行，不用 handler mock 的 201 代替。

### 9.2 分层覆盖

| 层次 | 必须覆盖 |
| --- | --- |
| 领域/数据库 | 工作区与意图原子性、唯一绑定、重复准备、操作代次、失败重试、容量阻塞、注册单次消费、维护/领取竞态、跨工作区拒绝 |
| API/schema/组件 | 所有状态及错误降级、权限与错误码、GET 只读、准备操作幂等、生成就绪门禁、草稿保留、切换工作区、所有支持的翻译 |
| 镜像/安全 | 唯一产物、节点与代理相同 digest/实际 image ID、固定 Claude/broker 路径、依赖/vendor 校验、非 root/只读、进程角色权限、无密钥泄漏 |
| 真实 Docker + 受控服务 | 新工作区→准备→注册→就绪→13 个可用 skill 的工具调用与产物→审核→作品入库→结算/退款；执行真实 daemon/Claude/broker |
| 故障矩阵 | Docker 创建结果未知、协调器重启、注册交接丢失、过期心跳、provider 已接受但响应丢失、重复完成回调、回传待处理、删除/维护与恢复竞争 |
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
5. 应用 additive 数据变更。优先保留原 `runtime_id`，将它转换为实际统一绑定并更新智能体关系；确需新绑定时保留旧历史记录，历史 task/generation/账本不改写。`aurora_sandbox_node` 停止参与新生命周期，确认引用迁移后退役，不能作为隐藏的第二事实来源。
6. 经原资源所有权检查关闭旧节点与代理，配置唯一镜像引用并以统一引导启动；保留逻辑绑定和所需持久数据。原物理资源 ID 不得伪装成新容器身份，替换使用明确的维护/迁移操作。
7. 新节点完成隔离、注册、健康与能力检查后，才解除准入屏障；运行受控生成与浏览器验收。
8. 删除旧 Dockerfile、独立发布任务、Aurora 专用 bootstrap/生命周期入口及失效配置，更新旧计划链接、镜像验证器、操作文档和受影响的 AGENTS 规则。

切换失败保留维护屏障、原操作、映射与数据，先诊断再重试。涉及身份或数据语义的切换不支持盲目回滚旧二进制；若要恢复旧版本，必须静止节点并使用已验证的迁移前快照与相匹配的镜像/配置。恢复前不允许新旧节点同时领取任务。

所有 DDL 遵循仓库规则：无外键、无级联；每个索引单独使用 `CREATE [UNIQUE] INDEX CONCURRENTLY` 迁移；条件对象变更幂等；SQL 修改后 `make sqlc`。既有 managed environment、数据库物理归属及 Fleet namespace 销毁规则不因本设计改变。

## 11. 实施边界与交付物

代码职责集中于 `server/internal/fleet`、`cloudruntime`、`fleetguard`、`daemon`、Aurora 业务接缝及相关注册/工作区 handler；不把产品逻辑塞进 Docker Provider，也不在 Aurora 新建调度器。

主要交付物：唯一 Dockerfile/发布链、统一身份与注册、事务性准备/恢复、运行时投影和准备 API、Aurora 就绪及恢复 UI、进程/网络隔离策略、13-route 与浏览器证据、一次性迁移和操作文档。

实施验证按改动范围运行窄检查，再执行根前端检查、Go 检查及相应 Docker/浏览器门。文档设计阶段只运行链接/引用和 diff 检查，不运行产品测试、构建镜像、启动环境或调用 provider。

本规格的书面批准是下一步编写实施计划的前提。完成规格不等于开始实施，不把历史计划的旧授权或旧验收状态当作新镜像已经通过验证。
