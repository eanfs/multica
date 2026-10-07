# 2026-10-07 Aurora Cloud Runtime AWS 部署验收记录

> 本文是 AWS 部署的原始证据记录，待归入 `docs/superpowers/plans/2026-10-06-aurora-cloud-runtime-acceptance.md` 的 AWS 小节。

## 环境

| 项 | 值 |
| --- | --- |
| 实例 | `i-038c12c8614eb2d55`（ap-southeast-1，t4g.xlarge / arm64） |
| 系统 | Amazon Linux 2023，kernel 6.1，Docker 25.0.16（AL2023 仓库仅提供 25.x） |
| LSM | selinux；无 apparmor（`--security-opt apparmor=` 被静默忽略）—— owner 选择「显式承认不启用」（空 `apparmor_profile`） |
| 域名 | `mca` = web(3009)，`mcapi` = API(3010)，`aod` = Aurora(3012) |
| 数据目录 | `/data/multica`（上传 `/data/multica/uploads`，运行时 `/data/multica/aurora`） |
| 部署方式 | `aws-deploy/scripts/deploy-multica.sh`（SSM Run Command）；镜像 linux/arm64，ECR 不可变 tag |
| 本次 tag | `v0.0.0-9fa8e1990`（显式指定：HEAD 已移到纯文档提交 `a3fc5348c`，无匹配镜像；脚本以 `ImageNotFoundException` 干净失败、未改动线上） |

## 结论：端到端成功

| 环节 | 证据（实例数据） |
| --- | --- |
| 生成 | `aurora_generation.status = completed`（poster） |
| 产物 | `aurora_artifact_staging`：provider_import / committed / 5,031,540 bytes |
| 资产 | `aurora_asset`：kind=image，media_url = https://mcapi.apexxai.net/uploads/workspaces/<ws>/aurora-artifacts/<gen>/primary-1.png（公网 200 image/png，2496x1664） |
| 结算 | `credit_ledger`：deduction -76；先前失败一次已 refund +76 |
| 节点 | `fleet_nodes`：running/running；容器 Up (healthy)；`aurora_sandbox_node`：online |
| 执行阶段 | 节点日志 runtime_started → first_output_received → first_tool_use → turn_completed；claude status=completed |
| broker | 工具调用 `mcp__aurora__aurora_seedream_generate` 被允许（非权限拒绝） |
| 出口 | egress sidecar 记录 host ark.cn-beijing.volces.com port 443 decision allowed |
| UI | 浏览器（Orca computer-use 驱动）：Poster 面板 Done + Result；My works 该条 Done, 76 credits；Library 可 Download |

## 真机暴露并修复的缺陷

| # | 现象 | 根因 | 修复（提交） |
| --- | --- | --- | --- |
| 1 | 节点引导从未写入（卷为空、无 sidecar、操作 create/applying/unavailable） | 引导 helper 用 User 10001:10001；Docker 25 在归档拷贝（CopyUIDGID）时按整串 user 规格做 chrooted getent，uid:gid 形式必失败（getent unable to find entry 10001:10001）。--user 10001 / aurora / 0 均成功，且 --user 10001 实际仍是 uid=10001(aurora) gid=10001(aurora) | `1bb9b9043`（multica）：helper 统一使用纯数字 uid 常量 + 反回归测试 |
| 2 | 节点退出 1：provider credential openai is invalid: secret file must not be empty | 部署对未 seed 的可选提供方写了 0 字节文件并列进配置；守护进程约定「缺失被容忍、空文件是致命错误」 | `dd4f510`（aws-deploy）：只渲染/创建真正 seed 过的提供方，缺 anthropic 时 fail-closed |
| 3 | 产物回写 422 asset media_url is not a fetchable http(s) URL | LocalStorage 未配置 LOCAL_UPLOAD_BASE_URL 时只存站内相对路径，审核 parseAssetURL 直接拒绝 | `4f2df3a`（aws-deploy）：overlay 注入 LOCAL_UPLOAD_BASE_URL=${MULTICA_PUBLIC_URL} |
| 4 | 每次改镜像摘要后 workspace 永久 503 fleet conflict | 存储的 create 意图 request_hash 绑定旧请求（含镜像摘要），重放校验正确拒绝；而清场用的 delete 操作因节点无容器永不执行，自愈路径被待决 delete 挡住 | `9fa8e1990`（multica，Task 27）：镜像级变化在同一身份上重新武装 + 无容器排队 delete 可执行，含 3 个回归测试 |
| 5 | 部署下发 MaxDocumentSizeExceeded（97KB） | 内联 SSM 脚本随运行时负载增长 | gzip+base64 负载（101,689B → 47,973B）+ 下发前本地尺寸守卫（Task 26） |
| 6 | 远端 MCA_FLEET_IMAGE: unbound variable | 远端 .env 渲染引用了 header 未导出的变量 | header 补该变量（控制器逐项比对远端引用 vs header 定义后修复） |

## 运行时姿态（已生效）

- `apparmor_profile: ""`（显式承认不启用）：部署跳过 profile 安装并打印姿态；Fleet 启动日志 aurora apparmor: disabled (operator-acknowledged)；配了 profile 时两侧 fail-closed。
- 仍强制：内联 seccomp、cap-drop ALL、no-new-privileges、只读 rootfs、internal 工作区网络 + egress 白名单、非 root 10001、资源上限、宿主 SELinux。
- max_nodes=2、沙箱内存 2 GiB（16 GiB 共置宿主，8 个栈），可由 app.env 提高。
- provider 凭据：anthropic-api-key 与 ark-api-key 为 46B / 0600 / 属主 10001；未 seed 的 openai / volc-asr 不写入、不挂载。

## 未运行 / 残留

| 项 | 说明 |
| --- | --- |
| 逐步阶段时间线 | daemon 只把阶段写入日志，未落库、未暴露；要真时间线需新增持久化 + 接口 |
| egress 间歇 target_refused | sidecar 对 host.docker.internal:3010 偶发拒绝（同主机另有大量 allowed）；守护进程轮询兜底，不影响执行；待收敛 |
| 真实 provider/Claude 门控烟测（Plan D 7 个） | 仍按仓库约定记为 skipped；本次是部署侧真机验证，未改这些门控 |
| apparmor/SELinux 内核级验收矩阵 | 目标宿主无 AppArmor；owner 已选择显式承认的空姿态 |

## 内容侧观察（非缺陷）

- 提示词 `生成国庆节的祝福图片` 被方舟以 `InputTextSensitiveContentDetected` 拒绝（状态 400，未扣费）—— 属服务商输入文本审核；改写为 `节日庆祝主题海报，红色与金色配色，喜庆氛围，简洁大气的中文排版` 后成功出图（1440x3040）。
