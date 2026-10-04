# 本地 Docker Cloud Runtime 设计

日期：2026-10-04
状态：用户已授权开发，按既有 15 项计划顺序实施；本次准备仅修订文档，代码尚未实现。

## 1. 目标与已确认决策

在当前仓库新增独立 Go Fleet 服务，为现有本地 Multica 环境提供动态 Docker 执行节点。节点运行现有 Daemon，仅预装 Claude Code，注册可绑定 Runtime，复用已有运行领取、执行、消息上报和结果回传协议。

已确认：

- Fleet 与 API 分进程，代码放在当前仓库。
- 保留 `make up` 管理的宿主机 API/Web 与共享 PostgreSQL；Fleet 在 Docker 中运行。
- 使用 Docker Engine API 管理节点，不调用 Docker CLI，不为每个节点生成 Compose 项目。
- 不安装 Omp、Codex 或其他智能体 CLI，不自动创建业务智能体。
- Claude 模型凭证独立配置，不继承宿主机登录信息。
- 节点专属持久卷保存身份、会话和工作目录。
- Fleet 与 API 使用当前 checkout 的同一个 PostgreSQL 数据库，新增 Fleet 专用表；不新建数据库、SQLite 或独立数据库卷，不复制运行队列。
- 本地 Fleet 配置与原 SaaS Cloud 配置分离、互斥。
- 忙碌节点不能停止、重启或删除；删除永久移除节点容器、配置和数据卷。
- 设计和计划是已授权开发的执行依据；准备报告经 controller 审阅后进入顺序实施。Docker、registry、共享数据库环境建立及真实模型执行仍各需明确安全授权。

非目标：计费、SaaS 配额、EC2、自动扩缩容、自动休眠、多机调度、生产级多租户沙箱、网页密钥管理、任意命令执行、移动端改造、宿主机目录自动挂载。Docker 不是虚拟机安全边界。

## 2. 当前代码边界

- [Cloud 路由](<../../../server/cmd/server/router.go#L2356-L2369>) 已提供节点操作代理。
- [节点代理](<../../../server/internal/handler/cloud_runtime.go#L103-L145>) 从认证上下文取得用户 ID，转发请求并保留远端状态码。
- [HTTP 客户端](<../../../server/internal/cloudruntime/client.go#L151-L171>) 权威写入 `X-User-ID`，但身份 Header 不是服务间认证。
- [节点契约](<../../../packages/core/runtimes/cloud-runtime.ts#L4-L69>) 包含现有字段及过渡状态轮询。
- [共享运行时页](<../../../packages/views/runtimes/components/runtimes-page.tsx#L83-L99>) 被 Web/Desktop 使用；[Web 路由](<../../../apps/web/app/%5BworkspaceSlug%5D/%28dashboard%29/runtimes/page.tsx>) 当前用前端环境变量控制入口。
- [节点 Token 契约](<../../../server/internal/auth/cloud_pat.go#L299-L315>) 验证 `mcn_` 并返回所有者和实例标识。
- [Daemon 认证](<../../../server/cmd/multica/cmd_daemon.go#L525-L539>) 读取 CLI 配置内 Token，不能仅设置 `MULTICA_TOKEN`。
- [Daemon 健康状态](<../../../server/internal/daemon/health.go#L475-L521>) 区分初始化和可领取，并报告活跃运行及待回传结果。
- [Cloud 组装](<../../../server/cmd/server/router.go#L459-L476>) 同时启用 entitlement 和席位策略；[Billing 代理](<../../../server/internal/handler/cloud_billing.go#L116-L133>) 还共享 Cloud HTTP 客户端。必须分开客户端组装，不能仅替换 URL。

以上描述现有代码，不表示新增模块已存在。保留 SaaS 行为和 API 响应兼容性，不增加旧协议适配层。普通智能体运行不经过 `/nodes/exec`。

## 3. 模块和部署

建议新增位置，均为拟实现路径：

| 模块 | 职责 | 依赖 |
| --- | --- | --- |
| `server/cmd/fleet/` | 配置、依赖组装、独立进程生命周期 | Fleet 包 |
| `server/internal/fleet/` | 所有者校验、节点/操作服务、HTTP 契约、恢复协调 | Store、Provider |
| `server/internal/fleet/docker/` | Docker Engine API；以小接口提供容器、卷、检查操作 | Docker Engine |
| `server/internal/fleet/store/` | PostgreSQL 事务、幂等和恢复记录 | 现有 pgx/sqlc、当前 checkout 数据库 |
| `docker/fleet/`、`docker/runtime/` | Fleet/Claude 节点镜像、固定 bootstrap 和健康工具 | 当前源码、固定 CLI 版本 |

单一 Compose 定义部署 Fleet 和固定网络；动态节点由 Fleet 创建，不成为静态 Compose 服务，也不新增数据库服务或 Fleet 数据库卷。使用 Docker Engine SDK，数据库复用现有 pgx/sqlc；不引入 SQLite 驱动或通用云 Provider 框架。

接入现有 Cloud HTTP 客户端、认证中间件、节点 handler 和 TaskService。前端逻辑归 core，共享界面归 views，平台接线归 Web/Desktop。环境脚本接入可选 Fleet 组件。

## 4. 本地配置与能力

API 新增 `MULTICA_LOCAL_FLEET_URL` 和 `MULTICA_LOCAL_FLEET_SECRET_FILE`，只用于节点管理、节点认证和节点协调。

1. 未配置时保持原有自托管/SaaS 行为。
2. 本地 URL 与 `MULTICA_CLOUD_URL` 同时非空，启动报错，不自动择一或回退。
3. 本地 URL 缺少服务密钥、格式错误或文件不可读，启动失败。
4. 本地模式不启用 Billing、entitlement、seat capacity；不提供假计费响应，不让 Billing handler 使用本地 Fleet 客户端。
5. 本地节点验证不使用正向缓存，以免凭证撤销延迟；不修改 SaaS 原有缓存规则。既有 WebSocket 的领取也要经过独立屏障检查。

Fleet 配置包括当前 checkout 命名空间、Docker socket、固定镜像、容器可达的 API 地址、现有数据库连接、资源白名单及用户 UUID 到 Claude 凭证 profile 的映射。数据库沿用当前环境的 `DATABASE_URL`，Fleet 使用独立、有界连接池；Fleet 只接入经验证的既有共享 PostgreSQL Docker 网络，以内部 alias 和内部端口连接；保留当前 worktree 的 database 名、凭证和全部连接选项，仅替换 Fleet 专用 URL 的 host/port。API 的宿主机 URL 不变。Linux 的 loopback 发布端口不能通过 gateway 替换访问；无可信共享网络证据时拒绝启动，不建第二实例、不公开 PG。只读取显式私密配置，不读取用户 home。模板无真实密钥，配置不进入 Git。

默认一个本地规格：2 CPU、4 GiB 内存、256 个进程，每节点同时 1 次运行，每个配置用户最多 2 个节点；管理员可调整，客户端只能选白名单。Docker named volume 不承诺硬磁盘容量配额，界面不提供 EC2 的磁盘大小参数。

通过现有 `GET /api/cloud-runtime/` 代理发现能力：`provider: docker`、允许操作、规格、持久存储、磁盘配额不支持。core 用 zod/parseWithFallback 解析；Web/Desktop 使用同一能力接线，不能只靠 Web 环境变量启用本地入口。未知能力默认不开放危险操作；原 Cloud 的旧响应保持兼容。

## 5. API 契约

Fleet 所有业务及内部接口要求服务密钥；认证成功后才信任 API 写入的 `X-User-ID`。节点查询和每次操作仍检查 Store 中的所有者，不能只检查实例是否存在。

| 方法 | Fleet 路径 | 行为 |
| --- | --- | --- |
| GET | `/api/v1/` | 服务和能力，不返回私密路径或密钥 |
| GET | `/api/v1/nodes` | 所有者过滤，沿用分页参数 |
| POST | `/api/v1/nodes` | 接受创建意图，返回节点快照、操作 ID |
| DELETE | `/api/v1/nodes` | 接受 `instance_id`，批准后不可恢复删除 |
| POST | `/api/v1/nodes/start` | 启动原容器 |
| POST | `/api/v1/nodes/stop` | 批准后停止、保留卷 |
| POST | `/api/v1/nodes/reboot` | 批准后重启、保留卷 |
| POST | `/api/v1/nodes/status` | 实际状态、就绪诊断 |
| POST | `/api/v1/pat/verify` | 沿用 `valid`、`owner_id`、`instance_id`、`instance_record_id` |
| GET | `/healthz` | 进程存活 |
| GET | `/readyz` | Store 可用且 Docker 可访问 |

Health/ready 接口不返回敏感详情，可用于本地探针。`/api/v1/nodes/exec` 明确未支持，能力不声明它。固定健康检查不是任意命令执行。

创建只接受名称、白名单规格。拒绝客户端指定 EC2 区域/AMI/IAM/subnet/SSH key、任意镜像、tags、挂载路径或脚本，不偷偷把 EC2 规格映射成 Docker。

响应保留现有核心字段。稳定 `id` 为 Fleet 节点 UUID，`instance_id` 为 Docker container ID，region 为 `local`，image 字段为批准镜像，非适用字符串为空。provider、操作进度、就绪诊断和错误码作为可选字段加入；不新增 Runtime mode 枚举。

客户端按用户意图生成幂等键，网络重试复用该键。Store 将键、所有者、请求指纹和结果原子记录；同键不同请求返回 409。异步接受意图与操作完成必须区分，不能将已接受显示为已就绪。

错误明确区分无权限、未配置、规格禁止、凭证 profile 缺失、节点忙碌、删除前仍有排队运行、操作冲突、Docker 不可达、初始化失败和状态不确定；HTTP 401/403/409/503 与语义匹配。远端原始错误脱敏后才显示。

节点归用户所有，不是任意工作区 ID 可以授权的资源。Daemon 沿现有规则同步该所有者有权访问的工作区；本地模式不新增节点单工作区绑定语义。

## 6. Store、身份与私密配置

现有 PostgreSQL 数据库是 API 与 Fleet 共同的事实来源。本版仍运行单 Fleet 实例，独立 pgx 连接池使用有界查询、锁等待和事务超时；连接池总预算计入现有后端容量。不复用任务队列表保存基础设施对象。

拟新增专用表：

- `fleet_nodes`：UUID、owner、container ID、固定 Daemon UUID、规格、命名空间、目标/观察状态、代次、时间和脱敏错误。
- `fleet_node_operations`：UUID、node、类型、幂等键、请求指纹、阶段、是否已批准维护、重试数和错误。
- `fleet_node_credentials`：Token 哈希、node、撤销时间；无明文。
- `fleet_credential_profiles`：所有者到 profile 的引用和配置版本，不存模型密钥。

所有读写限定 Fleet 命名空间；用户资源查询额外限定 owner，关联运行时和任务时保留工作区权限校验。无外键、级联删除或更新；清理由应用事务完成。

迁移进入现有 `server/migrations/`，查询进入 `server/pkg/db/queries/`，使用 sqlc 生成共享查询代码。现有环境启动流程先迁移，再启动 API/Fleet；Fleet 不启动另一套迁移，不建立数据库。Fleet 模式下缺表/版本不满足时明确不就绪，不回退到文件存储。

新增索引使用 `CREATE [UNIQUE] INDEX CONCURRENTLY`，每个独立单语句迁移、事务外运行；唯一索引和主键约束也按仓库规则拆分，不以表内约束绕过索引创建规则。条件迁移和幂等 DDL 沿用既有规则；SQL 变更后运行 `make sqlc`。

数据库去重范围包括节点身份、凭证哈希及命名空间/owner/幂等键。节点限额检查和创建意图在同一事务内，通过所有者级事务锁避免并发超额。

每节点有独立 data volume 和 secrets volume。data 保存 CLI 配置、固定 Daemon ID、Claude 会话/配置和运行工作目录；secrets 由固定 bootstrap 写入，节点只读挂载。权限限制到节点非 root UID。

模型密钥不进入镜像层、Docker Config.Env、命令参数、浏览器响应、PostgreSQL 或日志。入口程序读取私密文件后，仅在进程内提供 Claude 所需环境。实际模型进程能访问自己的密钥，不能宣称智能体与凭证强隔离。

节点生成的 `mcn_` 明文可暂存私密 bootstrap 卷以支持崩溃恢复，初始化后专属 Multica CLI 配置持有节点 Token；Fleet 数据库只存哈希。若崩溃发生在哈希提交后、私密文件写入前，恢复时撤销缺失文件对应凭证并以新凭证代次重新初始化，不能从哈希推导明文。删除移除两类卷；这是逻辑删除，不保证物理磁盘密码学擦除。

容器/卷标签同时包括 checkout 命名空间、Fleet 标识、node UUID 和资源角色。清理须同时匹配 Store 归属与标签，不按名称前缀删除。

## 7. 创建到运行

1. API 验证用户，Fleet 检查 profile、规格与数量限制，原子保存节点/操作，返回 `launching`。
2. Worker 使用已构建批准镜像；镜像不存在明确失败，不在用户创建请求中任意拉取/安装。
3. 创建带标签的卷和容器，写入 bootstrap 与 Claude 配置。节点以固定非 root 用户执行，正确设置卷权限。
4. 写入现有 CLI 格式的 Multica 配置、固定 Daemon ID 和容器可达 API 地址；不依赖 `MULTICA_TOKEN` 启动认证。
5. 执行现有 `multica daemon start --foreground`，正确转发 SIGTERM。关闭自更新/自重载，由镜像管理二进制版本。
6. Daemon 沿原协议认证、注册并上报 Claude Runtime。API 从可信节点身份写入 `managed_by: local_fleet`、`fleet_node_id` 元数据，不信任客户端自报所有者或受管标记。
7. Docker healthcheck 使用固定工具读取容器 loopback Daemon health；Fleet 通过 Docker 健康结果观察，不发布管理端口。
8. 完成 preflight、上报 Claude、存在可绑定工作区 Runtime 后，才发布可用。CLI 安装、模型配置存在和远端凭证有效性分别描述；默认不调用模型检查密钥。
9. 用户自行选择/创建智能体并绑定 Runtime。聊天/任务触发后，既有队列、Daemon 与 Claude 执行，结果沿原 HTTP/WebSocket 回传。

保持现有 Runtime `local` 模式和执行协议，管理元数据正交；没有 Omp Runtime，没有第二套队列。Runtime 元数据更新必须保留服务器写入的受管标识，避免 heartbeat/register 覆盖。

## 8. 生命周期和领取屏障

目标状态、容器观察状态、Daemon 就绪状态与操作阶段分开持久化。主显示状态：`launching`、`starting`、`running`、`stopping`、`stopped`、`rebooting`、`terminating`、`terminated`、`failed`。健康丢失不得继续显示可执行，错误和操作信息保留用于恢复。

- stop：保留容器、卷、身份、会话。
- start：启动原容器，Daemon 就绪后恢复领取。
- reboot：重启原容器，保留持久内容。
- delete：撤销凭证、删除容器和专属卷，最后标记 terminated；中途失败沿同一操作恢复。

`dispatched`、准备中、运行中、资源等待和待上报结果均算忙碌。停止允许保留排队运行，启动后继续；删除前必须取消关联排队/延后运行。第一版无 force 操作。

### 屏障机制

单次 health 检查存在检查后领取的竞态。`fleet_nodes` 的目标状态与 `fleet_node_operations` 的维护记录是持久化屏障。API/Fleet 使用命名空间与稳定 node UUID 派生的同一 PostgreSQL 事务级 advisory lock；状态与业务任务在同库协调，不再通过跨服务 HTTP 查询领取许可。相关状态写入遵守同一锁顺序。

1. 全部本地节点领取入口，包括 WS、HTTP、批量及旧单 Runtime，在实际领取事务中取得共享节点锁，直接读 Fleet 专用表中的目标状态、就绪、凭证撤销和未完成操作。锁持有到领取提交/回滚；批量节点锁按 UUID 排序。不能只锁 handler 预检后释放，也不能使用缓存绕过屏障。
2. stop/reboot/delete 的 API 协调取得同一节点排他锁，在一个短事务内持久化准备态和 operation ID，检查跨工作区 Runtime 的已领取/活跃任务；忙碌则不提交维护状态并返回 409。删除同时检查无排队/延后运行。
3. 提交准备态后，在事务和数据库锁之外通过 Fleet 的固定诊断接口检查待回传结果。新领取已被持久化屏障阻止。诊断明确忙碌时，通过短事务撤销准备态并返回 409；容器仍运行且诊断不可读/健康未知时，保留屏障、记录未批准错误并拒绝操作，等待显式恢复，绝不自动授权。
4. 健康检查通过后，开启第二个短事务、获取排他节点锁，再核验操作 ID/代次、屏障、忙碌状态和删除准入；原子批准 operation ID。Worker 只能执行已批准操作。
5. API 崩溃留下未批准准备态时，禁止新领取；协调器以同一 operation ID 请求 API 重新授权检查，不凭超时执行或解锁。
6. 已批准但 Docker 结果不确定，观察实际状态并重复同一操作。状态与操作完成记录在事务内更新；恢复到运行且 Daemon 就绪后解除屏障，删除屏障永久保留。
7. 节点注册和新运行准入也使用共享节点锁并读取同库状态，不能通过新工作区 Runtime 或并发入队绕过维护/删除；initializing 可注册但未就绪不能领取。删除准备态拒绝新入队；停止允许正常排队。

领取要求期望 running、凭证未撤销、健康/就绪成立且没有维护屏障；节点健康默认在 30 秒后过期，并叠加现有 Runtime 心跳新鲜度检查，不能用旧 running 快照长期放行。缺失节点、数据库错误或不确定状态时拒绝本地领取；普通非 Fleet Runtime 不依赖 Fleet HTTP 服务。API 从已验证 Token 和可信 Runtime 元数据确定 node UUID，不接受客户端任意绑定。

Fleet 内部保留固定节点诊断接口；API 提供带服务认证的操作重审入口，以恢复未批准意图。prepare/approve/abort 由共享数据库事务完成，不新增 HTTP 领取许可接口。数据库事务内不调用 Docker/Fleet HTTP，以免持锁等待外部服务。

维护准备期间正常结果回调继续允许，以便送达最终结果；删除确认无待回传后才撤销 Token。已建立 WS 同样每次检查许可，不能利用旧认证继续领取。不向 Daemon 增加新的 pause/任务协议。

### 删除后的业务实体

不删除或自动归档用户的智能体/任务。关联 Runtime 标为不可用并保留节点 tombstone，后端拒绝向删除节点的新运行准入；用户可重绑其他 Runtime。普通 Runtime 删除入口不能绕过 Fleet，旧客户端也必须由后端拦截。

## 9. 网络、安全与边界

- Fleet 宿主机端口仅绑定 loopback。它访问 Docker socket，是高权限可信控制面；不可公开给不可信租户。
- 节点不挂载 socket、不 privileged、不用 host network/PID；使用命名空间专属 Docker bridge，无业务端口映射。
- Docker Desktop 配置 `host.docker.internal`；Linux 使用 host-gateway 映射。API 端口从当前 checkout 环境读取，不硬编码、不复制主 checkout 配置进 worktree。
- API 必须确实能从容器网关访问。启动检测失败给出监听/路由诊断，不自动扩大公网监听、不关闭防火墙、不启动替代服务。
- 所有者和服务认证覆盖所有节点操作；客户端不能指定任意镜像、脚本、规格和宿主机挂载。
- Claude profile 按 owner 映射；不读取宿主机 OAuth/home，不暴露其他用户私密路径。
- 服务密钥和节点 Token 不传给模型；任务中的既有 `mat_` 行为保持。
- 现有 `mcn_` 主要映射所有者权限，本版不宣称单工作区精细授权或生产级多租户隔离。
- 宿主机本地目录不自动成为容器工作目录。仓库在节点卷内，沿用现有远端 checkout；本地目录挂载是独立后续设计。

## 10. 故障恢复

每次操作分阶段持久化，Docker 调用有界，PostgreSQL 事务不等待外部网络。浏览器断开不取消已接受意图，Worker 使用自己的有界上下文。默认数据库查询/锁等待超时 2 秒、固定诊断超时 5 秒、普通 Docker 操作超时 30 秒、初始化窗口 5 分钟、协调周期 5 秒；有界重试最多 5 次。状态仍不确定时记录失败并保留屏障，显式恢复继续同一操作，不将超时当作授权。

- 创建结果超时先检查固定标签和资源身份，不重复建容器。
- Fleet 启动及协调周期比较 Store 与 Docker，恢复操作/屏障，不改变 Node/Daemon/container identity。
- Docker 不可达时 Fleet 不就绪，协调器在数据库中标记状态未确认，受管领取失败关闭；不猜测删除成功或批量撤销凭证。Daemon 既有心跳新鲜度和节点健康记录共同约束领取，不能只相信历史 running 状态。
- 数据库不可达时 API/Fleet 不猜测节点操作成功，不启动新的 Docker 生命周期操作；已发出的调用可能已完成，恢复连接后按原 operation ID 检查对账。Fleet HTTP 不可达本身不影响已建立认证的 WS 领取；同库节点状态和健康仍有效时，该路径可继续执行。新认证和依赖 Fleet HTTP 的诊断/管理可能失败，不保证所有节点请求都可用。
- API 不可达时 initializing 有界退避，超出初始化窗口记录失败，不无限隐藏错误。
- 创建失败撤销该节点 Token，只清理本次操作新建且归属明确的资源；不误删已有卷。
- 节点默认禁用 Docker 自动 restart policy，避免显式停止被自动启动覆盖；意外停止由用户显式启动恢复。
- 外部删除容器显示实例丢失，不用新容器冒充原节点；保留数据诊断并允许受控删除清理，不自动重建。
- 有界退避，永久配置错误停止重试并可见；恢复重用原 operation ID。删除需等资源实际清理后才标记完成。

## 11. 开发入口与共享界面

新增可选 `fleet` 环境组件，计划支持 `make up C=api,web,fleet`。节点由 Fleet 动态管理，镜像构建/服务启动纳入现有环境脚本，不另建 PostgreSQL。

- `make status` 报告本 checkout 的 Fleet/节点、实际端口和就绪状态，无凭证输出。
- `make down` 先建立屏障，确认无活跃运行和待回传，再停止节点、Fleet、API/Web；忙碌/状态未知不强制停机，返回恢复说明。保留卷和数据库。
- `make destroy` 对 Fleet 数据单独明确确认，先清理经标签和 Store 双重校验的自有 Docker 资源，再通过应用事务清理当前命名空间 Fleet 表数据，并遵循原 checkout 数据库销毁流程。Docker 清理未完成时保留恢复记录，不先删数据库。不得全局 prune，不删除其他 checkout 数据或共享 PostgreSQL 实例。
- 私密配置模板提供 owner/profile/API 地址字段而非密钥，不要求聊天发送或命令参数公开密钥。

Web/Desktop 复用节点界面，能力发现后显示 Docker 规格；新增启停、重启、失败原因及不可恢复删除确认。受管标识和普通 Runtime 删除保护在前后端都生效。

TanStack Query 管服务器数据，新增响应过 zod/parseWithFallback；未知能力/状态安全降级。使用现有显示名 helper 和全部支持的 Web/Desktop 翻译，不重复实现导航，不新增 views store。Mobile 不在验收范围。

## 12. 测试与验收

### 默认测试：无真实模型、无 Docker 要求

- Store：使用现有 `server/internal/testutil` 数据库 fixture，覆盖事务、迁移、索引约束、幂等、并发限额、同键冲突、恢复、撤销、用户/命名空间隔离。无需 Docker/模型；数据库测试沿用仓库现有 PostgreSQL 测试环境。
- 节点服务：fake Provider 的创建/启停/删除/超时/归属校验/失败清理。
- API：配置互斥、Billing/配额/席位不被本地启用、服务认证、伪造 owner、节点 Token 与受管标识。
- 领取屏障：并发领取/停止，WS/HTTP/批量/旧入口，跨工作区，API 准备/批准边界崩溃，待回传，未知健康，删除后准入。
- 现有 Claude backend：测试创建的 fake CLI，不发现用户安装的真实 Claude，不读取账户。
- core：能力/节点/错误的缺失和畸形响应、mutation 状态及缓存。
- views：接线、危险确认、受管保护、错误恢复、安全降级。

### 显式 Docker 集成测试

单独 opt-in，用真实 Docker Engine 和 fake Claude 节点镜像，不用模型账户。完整验证前端/API 创建、Daemon 注册、绑定、提交、领取、执行、消息、完成结果和 Query 更新；覆盖持久身份/会话、维护竞态、Fleet/API 重启及跨命名空间资源保护。普通 `make test` 不要求 Docker socket。只操作本次测试标签资源，结束收集日志并清理自身资源。

### 真实 Claude 烟测

仅另获明确授权后执行，遵守 `agentintegration` 和 `MULTICA_RUN_REAL_AGENT_SMOKE=1` 闸门及特定测试入口；授权前不查找真实 CLI、不读取账户。设计批准不是使用模型账户/额度的授权。

### 第一版验收

1. 本地环境可添加 Fleet，不影响普通 Runtime 和 SaaS。
2. Claude Runtime 可见、可绑定，没有 Omp 或自动新建业务智能体。
3. fake Claude 完整运行链路成功，日志与结果展示正确。
4. 启停和重启保持原 Daemon/Runtime 身份与持久内容。
5. 忙碌及结果未回传时拒绝危险操作，并发领取不能穿透屏障。
6. 删除撤销凭证、清理专属资源，不删除业务实体和其他 checkout。
7. 操作中 Fleet/API 重启安全恢复，不重复创建或猜测解除屏障。
8. 模型密钥不出现在镜像、Docker Config.Env、浏览器响应、日志、PostgreSQL 或 Git。
9. Docker Desktop/Linux 网关配置可诊断，Web/Desktop 共用能力与保护。
10. 默认测试无真实模型调用，集成和真实烟测分别显式授权。
11. Fleet 与 API 使用当前 checkout 同一个 PostgreSQL 数据库，迁移复用现有 runner/sqlc，无 SQLite、第二套数据库或独立数据库卷。

## 13. 审查修订后的安全契约

以下细化上述章节，不增加 AWS 部署范围。

### 13.1 持久回传队列的可信统计

现有 Daemon health 扫描失败只记日志，HTTP 200 且旧计数默认零，不能据此批准维护。新增非可选对象 `report_queue_stats:{known:boolean,pending:number,failed:number}`；保留既有 HTTP liveness、status、旧 active/report counters 的含义和当前 namespace 范围。新增统计扫描 WorkspacesRoot 下 `.pending-terminal-reports/v1` 的所有 durable namespace 及各 `failed/`，复用现有 directory scanner 的缺目录、普通文件、stat/error 规则，不只扫当前身份。扫描/布局不可读时 known=false；旧响应缺字段、负数/非整数/畸形字段均为 unknown，绝不解释为零。未知仅阻止危险维护，不改变普通领取 policy。

### 13.2 离线删除证明及唯一数据布局

固定 data mount=/data、HOME=/data/home、WorkspacesRoot=/data/workspaces、layout version=1；bootstrap 在自有 data volume 写入非私密 layout manifest（namespace/FleetID/node/DaemonID/layout version）。在线 run 显式传 `--workspaces-root /data/workspaces` 并校验 manifest；在线 health 和离线扫描使用同一布局校验及 scanner。离线 mount 在容器 inspection 中必须为 SQL 记录的 DataVolume、同一 /data 路径及完整所有权标签；manifest 不存在、不匹配、symlink 越界、错误挂载/根目录均 unknown，不允许“空目录即空队列”。只有验证布局后，scanner 的 report 子目录缺失规则才可表示已知零。

stopped/missing + SQL idle 本身不够。删除准备态阻止 start/claim/register/enqueue 及受管身份迁移；Provider 在事务外重新 inspect 无写入节点，再创建短命诊断 helper：已批准并固定 digest 的节点镜像、entrypoint/argv 只为 `["/usr/local/bin/fleet-node","report-stats"]`，自有 data volume 只读、无 secrets 卷、无 socket/host 挂载、无网络（NetworkMode=none）、无 Daemon/Claude 启动、非 root UID/GID 10001、只读 rootfs、drop ALL capabilities、no-new-privileges、0.25 CPU/64 MiB/16 PIDs、5 秒超时、输出上限 64 KiB。helper 带 namespace/FleetID/node/role=diagnostic 标签，清理只移除自身 helper，不改 data。入口无 caller path/argv/profile 参数。

证明绑定 namespace/node/operation/generation/DataVolume/layout、当前检查时间及容器观察；审批第二短事务重核 SQL 和同一 operation CAS，Worker 删除前再校验容器未重新运行、卷身份和报告证明仍匹配。running 使用本 start epoch 的在线健康；离线证明不伪造 Ready 或 start epoch。unknown/任何 pending 或 failed 非零拒绝删除并保留数据；不可读保留未批准屏障。外部管理员仍可绕开 Fleet，不能宣称抵御 Docker 管理员。

### 13.3 受管 Runtime 的所有变更路径

普通注册/删除屏障不足：legacy daemon merge 会重新分配 task/agent 并删除 source Runtime。Task 6 在 merge 同一事务拒绝 source 或 target 任一受管标识（包括丢失 node 的受管 metadata），先按 UUID 排序节点锁，再既有 workspace/runtime/agent/task 行锁，锁后重读两端 metadata；禁止 helper 外一次性检查。ordinary→ordinary 保留原 fence。Task 7 保护 ordinary runtime delete/unbind-delete 入口，不删除业务智能体/任务。

### 13.4 固定幂等 Header 与内部恢复接口

浏览器为 create/start/stop/reboot/delete 每个意图生成一个 `Idempotency-Key`，重试全跳复用；API cloud_runtime proxy 只 allowlist 此 Header 及既有必要内容，不能批量转发 caller headers。可信 client 最后权威写入服务密钥与认证 owner，caller 不能覆盖 X-User-ID/X-Fleet-Service-Key；保持 SaaS Stripe passthrough 等既有非身份 Header 语义。

Fleet `POST /internal/v1/nodes/diagnose` 和 API `POST /internal/local-fleet/operations/review` 的同一 typed request：namespace/node_id/operation_id/generation/action。两端要求 X-Fleet-Service-Key + API/Fleet 权威设置的 X-User-ID，并在 SQL 匹配 namespace、owner、node、operation、generation、action；未批准操作仅能重审自身。诊断响应包含 known/counts、active_runs、start_epoch、当前 observation（时间/容器状态/身份）、offline/DataVolume/layout，不含私密路径、Token 或 key。Task 4 先提供 DTO/Provider seam，Task 7 产出重审实现和 router/client wiring；Task 10 消费同一 operation 两段检查，禁止超时自动批准。所有 HTTP/Docker I/O 在事务外，审批/撤销以同 operation/generation CAS 更新。

### 13.5 共享 PG 与前置环境

基线 Compose 的 PG 只发布 127.0.0.1:5432；Linux 不能替换为 host-gateway。Fleet 专用 DB URL 使用已验证现有共享 PG Docker 网络、内部 alias=postgres、内部 port=5432，并验证网络/容器与 managed environment 数据库来源一致；不硬猜 network 名、不启动第二 PG、不扩大 PG 发布地址。Fleet 加入共享 PG 网络及节点控制网络，动态节点只加入节点网络，不传 DB 凭证。动态节点 HostConfig.ExtraHosts 显式包含 host.docker.internal:host-gateway（Linux），用于 API 动态 worktree 端口；Compose extra_hosts 不会继承到 Engine 创建节点。

先验证隔离 worktree、registry/manifest 归属、生成环境文件及当前 DB 名/端口、依赖工具，再 env-exec。Task 1 直接在 server 下测试，无 DB。DB 缺失/registry 未登记/依赖未安装属于 setup gate，不能记作 TDD red。共享 DB/registry/Docker/registry 查询建立等待安全授权；本次不调用 make up 或 env-exec。

### 13.6 迁移与 main 边界

当前 origin main=ef253c73884ac9de2863075b195766b8abe45d80，迁移最高 563。Task 2 保持 564–575 顺序（执行前重查冲突），所有 ID/关系 ID/namespace/owner 必需字段显式 NOT NULL；无新 FK/内联 PK/UNIQUE 隐式索引。每个并发 index 迁移同时登记 cmd/migrate 的 concurrentIndexCleanups 和适用 concurrentDownIndexCleanups，测试既有 up/down map 覆盖与 INVALID index 重试。Aurora 已有 managed sandbox/auth/fleet 包不替代此本地 Claude-only Fleet；不改 Aurora launch/billing 或 AWS 部署，共享 Next.js 逻辑仍归 packages/nextjs。

## 14. 后续步骤

本文描述已授权实施的目标能力，不是已实现功能。保持现有 15 项实施计划，先实现 fake Provider、Store、配置分离和屏障，再接 Docker 镜像与共享界面。SQL 查询变更须重新生成 sqlc。

实现时同步更新开发环境与仓库约束说明，遵守数据保护和包边界；本次准备不修改这些工具，不启动 Docker、不读取真实私密配置、不执行 Claude；后续开发授权不等于环境建立、Docker 或模型账户执行授权。
