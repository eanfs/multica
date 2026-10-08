# Aurora 执行层改造为「13 Agent + 13 Skill」实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 Aurora 的 13 个 skill 通过 multica 既有的「agent 加载 skill」机制执行 —— 每个 skill 一个预置 system agent、一份可执行的 skill 文档,模型用普通工具(Bash / Read / Write)按文档完成任务;删掉 Aurora 专有的 MCP broker 执行面、`deploy/aurora-sandbox/` 全部内容与全部自定义隔离策略,节点密钥**复用既有的 Anthropic 凭据通道**(ARK 的 key 兼容 Anthropic 协议,直接作 `ANTHROPIC_API_KEY` + `ANTHROPIC_BASE_URL` 使用,不新增密钥变量)。

**Architecture:** Aurora 不再拥有独立执行通道。`daemon.isAuroraTask` 分支被删除后,Aurora 任务与普通 issue 任务逐字节同路径:入队写入 `agent_task_queue` 并绑定 agent 的 `runtime_id` → 节点领取 → daemon 把该 agent 启用的 skill 物化到 `<workdir>/.claude/skills/<slug>/SKILL.md` → `BuildPrompt` → 拉起 Claude(`bypassPermissions`,无工具白名单,无 MaxTurns)→ 产物落盘 → 回传结算。13 个 system agent、13 个 skill 行、13 条 `agent_skill` 关联**今天已经存在**(`aurora.EnsureSystemAgents`),本计划改的是它们的内容与执行面,不是重新搭建。节点镜像是 `docker/runtime/Dockerfile` 的单一产物,内含 daemon(`multica` + `fleet-node`)与 Claude。**计费不在本计划范围内** —— 见「用户已确认的决策」第 8 条。

**Tech Stack:** Go 1.26(Chi、pgx/v5、sqlc、Docker Engine SDK)、PostgreSQL 17、Node 22、Claude Code CLI、TanStack Query + zod + Vitest、Playwright、Docker Desktop。

**Spec：** 本计划**取代** [统一 Cloud Runtime 与 Aurora skills 执行层设计](../specs/2026-10-06-unified-cloud-runtime-aurora-design.md)(r2)中关于隔离、单镜像多入口、broker 桥接与 G0–G5 门的部分。规格的书面内容保留为历史记录,不再是执行依据。审查记录 [2026-10-07-unified-cloud-runtime-aurora-review.md](../specs/2026-10-07-unified-cloud-runtime-aurora-review.md) 中 R1、R2、R6、R9 四条针对进程隔离与受控 fixture 的发现**在本方向下不再适用**(见「行为变化与已知代价」)。

**被取代的计划：** 已关闭的 [PR #193](https://github.com/eanfs/multica/pull/193) 所载的统一 Cloud Runtime 执行计划(其文件只存在于分支 `docs/aurora-unified-runtime-plan`,不并入 main)、[Aurora Cloud Runtime 对接实施计划](2026-10-06-aurora-cloud-runtime-integration.md) 的 Task 2/5/6、[沙箱镜像与烟测](2026-09-25-aurora-sandbox-image-smoke.md)、[fleet 隔离与 egress](2026-09-25-aurora-sandbox-fleet-isolation.md)、[沙箱 skill runtime](2026-09-25-aurora-sandbox-skill-runtime.md)。这些计划的已交付代码在 Task 4–6 中被删除,其完成状态**不**等于本计划已实施。

---

## 用户已确认的决策(2026-10-08)

1. **13 个 skill = 13 个预置 agent + 13 个 skill**,不再使用 MCP tool。
2. **完全复用 issue 的干活流程** —— Aurora 任务就是普通 agent 任务。
3. **`deploy/aurora-sandbox/` 完全删除**,包括 `seccomp.json` 与 `vendor/`。
4. **不使用 AppArmor**,自定义 seccomp 也一起去掉,容器按 Docker 默认安全设置执行;安全问题本阶段忽略。
5. **provider 密钥复用既有的 `ANTHROPIC_*` 通道**,部署时解决,**不经服务端,也不新增密钥变量**(2026-10-08 订正):ARK 的 key 与 Anthropic Messages 协议兼容,直接作为 `ANTHROPIC_API_KEY` 使用、`ANTHROPIC_BASE_URL` 指向 Ark Agent Plan 端点(`https://ark.cn-beijing.volces.com/api/plan`)。skill 文档因此用 `$ANTHROPIC_API_KEY` 与 `${ANTHROPIC_BASE_URL}/v3/images/generations`,不引入 `ARK_API_KEY`。火山语音 ASR 不兼容该协议,它的凭据仍是一个独立变量(Task 3)。
6. **不做**「skill 脚本调 Multica 服务端、由服务端持密钥调 provider」这种代理。
7. **omp 暂不安装**(2026-10-08),统一镜像先只保留 Claude。
8. **credits 计费本次不处理。** 本计划照现状保留 `Credit.Reserve` / `settleAurora*`,但**不把它当作验证的门**:部署时遇到计费问题先记录、先绕过,继续验证执行链路。后续**另立计划**,把计费改为**用 multica web 既有的 token 用量统计来算 credits**(`task_usage` 表与 `POST /api/daemon/tasks/{taskId}/usage` 已经在收 input/output/cache token 与 `CostUsdTicks`),不再按 skill 固定积分数预留。
9. **最终验收口径(2026-10-08):在 `apps/web` 里能同时看到三样东西,才算两边完全打通** —— ① 由 Aurora 创建的 13 个 agent;② 每个 agent 下对应的 skill;③ 从 Aurora 下发的所有任务。三条任一不成立,不算打通。
10. **删掉 `OPENAI_API_KEY`,只保留火山引擎的接口与 `byted-ark-seedream-skill`。** 图片一路全部走 Seedream(`poster`、`xhs-image`、`text-image`、`product-image`、`image-edit`);OpenAI 的密钥、目标挂载、出口允许列表条目与 `openai-images` 路由一并清掉。这与 PR #194 已做的路由迁移方向一致。

---

## Global Constraints

- **先抽取,后删除。** 13 份 skill 文档的新内容必须在删除 `deploy/aurora-sandbox/` **之前**从既有实现中抽出(见 Task 1 与「删除清单」小节)。删除动作是 Task 5,排在 Task 1 之后,顺序不可调换。
- **不新增第二套执行机制。** 不引入新的队列、新的 agent 类型、新的领取通道或第二个 daemon。Aurora 继续使用 `agent_task_queue` 与 `agent_runtime`。
- **不改公开 API 的兼容边界。** `GET /api/aurora/runtime` 的既有字段保留;`@api/aurora/generations` 的 503 `aurora_runtime_unavailable` 语义保留(未配置运行时仍 fail closed,不排队不预留积分)。
- **13 个 skill 必须有可执行文档。** 每个 skill 的 `.md` 必须包含:输入、真实步骤(具体命令或 HTTP 端点/认证/请求体/轮询)、产物写入路径、manifest 写入、失败处理。禁止写成"调用某个工具"。
- **产物 manifest 契约保留。** 模型负责写 `<outputRoot>/.multica/aurora-artifacts.v1.json`。daemon 只把它当期望路径清单,自行重算 size / SHA-256 / MIME,并拒绝越界路径、非单链接普通文件与超出上限的条目(`server/internal/daemon/aurora_manifest.go`)。
- **计费诚实标注。** `aurora_provider_run` 的 create-once 与 `ambiguous` 冻结在模型直连 provider 后**只剩记账作用**。代码保留,文档与 `AGENTS.md` 必须写明"不再有强制力",不得再把它当成防重复扣费的保证。
- **密钥的落点与风险必须写在部署文档里。** 容器 env 对容器内任何进程可读,`docker inspect` 也可读。Fleet 配置文件含密钥时权限 0600 且不入库;不得写入镜像、SQL、日志或 Git。
- **迁移与 SQL 规则沿用仓库既有约定。** 无外键、无级联;每个迁移创建的索引单独一个 `CREATE [UNIQUE] INDEX CONCURRENTLY` 单语句文件并登记进 `server/cmd/migrate/main.go`;条件 DDL 用 `IF EXISTS`/`IF NOT EXISTS`;SQL 改完 `make sqlc`。**编号在历史上不唯一**(同一数字可属两条无关系列),取最高号必须按数字排序。当前最高号为 `584`。
- **默认测试不依赖真实 Docker、真实模型或真实供应商账户。** Docker 链路用 `dockerintegration` + `MULTICA_RUN_DOCKER_INTEGRATION=1`;真实 CLI 用 `agentintegration` + `MULTICA_RUN_REAL_AGENT_SMOKE=1`;两者分别显式授权,**未授权记 SKIP,绝不记 PASS**。默认测试不得解析或执行用户安装的 agent CLI(`scripts/agent-cli-command-names.txt` 的 PATH 哨兵会捕获;`omp` 已在名单内)。
- **包边界。** `packages/core` 无 UI/localStorage/`process.env`;`packages/views` 无 store 定义、无 `next/*`/`react-router-dom`;平台接线留在 `apps/aurora`。新增响应过 zod + `parseWithFallback`。
- **五语文案齐备**(en / zh-Hans / fr / ja / ko),否则 `packages/views/locales/parity.test.ts` 红。中文称「运行时」,`skill` 保持英文。
- **代码注释用英文。** 不新增内部兼容 shim 或旧字段回退链(API 响应边界除外)。
- **每个任务一个原子 conventional commit**,不用 `git add .`。缺环境不是有效的 red。

---

## 删除清单与抽取顺序

### 必须先在 Task 1 抽取,之后才能删

| 抽取源 | 抽出什么 | 去向 |
| --- | --- | --- |
| `deploy/aurora-sandbox/runtime/src/tools/*.mjs`(9 个) | 每个本地步骤的真实命令与参数:ImageMagick argv、转义 HTML + 无头 Chromium 打印、`pdftotext`/DOCX 抽取、FFmpeg 抽取与字幕合成、文本产物写入 | 对应 skill 文档的 Steps |
| `deploy/aurora-sandbox/runtime/src/policy.mjs` | 每个 skill 允许的 provider / model / origin / 输入输出规则 | 对应 skill 文档的 Inputs 与 Required outputs |
| `deploy/aurora-sandbox/runtime/src/provider-run.mjs`、`importer.mjs` | 计费 create 的记账调用与结果导入调用的端点与顺序 | 对应 skill 文档的 Steps 与 `aurora_provider_run` 记账步骤 |
| `deploy/aurora-sandbox/runtime/src/manifest.mjs` | manifest 的字段、上限与写入位置 | 每个 skill 文档的"写 manifest"步骤 |
| `deploy/aurora-sandbox/vendor/volcengine/*/SKILL.md`、`references/` | ARK Seedream / Seedance 的上游真实用法(端点、认证头、请求体、异步轮询) | `poster`、`xhs-image`、`text-image`、`image-video`、`text-video` 的 Steps |
| `deploy/aurora-sandbox/runtime/test/*.test.mjs`(15 个) | 每个 skill 的输入约束与失败模式断言 | 新 skill 文档的 Failure behavior;回归测试迁到 Vitest 或 Go 侧 |

### 抽取完成后整目录删除

`Dockerfile`、`Dockerfile.egress`、`docker-bake.hcl`、`versions.json`、`apt-packages.lock`、`.dockerignore`、`README.md`、`multica-aurora-sandbox.apparmor`、`seccomp.json`、`docker-security-test.sh`、`fleet-sandbox-acceptance.sh`、`docker-smoke.sh`、`fixture/`、`fixtures/`、`runtime/`、`vendor/`。

目录外必须一并处理的引用:

| 引用 | 处理 |
| --- | --- |
| `.github/workflows/aurora-sandbox.yml` | 删除(供应链、签名、验收全部随镜像改造重写;若统一镜像需要 CI,在 Task 2 里新建最小 workflow) |
| `.github/aurora-sandbox-vex.json` | 删除(只被上面的 workflow 读取) |
| `scripts/verify-aurora-sandbox-image.sh`、`verify-aurora-sandbox-locks.mjs`(+`.test.mjs`)、`verify-aurora-sandbox-managed-agent.sh`(+`.test.sh`)、`verify-aurora-volc-skills.mjs`、`update-aurora-volc-skills.sh`、`update-aurora-sandbox-apt-lock.sh` | 删除(全部断言 `deploy/aurora-sandbox` 的路径与契约) |
| `server/internal/fleet/docker/seccomp.go` | 删除(不再有配置 seccomp 的路径) |
| `server/internal/fleet/model/aurora.go` 的 `SeccompProfile`、`AppArmorProfile`、`ProviderSecretFiles`、`ProxyImage`、`AuroraOpenAIAPIKeyTarget` 与 `ProviderSecretTargets`/`providerSecretOrder` 里的 `openai-api-key` | 删除字段与校验(Task 3) |
| OpenAI 出口与路由:`auroraegress.CompiledProviderHosts` 的 `api.openai.com:443`、`execution_policy.go` 的 `openai-images`/`openai-images-edit` 两个路由、`runtime/src/tools/openai-images.mjs` 与它的测试、`runtime/test/provider-openai.test.mjs` | 删除(决策 10);两个 skill 改走 seedream。`auroraManifestProducersByRoute` 里的两条 OpenAI 条目随之成为死项,一并删除 |
| `aws-deploy` 仓库的 `AURORA_SANDBOX_IMAGE`、`APPARMOR_PROFILE`、`AURORA_EGRESS_*` 部署变量 | 在独立仓库处理,本计划只列出 |

---

## 首个验证切片(先做这个,再谈其余)

用户要求先测**生成图片的 skill**,所以首个切片选 **`text-image`**(文字生成图片)。在 5 个图片 skill 里它的约束最少:

| 条件 | `text-image` 的取值 | 意义 |
| --- | --- | --- |
| 附件 | **没有任何附件约束**(`executionPolicies["text-image"]` 不设 `Attachments`) | 只给 prompt 就能跑,不必先验证附件暂存 |
| 产物 | `image`(1 张 PNG) | 图片产物走 provider 返回 URL + 服务端导入,不经过本地文件 |
| provider | 火山方舟 Seedream(路由 `volcengine-seedream`) | **要真的调外部接口、真的出图、真的花钱** |
| 镜像依赖 | 只需 bash 与 `curl` | 现有沙箱镜像已具备,**本次不动镜像** |

因此首个切片的范围是**四步**:

1. 把 `server/internal/aurora/workflows/text-image.md` 改写成可执行步骤(Task 1)。
2. 删除 Aurora 专有执行分支,否则 broker 执行面仍然生效、模型拿不到 Bash(Task 4)。
3. **确认凭据通道够用** —— 删掉 broker 后,模型侧凭据只剩 `claudeChildEnv()` 注入的 `ANTHROPIC_API_KEY`(加可选的 `ANTHROPIC_BASE_URL`/`ANTHROPIC_MODEL`)。按决策 5,ARK 的 key 就是这一把,所以**不新增任何变量**;这一步只是在 Task 1 里补一条断言把它钉住(Task 1 的 Step 5)。
4. 在**现有的**沙箱镜像上跑一次往返:提交 → 领取 → 模型调 ARK 出图 → 服务端导入 → 产物入库(Task 7 的第 2 步)。

**三个必须先满足的前提,缺一个就跑不出结果:**

| 前提 | 为什么 | 不满足时会怎样 |
| --- | --- | --- |
| 一把**真实可用的 ARK API Key**(按决策 5 作为 `ANTHROPIC_API_KEY` 注入) | 要真的调 Seedream | 401,任务失败 |
| 该 Key 的**额度/预算** | 每次出图都计费 | 429 `AccountQuotaExceeded`,任务失败 |
| `LOCAL_UPLOAD_BASE_URL` 指向**审核方能抓到的地址** | 图片产物要先过内容审核,审核器要能按 URL 取到对象;纯本地对象存储在图片这一路走不通(文本产物没有这个问题) | 生成卡在审核,`status` 到不了 `completed` |

第三条是既有约束,不是本次改动引入的。若只卡在审核,按 Task 7 第 2 步的判定口径处理:记下现象、判执行链路通过(模型确实调通了 ARK 并拿到图),但**不要**声称端到端 completed。

通过之后再做其余 4 个图片 skill、其余 8 份文档、镜像统一、剩余凭据(火山语音 ASR)归位与目录删除。**删目录(Task 5)必须等 13 份全部抽取完**,这一点不因为首个切片通过而放宽。

---

## 范围、依赖与实施顺序

| 里程碑 | 任务 | 可判定交付 | 依赖 |
| --- | --- | --- | --- |
| **M0 单 skill 验证** | 1(仅 `text-image`,含 Step 5)、4、7(第 2 步) | 一次完整往返:提交 → 领取 → 模型调 ARK 出图 → 服务端导入 → 产物入库。**不改镜像、不删目录**;需要一把可用的 ARK Key | 无 |
| **M1a 图片 skill** | 1b(4 个图片 skill) | `poster`、`xhs-image`、`product-image`、`image-edit` 各跑通一次 | M0 通过 |
| **M1b 其余内容** | 1b(其余 8 份) | 13 份文档全部可执行,每份通过"无工具名"检查 | M1a |
| **M2 执行面** | 2、3 | 统一节点镜像构建成功;**omp 本阶段不安装**;凭据复用 `ANTHROPIC_*`,火山语音 ASR 单独一个变量 | M1b |
| **M3 清理** | 5、6 | `deploy/aurora-sandbox/` 与全部引用消失;仓库测试全绿 | M2 |
| **M4 打通与验收** | 6b、7 | `apps/web` 里同时看到 13 个 agent、各自的 skill、Aurora 下发的全部任务;13 个 skill 各跑一次 + 端到端记录 | M3 |

**并行边界:** Task 1 的文档内容与 Task 2 的镜像互不依赖。Task 5(删除)必须等 13 份全部抽取完(Task 1 + 1b)。Task 6b(可见性)与 Task 7(验收)可以并行开发,但验收要等 6b 落地。

---

## 文件结构与职责

### 新增

- `scripts/check-runtime-image.sh` — 统一镜像的入口、属主与"env 里无密钥"断言(Task 2)。
- `.env.example` 的 Aurora 段 — 火山语音 ASR 的变量名与说明(值留空);ARK 不需要新变量(决策 5)。
- `docs/superpowers/plans/2026-10-08-aurora-agent-skill-runtime-acceptance.md` — Task 7 的验收记录。

### 修改

- `server/internal/aurora/workflows/*.md`(13 份)— 全部重写为可执行步骤。
- `server/pkg/db/queries/aurora_agents.sql` + 新迁移 — 13 个 agent 由 `kind='system'` 改为 `kind='user'`,并就地转换已有行(Task 6b)。
- `docker/runtime/Dockerfile` — 折叠为唯一节点镜像入口,增加媒体工具与 bash(不加 omp)。
- `server/internal/daemon/daemon.go` — 删除 `isAuroraTask` 的两处分支。
- `server/internal/daemon/prompt.go` — 删除 `buildAuroraPrompt` 的工具名与 workflow 内联。
- `server/internal/aurora/execution_policy.go` — 保留 `Route`(产物校验用),`RequiredTools` 不再作为工具白名单。
- `server/internal/fleet/model/aurora.go` — 删除 seccomp / AppArmor / provider 密钥文件 / 代理镜像字段,放宽 `claude_env` 的密钥关键字禁令。
- `server/internal/fleet/docker/provider.go`、`inspect.go` — 删除 SecurityOpt 与 secret 挂载,改为注入 provider env。
- `server/internal/daemon/managed_secrets.go` — 只新增火山语音 ASR 的环境变量读取;Anthropic(即 ARK)沿用既有的 `claudeChildEnv()`。
- `packages/views/aurora/runtime-status.tsx`、`packages/core/aurora/*`、`packages/views/locales/*/aurora.json` — 节点未就绪时的原因与重试。
- `AGENTS.md` — 记录本方向,并把 create-once 的降级写清。

### 删除

见「删除清单」小节。

### 现有接入位置(行号以写作时的 HEAD 为准)

- 13 个 agent 与 skill 的种子:`server/internal/aurora/agents.go:89` `EnsureSystemAgents`;SQL 在 `server/pkg/db/queries/aurora_agents.sql:29,49`。
- 可见性(为什么今天在 apps/web 看不到):`server/pkg/db/queries/agent.sql:3` `ListAgents`、`:8` `ListAllAgents`、`:39` `GetAgentInWorkspace` 全部 `kind = 'user'`;正确做法在同文件 `:2843` `CreateSystemUserAgent`(Mika)的注释里。
- 可见性修复的落点:`aurora_agents.sql:29` 的 `kind='system'` 字面量;`GetAgentBySystemKey`(`agent.sql:2833`)不过滤 kind,所以生成路径不受影响。
- skill 下发:`server/cmd/server/router.go:1620` → `server/internal/handler/daemon.go:4125`;daemon 侧 `server/internal/daemon/daemon.go:7912` → `execenv/context.go:328`(`claude` → `<workDir>/.claude/skills`)。
- Aurora 执行分支:`server/internal/daemon/daemon.go:7829-7836`、`:8884-8914`;`aurora_tool_surface.go:34,41,49,118`;`aurora_broker.go:32,271`。
- 产物:`server/internal/daemon/aurora_manifest.go:55,58,183`;上报 `server/internal/handler/aurora_artifact.go:67`。
- 密钥现状:`server/internal/fleet/model/aurora.go:58-61`(`/run/secrets/*` 目标)、`docker/provider.go:173`(`providerSecretMounts`)、`daemon/managed_secrets.go:185,198`。

---



## Task 1: 实现第一个 skill(`text-image`)

**本任务只落地一个 skill。** 其余 4 个图片 skill 与其余 8 份见 Task 1b,排在首个端到端验证通过之后 —— 一次改一份,每份都能先跑通再动下一份。

**Files:**
- Modify: `server/internal/aurora/workflows/text-image.md`
- Modify: `server/internal/aurora/workflows_test.go`(新增守卫测试与 `rewrittenSkills` 清单)
- Modify: `server/internal/daemon/managed_secrets_test.go`(Step 5:钉住凭据通道,不新增变量)
- Read(只读,不修改):`deploy/aurora-sandbox/runtime/src/{policy,provider-run,importer,manifest}.mjs`、`runtime/src/tools/seedream.mjs`、`vendor/volcengine/byted-ark-seedream-skill/{SKILL.md,references/MODELS.md}`

**Interfaces:**
- Produces: 5 段固定结构 —— `## Inputs`、`## Steps`、`## Required outputs`、`## Artifact manifest`、`## Failure behavior`。
- Produces: `var rewrittenSkills = []string{"text-image"}` —— 已完成改写的清单,Task 1b 逐项往里加。
- Produces: agent 子进程 env 里出现 `ANTHROPIC_API_KEY`(取值即 ARK 的那把 key,是值不是文件路径),并且**不出现** `ARK_API_KEY` 或任何 `*_API_KEY_FILE`。
- Consumes: `aurora.Workflow(skillID)` 内嵌读取(`server/internal/aurora/workflows.go:13-33`);文件名与 skill ID 一一对应,不新增文件。

- [ ] **Step 1: 写"无工具名"守卫测试(红)**

在 `server/internal/aurora/workflows_test.go` 追加:

```go
// rewrittenSkills lists the skill documents that have been converted to the
// ordinary-agent form. It grows one skill at a time: each one is verified end
// to end before the next is written, so a document that still tells the model
// to call a brokered MCP tool is a bug, not a stale comment.
var rewrittenSkills = []string{"text-image"}

func TestRewrittenWorkflowsDescribeRealSteps(t *testing.T) {
	for _, id := range rewrittenSkills {
		brief, ok := Workflow(id)
		if !ok {
			t.Fatalf("skill %q has no workflow document", id)
		}
		for _, banned := range []string{"mcp__aurora__", "aurora.seedream_generate", "aurora.seedance_generate",
			"aurora.openai_image", "aurora.volc_asr_transcribe", "aurora.read_document", "aurora.id_photo",
			"aurora.render_video_captions", "aurora.render_resume", "aurora.write_text_artifact",
			"MCP broker", "brokered tool", "no shell"} {
			if strings.Contains(brief, banned) {
				t.Errorf("skill %q still references %q", id, banned)
			}
		}
		for _, want := range []string{"## Inputs", "## Steps", "## Required outputs", "## Artifact manifest", "## Failure behavior"} {
			if !strings.Contains(brief, want) {
				t.Errorf("skill %q is missing the %q section", id, want)
			}
		}
	}
}

// TestEveryAvailableSkillHasADocument keeps the catalog and the embedded bundle
// in step. It does not require the document to be rewritten yet.
func TestEveryAvailableSkillHasADocument(t *testing.T) {
	for _, e := range Catalog() {
		if _, ok := Workflow(e.ID); e.Available && !ok {
			t.Errorf("available skill %q has no workflow document", e.ID)
		}
	}
}
```

Run: `(cd server && go test ./internal/aurora -run 'TestRewrittenWorkflows|TestEveryAvailableSkill' -count=1)`
Expected: `TestRewrittenWorkflowsDescribeRealSteps` FAIL —— `text-image.md` 命中 `mcp__aurora__` 与 `no shell`;`TestEveryAvailableSkillHasADocument` PASS

- [ ] **Step 2: 抽取现有实现**

逐条抄下事实,不要凭印象。**凡在下面这些文件里找不到依据的值,停下来问,不要补一个看起来合理的。**

| 来源 | 要抽出的事实 |
| --- | --- |
| `runtime/src/policy.mjs` 的 `PROVIDER_RUN_OPERATIONS` | seedream 那条的**字面量**操作名(文档里的 `/<operation>/finish` 要用它) |
| `runtime/src/policy.mjs` 的 seedream 条目 | 允许的 provider、model、origin、输入输出规则 |
| `runtime/src/tools/seedream.mjs` | 调用顺序:先 `begin` 拿 create 租约 → 再调 provider → 再 `finish`;请求的 `output_format`;URL 与 base64 两种返回的处理 |
| `vendor/volcengine/byted-ark-seedream-skill/SKILL.md` | 默认端点 `https://ark.cn-beijing.volces.com/api/plan/v3/images/generations`、模型名 `doubao-seedream-5.0-lite` / `-pro`、`size` 取值、`response_format` 固定 `url`、`watermark` 默认 true |
| `vendor/volcengine/byted-ark-seedream-skill/references/MODELS.md` | `size` 与参考图的合法取值与上限(文档里要写清本 skill 用哪一档) |
| `runtime/src/provider-run.mjs` | `begin` / `finish` 的**请求体字段**(见 Step 3 的代码块;已抄录,直接用) |
| `runtime/src/importer.mjs` | 导入请求体字段与返回的 `staging_id` / `size_bytes` / `sha256` |
| `runtime/src/manifest.mjs` + `server/internal/daemon/aurora_manifest.go` | manifest 的头部字段、`producer.id` 必须是 `byted-ark-seedream-skill`、至少一个 `primary`、20 条与 600 MiB 上限、`size_bytes` 必须是非负整数 |

- [ ] **Step 3: 改写 `text-image.md`**

下面这份是按已核实的实现写成的。`<operation>` 与 `size` 按 Step 2 抽出的字面量填;其余字段都已在实现里核对过。

````markdown
# Text to Image

Turn a written request into one image with the Volcengine Ark Seedream model.

You run on a normal Multica agent: you have a shell and the network access this
node provides. Do the work yourself.

## Inputs

- `prompt`: the user's request (required).
- This skill takes no attachments. Use text only.

## Steps

1. Open the create lease before spending a provider call:

   ```bash
   curl -fsS -X POST "$MULTICA_SERVER_URL/api/agent/tasks/$MULTICA_TASK_ID/aurora-provider-runs/begin" \
     -H "Authorization: Bearer $MULTICA_TOKEN" -H 'content-type: application/json' \
     -d '{"provider":"volcengine-agentplan","operation":"<operation>","model":"doubao-seedream-5.0-lite","arguments":{}}'
   ```

   Stop and report a failure when the response has `create_allowed` false and no
   `external_id`. That means the server did not grant a create; submitting
   anyway would bill a second generation for the same task.

2. Ask Seedream for the image:

   ```bash
   curl -fsS -X POST "${ANTHROPIC_BASE_URL:-https://ark.cn-beijing.volces.com/api/plan}/v3/images/generations" \
     -H "Authorization: Bearer $ANTHROPIC_API_KEY" -H 'content-type: application/json' \
     -d "$(jq -n --arg p "$PROMPT" '{model:"doubao-seedream-5.0-lite",prompt:$p,size:"<size>",response_format:"url",watermark:false}')" \
     -o /tmp/seedream.json
   ```

   The result URL is `data[0].url`. Stop and report a failure when the response
   has no image.

3. Hand that URL to the task-scoped importer. Do not download it yourself: this
   node's egress allowlist does not cover the provider's media host, and the
   importer keeps the URL out of the task.

   ```bash
   curl -fsS -X POST "$MULTICA_SERVER_URL/api/agent/tasks/$MULTICA_TASK_ID/aurora-artifacts/import" \
     -H "Authorization: Bearer $MULTICA_TOKEN" -H 'content-type: application/json' \
     -d "$(jq -n --arg u "$(jq -r '.data[0].url' /tmp/seedream.json)" '{url:$u,kind:"image",name:"text-image.png",mime_type:"image/png",size_bytes:null,metadata:{}}')" \
     -o /tmp/import.json
   ```

   The response carries `staging_id`, `size_bytes` and `sha256`.

4. Close the run:

   ```bash
   curl -fsS -X PUT "$MULTICA_SERVER_URL/api/agent/tasks/$MULTICA_TASK_ID/aurora-provider-runs/<operation>/finish" \
     -H "Authorization: Bearer $MULTICA_TOKEN" -H 'content-type: application/json' \
     -d '{"state":"succeeded"}'
   ```

## Required outputs

- One primary `image` artifact, staged on the server under the `staging_id` from
  step 3.

## Artifact manifest

Write `<outputRoot>/.multica/aurora-artifacts.v1.json`. Every value below comes
from a previous step; copy it, do not invent it.

```json
{
  "schema": "com.multica.aurora.artifacts",
  "version": 1,
  "task_id": "<the task id from your task context>",
  "skill_id": "text-image",
  "producer": { "id": "byted-ark-seedream-skill", "version": "5.0.0" },
  "provider_run": { "provider": "volcengine-agentplan", "model": "doubao-seedream-5.0-lite", "external_id": null },
  "artifacts": [
    {
      "id": "image-1",
      "source": { "type": "staged_object", "staging_id": "<staging_id from step 3>" },
      "name": "text-image.png",
      "kind": "image",
      "role": "primary",
      "format": "png",
      "mime_type": "image/png",
      "size_bytes": 0,
      "sha256": "<sha256 from step 3>",
      "metadata": {}
    }
  ]
}
```

The daemon re-checks this manifest against the task and the skill: the producer
id must be `byted-ark-seedream-skill`, the skill id must be `text-image`, at
least one artifact must be `primary`, and a missing manifest, more than 20
artifacts, or more than 600 MiB in total is rejected. Take `size_bytes` and
`sha256` from the import response rather than the placeholders above.

## Failure behavior

- A non-2xx from Seedream: report the status and the body, then call
  `.../<operation>/finish` with `{"state":"failed","error_code":"provider_failed"}`,
  and write no manifest.
- Never submit a second Seedream create for the same task.
- Never claim completion unless step 3 returned a `staging_id` and the manifest
  names it.
````

Step 3 的每个字段都必须能追溯到 Step 2 抽出的事实;凡在实现里找不到依据的,停下来问,不要补一个看起来合理的值。

- [ ] **Step 4: 运行守卫测试**

Run: `(cd server && go test ./internal/aurora -run 'TestRewrittenWorkflows|TestEveryAvailableSkill' -count=1)`
Expected: PASS

Run: `(cd server && go test ./internal/aurora -count=1)`
Expected: PASS

- [ ] **Step 5: 钉住凭据通道(不新增变量)**

删掉 broker 后,模型侧凭据只剩 `claudeChildEnv()`(`server/internal/daemon/managed_secrets.go`)注入的 `ANTHROPIC_API_KEY`,加可选的 `ANTHROPIC_BASE_URL`/`ANTHROPIC_MODEL`。按决策 5,ARK 的 key 就是这一把,所以这一步**不新增 `ARK_API_KEY`,也不改任何生产代码** —— 只是把「执行提供者拿得到这三把值、且拿不到 broker 的文件路径」钉成断言,免得后来者以为还要再铺一条通道。

```go
func TestManagedAgentEnvCarriesTheAnthropicCredentialOnly(t *testing.T) {
	// 断言执行提供者的 env 里有 ANTHROPIC_API_KEY(值),
	// 且没有 ARK_API_KEY,也没有 ARK_API_KEY_FILE / VOLC_ASR_API_KEY_FILE。
}
```

Run: `(cd server && go test ./internal/daemon -run TestManagedAgentEnvCarriesTheAnthropicCredentialOnly -count=1)`
Expected: PASS。这是**特征化断言**,不是红-绿:实现已由 #198 落地,断言应当一次即过。若为红,说明 #198 的凭据面被改坏了。

Run: `(cd server && go test ./internal/daemon -run 'TestManagedAgentEnvCarriesTheAnthropicCredentialOnly|TestManagedSecret|TestProviderSecretMount' -count=1)`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add server/internal/aurora/workflows/text-image.md server/internal/aurora/workflows_test.go server/internal/daemon
git commit -m "feat(aurora): make the text-image skill document executable by an ordinary agent"
```

## Task 1b: 其余 12 份 skill 文档

**前置:** M0 的 `text-image` 往返通过(否则不要把 12 份一起押上)。

**Files:**
- Modify: `server/internal/aurora/workflows/*.md`(12 份)
- Modify: `server/internal/aurora/workflows_test.go` 的 `rewrittenSkills`

**Interfaces:**
- Consumes: Task 1 的模板、守卫测试与 provider 调用样式。
- Produces: `rewrittenSkills` 含全部 13 项;抽出的内容进入 Task 7 的验收。

- [ ] **Step 1: 按批次逐份抽取**(每份从 `runtime/src/tools/*.mjs`、`runtime/src/policy.mjs`、`vendor/volcengine/*/SKILL.md` 与 `runtime/test/*.test.mjs` 抽;顺序如下,先把同类做完再换类)

| 批次 | skill | 备注 |
| --- | --- | --- |
| **M1a 图片(先做这 4 个)** | `poster`、`xhs-image`、`product-image`、`image-edit` | **全部路由 `volcengine-seedream`**,复用 Task 1 的调用样式。`product-image` 与 `image-edit` 今天在 `execution_policy.go` 里还写着 `openai-images` / `openai-images-edit`,**本批要先把它们改成 seedream**(见「决策 10」与 Task 3 的清理),再写文档。`image-edit` 需要 1 张参考图,用 Seedream 的 `reference_images`(Base64 Data URI 或图片 URL) |
| **M1b 视频** | `image-video`、`text-video` | 路由 `volcengine-seedance`,异步:创建 → 轮询 → 结果 URL,轮询间隔与超时按实现写清 |
| **M1b 其余** | `transcription`、`video-captions`、`id-photo`、`resume`、`document-summary`、`xhs-copy` | 前两个用 ASR(`volcengine-asr`);其余多为本地工具(`pdftotext`、ImageMagick、无头 Chromium、FFmpeg、文本写入) |

- [ ] **Step 2: 每写完一份,把它的 id 加进 `rewrittenSkills` 并跑守卫测试**

Run: `(cd server && go test ./internal/aurora -run TestRewrittenWorkflowsDescribeRealSteps -count=1)`
Expected: PASS

- [ ] **Step 3: 12 份全部完成后跑全量**

Run: `(cd server && go test ./internal/aurora -count=1)`
Expected: PASS

Run: `grep -L "https://" server/internal/aurora/workflows/*.md`
Expected: 只列出纯本地的 skill(`id-photo`、`resume` 等)。任何 provider skill 出现在列表里,说明它的端点还没写完。

- [ ] **Step 4: Commit**

```bash
git add server/internal/aurora/workflows server/internal/aurora/workflows_test.go
git commit -m "feat(aurora): convert the remaining skill documents to ordinary-agent steps"
```

---

## Task 2: 统一的节点镜像

**Files:**
- Modify: `docker/runtime/Dockerfile`
- Create: `scripts/check-runtime-image.sh`
- Delete: `deploy/aurora-sandbox/Dockerfile`、`deploy/aurora-sandbox/Dockerfile.egress`、`deploy/aurora-sandbox/docker-bake.hcl`

**Interfaces:**
- Produces: 单一镜像 —— `ENTRYPOINT ["/usr/local/bin/fleet-node","run"]`、`HEALTHCHECK fleet-node health`、`USER 10001:10001`、`/data` + `/secrets` 布局;可用入口 `/usr/local/bin/multica`、`/usr/local/bin/claude`。
- Consumes: Task 3 的变量名(火山语音 ASR);Anthropic/ARK 复用既有通道。
- **omp 本阶段不安装**(用户 2026-10-08 决定)。镜像里先只保留 Claude;加第二个 agent CLI 是后续独立改动,`scripts/agent-cli-command-names.txt` 里已有 `omp` 名字,不影响现在的解析与探测逻辑。

- [ ] **Step 1: 写镜像契约测试**

`docker/runtime/Dockerfile.test` 是既有的通用 fixture;新增一份断言脚本 `scripts/check-runtime-image.sh`,检查最终镜像里:三个入口(`multica`、`fleet-node`、`claude`)存在且可执行、`/data` 与 `/secrets` 属 `10001:10001`、`Config.Env` 里**没有**密钥值、`/usr/local/bin/claude --version` 可运行。

Run: `bash scripts/check-runtime-image.sh`（首次失败,脚本不存在)
Expected: FAIL

- [ ] **Step 2: 扩充 `docker/runtime/Dockerfile`**

在现有 31 行的基础上增加 Aurora 需要的媒体工具(Chromium、FFmpeg、ImageMagick、poppler-utils、fonts-noto-cjk)与 bash(`bypassPermissions` 下的 skill 步骤要能在 shell 里跑)。保留 `git`、`ca-certificates`。

**不要**安装 omp。**不要**从这里安装 `deploy/aurora-sandbox/` 的任何东西:broker、vendor 树、`@anthropic-ai/claude-code` 的第二份来源。Claude 只保留一份(`docker/runtime/claude-version.txt`),并把它作为 `/usr/local/bin/claude` 的唯一来源。

- [ ] **Step 3: 构建并核对**

```bash
docker build -f docker/runtime/Dockerfile -t multica-runtime-node:dev .
bash scripts/check-runtime-image.sh multica-runtime-node:dev
```

Expected: 全部检查通过;`docker inspect` 的 `Config.Env` 里没有 `ANTHROPIC_API_KEY`、`ARK_API_KEY`、`VOLC_ASR_API_KEY`。

- [ ] **Step 4: 删除两个独立构建入口与 bake**

```bash
git rm deploy/aurora-sandbox/Dockerfile deploy/aurora-sandbox/Dockerfile.egress deploy/aurora-sandbox/docker-bake.hcl
```

若部署仍需一个 build helper,在 `docker/runtime/` 下新建最小 `build.sh`,只构建这一个镜像、只打一个 tag。

- [ ] **Step 5: Commit**

```bash
git add docker/runtime scripts/check-runtime-image.sh
git commit -m "feat(runtime): build one node image with the daemon and Claude"
```

---

## Task 3: 归位剩余凭据(火山语音 ASR)

**Files:**
- Modify: `server/internal/fleet/model/aurora.go`、`server/internal/fleet/docker/provider.go`、`server/internal/fleet/docker/inspect.go`
- Modify: `server/internal/daemon/managed_secrets.go`
- Modify: `server/internal/aurora/execution_policy.go`(`product-image`、`image-edit` 改为 seedream 路由)、`server/internal/aurora/workflows_test.go` 里的生产者期望
- Modify: `.env.example`、`fleet-config.example.json`
- Delete: `server/internal/fleet/docker/seccomp.go`
- Modify: `server/internal/daemon/managed_secrets_test.go`、`server/internal/fleet/model/aurora_test.go`、`server/internal/fleet/docker/aurora_test.go`

**Interfaces:**
- Produces: **一个**固定的环境变量名 `VOLC_ASR_API_KEY`,由 Fleet 写进节点容器 env,由 daemon 读取。**`ARK_API_KEY` 不存在**(决策 5:ARK 复用 `ANTHROPIC_API_KEY`),**`OPENAI_API_KEY` 不存在**(决策 10)。
- **顺序说明:** Anthropic(即 ARK)那条通道已经可用,本任务只处理火山语音 ASR —— 它不兼容 Anthropic 协议,必须有自己的变量。取值同样从"读文件"改为"读 env"。
- Produces: `model.AuroraConfig` 删除 `SeccompProfile`、`AppArmorProfile`、`ProviderSecretFiles`、`ProxyImage` 四个字段。
- Produces: OpenAI 相关的东西一并消失 —— `model.AuroraOpenAIAPIKeyTarget`、`ProviderSecretTargets` 里的 `openai-api-key`、`providerSecretOrder` 的该项、`auroraegress.CompiledProviderHosts` 里的 `api.openai.com:443`、以及 `execution_policy.go` 里两个 skill 的 OpenAI 路由。
- Consumes: `docker/runtime/Dockerfile` 的镜像契约(Task 2)。

- [ ] **Step 1: 写"密钥经 env、不经文件"的失败测试**

在 `server/internal/daemon/managed_secrets_test.go` 追加:

```go
func TestProviderSecretsReadTheAsrKeyFromTheEnvironment(t *testing.T) {
	t.Setenv("VOLC_ASR_API_KEY", "volc-from-env")
	secrets, err := loadManagedProviderSecrets(paths, endpoint)
	if err != nil {
		t.Fatalf("loadManagedProviderSecrets: %v", err)
	}
	env := secrets.agentChildEnv()
	// 断言 env 里有 VOLC_ASR_API_KEY=volc-from-env,
	// 且既没有 ARK_API_KEY,也没有任何 *_API_KEY_FILE。
}

func TestProviderSecretsRejectMissingValues(t *testing.T) {
	// 该变量设置了但为空 → 返回错误,不静默降级成空字符串
}
```

Run: `(cd server && go test ./internal/daemon -run 'TestProviderSecrets' -count=1)`
Expected: FAIL —— 现有实现从 `/run/secrets/*` 读文件

- [ ] **Step 2: 改 daemon 侧的读取**

`server/internal/daemon/managed_secrets.go`:只把**火山语音 ASR** 的取值从"读文件"改为读环境变量(设置了但为空即报错,未设置则留空、由调用方 fail closed)。Anthropic(即 ARK)沿用既有的 `claudeChildEnv()`,本任务不动它。`mcpBrokerChildEnv` 与三个 `*_API_KEY_FILE` 常量已由 #198 删除。

- [ ] **Step 3: 删掉配置里的密集字段并放宽 env 禁令**

`server/internal/fleet/model/aurora.go`:

- 删除 `ProxyImage`、`SeccompProfile`、`AppArmorProfile`、`ProviderSecretFiles` 四个字段与 `Validate()` 里对应四段校验;`egressProxySpec` 改为取 `Config.Image`(节点与 sidecar 同镜像,入口不同)。
- **放宽 `claude_env` 的密钥关键字禁令**:现在任何含 `API_KEY`/`TOKEN`/`SECRET`/`PASSWORD` 的键一律被拒(`claudeEnvSecretMarkers`)。改为只放行这一个固定名字,其余仍拒:

```go
// providerEnvNames are the only secret-bearing keys the operator may inject
// through the Fleet config. They exist because the node runs an ordinary agent
// that calls the providers directly; the values must be supplied at deploy time
// from a 0600 file and must never reach the image, SQL, logs, or Git.
//
// ARK_API_KEY and OPENAI_API_KEY are deliberately absent. The Ark key is
// Anthropic-Messages-compatible, so the image and video calls reuse the
// credential that already arrives as ANTHROPIC_API_KEY (decision 5), and the
// OpenAI route is gone (decision 10). Only the Volcengine speech endpoint is
// not Anthropic-compatible, so it is the single separate variable.
var providerEnvNames = map[string]bool{
	"VOLC_ASR_API_KEY": true,
}
```

并把这一条写进 `Validate()` 的注释,免得后来者以为禁令被整体移除。

- [ ] **Step 4: 改 provider 与 adoption 校验**

`server/internal/fleet/docker/provider.go`:

- `NodeHostConfig` 删除 `seccomp=` 与 `apparmor=` 两个 SecurityOpt(Aurora 分支只剩 `ReadonlyRootfs` 与三处 tmpfs);保留 `CapDrop: ["ALL"]` 与 `no-new-privileges`。
- `nodeEnv` 增加一个 provider 变量 `VOLC_ASR_API_KEY`(从配置取),删除 `providerSecretMounts` 与 `/run/secrets` 挂载。Anthropic/ARK 的槽位不变。

`server/internal/fleet/docker/inspect.go` 的 adoption 校验按同一组期望值重算;`reflect.DeepEqual` 的 `SecurityOpt` 与 `Mounts` 期望随之上移。

- [ ] **Step 5: 文档与样例**

`.env.example` 增加 `VOLC_ASR_API_KEY`(值留空,注释写明"部署时注入;容器 env 对容器内进程可读"),并写明 **ARK 的 key 走既有的 `ANTHROPIC_API_KEY` + `ANTHROPIC_BASE_URL`**(决策 5),删除 `OPENAI_API_KEY` 条目;`fleet-config.example.json` 删除 `proxy_image` / `seccomp_profile` / `apparmor_profile` / `provider_secret_files`,在 `claude_env` 里增加 `VOLC_ASR_API_KEY`。

- [ ] **Step 6: 运行测试**

Run: `(cd server && go test ./internal/fleet/... ./internal/daemon -count=1)`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add server/internal/fleet server/internal/daemon .env.example fleet-config.example.json
git commit -m "feat(aurora): read provider keys from the container environment"
```

---

## Task 4: 删除 Aurora 专有的执行分支

**Files:**
- Modify: `server/internal/daemon/daemon.go`、`server/internal/daemon/prompt.go`
- Delete: `server/internal/daemon/aurora_tool_surface.go`、`server/internal/daemon/aurora_broker.go`、`server/internal/daemon/aurora_tool_surface_test.go`、`server/internal/daemon/aurora_broker_test.go`
- Modify: `server/internal/aurora/execution_policy.go`、`server/internal/handler/aurora.go`

**Interfaces:**
- Produces: Aurora 任务等同普通任务 —— 无 `MaxTurns`、无工具白名单、无 deny 清单、无 `McpConfig` 覆盖;`permissionMode` 走后端默认(`bypassPermissions`)。
- Produces: `aurora.ExecutionPolicy(skillID)` 保留 `Route` 字段(产物校验仍需要),`RequiredTools` 不再被 daemon 使用。
- Consumes: Task 1 的 skill 文档(承载 workflow 文本)。

- [ ] **Step 1: 写"任务姿态与普通任务一致"的失败测试**

```go
// server/internal/daemon/daemon_test.go
func TestAuroraTaskGetsTheOrdinaryExecutionSurface(t *testing.T) {
	// 构造一个 aurora:<skill> 的 system agent 任务,跑到 execOpts 组装处,
	// 断言:MaxTurns == 0、AllowedTools 为空、DisallowedTools 为空、McpConfig 为 nil、
	// PermissionMode 为空(即由后端决定默认值)。
}
```

Run: `(cd server && go test ./internal/daemon -run TestAuroraTaskGetsTheOrdinaryExecutionSurface -count=1)`
Expected: FAIL —— 今天 Aurora 分支会设置这五个字段

- [ ] **Step 2: 删两处分支与两个文件**

- `daemon.go:7829-7836`:删除 `if isAuroraTask(task) { … }` 整段(含 `cleanAuroraSandboxIO` 的 defer)。
- `daemon.go:8884-8914`:删除 `if auroraSandbox != nil { … }` 整段。
- `git rm` `aurora_tool_surface.go`、`aurora_broker.go` 及两个测试文件。
- `daemon.go` 里 `auroraSandbox` 变量、`auroraToolSurface` 调用点、`isAuroraTask` 的其余引用(Task 清单确认后逐处清理)。

- [ ] **Step 3: 改提示词组装**

`server/internal/daemon/prompt.go:396-434` 的 `buildAuroraPrompt`:删除 broker 工具名列表与 workflow 内联,保留任务上下文(输入附件路径、输出目录、task id、生成 id)。workflow 文本改由 `<workdir>/.claude/skills/<slug>/SKILL.md` 承载 —— 这是普通 agent 的既有机制。

`server/internal/daemon/execenv/runtime_config_sections.go:682-687` 的 `writeWorkflowAurora` 同步改为指向 skill 文件而不是"per-turn message carries the workflow"。

- [ ] **Step 4: 收敛 `execution_policy.go`**

保留 `Route` 与 `SkillExecutionPolicy` 的其余字段;`RequiredTools` 不再被任何 daemon 路径读取(它只被 `aurora_tool_surface.go` 用过)。在字段上写注释说明它现在只用于文档与产物契约,避免有人再把它当成工具白名单。

- [ ] **Step 5: 处理 `aurora_provider_run` 的记账路由**

四个路由(`server/cmd/server/router.go:1719-1723`)与 handler **保留**,`provider-run.mjs` 被删后它们由模型用 curl 调用(Task 1 的文档里已写入)。在 `server/internal/handler/aurora_provider_run.go` 的包注释与 `AGENTS.md` 里写明:**这些状态不再有强制力** —— 模型直连 provider,`ambiguous` 冻结不能阻止第二次提交;它们只是记账与可观测性。

- [ ] **Step 6: 运行测试**

Run: `(cd server && go test ./internal/daemon ./internal/aurora ./internal/handler -count=1)`
Expected: PASS

Run: `(cd server && go build ./...)`
Expected: 无输出

- [ ] **Step 7: Commit**

```bash
git add server/internal
git commit -m "refactor(aurora): run Aurora tasks on the ordinary agent surface"
```

---

## Task 5: 删除 `deploy/aurora-sandbox/` 与其全部引用

**前置:** Task 1 必须已完成。此任务开始前运行 Step 1 的守卫测试确认。

**Files:**
- Delete: `deploy/aurora-sandbox/`(整目录)
- Delete: `.github/workflows/aurora-sandbox.yml`、`.github/aurora-sandbox-vex.json`
- Delete: `scripts/verify-aurora-sandbox-*.sh`、`scripts/verify-aurora-sandbox-locks.mjs`(+`.test.mjs`)、`scripts/verify-aurora-volc-skills.mjs`、`scripts/update-aurora-volc-skills.sh`、`scripts/update-aurora-sandbox-apt-lock.sh`、`scripts/check-image-budget.mjs` 的 Aurora 分支
- Modify: `package.json` 的 `test:aurora-runtime` 脚本、`.github/ci-paths.json`(若含 Aurora 路径)、`AGENTS.md`

**Interfaces:**
- Consumes: Task 1 交付的 13 份文档(删除后它们是 skill 内容的唯一来源)。
- Produces: 仓库内不再有任何对 `deploy/aurora-sandbox` 的引用。

- [ ] **Step 1: 确认抽取已完成**

Run: `(cd server && go test ./internal/aurora -run TestRewrittenWorkflowsDescribeRealSteps -count=1)`
Expected: PASS,**且 `rewrittenSkills` 已含全部 13 项**(Task 1 + Task 1b 完成)。

```bash
grep -c '"' server/internal/aurora/workflows_test.go | head -1   # 人工确认 rewrittenSkills 是 13 项
```

**任一条不满足则停止本任务** —— 删除会让未抽取的内容无法复原。

同时确认 13 份文档里每个 provider 调用都写全了端点、认证头、请求体与轮询方式:

Run: `grep -L "https://" server/internal/aurora/workflows/*.md`
Expected: 只列出纯本地的 skill(`id-photo`、`resume`、`xhs-copy` 等)。任何 provider skill 出现在列表里,说明它还没写完。

- [ ] **Step 2: 删除目录与文件**

```bash
git rm -r deploy/aurora-sandbox
git rm .github/workflows/aurora-sandbox.yml .github/aurora-sandbox-vex.json
git rm scripts/verify-aurora-sandbox-image.sh scripts/verify-aurora-sandbox-managed-agent.sh scripts/verify-aurora-sandbox-managed-agent.test.sh scripts/verify-aurora-sandbox-locks.mjs scripts/verify-aurora-sandbox-locks.test.mjs scripts/verify-aurora-volc-skills.mjs scripts/update-aurora-volc-skills.sh scripts/update-aurora-sandbox-apt-lock.sh
git rm server/internal/fleet/docker/seccomp.go
```

- [ ] **Step 3: 清理引用**

```bash
grep -rn "aurora-sandbox\|seccomp\|apparmor\|AURORA_SANDBOX_IMAGE\|AURORA_EGRESS\|proxy_image" \
  --include='*.go' --include='*.yml' --include='*.json' --include='*.sh' --include='*.mjs' --include='*.ts' \
  . | grep -v node_modules | grep -v '^./docs/'
```

逐处处理,直到只剩注释里的历史说明。已知需要处理的:

- `package.json` 的 `"test:aurora-runtime": "pnpm --dir deploy/aurora-sandbox/runtime test"` → 删除。
- `server/internal/fleet/model/aurora_test.go`、`server/pkg/agent/aurora_sandbox_smoke_test.go`、`server/internal/fleet/integration/aurora_test.go` 里指向该目录的 fixture 与镜像常量 → 更新或删除用例。
- `server/cmd/fleet/main.go` 的 AppArmor 启动预检(`apparmorPreflightError`)→ 随字段删除。
- `deploy/aurora-sandbox/README.md` 描述的操作流程 → 并入 `docker/runtime/` 的说明或删除。

- [ ] **Step 4: 运行全量检查**

Run: `pnpm typecheck && pnpm lint && pnpm test`
Expected: PASS

Run: `(cd server && go build ./... && go test ./internal/fleet/... ./internal/daemon ./internal/aurora ./internal/handler -count=1)`
Expected: PASS

Run: `bash scripts/test-go.test.sh`
Expected: PASS（该脚本自身对仓库脚本的测试）

- [ ] **Step 5: Commit**

```bash
git add AGENTS.md .github package.json scripts server deploy
git commit -m "chore(aurora): delete the sandbox image directory and its references"
```

（不用 `git add .` 或 `git add -A`:上面只 stage 本任务涉及的路径。`git rm` 已经在 Step 2 把删除登记进索引。）

---

## Task 6: 运行时投影与就绪可见

节点不再需要"准备/等待配置/容量"那套模型,但**节点没起来时必须让用户看见原因**,而不是只拿到一个 503。

**Files:**
- Modify: `server/internal/handler/aurora_runtime_view.go`
- Modify: `packages/core/aurora/{schema,types,queries}.ts`、`packages/views/aurora/runtime-status.tsx`
- Modify: `packages/views/locales/*/aurora.json`

**Interfaces:**
- Produces: 既有投影保留并在失败时给出可公开的原因码;前端据此渲染原因与"重试"。
- Consumes: `GET /api/aurora/runtime` 的既有字段(不新增必填字段,保证旧客户端不失败)。

- [ ] **Step 1: 写失败原因可见的测试(红)**

```go
// server/internal/handler/aurora_runtime_view_test.go
func TestRuntimeViewReportsAConcreteReasonWhenTheNodeIsNotReady(t *testing.T)
func TestRuntimeViewStaysReadOnly(t *testing.T) // 既有用例必须继续通过
```

```tsx
// packages/views/aurora/runtime-status.test.tsx
it("shows why the runtime is not ready instead of a bare failure", async () => {});
it("offers a retry that re-reads the projection", async () => {});
```

- [ ] **Step 2: 实现**

投影从 `aurora_sandbox_node` 与 `fleet_nodes` 读出 `state` / `error_code`,映射成公开原因码(`runtime_unconfigured`、`runtime_offline`、`runtime_policy_unavailable`)。前端在 `failed` 与 `offline` 两态渲染原因与重试按钮;`ready` 为假且状态不明时**不**显示"已完成"。

- [ ] **Step 3: 五语文案**

新增键必须五语齐备(`parity.test.ts` 会拦)。

- [ ] **Step 4: 运行**

Run: `pnpm typecheck && pnpm test`
Expected: PASS

Run: `(cd server && go test ./internal/handler -run 'AuroraRuntime' -count=1)`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add server/internal/handler packages/core/aurora packages/views/aurora packages/views/locales
git commit -m "feat(aurora): show why the runtime is not ready"
```

---

## Task 6b: 让 13 个 agent 在 apps/web 里可见可用

**这是「两边完全打通」的第一条,也是今天唯一明确不成立的一条。**

现状:13 个 agent 由 `UpsertAuroraSystemAgent` 写死 `kind='system'`,而 `agent.sql:3/8/39` 的列表与详情查询全部过滤 `kind = 'user'` —— 所以在 apps/web 的 agent 列表、指派选择器、聊天入口里**都看不到它们**,也无法把 issue 指派给它们。

仓库里已有正确做法可照抄:`agent.sql:2843` 的 `CreateSystemUserAgent`(Mika 的载体),其注释写明:「创建一个成员仍能看见、能聊天、能指派 issue 的产品预置 agent。**故意用 `kind='user'`** —— `kind='system'` 会把该行从 agent 列表与指派界面隐藏,并随 runtime 硬删除。」

**Files:**
- Modify: `server/pkg/db/queries/aurora_agents.sql`(`UpsertAuroraSystemAgent`)
- Create: `server/migrations/<n>_aurora_agents_visible.up.sql/.down.sql`
- Modify: `server/internal/aurora/agents.go`、`agents_test.go`
- Modify: `server/internal/handler/agent.go`(若需要拒绝对这几个 agent 的归档/删技能)
- Modify: `packages/views/agents/components/tabs/activity-tab.tsx`(仅在无 issue 任务渲染不良时)

**Interfaces:**
- Produces: 13 个 agent 的 `kind='user'`,其余不变(`system_key='aurora:<skillID>'` 保留 —— `GetAgentBySystemKey` 不过滤 kind,生成路径不受影响)。
- Produces: 迁移把已有 workspace 的 `kind='system'` Aurora agent 就地转换,幂等。

- [ ] **Step 1: 写"可见"的失败测试**

```go
// server/internal/aurora/agents_test.go
func TestAuroraAgentsAreVisibleToMembers(t *testing.T) {
	// EnsureSystemAgents 之后:
	//   ListAgents(workspaceID) 必须包含 13 个 system_key 前缀为 "aurora:" 的 agent
	//   每个 agent 的 kind 必须是 "user"
}
```

Run: `(cd server && go test ./internal/aurora -run TestAuroraAgentsAreVisibleToMembers -count=1)`
Expected: FAIL —— 今天是 `kind='system'`,不出现在 `ListAgents` 里

- [ ] **Step 2: 改种子与 SQL**

`aurora_agents.sql` 的 `UpsertAuroraSystemAgent`:把 `kind` 从 `'system'` 改为 `'user'`,并在 `DO UPDATE` 里补 `kind = EXCLUDED.kind`(否则已存在的行不会被纠正)。注释改写为"Mika 的 `CreateSystemUserAgent` 同构:产品预置但成员可见可指派"。

`server/internal/aurora/agents.go` 的 `systemAgentDef` / `SystemAgentDef` 注释同步:它们不再是"不可见的执行载体"。

- [ ] **Step 3: 写迁移**

必须按当前最高号顺延,并确认幂等:

```sql
-- (n)_aurora_agents_visible.up.sql
-- Aurora's 13 skill agents become member-visible, following the same shape as
-- the Mika carrier (CreateSystemUserAgent): kind='user' is what makes a
-- product-defined agent appear in agent lists and assignment surfaces. The
-- system_key identity is unchanged, so GetAgentBySystemKey keeps resolving the
-- generation path.
UPDATE agent SET kind = 'user', updated_at = now()
WHERE kind = 'system' AND system_key LIKE 'aurora:%';
```

`.down.sql` 是反向 UPDATE(`WHERE kind='user' AND system_key LIKE 'aurora:%'` → `'system'`)。列变更不需要索引;若确实新增索引,单独一个 `CREATE [UNIQUE] INDEX CONCURRENTLY` 文件并登记进 `server/cmd/migrate/main.go`。

Run: `make migrate-up && (cd server && go run ./cmd/migrate up)`
Expected: 迁移记录一次;重跑无变化

- [ ] **Step 4: 处理两个新出现的边界**

1. **归档会让生成失败。** `GetAgentBySystemKey` 要求 `archived_at IS NULL`。agent 可见后用户能归档它,而 `UpsertAuroraSystemAgent` 的 `DO UPDATE` **不会**清 `archived_at` —— 于是该 skill 的生成会失败在"找不到 agent"。加一条:生成前若目标 agent 已归档,就地解除归档(或明确报出可读的错)。二选一,写进代码注释。
2. **skill 关联变成用户可编辑。** agent 可见后,Skills 页签能 `DELETE /api/agents/{id}/skills/{skillId}` 摘掉那条关联,任务就会在没有 skill 文档的情况下执行。对这个 13 个 agent 的关联做只读保护,或在交付时把"未见 skill 文档"当成硬失败并给出可读原因。

- [ ] **Step 5: 在 apps/web 里逐条确认**

```bash
make up C=api,web
# 打开 /{workspaceSlug}/agents
```

Expected(三条分别截图/记录):
1. 列表里出现 13 个 Aurora agent(名称与 `aurora.Catalog()` 的 `Name` 一致);
2. 打开任一个 → Skills 页签显示它的那一份 skill;
3. 打开任一个 → Work 页签显示从 Aurora 下发的任务(无 issue 的 quick-create 任务也要能读,不能空行)。

- [ ] **Step 6: 运行测试**

Run: `(cd server && go test ./internal/aurora ./internal/handler -count=1)`
Expected: PASS

Run: `pnpm typecheck && pnpm test`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add server/pkg/db server/migrations server/internal/aurora server/internal/handler packages/views/agents
git commit -m "feat(aurora): make the 13 skill agents visible and assignable in the web app"
```

---

## Task 7: 端点回传与结算的一次完整往返

**Files:**
- Modify: `server/internal/service/aurora_completion.go`(只在必要时)
- Create: `docs/superpowers/plans/2026-10-08-aurora-agent-skill-runtime-acceptance.md`

**Interfaces:**
- Consumes: Task 1–4 的全部改动。
- Produces: 一次真实往返的证据记录。

- [ ] **Step 1: 单元层面确认结算未变(但结算不是本任务的门)**

```bash
( cd server && go test ./internal/service -run 'Aurora' -count=1 )
( cd server && go test ./internal/handler -run 'Aurora' -count=1 )
```

Expected: PASS。`settleAuroraOnCompleted` 仍要求至少一个已提交资产,否则走退款 —— 这条不变。

**计费按现状保留,不修、不算、不验证。** 部署时遇到 credits 相关的问题(余额不足、预留失败、结算异常、账本对不上),**记录现象后绕过,继续验证执行链路**,不要为了让它跑通去改计费逻辑。计费要改成按 token 用量计算,那是后续独立计划的事(见「用户已确认的决策」第 8 条)。

- [ ] **Step 2: 在 Docker Desktop 上跑一次真实往返**

**M0 阶段先跑一次,用现有的沙箱镜像,不构建新镜像。** 首个切片的改动就是 Task 1 的文档与凭据断言、Task 4 的删分支,加上这一次运行。

```bash
make up C=api,fleet
# 需要:一把真实可用且有额度的 ARK Key(按决策 5 作为 ANTHROPIC_API_KEY 注入,ANTHROPIC_BASE_URL 指向 Ark Agent Plan)、LOCAL_UPLOAD_BASE_URL 指向审核方能抓到的地址
# 在 Aurora 应用里对 skill「文字生成图片」提交一次生成,只填 prompt
```

Expected: 提交 → 节点领取 → 模型走 `begin` → 调 ARK 出图 → 导入拿到 `staging_id` → 写 manifest → daemon 收集 → 资产入库 → `aurora_generation.status` 变为 `completed`。

失败时的判定口径:

| 卡在哪 | 说明什么 | 怎么处理 |
| --- | --- | --- |
| `begin` 未授予 create,或 ARK 返回 401/429 | 密钥或额度问题,**不是**执行链路问题 | 换一把可用且有额度的 Key 再跑;不要改代码绕过 |
| 模型调不到 ARK(连不通、被拒) | egress 允许列表没有覆盖该端点 —— 这是既有允许列表的问题 | 记录实际错误;确认端点是否在允许列表内,按需要在 Fleet 配置的 `egress_hosts` 里加 |
| 导入返回 4xx | 文档里的 payload 与真实路由不符 | 改**文档**,不要改路由 |
| 产物没入库 / `unknown staging artifact` / manifest 被拒 | 文档里的 manifest 段与 `aurora_manifest.go` 的校验不一致 | 改**文档**,不要改校验 |
| **只有**审核卡住(`LOCAL_UPLOAD_BASE_URL` 抓不到) | 环境问题,不是本次改动引入的 | 记下现象,**判执行链路通过**(模型确实调通了 ARK 拿到图并成功导入),但不要声称端到端 `completed` |
| credits 相关(预留失败、余额不足、结算写不进账本) | 计费不在本计划范围内 | 记下现象,**判本切片通过** |

反过来,**模型拿不到 Bash、或拿不到 `ANTHROPIC_API_KEY`/`ANTHROPIC_BASE_URL`,都不算通过** —— 那说明 Task 4 做漏了,或凭据通道没配好。

**M4 阶段用统一镜像重跑一次**(Task 2 构建的镜像已由 `AURORA_SANDBOX_IMAGE` 指向):

```bash
MULTICA_RUN_DOCKER_INTEGRATION=1 make env-exec ARGS="-- pnpm exec playwright test --project=fleet-docker"
```

Expected: 同样通过,且浏览器 trace 与账本断言齐全。

- [ ] **Step 3: 逐个 skill 跑一遍**

13 个 skill 各跑一次,记录:输入、实际执行的命令、产物、账本变化。**未授权的真实 provider 调用记 SKIP 并写明原因,绝不记 PASS。**

- [ ] **Step 4: 最终验收的三条可见性检查(M4 的门)**

这一组是用户定义的"两边完全打通"判据。**三条都要过,任何一条不过就是没打通**,不得用前几步的绿灯替代:

| # | 在 `apps/web` 里看什么 | 通过的判据 |
| --- | --- | --- |
| ① | `/{ws}/agents` 列表 | 13 个由 Aurora 创建的 agent 都出现,名称与 `aurora.Catalog()` 的 `Name` 一致 |
| ② | 任一 agent 的 Skills 页签 | 显示该 agent 对应的那一份 skill(内容与 `server/internal/aurora/workflows/<id>.md` 一致) |
| ③ | 任一 agent 的 Work 页签 | 显示从 Aurora 下发的全部任务,包括没有 issue 的 quick-create 任务,且不是空行、不是"未知 issue" |

把三条的截图或记录写进验收文件。第 ③ 条如果只在有 issue 的任务上成立、无 issue 的行渲染为空,记 FAIL 并去修渲染,不要改判据。

- [ ] **Step 5: 写验收记录**

新建 `docs/superpowers/plans/2026-10-08-aurora-agent-skill-runtime-acceptance.md`,逐条写代码 HEAD、镜像 digest、命令、观察结果、资产与账本断言,以及上面三条的结论。不得记录密钥。

- [ ] **Step 6: Commit 并开 PR**

```bash
git add docs/superpowers/plans/2026-10-08-aurora-agent-skill-runtime-acceptance.md
git commit -m "docs(aurora): record the agent+skill round trip acceptance"
git push -u origin "$(git rev-parse --abbrev-ref HEAD)"
gh pr create --repo eanfs/multica --fill
```

---

## 行为变化与已知代价

这些是本次方向**主动接受**的后果,必须与代码一起记录,不得留空:

| 项 | 之前 | 现在 |
| --- | --- | --- |
| 容器隔离 | AppArmor 进程策略 + 自定义 seccomp + 只读根 + cap drop | **Docker 默认**。AppArmor 与自定义 seccomp 都不再使用;只读根与 `no-new-privileges` 保留(零代码改动,不挡任何步骤) |
| 隔离验证 | Linux 安全验收矩阵 + 冒烟,CI 每次跑 | **无**。删掉的验收脚本没有替代品;不得声称容器被验证过 |
| provider 密钥 | 只读文件挂载,只有 broker 子进程拿到路径 | **复用 `ANTHROPIC_API_KEY` + `ANTHROPIC_BASE_URL`**(ARK 的 key 兼容 Anthropic 协议,决策 5),注入 agent 进程 env,对容器内任何进程可读、`docker inspect` 亦可见。火山语音 ASR 是唯一的独立变量 |
| 计费 create-once | broker 强制:重试不会第二次提交 create | **仅记账**。模型直连 provider,`ambiguous` 冻结不再有强制力;重复提交计费请求成为可能 |
| 产物 manifest 的作者 | broker(`manifest.mjs`) | **模型**。契约不变:daemon 只当路径清单,自行重算 size/SHA/MIME 并按 `Route` 校验 |
| manifest 的 `producer.id` | broker 声明的固定 producer,代表"这份 manifest 来自哪个受控实现" | **模型自述的字符串**。daemon 仍要求它等于 `auroraManifestProducersByRoute[route]`(例如 `text-image` 必须是 `byted-ark-seedream-skill`),但填写者是模型 —— 这个字段从此只是形状检查,不是来源证明 |
| 13 个 skill 的实现 | 9 个受控 MCP 工具 + vendored 上游树 | **13 份可执行文档**;上游树与补丁随目录删除,来源记录不再有库存 |
| provider 范围 | 火山(Seedream/Seedance/ASR)+ OpenAI 图片 | **只有火山**。OpenAI 的密钥、挂载目标、出口允许列表条目与两个路由全部删除,`product-image`、`image-edit` 改走 Seedream |
| 模型可用的工具 | Bash/Read/Write 等被 deny,只能调 9 个 broker 工具 | **普通 agent 的全部工具**(`bypassPermissions`) |
| 平台要求 | 需要能加载 AppArmor 的 Linux 引擎 | **Docker Desktop 即可** —— 排除它的唯一理由是 AppArmor |

---

## 仍未执行与阻塞(不得声称已通过)

| 项 | 状态 |
| --- | --- |
| `text-image` 的单 skill 往返 | **未运行**。这是 M0,唯一能证明方向可行的一步;需要一把真实可用的 ARK Key 与额度 |
| 其余 4 个图片 skill | 未起草(Task 1b 的 M1a 批次,依赖 M0 通过) |
| 其余 8 份 skill 文档 | 未起草(Task 1b 的 M1b 批次) |
| `LOCAL_UPLOAD_BASE_URL` 是否满足图片审核 | 未验证。本地纯对象存储抓不到图片产物,需要指向审核方能访问的地址;文本产物没有这个问题 |
| **apps/web 三条可见性** | **今天 ①不成立**(13 个 agent 是 `kind='system'`,不出现在列表与指派界面);②③随 ① 修复后需实测。Task 6b 是修复,Task 7 Step 4 是判据 |
| 统一节点镜像 | 未构建 |
| omp | **本阶段不安装**(用户决定),不是欠账 |
| Docker Desktop 上的端到端往返 | 未运行 |
| 真实 provider 调用 | 未授权、未运行;需要三把真实密钥与预算 |
| credits 计费 | **本计划不动**。按 skill 固定积分的预留/退款照旧保留,但不再是验证的门;改用 token 用量计算是后续独立计划 |
| 隔离验证 | **本方向下不存在**。不要把它列为"待补",它已被明确放弃 |
| `aws-deploy` 仓库的 `AURORA_SANDBOX_IMAGE` / `APPARMOR_PROFILE` / `AURORA_EGRESS_*` | 未处理(独立仓库) |

---

## Spec Coverage 自审表

| 用户决策 | 任务 | 关键证据 |
| --- | --- | --- |
| 13 skill = 13 agent + 13 skill,不用 MCP tool | 1、1b、4 | `TestRewrittenWorkflowsDescribeRealSteps`;`TestAuroraTaskGetsTheOrdinaryExecutionSurface` |
| 完全复用 issue 干活流程 | 4 | Aurora 分支删除后 `runTask` 无 Aurora 条件;skill 由既有 `ensureTaskSkillBundles` 下发 |
| 先做一个 skill 加速验证 | 1、4、7 | `text-image` 的端到端往返在**现有镜像**上通过,不删目录。前提是有一把可用且有额度的 ARK Key |
| `deploy/aurora-sandbox/` 完全删除 | 5 | 全仓 grep 无残留;`git ls-files deploy/aurora-sandbox` 为空 |
| 不用 AppArmor、不用自定义 seccomp | 3、5 | 字段删除;`seccomp.go` 删除;`NodeHostConfig` 只剩 tmpfs 与只读根 |
| 密钥经 `.env` 进容器 env | 3 | `TestProviderSecretsComeFromTheEnvironment`;镜像 `Config.Env` 无密钥 |
| 不做服务端 provider 代理 | 1b、3 | skill 文档里是 provider 直连;没有新增服务端转发路由 |
| omp 暂不安装 | 2 | 镜像契约测试只断言三个入口 |
| 删掉 OpenAI,只用火山引擎 | 3、1b | 全仓 grep 无 `OPENAI_API_KEY` / `openai-images` 残活;`product-image`、`image-edit` 的路由是 `volcengine-seedream` |
| **apps/web 里三条可见性(agent / skill / 任务)** | **6b、7** | `TestAuroraAgentsAreVisibleToMembers`;`apps/web` 里 `/{ws}/agents` 列表、Skills 页签、Work 页签三处截图与记录 |

---

## 执行交接

**Plan saved to `docs/superpowers/plans/2026-10-08-aurora-agent-skill-runtime.md`.** 两种执行方式:

1. **Subagent-Driven(推荐)** —— 每个任务派一个新的子代理,任务之间由我审查。Task 1 是内容工作,适合逐份审。
2. **Inline Execution** —— 在本会话内按 executing-plans 批量执行,带检查点。

**建议从 M0 开始**,它的范围是三条改动加一次运行:

1. 改 `server/internal/aurora/workflows/text-image.md` 一份文档 + 把 ARK 密钥接进 agent env(Task 1,含 Step 5)
2. 删 Aurora 执行分支(Task 4)
3. 在现有镜像上跑一次往返(Task 7 Step 2)

**开始前需要确认的只有一件事:** `text-image.md` 里的 provider 调用(端点、模型名、`size`、请求体、响应里取 URL 的路径)是我从 `runtime/src/tools/seedream.mjs` 与 `vendor/volcengine/byted-ark-seedream-skill/` 抄出来的。抄出来之后请你或熟悉这块的人过一眼 —— 写错了不会有测试拦住,只会表现为 4xx 或空结果。另外那两处待填的字面量(provider run 的 operation 名、`size` 取值)也要从实现里取,别猜。
