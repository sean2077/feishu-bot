# 验证与验收

本页定义测试、平台 CI、发布和真实验收各自能证明什么。报告结果时只写实际运行过的那一类，不用一类代替另一类。

| 证据 | 来源 | 能证明 | 不能证明 |
|---|---|---|---|
| 模拟与本地替身测试 | `go test`；原生协议 fixture 使用真实 CLI 与本地确定性模型 | 代码契约、状态不变量、固定 CLI 版本的协议行为 | 真实飞书投递、客户端体验、真实模型能力 |
| 平台 CI | 实际完成的 GitHub Actions 运行 | 对应提交在所列 runner 上通过同样的检查 | 其他提交、其他平台、真实服务 |
| 发布 | 实际完成的 tag 工作流及公开 Release 页面 | 该版本附件已构建、验证并公开 | 部署后的飞书／模型可用性 |
| 真实验收 | 独立飞书应用、测试聊天和所选模型服务 | 所记录环境中的端到端行为 | 未记录的客户端、模型或配置 |

工作流存在不代表已经通过；`skip` 不代表已验证；本地模拟成功不代表手机、桌面和真实模型均可用。

## Go 检查

```sh
go test -race ./...
go vet ./...
go build -trimpath -o ./bin/feishu-bot ./cmd/feishu-bot
```

需要 Go 1.25+（`go.mod` 为 `go 1.25.0`）。生产构建使用 `CGO_ENABLED=0`，竞态检测需要平台 C 工具链。Go 测试使用临时 SQLite／目录、虚构身份、本地 HTTP／WebSocket 和可控的原生进程替身，不读取部署配置，也不调用真实服务。

测试与被测包同目录，使用 `*_test.go`；平台专用测试使用 `_windows_test.go` 等文件名或构建约束。真实 CLI fixture 位于 `internal/native`、`internal/integration` 和 `cmd/benchmark-native`。CI 不设覆盖率门槛；新增行为应覆盖可观察的契约结果，而不是追求覆盖率数字。

| 范围 | 检查重点 |
|---|---|
| config／cli | 严格字段与限制、模型配置白名单、无覆盖 init、分阶段 doctor、受限磁盘扫描、显式聊天探针、关闭顺序 |
| store／workspace | 旧 schema 增量迁移、原生 ID、聊天内会话名称与 resume、launch identity、未知写入恢复、路径与快照 |
| gateway | 授权及零副作用、忙时输入／补充／compact、审批、历史范围、图片批次、按实际字节限卡与完整文件、模型切换边界、签名快捷操作、容量准入（聊天额度、内存阈值、观测不可用）与复用保温 |
| feishu | 原始事件、明确 @、逐群免 @、交付步骤日志、限流与未知结果分类、CardKit 序号和过期同卡替换、快照恢复与保留、有界文件与可取消连接 |
| native | stream-json、公开内容归属、会话显示名、权限／MCP 控制、原生配置、auto 分类器文本与本地分类器注入、项目系统提示词注入、热续轮、停止与恢复 |
| process | 子进程组／Job Object、父进程先退出后的派生进程清理、终止确认、进程树内存观测；Darwin 还核对进程表身份和僵尸状态 |
| daemon | install/uninstall/start/stop/restart/status/logs；env 文件、二进制复制、离线预检、幂等、同名任务所有权、stale lock 恢复、日志轮转、任务动作无脚本 |
| integration | 真实原生 CLI + 网关 + SQLite 配合本地模型：审批写入、追问、完整交付、会话显示名、空闲回收与重启续接 |

### 回归要点

新增或修改行为时，测试观察外部结果或状态不变量：不以“调用了 stop”代替进程已退出，不以“发送函数返回”代替完整交付，不以“模型说记得”代替相同 session ID。

- **交付**：覆盖中文、emoji、JSON 转义、代码块、元数据和按钮的实际序列化预算；长答案保留完整文件，旧 outbox 加入新按钮也不静默丢字。明确限流按服务端期限重试；未知卡片创建／文件发送不自动重放。在预览创建与最终落库之间、消息确认与本地映射之间分别注入中断，确认恢复沿用原交付身份与同一卡片。模型结束但文件未确认时卡片不标记完成；此时停止会取消补发并持久关闭原卡片，保留原文和所有权。附件阻塞按同一轮次的触发来源计算，更早轮次的失败附件不阻塞后续回复；审批与追问卡超预算时保留选项值和按钮，并向模型返回可读错误。
- **快捷操作与模型配置**：验证实际消息、聊天、会话、轮次和版本绑定，覆盖伪造、跨聊天、过期、重复点击及重启后的旧按钮。模型配置切换覆盖普通忙时输入、补充、compact、审批、后台工作与未完成交付，只在可确认的会话边界应用并保留原生 ID；移除配置后保留输入、停止自动重试且只提示一次。项目默认配置只作用于未显式选择的会话，显式 `default` 优先；改变项目默认后按同一原生 ID 续接时切换模型并保留历史，凭据和端点不变。重新取回已交付附件不唤醒模型，也不能通过符号链接越出快照目录。
- **预检索上下文**：配置校验覆盖非 `readOnly` action、未声明参数、`workspaceFile`、参数模板未放在字面量 `--` 之后、超时超过 action 上限和 `maxChars` 范围。网关测试确认只在新一轮人类输入触发，管理命令、compact、轮次中补充／普通输入和纯图片跳过；预检索与 worker 启动并行（双方都被 fake 阻塞后同时放行）；超时、非零退出、空输出和参数失败时输入照常发送、写 `pre_context_failed` 且每次都写；输出按 UTF-8 字符边界截断并标记 `truncated`；`turn_result` 带 `preContext` 和 `preContextMs`；含 shell 元字符和以 `-` 开头的提问作为单个 argv 原样传入。以上是本地模拟证据，不代表真实检索服务的质量或延迟。
- **追问表单**：用官方形状的 `form_value` 经生产回调解析与网关提交，覆盖单／多选、自定义覆盖、空白回退、缺题、非法选项与字段、长度上限、跨操作者、版本／会话／卡片绑定、过期与重放；持有 SQLite 连接让会话在提交前切换，确认旧表单不能迟到落库。真实原生组合 fixture 也用新表单完成 AskUserQuestion，再在后续本地模型请求中检查自定义文字与多选答案；`/answer` 保留独立恢复回归。
- **Markdown 展示**：覆盖四表格预算、第五个表格起的文字降级、代码块中的样例、普通管道符、开放及超长围栏、UTF-8 截断和最终状态追加后的二次预算。实际本地 CardKit HTTP 流式请求仍更新原实体；完整 outbox 正文与 `reply.md` 保持原文一致，文件确认前没有 Done。该证据不替代手机／桌面的真实渲染验收。
- **静默连接与重连**：本地真实 WebSocket 对端持续读取以保持 TCP 可写，但停止 Pong 和事件，确认 watchdog 关闭旧 socket、重新连接并接收事件；正常空闲 Pong 不误断。重连时保持旧 handler 未完成，确认它不被取消、新连接其他聊天可处理、同聊天排队顺序保留、晚到旧 ACK 不关闭新连接；服务停止取消并回收连接与处理。测试缩短私有时钟参数，生产仍保持 1 秒检查／30 秒无入站等待。
- **话题工作区**：临时目录验证显式话题绑定、未绑定默认目录、保留已有 `.git` 文件、独立文件与附件范围、缺失目录及状态树重叠拒绝。网关 Worker 实际收到所选 cwd；重启后改变绑定时，已有身份和 legacy 无身份原生会话均拒绝迁移，显式 `/new` 才进入新目录；本地终端交接执行同样保护。`doctor` 不替管理员创建工作目录。
- **话题群按需历史**：默认 `topicHistory: "topic"` 行为不变；非 chat 配置拒绝 `scope=chat`。验证群时间线时间范围、已观察/未观察/跨群 threadId 拒绝、`expandThreads` 按最近回复排序、20 话题上限、先减回复再减话题的预算顺序、单个话题失败时跳过且全部失败才报错、省略/失败统计、游标不能切换 scope、审计不含内容，以及同群其他话题房间的回填和不跨群回填。真实飞书测量只确认了 chat 容器仅返回话题根、thread 容器忽略时间过滤；不能把本地假 transport 当作真实 API 完整性证明。
- **私聊跨群历史与轮次污染**：验证 `dms[].historyChats` 只接受显式群、名称/ID 唯一；发起人必须是 DM 用户且为维护者或显式成员，`allowAllMembers` 不授权；无活动人类输入、群读取跨群、目标为兜底或私聊均拒绝。跨群时间线/threadId/expandThreads 复用现有预算与游标，结果不含附件、写 `history_cross_chat_read` 且不含正文；游标绑定来源 DM 和目标 chat。污染状态覆盖后台任务仍在运行、下一轮人类输入清除、`/stop`/超时 fail-closed 和只读/写工具分流；审批写 `tainted_turn_prompt`，并验证 `send_file` 与非只读 action。真实 CLI 用例覆盖 PreToolUse 阻塞审批期间工具未执行，以及真实子 agent 内的 Write 仍经过同一 hook；未做真实飞书跨群读取验收。
- **回调截止与群级额度**：占用实际 SQLite 单连接，确认操作已在等待存储后再取消回调；补充入队、快捷声明、批准、拒绝和提交回答都不得迟到提交，未接受的控件仍可重试。审批持久提交后、回调返回前取消时，原生仍收到已接受的决定。不同话题的消息与卡片补充同时竞争最后一个输入额度，只能有一个成功。回执覆盖确认创建后清除响应丢失／重启、未知创建继续核对，以及 OnIt 明确失败与 Done／取消目标同时到达。
- **daemon 升级**：在二进制准备、服务注册、元数据发布和启动边界注入失败，核对恢复副本、旧文件／服务状态，以及启动结果未知时保留的新安装。Linux 使用真实 `linuxManager` 与临时目录中的 `systemctl` 替身，检查 manager 命令不带 unit 参数、先按候选定义清除启用状态再恢复旧定义、`enabled`／`disabled` 恢复、特殊状态提前拒绝及恢复失败时保留副本；这不代表操作过真实 systemd。Windows 设置 `FEISHU_BOT_TASK_TEST=1` 后，`TestWindowsTaskRegistration` 用唯一临时任务走生产 COM 查询和 `schtasks` 注册／导出／恢复／删除路径，验证确实缺失、已有任务、取消查询及禁用状态恢复，清理失败即测试失败。未设置变量时该用例明确跳过，Windows 交叉编译不能替代它。

## 平台 CI

工作流定义在 `.github/workflows/`；三个工作流共用组合 action [`.github/actions/verify`](../.github/actions/verify/action.yml)：`go test -race -count=1 -timeout 360s ./...`、`go vet ./...`、进程监督器单测，以及安装并核对固定原生 CLI 后的[协议回归](#真实-claude-协议-fixture)。

| 工作流（名称） | 触发 | 内容 |
|---|---|---|
| `ci.yml`（`Verify Go gateway`） | push `main`、PR、手动 | 见下文；同一 ref 的新运行取消旧运行 |
| `native-endurance.yml`（`Native endurance observations`） | 每周日 02:00 UTC（仅默认分支）、手动、修改该工作流或 `cmd/benchmark-native/**` 的 PR | Linux／Windows 持续负载观察；参数见[运行基线](runtime-baseline.md#并发与持续负载) |
| `release.yml`（`Release`） | 仅推送稳定版本 tag | 见[版本发布](releasing.md) |

`Verify Go gateway` 的 job：

- **cross-platform**（每项 25 分钟）：Ubuntu／Windows × Go 1.25／1.27，另有 Go 1.27 的 Linux arm64（`ubuntu-24.04-arm`）、macOS amd64（`macos-15-intel`）和 macOS arm64（`macos-15`）。每项运行共用 action、无 CGO 生产二进制的[实际构建产物回归](#实际构建产物回归)及 TLS fixture 收尾。仅在 Windows／Go 1.27 上运行 `TestWindowsTaskRegistration`；仅在 Ubuntu／Go 1.27 上记录渲染分配基准和 `gofmt -d` 建议补丁（artifact `gofmt-patch`，格式差异不使任务失败，gofmt 自身出错才失败）；Ubuntu／Windows 的 Go 1.27 记录资源矩阵与短时并发观察（artifact `native-resource-observations-<OS>-<ARCH>`）。
- **release-tools**（10 分钟）：`python -m unittest discover -s .github/scripts -p 'test_*.py'` 离线检查版本、发布说明、打包与发布状态机（不上传附件），并用固定校验和的 actionlint 检查工作流。
- **candidate-native**（20 分钟）：仅手动运行且填写 `candidate_sdk_version` 时执行，见下节。

## 真实 Claude 协议 fixture

协议测试使用原生 **Claude Code 2.1.321** 和仅监听回环地址的确定性模型服务；不使用真实模型凭据，不连接真实飞书。指向已安装的固定版本 CLI：

```sh
CLAUDE_NATIVE_PATH=/absolute/path/to/claude go test -race -count=1 -timeout 180s -v ./internal/native ./internal/integration ./cmd/benchmark-native
```

PowerShell：

```powershell
$env:CLAUDE_NATIVE_PATH = "C:/tools/claude.exe"
go test -race -count=1 -timeout 180s -v ./internal/native ./internal/integration ./cmd/benchmark-native
```

未设置变量时真实 CLI 用例明确 skip，普通单元测试继续运行；不得把 skip 写成真实协议已验证。CI 通过 npm 安装 `@anthropic-ai/claude-agent-sdk@0.3.321` 携带的原生 CLI（`--ignore-scripts`），核对 runner 平台／架构和 `--version` 为 2.1.321，缺失或不匹配即失败。这是获取 fixture 的测试依赖，生产服务不运行该 SDK。

- **组合回归**通过网关实际审批卡和追问表单完成原生 Write／AskUserQuestion，混合自定义与多选回答，核对下一次本地模型请求中的工具结果、临时文件内容与超长回复导出，确认文件交付回执之前没有 Done；随后触发空闲回收、停止并重开数据库，检查三代原生进程保持同一会话 ID 和历史。飞书传输和模型仍是本地替身。
- **文件路径 fixture** 让固定 CLI 实际执行 Read／Grep／Glob，检查当前工作区、当前话题附件、其他话题附件、波浪号、首尾空白和符号链接路径；Windows 另覆盖 MSYS 路径。允许路径返回测试文件内容，受保护路径返回工具错误且不含受保护内容；无法创建符号链接的 runner 会明确记录跳过的子项。原生路径规范化顺序只能由这类实际工具调用证明，不能由直接构造 hook 输入的单元测试推断。`FuzzOtherTopicFilePathsRemainDenied` 另检查任意子路径仍被附件范围规则拒绝，不代表真实模型效果或同账号 OS 沙箱隔离。

原生协议是版本敏感边界。更改 CLI 版本、控制帧、环境继承或工具设置时，验证初始化、连续两轮、原生 ID、权限／追问控制、MCP、compact 和停止后 resume，并记录实际覆盖范围。合成 token 只检查采集／去重，不是账单。

auto 模式另有本地证据：固定 2.1.321 + 假 Messages 服务验证本地分类器请求、拒绝／放行／无法评估三段 tool_result 文本、卡片提示和审计；真实网关分类器的策略、约 40 秒延迟、计费和账号可用性属于真实模型验收，不能由该 fixture 证明。

**候选版本**：手动运行 `Verify Go gateway` 时填写 `candidate_sdk_version`（`0.X.Y`）与 `candidate_native_version`（`X.Y.Z`），`candidate-native` job 在 Linux x64 安装该官方包、要求 `--version` 精确等于 `<candidate_native_version> (Claude Code)`，再运行上面三个包的协议回归。它不运行产物旅程或其他平台；成功也不会修改仓库固定的版本，不能把其他版本标为已验证。

## 实际构建产物回归

常规 CI 在各平台用 `CGO_ENABLED=0` 构建写入当前提交身份（版本 `0.0.0-ci.<commit>`）的生产二进制，由 `.github/scripts/artifact_fixture.py` 写入 `FEISHU_ARTIFACT_PATH`、`FEISHU_ARTIFACT_VERSION`、`FEISHU_ARTIFACT_COMMIT`、`FEISHU_ARTIFACT_SHA256` 和 `FEISHU_ARTIFACT_BUILDINFO`，再执行 `internal/cli` 的 `TestBuiltArtifact*`。提供产物路径时，缺少身份字段、平台不符或哈希不匹配直接失败。版本、初始化、本地诊断和基础模型探针直接调用这个二进制，不能由测试链接的替代服务冒充。

**完整旅程**：本地 TLS／WebSocket／REST 飞书替身与真实原生 CLI 依次完成：MCP 历史读取、普通成员审批被拒、维护者批准 Write、断线重连后回答原追问、同卡长回复、完整文件确认之前没有 Done、`/stop`、服务进程重启和同一网关／原生会话的历史恢复。同时断言模型请求次数和卡片／文件数量，防止重复工具执行或重复交付被成功结果掩盖。

**升级旅程**：另行构建固定基线提交 `f727a9cd074e1001000fa7c14ecad38d25e369d1` 的旧二进制（`FEISHU_PREVIOUS_ARTIFACT_*`），以真实 `run --service --env-file --log-file` 进程完成首轮，再在同一私有安装路径替换为当前产物并恢复。检查网关／原生 ID、历史、配置／环境／项目文件字节及服务日志前缀；测试侧以只读 SQL 读取旧数据库，不在新服务启动前替它迁移。新旧产物的 commit、version 和 SHA-256 必须不同。它验证服务进程模式的替换与恢复，不替代各 OS 服务注册器或生产租户升级验收。

**测试 CA**：Linux 的假 CA 只放入测试子进程的证书环境。Windows／macOS 使用系统校验，仅当 `GITHUB_ACTIONS=true`、`RUNNER_ENVIRONMENT=github-hosted` 且显式 `FEISHU_ARTIFACT_SYSTEM_TRUST=1` 时允许安装临时 CA；普通开发机和 self-hosted runner 拒绝这类信任写入。生产路径和脚本都保持正常 OS 授权与 TLS 验证。

- Windows 要求运行身份已有管理员权限，导入 `LocalMachine Root` 后按 SHA-1／SHA-256 双指纹确认、删除和复查；权限不足直接失败，不提权。
- macOS 另要求 `FEISHU_ARTIFACT_MACOS_TRUST_SCOPE=hosted_job` 与 `FEISHU_ARTIFACT_RUNNER_LABEL` 为 `macos-15`／`macos-15-intel`，并把 repository、workflow ref、run、attempt 和 job 身份绑定到 journal。证书放在专用用户 keychain，经 runner 已有的 `sudo -n` 写入仅限 SSL／`open.feishu.cn` 的管理员信任。每次测试仍须确认服务与原生进程停止、恢复原 keychain 搜索列表并删除专用 keychain，任一步未确认即失败。CA 私钥只在 fixture 进程内，不进入 journal、缓存或 artifact。
- macOS 管理员信任的生命周期是整个一次性 job：收尾记录 `awaiting_hosted_runner_teardown`，由 GitHub 在 job 结束后退役 VM，而不声称测试已同步撤销系统信任（[GitHub 托管 runner 文档](https://docs.github.com/en/actions/how-tos/manage-runners/github-hosted-runners/use-github-hosted-runners)说明每个 job 使用新 VM 并在结束后退役；进程内无法观察退役，也不能据此宣称物理介质已擦除）。该信任在同一 job 后续步骤仍可能有效，因此这些 job 只使用测试身份、不接入真实飞书凭据，公开发布在独立 job。工作流以 `always()` 运行 `artifact_trust.py cleanup-all` 复查本地资源，macOS 上传 `tls-fixture-lifetime-*` 生命周期记录；旧的逐测试 journal 不会因新环境变量改用此模式。

**进程监督**：完整产物旅程由外层监督器 `.github/scripts/artifact_processes.py` 执行：Go 测试 420 秒截止，外层 450 秒截止，另有 35 秒回收预算。Unix 用持续存活的独立 session leader 追踪不同进程组，Windows 在恢复启动前把进程加入持有的 Job Object；Linux 核对 pidfd／启动身份，macOS 核对 SID／libproc 启动时间。退出码 124 表示超时且已完成回收，125 表示监督或回收无法确认，两者都不是成功。共用 action 在全部原生平台运行监督器的真实故障用例，覆盖正常父进程退出、命令崩溃、多进程组与硬超时。Unix 监督器自身被 SIGKILL、主动另建 session 的逃逸进程和整机强制结束仍依赖一次性 runner 回收；监督器不是容器隔离。

普通本地测试未提供产物或原生 CLI 路径时明确 skip；显式产物检查缺少要求的身份、旧二进制或固定 CLI 时不能报告完整成功。本地终端交接另有 Linux 真 PTY fixture（`internal/process`）验证前台进程组、终端输入、启动失败及取消后的恢复和后代清理；macOS／Windows 的实际交互终端体验仍需人工验收。

## 发布包验证

版本 tag 的提交在 Linux amd64／arm64、Windows amd64、macOS amd64／arm64 原生 runner 上运行共用 action，再验证将要上传的最终文件：解压便携包、核对 BUILDINFO 的版本／提交／SHA-256，从解包路径运行基本命令及上面的完整与升级旅程；安装包重复构建并逐字节比较，实际安装后从安装路径运行同一旅程，再卸载。安装前拒绝既有包、相关 MSI 产品或安装目录；安装部分失败也进入身份核验后的收尾，卸载失败或残留文件使验证失败并保留诊断。交叉编译成功不能代替目标架构或安装包的实际执行。步骤、产物和重跑规则见[版本发布](releasing.md)。

离线发布工具测试（`release-tools` job）、原生平台构建、真实 GitHub 发布和真实飞书／模型验收是四类证据。只有实际 tag 工作流完成并核对公开页面及附件，才能报告该版本发布成功。macOS 测试不代表提供了内置 daemon 或 Apple Developer ID 签名／公证。

## 飞书与模型真实验收

在独立应用、授权测试群及私聊中执行。至少准备维护者、普通获准成员和未授权成员，记录 OS、网关／Claude／客户端版本、配置摘要及结果；凭据留在运行主机，避免保存完整原始事件。接入前的分层检查见[首次接入](first-connection.md)。

1. **入口与权限**：分别测试明确 @、无 @ 引用、@全体、未授权作者、未授权群和私聊。在 `mentionOptional` 群测试未 @ 的获准文本、图片和文件能够触发，未授权作者仍静默，默认群不受影响；关闭开关并重启后恢复要求 @，已排队的未 @ 输入不再投递。拒绝应无正文入库、下载、回执或模型调用。测试 text、post、image、file、引用和线程；不完整输入明确拒绝。
2. **连续对话与补充**：整群范围的同群不同作者／线程保持同一会话。`sessionScope: "thread"` 群中两个顶层 @ 形成两个话题和会话，话题内 @ 续接、未 @ 不触发，重启后续接同一话题会话；回复、状态卡和文件留在话题内，历史只读本话题，顶层管理命令只提示；另在话题形式群确认首帖 @ 可建立会话、话题内卡片可更新。普通忙时输入进入原生流。`/btw`／`/ps` 不启动空闲任务，字面 slash 正文不变成管理命令。目标结束或 compact 等待时不越过边界。
3. **审批与追问**：普通成员不能批准共享外部写操作；维护者可批准／拒绝。整张表单支持单选、多选、自定义混合提交；非空自定义覆盖该题选项，空白回退选项，空提交提示缺少的题号并可补齐。选择不刷新共享卡片或覆盖正在输入的文字；分别用两名作者验证不混合个人草稿，第一位合法完整提交者生效。过期或已结束请求的按钮不能复活请求，`/answer` 保留恢复入口。手机与桌面分别检查输入、选择、取消、失败重试及客户端缓存表现。
4. **流式与完整交付**：同卡显示公开思考／答案，隐藏工具和签名；思考默认折叠、可展开。超长回复或必要附件未完成时没有 Done。人工核对未知上传结果后，才显式使用 `/retry-output`；不得通过盲目重发制造验收。
5. **图片**：默认逐条；开启窗口后检验相同作者／线程／引用的连续独立图片合批。每条群图片默认独立明确 @；`mentionOptional` 群按作者授权独立触发。数量、单文件、总量或下载失败时整批取消。
6. **停止与恢复**：在任务、审批、图片窗口、慢下载和最终交付时分别测试 `/stop` 和重启；未知外部写入不重放，已知卡片不再停留在流式状态。停止／回收／恢复保留原生 ID，仅 `/new` 新建；改变 cwd／配置身份会拒绝旧会话。`/rename`、会话恢复按钮和模型配置切换要求本聊天维护者权限；获准成员取回已交付附件仍受当前聊天和原输出所有权约束。
7. **容量**：设置较小的 `runtime.maxWorkersPerChat` 或 `maxRssMiB` 时，额度满的聊天收到“输入已保存”提示、`/inputs` 显示等待名额，其他聊天不受影响；空闲进程回收后自动续接原会话。对照[运行诊断](runtime-diagnostics.md#容量等待复用与回收)中的事件。
8. **用量**：无结果时未知；连续轮次、工具轮次、重启和重复结果不重复累计。查询不调用模型，不混入其他聊天、子代理或累计 modelUsage。
9. **继承与后台**：先用无副作用 hook／技能／MCP 验证配置来源，再测试实际需要的原生后台能力。只清理本次创建的测试任务，记录工具返回的 ID。后台输出归属不能借用随后出现的前台输入。
10. **服务生命周期**：Windows 登录任务直接执行安装目录中的二进制，不出现脚本或控制台窗口；`daemon status` 报告 PID／配置／日志；`daemon logs -f` 能跟上轮转；`daemon stop` 优雅退出并释放 `gateway.lock`；`daemon start`／`daemon restart` 后 `/status` 仍是同一 session ID 和原生 session ID。未经核实不要删除 stale lock；`daemon start --reclaim-stale-lock` 只清理已无进程持有的锁。`daemon uninstall` 删除任务／unit 但保留 config、service.env、日志和 state。
11. **其他机器人与提及**：显式群开启 `allowOtherBots` 后，其他机器人未 @ 的消息不触发、只在历史／背景可见；明确 @ 只触发一轮；同一话题同一机器人连续三次 @ 时第三次被截断并提示；人类消息后重新计数。`mention_bot` 只能由本轮全部 senders 均为 maintainer 的请求调用，目标必须在 `otherBots` 名单且有 `openId`；非名单目标、第二个目标、非 maintainer 均被拒。用真实飞书 post 验证 at 元素的 `openId` 才能产生 mentions 数组，appId 不行；对方回复该消息或同话题后续消息不触发新一轮。关闭 `allowOtherBots` 后工具不可用且行为与旧版一致。

手机和桌面分别记录。模型流式、多模态、工具和长上下文能力分别验收；基础 Messages 探针成功不替代这些结果。共享 cwd 的原生持久定时任务仍遵循 Claude 的项目级规则，不能宣称跨聊天隔离。

## 性能与回归边界

性能观察的口径、命令和历史数据由[运行基线](runtime-baseline.md)维护；本节只划定它们作为证据的范围。

- **本地查询**：大量历史下的查询用生产 Go SQLite 驱动、完整 schema、WAL 和单连接设置验证：

  ```sh
  go test ./internal/store -run TestInboxQueriesUseLiveIndexesWithLargeHistory -v
  go test ./internal/store -run '^$' -bench '^BenchmarkInboxHistory$' -benchmem -count=3
  ```

  执行计划测试检查群级在途计数、待处理房间发现和话题内有序队列使用对应部分索引，不扫描或重排全部历史。基准保持活跃输入数不变，比较 1,000／100,000 条终态及未知历史，并在独立临时数据库中比较加入活跃／排队索引前后的查询。它只测本地查询和反序列化，不测飞书历史 API、模型延迟或服务吞吐；记录平台、Go 版本和多次采样，不把一次观察当成性能保证。
- **资源矩阵与并发观察**：CI 的短时矩阵、6 秒并发阶段和渲染分配基准用于发现回归和核对机制，不证明长期稳定性、真实飞书延迟或整套服务内存降低的比例。
- **Endurance**：每周与手动的 10／30／60 分钟运行检查持续时长、并发交付、回收后身份和清理确认，内存与磁盘增长只作观测、不设阈值；只有实际完成的 workflow run 才是对应时长的证据，配置工作流本身不代表长时间运行已经验收。
- **迁移比较**：Go 迁移移除了每会话 Node worker，但没有预设内存降幅。历史 TypeScript 测试的通过记录是迁移参照，不计作 Go 回归覆盖；旧报告保留在历史迭代文档和 Git 历史中。
