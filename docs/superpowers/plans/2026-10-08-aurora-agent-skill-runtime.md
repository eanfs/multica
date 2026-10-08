# Aurora 执行层改造为「13 Agent + 13 Skill」实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 Aurora 的 13 个 skill 通过 multica 既有的「agent 加载 skill」机制执行 —— 每个 skill 一个预置 system agent、一份可执行的 skill 文档,模型用普通工具(Bash / Read / Write)按文档完成任务;删掉 Aurora 专有的 MCP broker 执行面、`deploy/aurora-sandbox/` 全部内容与全部自定义隔离策略,节点密钥改由容器 env 在部署时注入。

**Architecture:** Aurora 不再拥有独立执行通道。`daemon.isAuroraTask` 分支被删除后,Aurora 任务与普通 issue 任务逐字节同路径:入队写入 `agent_task_queue` 并绑定 agent 的 `runtime_id` → 节点领取 → daemon 把该 agent 启用的 skill 物化到 `<workdir>/.claude/skills/<slug>/SKILL.md` → `BuildPrompt` → 拉起 Claude(`bypassPermissions`,无工具白名单,无 MaxTurns)→ 产物落盘 → 回传结算。13 个 system agent、13 个 skill 行、13 条 `agent_skill` 关联**今天已经存在**(`aurora.EnsureSystemAgents`),本计划改的是它们的内容与执行面,不是重新搭建。节点镜像是 `docker/runtime/Dockerfile` 的单一产物,内含 daemon(`multica` + `fleet-node`)、Claude 与 omp。

**Tech Stack:** Go 1.26(Chi、pgx/v5、sqlc、Docker Engine SDK)、PostgreSQL 17、Node 22、Claude Code CLI、omp、TanStack Query + zod + Vitest、Playwright、Docker Desktop。

**Spec：** 本计划**取代** [统一 Cloud Runtime 与 Aurora skills 执行层设计](../specs/2026-10-06-unified-cloud-runtime-aurora-design.md)(r2)中关于隔离、单镜像多入口、broker 桥接与 G0–G5 门的部分。规格的书面内容保留为历史记录,不再是执行依据。审查记录 [2026-10-07-unified-cloud-runtime-aurora-review.md](../specs/2026-10-07-unified-cloud-runtime-aurora-review.md) 中 R1、R2、R6、R9 四条针对进程隔离与受控 fixture 的发现**在本方向下不再适用**(见「行为变化与已知代价」)。

**被取代的计划：** 已关闭的 [PR #193](https://github.com/eanfs/multica/pull/193) 所载的统一 Cloud Runtime 执行计划(其文件只存在于分支 `docs/aurora-unified-runtime-plan`,不并入 main)、[Aurora Cloud Runtime 对接实施计划](2026-10-06-aurora-cloud-runtime-integration.md) 的 Task 2/5/6、[沙箱镜像与烟测](2026-09-25-aurora-sandbox-image-smoke.md)、[fleet 隔离与 egress](2026-09-25-aurora-sandbox-fleet-isolation.md)、[沙箱 skill runtime](2026-09-25-aurora-sandbox-skill-runtime.md)。这些计划的已交付代码在 Task 4–6 中被删除,其完成状态**不**等于本计划已实施。

---

## 用户已确认的决策(2026-10-08)

1. **13 个 skill = 13 个预置 agent + 13 个 skill**,不再使用 MCP tool。
2. **完全复用 issue 的干活流程** —— Aurora 任务就是普通 agent 任务。
3. **`deploy/aurora-sandbox/` 完全删除**,包括 `seccomp.json` 与 `vendor/`。
4. **不使用 AppArmor**,自定义 seccomp 也一起去掉,容器按 Docker 默认安全设置执行;安全问题本阶段忽略。
5. **provider 密钥通过 `.env` 写入容器环境变量**,部署时解决,**不经服务端**。
6. **不做**「skill 脚本调 Multica 服务端、由服务端持密钥调 provider」这种代理。

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
| `server/internal/fleet/model/aurora.go` 的 `SeccompProfile`、`AppArmorProfile`、`ProviderSecretFiles`、`ProxyImage` | 删除字段与校验(Task 3) |
| `aws-deploy` 仓库的 `AURORA_SANDBOX_IMAGE`、`APPARMOR_PROFILE`、`AURORA_EGRESS_*` 部署变量 | 在独立仓库处理,本计划只列出 |

---

## 范围、依赖与实施顺序

| 里程碑 | 任务 | 可判定交付 | 依赖 |
| --- | --- | --- | --- |
| **M1 抽取与内容** | 1 | 13 份可执行 skill 文档写入 `server/internal/aurora/workflows/`,每份通过"无工具名"检查 | 无 |
| **M2 执行面** | 2–4 | 统一节点镜像构建成功;Aurora 执行分支删除;密钥走 env | Task 1 |
| **M3 清理** | 5–6 | `deploy/aurora-sandbox/` 与全部引用消失;仓库测试全绿 | Task 2–4 |
| **M4 接线与验收** | 7–8 | 节点未就绪时 UI 可见原因;一次端到端往返在 Docker Desktop 上跑通 | Task 2–6 |

**并行边界:** Task 1(文档内容)与 Task 2(镜像)互不依赖,可并行。Task 5(删除)必须等 Task 1 完成。Task 7/8 依赖 Task 2–4。

---

## 文件结构与职责

### 新增

- `docker/runtime/omp-version.txt` — omp 的锁定版本(与既有 `claude-version.txt` 同样式)。若 omp 不以 npm 分发,改为记录安装来源与校验和。
- `.env.example` 的 Aurora 段 — provider 密钥的变量名与说明(值留空)。
- `docs/superpowers/plans/2026-10-08-aurora-agent-skill-runtime-acceptance.md` — Task 8 的验收记录。

### 修改

- `server/internal/aurora/workflows/*.md`(13 份)— 全部重写为可执行步骤。
- `docker/runtime/Dockerfile` — 折叠为唯一节点镜像入口,增加 omp 与媒体工具。
- `server/internal/daemon/daemon.go` — 删除 `isAuroraTask` 的两处分支。
- `server/internal/daemon/prompt.go` — 删除 `buildAuroraPrompt` 的工具名与 workflow 内联。
- `server/internal/aurora/execution_policy.go` — 保留 `Route`(产物校验用),`RequiredTools` 不再作为工具白名单。
- `server/internal/fleet/model/aurora.go` — 删除 seccomp / AppArmor / provider 密钥文件 / 代理镜像字段,放宽 `claude_env` 的密钥关键字禁令。
- `server/internal/fleet/docker/provider.go`、`inspect.go` — 删除 SecurityOpt 与 secret 挂载,改为注入 provider env。
- `server/internal/daemon/managed_secrets.go` — 从 env 读取 provider 密钥。
- `packages/views/aurora/runtime-status.tsx`、`packages/core/aurora/*`、`packages/views/locales/*/aurora.json` — 节点未就绪时的原因与重试。
- `AGENTS.md` — 记录本方向,并把 create-once 的降级写清。

### 删除

见「删除清单」小节。

### 现有接入位置(行号以写作时的 HEAD 为准)

- 13 个 agent 与 skill 的种子:`server/internal/aurora/agents.go:89` `EnsureSystemAgents`;SQL 在 `server/pkg/db/queries/aurora_agents.sql:29,49`。
- skill 下发:`server/cmd/server/router.go:1620` → `server/internal/handler/daemon.go:4125`;daemon 侧 `server/internal/daemon/daemon.go:7912` → `execenv/context.go:328`(`claude` → `<workDir>/.claude/skills`)。
- Aurora 执行分支:`server/internal/daemon/daemon.go:7829-7836`、`:8884-8914`;`aurora_tool_surface.go:34,41,49,118`;`aurora_broker.go:32,271`。
- 产物:`server/internal/daemon/aurora_manifest.go:55,58,183`;上报 `server/internal/handler/aurora_artifact.go:67`。
- 密钥现状:`server/internal/fleet/model/aurora.go:58-61`(`/run/secrets/*` 目标)、`docker/provider.go:173`(`providerSecretMounts`)、`daemon/managed_secrets.go:185,198`。

---

## Task 1: 抽取并重写 13 份 skill 文档

**Files:**
- Modify: `server/internal/aurora/workflows/*.md`(13 份)
- Create: `server/internal/aurora/workflows_test.go` 的追加用例(见 Step 5)
- Read(只读,不修改):`deploy/aurora-sandbox/runtime/src/{policy,provider-run,importer,manifest}.mjs`、`runtime/src/tools/*.mjs`、`vendor/volcengine/*/SKILL.md` 与 `references/`、`runtime/test/*.test.mjs`

**Interfaces:**
- Produces: 每份文档固定五段 —— `## Inputs`、`## Steps`、`## Required outputs`、`## Artifact manifest`、`## Failure behavior`。
- Produces: 13 份文档里的每一步都是真实命令或 HTTP 调用;不得出现 `mcp__aurora__`、`aurora.<tool>` 或"调用 brokered tool"字样。
- Consumes: 现有 `aurora.Workflow(skillID)` 内嵌读取(`server/internal/aurora/workflows.go:13-33`),路径与 skill ID 一一对应,不新增文件。

- [ ] **Step 1: 写"无工具名"守卫测试(红)**

在 `server/internal/aurora/workflows_test.go` 追加:

```go
// TestWorkflowsDescribeRealStepsNotBrokerTools pins the direction of the
// Aurora execution layer: the 13 skill documents drive an ordinary agent, so a
// document that still tells the model to call a brokered MCP tool is a bug,
// not a stale comment.
func TestWorkflowsDescribeRealStepsNotBrokerTools(t *testing.T) {
	for _, e := range Catalog() {
		brief, ok := Workflow(e.ID)
		if !ok {
			if e.Available {
				t.Fatalf("available skill %q has no workflow document", e.ID)
			}
			continue
		}
		for _, banned := range []string{"mcp__aurora__", "aurora.seedream_generate", "aurora.seedance_generate",
			"aurora.openai_image", "aurora.volc_asr_transcribe", "aurora.read_document", "aurora.id_photo",
			"aurora.render_video_captions", "aurora.render_resume", "aurora.write_text_artifact",
			"MCP broker", "brokered tool"} {
			if strings.Contains(brief, banned) {
				t.Errorf("skill %q still references %q", e.ID, banned)
			}
		}
		for _, want := range []string{"## Inputs", "## Steps", "## Required outputs", "## Artifact manifest", "## Failure behavior"} {
			if !strings.Contains(brief, want) {
				t.Errorf("skill %q is missing the %q section", e.ID, want)
			}
		}
	}
}
```

Run: `(cd server && go test ./internal/aurora -run TestWorkflowsDescribeRealStepsNotBrokerTools -count=1)`
Expected: FAIL —— 13 份都命中 `mcp__aurora__`,且缺少两个新段落

- [ ] **Step 2: 逐份抽取现有实现**

对每个 skill 打开对应实现,把每一步抄成文字加命令。对照表:

| skill | 实现来源 | 要抽出的关键事实 |
| --- | --- | --- |
| `id-photo` | `runtime/src/tools/id-photo.mjs` | 固定 ImageMagick argv、输出尺寸与格式 |
| `resume` | `runtime/src/tools/resume.mjs` | 转义 HTML 模板要点、无头 Chromium 打印参数、输出 PDF |
| `video-captions` | `runtime/src/tools/hyperframes.mjs` | FFmpeg 抽音 → ASR → 固定模板合成字幕;cue 输入格式 |
| `transcription` | `runtime/src/tools/volc-asr.mjs` | ASR 端点、认证头、有界 base64 流式、音频抽取命令 |
| `document-summary`、`xhs-copy` | `runtime/src/tools/documents.mjs`、`text-artifact.mjs` | `pdftotext` / DOCX ZIP-XML 抽取的固定命令、文本产物命名规则 |
| `poster`、`xhs-image`、`text-image` | `runtime/src/tools/seedream.mjs` + `vendor/volcengine/byted-ark-seedream-skill/SKILL.md` | ARK 端点、认证头、请求体、同步返回形状、参考图上传 |
| `image-video`、`text-video` | `runtime/src/tools/seedance.mjs` + `vendor/volcengine/byted-ark-seedance-skill/SKILL.md` | ARK 异步创建 → 轮询 → 下载;`external_id` 语义 |
| `product-image`、`image-edit` | `runtime/src/tools/openai-images.mjs` | OpenAI 端点、模型名、图片输入方式 |

每个 skill 同时读 `runtime/src/policy.mjs` 对应条目,把 provider / model / origin / 输入输出规则抄进 `## Inputs` 与 `## Required outputs`。

- [ ] **Step 3: 写样例文档(先写一份,作为其余 12 份的模板)**

`id-photo.md` 作为最简的一份,写出完整形态:

```markdown
# ID Photo

Turn one supplied portrait into an identification photo.

You run on a normal Multica agent: you have a shell, the workspace files, and
the media tools installed in this image. Do the work yourself.

## Inputs

- `prompt`: the user's instruction (required).
- One attached image, materialised under the task input directory. Read the
  exact path from the task context; do not guess it.

## Steps

1. Locate the single input image under the task input directory.
2. Run the fixed transform:

   ```bash
   convert "<input>" -resize 600x800^ -gravity center -extent 600x800 -strip "<output>"
   ```

3. Write the result to `<outputRoot>/id-photo.png`. Resize and crop only by the
   fixed geometry above; never crop to a face you chose yourself.

## Required outputs

- `<outputRoot>/id-photo.png` — one image, PNG, at most 25 MiB.

## Artifact manifest

Write `<outputRoot>/.multica/aurora-artifacts.v1.json` with the exact fields the
daemon validates:

```json
{
  "schema": "com.multica.aurora.artifacts",
  "version": 1,
  "task_id": "<the task id from your task context>",
  "skill_id": "id-photo",
  "artifacts": [
    { "source": { "type": "file", "path": "id-photo.png" },
      "kind": "image", "role": "primary", "name": "id-photo.png",
      "format": "png" }
  ]
}
```

The daemon recomputes size, SHA-256 and MIME from the file itself. A manifest
that names a path outside the output root, or more than 20 artifacts, or more
than 600 MiB in total, is rejected and the generation fails.

## Failure behavior

- If the input image is missing or unreadable, stop and report the failure. Do
  not synthesise an image.
- If the transform exits non-zero, report the failure with its stderr. Do not
  retry more than once.
- Never claim completion unless the output file exists and the manifest names it.
```

（其余 12 份同样式;`## Steps` 里的命令必须来自 Step 2 的对照表,不得凭印象写。）

- [ ] **Step 4: 写完其余 12 份**

逐份按 Step 3 的模板落地。硬性要求:

- 每个会调用 provider 的 skill,Steps 里必须给出**完整端点、认证头、请求体字段、轮询方式**(异步的写清轮询间隔与超时),以及**先调用服务端记账路由再调 provider** 的顺序:

```bash
# Record the create attempt before spending a provider call. This route is
# bookkeeping only now: nothing enforces it, so call it first anyway.
curl -fsS -X POST "$MULTICA_SERVER_URL/api/agent/tasks/$MULTICA_TASK_ID/aurora-provider-runs/begin" \
  -H "Authorization: Bearer $MULTICA_TOKEN" -d '{"provider":"ark","operation":"create","model":"..."}'
```

- 每个 skill 的 `## Artifact manifest` 段写明它产出的 artifact 数量与 `kind`(与 `aurora.ExecutionPolicy(skillID)` 的 `Route` 契约一致,否则 Task 5 的删除会连带删掉校验依据)。
- 每一步都写清楚读哪个环境变量拿密钥(变量名在 Task 3 定);**不得**写"调用某个工具"。

- [ ] **Step 5: 运行守卫测试**

Run: `(cd server && go test ./internal/aurora -run TestWorkflowsDescribeRealStepsNotBrokerTools -count=1)`
Expected: PASS

Run: `(cd server && go test ./internal/aurora -count=1)`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add server/internal/aurora/workflows server/internal/aurora/workflows_test.go
git commit -m "feat(aurora): make the 13 skill documents executable by an ordinary agent"
```

---

## Task 2: 统一的节点镜像

**Files:**
- Modify: `docker/runtime/Dockerfile`
- Create: `docker/runtime/omp-version.txt`
- Delete: `deploy/aurora-sandbox/Dockerfile`、`deploy/aurora-sandbox/Dockerfile.egress`、`deploy/aurora-sandbox/docker-bake.hcl`

**Interfaces:**
- Produces: 单一镜像 —— `ENTRYPOINT ["/usr/local/bin/fleet-node","run"]`、`HEALTHCHECK fleet-node health`、`USER 10001:10001`、`/data` + `/secrets` 布局;可用入口 `/usr/local/bin/multica`、`/usr/local/bin/claude`、omp 的可执行入口。
- Consumes: Task 3 的环境变量名(密钥经 env 注入)。

- [ ] **Step 1: 写镜像契约测试**

`docker/runtime/Dockerfile.test` 是既有的通用 fixture;新增一份断言脚本 `scripts/check-runtime-image.sh`,检查最终镜像里:四个入口存在且可执行、`/data` 与 `/secrets` 属 `10001:10001`、`Config.Env` 里**没有**密钥值、`/usr/local/bin/claude --version` 与 omp 的版本探测可运行。

Run: `bash scripts/check-runtime-image.sh`（首次失败,脚本不存在)
Expected: FAIL

- [ ] **Step 2: 扩充 `docker/runtime/Dockerfile`**

在现有 31 行的基础上增加:omp 的安装(按 `docker/runtime/omp-version.txt` 锁定版本)、Aurora 需要的媒体工具(Chromium、FFmpeg、ImageMagick、poppler-utils、fonts-noto-cjk)、以及 bash(`bypassPermissions` 下的 skill 步骤要能在 shell 里跑)。保留 `git`、`ca-certificates`。

**不要**从这里安装 `deploy/aurora-sandbox/` 的任何东西:broker、vendor 树、`@anthropic-ai/claude-code` 的第二份来源。Claude 只保留一份(`docker/runtime/claude-version.txt`),并把它作为 `/usr/local/bin/claude` 的唯一来源。

- [ ] **Step 3: 构建并核对**

```bash
docker build -f docker/runtime/Dockerfile -t multica-runtime-node:dev .
bash scripts/check-runtime-image.sh multica-runtime-node:dev
```

Expected: 全部检查通过;`docker inspect` 的 `Config.Env` 里没有 `ARK_API_KEY`、`OPENAI_API_KEY`、`VOLC_ASR_API_KEY`。

- [ ] **Step 4: 删除两个独立构建入口与 bake**

```bash
git rm deploy/aurora-sandbox/Dockerfile deploy/aurora-sandbox/Dockerfile.egress deploy/aurora-sandbox/docker-bake.hcl
```

若部署仍需一个 build helper,在 `docker/runtime/` 下新建最小 `build.sh`,只构建这一个镜像、只打一个 tag。

- [ ] **Step 5: Commit**

```bash
git add docker/runtime scripts/check-runtime-image.sh
git commit -m "feat(runtime): build one node image with the daemon, Claude, and omp"
```

---

## Task 3: provider 密钥改由容器 env 注入

**Files:**
- Modify: `server/internal/fleet/model/aurora.go`、`server/internal/fleet/docker/provider.go`、`server/internal/fleet/docker/inspect.go`
- Modify: `server/internal/daemon/managed_secrets.go`
- Modify: `.env.example`、`fleet-config.example.json`
- Delete: `server/internal/fleet/docker/seccomp.go`
- Modify: `server/internal/daemon/managed_secrets_test.go`、`server/internal/fleet/model/aurora_test.go`、`server/internal/fleet/docker/aurora_test.go`

**Interfaces:**
- Produces: 四个固定的环境变量名 `ARK_API_KEY`、`OPENAI_API_KEY`、`VOLC_ASR_API_KEY`、`ANTHROPIC_API_KEY`,由 Fleet 写进节点容器 env,由 daemon 直接读取。
- Produces: `model.AuroraConfig` 删除 `SeccompProfile`、`AppArmorProfile`、`ProviderSecretFiles`、`ProxyImage` 四个字段。
- Consumes: `docker/runtime/Dockerfile` 的镜像契约(Task 2)。

- [ ] **Step 1: 写"密钥经 env、不经文件"的失败测试**

在 `server/internal/daemon/managed_secrets_test.go` 追加:

```go
func TestProviderSecretsComeFromTheEnvironment(t *testing.T) {
	t.Setenv("ARK_API_KEY", "ark-from-env")
	t.Setenv("OPENAI_API_KEY", "openai-from-env")
	t.Setenv("VOLC_ASR_API_KEY", "volc-from-env")
	secrets, err := loadManagedProviderSecrets()
	if err != nil {
		t.Fatalf("loadManagedProviderSecrets: %v", err)
	}
	env := secrets.mcpBrokerChildEnv() // 名称随后调整;先钉住"值来自 env"
	if !slices.Contains(env, "ARK_API_KEY=ark-from-env") {
		t.Fatalf("provider key did not come from the environment: %v", env)
	}
}

func TestProviderSecretsRejectMissingValues(t *testing.T) {
	// 三个键里缺一个 → 返回错误,不静默降级成空字符串
}
```

Run: `(cd server && go test ./internal/daemon -run 'TestProviderSecrets' -count=1)`
Expected: FAIL —— 现有实现从 `/run/secrets/*` 读文件

- [ ] **Step 2: 改 daemon 侧的读取**

`server/internal/daemon/managed_secrets.go`:把 `readManagedSecretFile` 的固定路径读取改为读环境变量;保留"缺一个就报错、不静默降级"的行为。`claudeChildEnv` 继续提供 `ANTHROPIC_API_KEY`,其余三把按 skill 需要注入**同一进程**的环境(模型与脚本同进程,这是本方向接受的形态)。

- [ ] **Step 3: 删掉配置里的密集字段并放宽 env 禁令**

`server/internal/fleet/model/aurora.go`:

- 删除 `ProxyImage`、`SeccompProfile`、`AppArmorProfile`、`ProviderSecretFiles` 四个字段与 `Validate()` 里对应四段校验;`egressProxySpec` 改为取 `Config.Image`(节点与 sidecar 同镜像,入口不同)。
- **放宽 `claude_env` 的密钥关键字禁令**:现在任何含 `API_KEY`/`TOKEN`/`SECRET`/`PASSWORD` 的键一律被拒(`claudeEnvSecretMarkers`)。改为只放行四个固定名字,其余仍拒:

```go
// providerEnvNames are the only secret-bearing keys the operator may inject
// through the Fleet config. They exist because the node runs an ordinary agent
// that calls the providers directly; the values must be supplied at deploy time
// from a 0600 file and must never reach the image, SQL, logs, or Git.
var providerEnvNames = map[string]bool{
	"ARK_API_KEY": true, "OPENAI_API_KEY": true, "VOLC_ASR_API_KEY": true, "ANTHROPIC_API_KEY": true,
}
```

并把这一条写进 `Validate()` 的注释,免得后来者以为禁令被整体移除。

- [ ] **Step 4: 改 provider 与 adoption 校验**

`server/internal/fleet/docker/provider.go`:

- `NodeHostConfig` 删除 `seccomp=` 与 `apparmor=` 两个 SecurityOpt(Aurora 分支只剩 `ReadonlyRootfs` 与三处 tmpfs);保留 `CapDrop: ["ALL"]` 与 `no-new-privileges`。
- `nodeEnv` 增加四个 provider 变量(从配置取),删除 `providerSecretMounts` 与 `/run/secrets` 挂载。

`server/internal/fleet/docker/inspect.go` 的 adoption 校验按同一组期望值重算;`reflect.DeepEqual` 的 `SecurityOpt` 与 `Mounts` 期望随之上移。

- [ ] **Step 5: 文档与样例**

`.env.example` 增加四个变量(值留空,注释写明"部署时注入;容器 env 对容器内进程可读");`fleet-config.example.json` 删除 `proxy_image` / `seccomp_profile` / `apparmor_profile` / `provider_secret_files`,增加 `claude_env` 里的四个键。

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

Run: `(cd server && go test ./internal/aurora -run TestWorkflowsDescribeRealStepsNotBrokerTools -count=1)`
Expected: PASS。**失败则停止本任务** —— 删除会让内容无法复原。

同时确认 13 份文档里每个 provider 调用都写全了端点、认证头、请求体与轮询方式:

Run: `grep -L "https://" server/internal/aurora/workflows/*.md`
Expected: 只列出纯本地的 skill(id-photo、resume 等)。任何出现在列表里的 provider skill 都还没写完。

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

## Task 7: 端点回传与结算的一次完整往返

**Files:**
- Modify: `server/internal/service/aurora_completion.go`(只在必要时)
- Create: `docs/superpowers/plans/2026-10-08-aurora-agent-skill-runtime-acceptance.md`

**Interfaces:**
- Consumes: Task 1–4 的全部改动。
- Produces: 一次真实往返的证据记录。

- [ ] **Step 1: 单元层面确认结算未变**

```bash
( cd server && go test ./internal/service -run 'Aurora' -count=1 )
( cd server && go test ./internal/handler -run 'Aurora' -count=1 )
```

Expected: PASS。`settleAuroraOnCompleted` 仍要求至少一个已提交资产,否则走退款 —— 这条不变。

- [ ] **Step 2: 在 Docker Desktop 上跑一次真实往返**

```bash
docker build -f docker/runtime/Dockerfile -t multica-runtime-node:dev .
make up C=api,fleet
# 用 .env 注入四个 provider 密钥与 AURORA_SANDBOX_IMAGE=<上面的镜像>
MULTICA_RUN_DOCKER_INTEGRATION=1 make env-exec ARGS="-- pnpm exec playwright test --project=fleet-docker"
```

Expected: 生成 → 节点领取 → 模型用普通工具执行 → 产物入库 → 结算;浏览器 trace 与账本断言齐全。

- [ ] **Step 3: 逐个 skill 跑一遍**

13 个 skill 各跑一次,记录:输入、实际执行的命令、产物、账本变化。**未授权的真实 provider 调用记 SKIP 并写明原因,绝不记 PASS。**

- [ ] **Step 4: 写验收记录**

新建 `docs/superpowers/plans/2026-10-08-aurora-agent-skill-runtime-acceptance.md`,逐条写代码 HEAD、镜像 digest、命令、观察结果、资产与账本断言。不得记录密钥。

- [ ] **Step 5: Commit 并开 PR**

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
| provider 密钥 | 只读文件挂载,只有 broker 子进程拿到路径 | **容器 env**,对容器内任何进程可读,`docker inspect` 亦可见 |
| 计费 create-once | broker 强制:重试不会第二次提交 create | **仅记账**。模型直连 provider,`ambiguous` 冻结不再有强制力;重复提交计费请求成为可能 |
| 产物 manifest 的作者 | broker(`manifest.mjs`) | **模型**。契约不变:daemon 只当路径清单,自行重算 size/SHA/MIME 并按 `Route` 校验 |
| 13 个 skill 的实现 | 9 个受控 MCP 工具 + vendored 上游树 | **13 份可执行文档**;上游树与补丁随目录删除,来源记录不再有库存 |
| 模型可用的工具 | Bash/Read/Write 等被 deny,只能调 9 个 broker 工具 | **普通 agent 的全部工具**(`bypassPermissions`) |
| 平台要求 | 需要能加载 AppArmor 的 Linux 引擎 | **Docker Desktop 即可** —— 排除它的唯一理由是 AppArmor |

---

## 仍未执行与阻塞(不得声称已通过)

| 项 | 状态 |
| --- | --- |
| 13 份 skill 文档的内容 | **未起草**。Task 1 是内容工作,若某一步缺少可对照的实现,必须停下来问,不得凭印象写 |
| 统一节点镜像 | 未构建。omp 的安装方式与版本需要先确认(是否 npm 分发) |
| Docker Desktop 上的端到端往返 | 未运行 |
| 真实 provider 调用 | 未授权、未运行;需要三把真实密钥与预算 |
| 隔离验证 | **本方向下不存在**。不要把它列为"待补",它已被明确放弃 |
| `aws-deploy` 仓库的 `AURORA_SANDBOX_IMAGE` / `APPARMOR_PROFILE` / `AURORA_EGRESS_*` | 未处理(独立仓库) |

---

## Spec Coverage 自审表

| 用户决策 | 任务 | 关键证据 |
| --- | --- | --- |
| 13 skill = 13 agent + 13 skill,不用 MCP tool | 1、4 | `TestWorkflowsDescribeRealStepsNotBrokerTools`;`TestAuroraTaskGetsTheOrdinaryExecutionSurface` |
| 完全复用 issue 干活流程 | 4 | Aurora 分支删除后 `runTask` 无 Aurora 条件;skill 由既有 `ensureTaskSkillBundles` 下发 |
| `deploy/aurora-sandbox/` 完全删除 | 5 | 全仓 grep 无残留;`git ls-files deploy/aurora-sandbox` 为空 |
| 不用 AppArmor、不用自定义 seccomp | 3、5 | 字段删除;`seccomp.go` 删除;`NodeHostConfig` 只剩 tmpfs 与只读根 |
| 密钥经 `.env` 进容器 env | 3 | `TestProviderSecretsComeFromTheEnvironment`;镜像 `Config.Env` 无密钥 |
| 不做服务端 provider 代理 | 1、3 | skill 文档里是 provider 直连;没有新增服务端转发路由 |

---

## 执行交接

**Plan saved to `docs/superpowers/plans/2026-10-08-aurora-agent-skill-runtime.md`.** 两种执行方式:

1. **Subagent-Driven(推荐)** —— 每个任务派一个新的子代理,任务之间由我审查。Task 1 是内容工作,适合逐份审。
2. **Inline Execution** —— 在本会话内按 executing-plans 批量执行,带检查点。

**必须先确认两件事:**

- **omp 怎么装、装哪个版本。** 我只知道它在 `scripts/agent-cli-command-names.txt` 的名单里,没查过它的分发方式。这决定 Task 2 能否落地。
- **13 份文档里的 provider 调用细节由谁提供。** 我可以从现有实现与 vendor 上游文档抽取(Task 1 的做法),但抽取出来的端点与参数需要你或熟悉 provider 的人过一遍 —— 写错了不会有测试拦住,只会表现为生成失败。
