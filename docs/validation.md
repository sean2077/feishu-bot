# 验证与验收

测试、平台 CI 和实际飞书／模型验收是不同证据。工作流存在不代表已经通过；本地模拟成功也不代表手机、桌面和真实模型均可用。

## Go 检查

```sh
go test -race ./...
go vet ./...
go build -trimpath -o ./bin/feishu-bot ./cmd/feishu-bot
```

Go 1.25+ 可构建；常规 CI 保留 Ubuntu／Windows 与 Go 1.25／1.27，并增加 Linux arm64、macOS amd64／arm64 的 Go 1.27 检查。生产构建支持 `CGO_ENABLED=0`，竞态检测使用 runner 的 C 工具链。Go 测试使用临时 SQLite／目录、虚构身份、本地 HTTP／WebSocket 和可控 Worker，不读取部署配置或调用真实服务。

测试与被测包同目录，使用 `*_test.go`；平台专用测试使用 `_windows_test.go` 等文件名或构建约束。原生协议 fixture 位于 `internal/native`、`internal/integration` 和 `cmd/benchmark-native`。当前 CI 不设覆盖率门槛；新增行为应覆盖可观察的契约结果，而不是追求覆盖率数字。

| 范围 | 检查重点 |
|---|---|
| config／cli | 严格字段与限制、模型配置白名单、无覆盖 init、分阶段 doctor、受限磁盘扫描、显式聊天探针、关闭顺序 |
| store／workspace | 旧 schema 增量迁移、原生 ID、聊天内会话名称与 resume、launch identity、未知写入恢复、路径与快照 |
| gateway | 授权及零副作用、忙时输入／补充／compact、审批、历史范围、图片批次、按实际字节限卡与完整文件、模型切换边界、签名快捷操作 |
| feishu | 原始事件、明确 @、逐群免 @、交付步骤日志、限流与未知结果分类、CardKit 序号和过期同卡替换、快照恢复与保留、有界文件与可取消连接 |
| native | stream-json、公开内容归属、会话显示名、权限／MCP 控制、原生配置、热续轮、停止与恢复 |
| process | 对应平台上的子进程组／Job Object、父进程先退出后的派生进程清理、终止确认；Darwin 还核对进程表身份和僵尸状态 |
| daemon | install/uninstall/start/stop/restart/status/logs；env 文件、二进制复制、离线预检、幂等、同名任务所有权、stale lock 恢复、日志轮转、任务动作无脚本 |
| integration | 真实 Claude + Gateway + SQLite 的审批写入、追问、完整交付、会话显示名、空闲回收与重启续接 |

新增或修改具体行为时，使用能观察外部结果或状态不变量的测试，不以“调用了 stop”代替进程已退出、不以“发送函数返回”代替完整交付、不以“模型说记得”代替相同 session ID。

交付回归同时覆盖中文、emoji、JSON 转义、代码块、元数据和按钮的实际序列化预算。长答案必须保留完整文件；旧 outbox 加入新按钮也不能静默丢字。明确限流按服务端期限重试；未知卡片创建／文件发送不自动重放。测试在预览创建和最终落库之间、消息确认和本地映射之间分别注入中断，确认恢复仍使用原交付身份与同一卡片。模型结束但文件未确认时，卡片不能标记完成；此时停止任务会取消补发并持久化关闭原卡片，保留原文和所有权。附件阻塞按同一轮次的触发来源计算：更早轮次的失败附件不阻塞后续回复；相似地，审批与追问卡超预算时保留选项值和按钮，并向模型返回可读错误。

快捷操作回归验证实际消息、聊天、会话、轮次和版本绑定，覆盖伪造、跨聊天、过期、重复点击及重启后的旧按钮。模型配置切换测试覆盖普通忙时输入、补充、compact、审批、后台工作与未完成交付，核对只在可确认的会话边界应用配置并保留原生 ID；移除配置后保留输入、停止自动重试且只提示一次。重新取回已交付附件不得唤醒模型，也不得通过符号链接越出快照目录。

回调截止回归占用实际 SQLite 单连接，确认操作已经等待存储后再取消回调；补充入队、快捷声明、批准、拒绝和提交回答均不得迟到提交，未接受的控件仍可重试。另在审批持久提交后、回调返回前取消，确认原生仍收到已接受的决定。群级容量回归让不同话题的消息与卡片补充同时竞争最后一个输入额度，只能有一个提交成功。回执回归分别覆盖确认创建后的清除响应丢失／重启、未知创建继续核对，以及 OnIt 明确失败与 Done／取消目标同时到达。

daemon 升级回归在二进制准备、服务注册、元数据发布和启动边界注入失败，核对恢复副本、旧文件／服务状态和启动结果未知时保留的新安装。Linux 使用真实 `linuxManager` 与临时目录中的 `systemctl` 替身，检查 manager 命令没有 unit 参数、先按候选定义清除启用状态再恢复旧定义、`enabled`／`disabled` 恢复、特殊状态提前拒绝及恢复失败时保留副本；这些检查不代表已操作真实 systemd。Windows 设置 `FEISHU_BOT_TASK_TEST=1` 后，`TestWindowsTaskRegistration` 使用唯一临时任务，经过生产 COM 查询和 `schtasks` 注册／导出／恢复／删除路径，验证确实缺失、已有任务、取消查询及禁用状态恢复；清理失败会使测试失败。未设置变量时该真实任务用例明确跳过，Windows 交叉编译不能替代它。

## 真实 Claude 协议 fixture

协议测试使用原生 **Claude Code 2.1.316** 和仅监听回环地址的确定性模型服务；不使用真实模型凭据，不连接真实飞书。指向已安装的固定版本 CLI：

```sh
CLAUDE_NATIVE_PATH=/absolute/path/to/claude go test -race -count=1 -timeout 180s -v ./internal/native ./internal/integration ./cmd/benchmark-native
```

PowerShell：

```powershell
$env:CLAUDE_NATIVE_PATH = "C:/tools/claude.exe"
go test -race -count=1 -timeout 180s -v ./internal/native ./internal/integration ./cmd/benchmark-native
```

没有设置变量时，真实 CLI 用例明确 skip，普通单元测试继续运行。CI 在单独测试步骤中安装固定 SDK 包所携带的原生 CLI，并核对 runner 的平台、架构和 CLI 版本；缺失或不匹配会使该步骤失败。这是获取 fixture 的构建依赖，生产服务不运行该 SDK。不得将 skip 写为真实协议已验证。

组合回归通过网关实际审批卡和 `/answer` 完成原生 Write／AskUserQuestion，核对临时文件内容与超长回复导出，确认文件交付回执之前没有 Done。随后触发空闲回收、停止并重开数据库，检查三代原生进程保持同一会话 ID 和历史。测试中的飞书传输和模型仍为本地替身。

原生协议是版本敏感边界。更改 CLI 版本、控制帧、环境继承或工具设置时，应验证初始化、连续两轮、原生 ID、权限／追问控制、MCP、compact 和停止后 resume，并记录实际覆盖范围。测试中的合成 token 只检查采集／去重，不是账单。

文件路径 fixture 让固定 CLI 实际执行 Read／Grep／Glob，检查当前工作区、当前话题附件、其他话题附件、波浪号、首尾空白和符号链接路径；Windows 还覆盖 MSYS 路径。允许路径必须返回测试文件内容，受保护路径必须返回工具错误且不含受保护内容；无法创建符号链接的 runner 会明确记录跳过的子项。原生路径规范化顺序只能由这类实际工具调用证明，不能由直接构造 hook 输入的单元测试推断。`FuzzOtherTopicFilePathsRemainDenied` 另外检查任意子路径仍被附件范围规则拒绝，不代表真实模型效果或同账号 OS 沙箱隔离。

常规 CI 继续固定该版本；手动 `Verify Go gateway` 可同时指定官方候选包版本与预期原生版本运行相同协议回归。候选检查成功不会自动修改已测版本，也不会把其他版本标为已验证。

## 实际构建产物回归

常规 CI 在各平台用 `CGO_ENABLED=0` 构建写入当前提交身份的生产二进制，随后通过 `FEISHU_ARTIFACT_PATH`、`FEISHU_ARTIFACT_VERSION`、`FEISHU_ARTIFACT_COMMIT`、`FEISHU_ARTIFACT_SHA256` 和 `FEISHU_ARTIFACT_BUILDINFO` 执行 `TestBuiltArtifact*`。有产物路径时，缺少身份字段、平台不符或文件哈希不匹配均直接失败。版本、初始化、本地诊断和基础模型探针直接调用这个二进制，不能由测试链接的替代服务冒充。

各平台的显式产物检查运行本地 TLS／WebSocket／REST 飞书替身与真实原生 CLI 的完整旅程：MCP 历史读取、普通成员审批被拒、维护者批准 Write、断线重连后回答原追问、同卡长回复、完整文件确认之前没有 Done、`/stop`、服务进程重启和同一网关／原生会话的历史恢复。模型请求次数和卡片／文件数量同时断言，防止重复工具执行或重复交付被成功结果掩盖。

Linux 假 CA 只放入测试子进程证书环境。Windows／macOS 的实际产物旅程使用系统校验：仅在 `GITHUB_ACTIONS=true`、`RUNNER_ENVIRONMENT=github-hosted` 且显式 `FEISHU_ARTIFACT_SYSTEM_TRUST=1` 的隔离 runner 中允许安装临时 CA。Windows 验证运行身份已经具有管理员权限，使用正常 `LocalMachine Root` 导入，并按 SHA-1／SHA-256 双指纹确认、删除和复查；权限不足直接失败，不启动提权。普通开发机／self-hosted runner 拒绝这类信任写入。

macOS 在安装前额外要求 `FEISHU_ARTIFACT_MACOS_TRUST_SCOPE=hosted_job`，固定 `macos-15`／`macos-15-intel` runner label 与本仓工作流／job，并将 repository、workflow ref、run、attempt 和 job 身份绑定到 journal。证书放在专用用户 keychain，使用 runner 已有的 `sudo -n` 写入仅针对 SSL／`open.feishu.cn` 的管理员信任。每次测试仍须确认服务与原生进程停止、恢复原 keychain 搜索列表、删除专用 keychain；任一步未确认均使验证失败。CA 私钥只在 fixture 进程内，不进入 journal、缓存或 artifact。

macOS 管理员信任记录的生命周期是整个一次性 job；本地收尾明确记录 `awaiting_hosted_runner_teardown`，由 GitHub 在 job 结束后退役该 VM，而非声称测试已同步撤销系统信任。[GitHub 官方文档](https://docs.github.com/en/actions/how-tos/manage-runners/github-hosted-runners/use-github-hosted-runners)说明每个托管 job 新建 VM、结束后自动退役；进程内无法观察最终退役，也不能据此宣称物理介质已安全擦除。该信任在同一 job 的后续步骤仍可能有效，所以 fixture job 仅使用测试身份，不接入真实飞书凭据；发布步骤在独立 job。工作流 `always()` 复查本地资源并输出生命周期记录；旧的逐测试 journal 不会因新环境变量而改用此模式。脚本保持正常 OS 授权与 TLS 验证。

升级旅程另外构建固定基线提交 `f727a9cd074e1001000fa7c14ecad38d25e369d1` 的旧二进制，以真实 `run --service --env-file --log-file` 进程完成首轮，然后在同一私有安装路径替换为当前产物并恢复。检查网关／原生 ID、历史、配置／环境／项目文件字节及服务日志前缀；测试侧读取旧数据库使用只读 SQL，不能在新服务启动前替它迁移。旧、新产物的 commit、version 和 SHA-256 必须不同。它验证服务进程模式的替换与恢复，不替代每种 OS 服务注册器或生产租户升级验收。

普通本地测试未提供产物或原生 CLI 路径时会明确 skip；显式产物检查缺少要求的身份、旧二进制或固定 CLI 时不能报告完整成功。本地终端交接另有 Linux 真 PTY fixture 验证前台进程组、终端输入、启动失败及取消后的恢复和后代清理；macOS／Windows 的实际交互终端体验仍需人工验收。

完整产物旅程由外层进程监督器执行：Go 测试 420 秒截止，外层执行 450 秒截止，另有 35 秒回收预算。Unix 用持续存活的独立 session leader 追踪不同进程组，Windows 在恢复启动前把进程加入持有的 Job Object；Linux 核对 pidfd／启动身份，macOS 核对 SID／libproc 启动时间。退出码 124 表示执行超时且已完成回收，125 表示监督或回收无法确认；都不能成为成功验证。全部原生平台还运行监督器的真实故障用例，覆盖正常父进程退出、命令崩溃、多进程组与硬超时。Unix 监督器自身被 SIGKILL、主动另建 session 的逃逸进程和整机强制结束仍依赖一次性 runner 回收，监督器不是容器隔离。

## 发布包验证

版本 tag 对应的提交在 Linux amd64／arm64、Windows amd64、macOS amd64／arm64 原生 runner 上分别运行竞态测试、vet 和固定原生 CLI 回归。随后用 `CGO_ENABLED=0` 构建生产二进制，对将要上传的便携包解压，校验 BUILDINFO 中版本／提交／SHA-256，再从解包路径执行基本命令及完整 `TestBuiltArtifact*` 旅程；安装包还须重复构建并逐字节比较，在原生平台实际安装、从已安装二进制运行同一完整旅程，再卸载。安装前拒绝既有包、相关 MSI 产品或安装目录；安装部分失败也进入身份核验后的收尾，卸载失败或仍有残留文件会使验证失败并保留诊断。交叉编译成功不能代替目标架构或安装包的实际执行。

常规 CI 通过 `python -m unittest discover -s .github/scripts -p 'test_*.py'` 检查版本、发布说明、打包与发布状态机；这些测试不上传附件。发布 job 再核对完整文件集合、构建身份与 SHA-256，上传草稿后重新下载验证，最后公开 Release。具体契约与重跑方式见[版本发布](releasing.md)。

离线发布工具测试、原生平台构建、真实 GitHub 发布和真实飞书／模型验收是四类证据。只有实际 tag 工作流完成并核对公开页面及附件，才能报告该版本发布成功。macOS 的测试不代表内置 daemon 或 Apple Developer ID 签名／公证已提供。

## 飞书与模型真实验收

在独立应用、授权测试群及私聊中执行。至少准备维护者、普通获准成员和未授权成员，记录 OS、网关／Claude／客户端版本、配置摘要及结果；凭据留在运行主机，避免保存完整原始事件。

1. **入口与权限。**分别测试明确 @、无 @ 引用、@全体、未授权作者、未授权群和私聊。另在 `mentionOptional` 群测试未 @ 的获准文本、图片和文件能够触发，未授权作者仍静默，默认群不受影响；关闭开关并重启后恢复要求 @，已排队的未 @ 输入不再投递。拒绝应无正文入库、下载、回执或模型调用。测试 text、post、image、file、引用和线程；不完整输入明确拒绝。
2. **连续对话与补充。**整群范围的同群不同作者／线程保持同一会话；`sessionScope: "thread"` 群中两个顶层 @ 形成两个话题和会话，话题内 @ 续接、未 @ 不触发，重启后续接同一话题会话，回复、状态卡和文件留在话题内，历史只读本话题，顶层管理命令只提示；另在话题形式群确认首帖 @ 可建立会话、话题内卡片可更新。普通忙时输入进入原生流。`/btw`／`/ps` 不启动空闲任务，字面 slash 正文不变成管理命令。目标结束或 compact 等待时不越过边界。
3. **审批与追问。**普通成员不能批准共享外部写操作；维护者可批准／拒绝。多选可逐项切换，空提交会提示缺少的题号并可补齐；单选点击其他项可改选，重复点击同一项保持选中。群内选择只反馈给操作者，私聊卡片显示当前选择，第一位提交者生效。过期或已结束请求的按钮不能复活请求，`/answer` 保留恢复入口。
4. **流式与完整交付。**同卡显示公开思考／答案，隐藏工具和签名；思考默认折叠、可展开。超长回复或必要附件未完成时没有 Done。人工核对未知上传结果后，才显式使用 `/retry-output`；不得通过盲目重发制造验收。
5. **图片。**默认逐条；开启窗口后检验相同作者／线程／引用的连续独立图片合批。每条群图片默认独立明确 @；`mentionOptional` 群按作者授权独立触发。数量、单文件、总量或下载失败时整批取消。
6. **停止与恢复。**任务、审批、图片窗口、慢下载和最终交付时分别测试 `/stop`、重启；检查未知外部写入不重放、已知卡片不再停留在流式状态。停止／回收／恢复保留原生 ID，仅 `/new` 新建；改变 cwd／配置身份会拒绝旧会话。核对 `/rename`、会话恢复按钮和模型配置切换要求本聊天维护者权限；获准成员取回已交付附件仍受当前聊天和原输出所有权约束。
7. **用量。**无结果时未知；连续轮次、工具轮次、重启和重复结果不重复累计。查询不调用模型，不混入其他聊天、子代理或累计 modelUsage。
8. **继承与后台。**先用无副作用 hook／技能／MCP 验证配置来源，再测试实际需要的原生后台能力。只清理本次创建的测试任务，记录工具返回的 ID。后台输出归属不能借用随后出现的前台输入。
9. **服务生命周期。** Windows 登录任务直接执行安装目录中的二进制，不出现脚本或控制台窗口；`daemon status` 报告 PID/配置/日志；`daemon logs -f` 能跟上轮转；`daemon stop` 优雅退出并释放 `gateway.lock`；`daemon start`／`daemon restart` 后 `/status` 仍是同一 session ID 和原生 session ID。未经核实不要删除 stale lock；显式 `daemon start --reclaim-stale-lock` 只清理已无进程持有的锁。`daemon uninstall` 删除任务/unit 但保留 config、service.env、日志和 state。

手机和桌面分别记录。模型流式、多模态、工具和长上下文能力分别验收；基础 Messages 探针成功不替代这些结果。共享 cwd 的原生持久定时任务仍遵循 Claude 的项目级规则，不能宣称跨聊天隔离。

## 性能与回归边界

大量历史输入下的本地查询用生产 Go SQLite 驱动、完整 schema、WAL 和单连接设置验证：

```sh
go test ./internal/store -run TestInboxQueriesUseLiveIndexesWithLargeHistory -v
go test ./internal/store -run '^$' -bench '^BenchmarkInboxHistory$' -benchmem -count=3
```

执行计划测试检查群级在途计数、待处理房间发现和话题内有序队列使用对应部分索引，不扫描或重新排序全部历史。基准保持活跃输入数量不变，比较 1,000／100,000 条终态及未知历史记录，并在独立临时数据库中比较加入活跃／排队索引前后的查询。它测量本地查询和反序列化成本，不测飞书历史 API、模型延迟或整个服务吞吐量；应记录实际平台、Go 版本和多次采样，不能把一次观察当成性能保证。

Go 迁移移除了每会话 Node worker，但没有预设内存降幅。比较需要固定原生 CLI、输入、模型 fixture、暖会话数和后台负载，并区分网关与完整进程树。详见[运行基线](runtime-baseline.md)。历史 TypeScript 测试的通过记录是迁移参照，不计作 Go 回归覆盖；旧报告保留在历史迭代文档和 Git 历史中。

当前资源矩阵在 Linux／Windows 原生 runner 记录无会话、单／多热会话、中文长回答、快／慢传输、附件和回收后恢复的观察，并单独运行两聊天的实际并发轮次与 6 秒持续负载，记录 SQLite／WAL、goroutine、进程内存及维护趋势。Linux 另保留渲染旧算法与缓冲算法的分配基准。持续阶段支持本地延长，但短时 CI 不作为长期稳定性的证明。采样结果、Go 微基准和整机部署内存是不同口径；少量 CI 样本不能证明真实飞书延迟或整套服务内存降低的比例。

独立的原生 endurance 工作流提供每周 30 分钟、手动 10／30／60 分钟的 Linux／Windows 观察，PR 中只做 30 秒冒烟。除原有趋势外记录当前 Go 堆、数据目录字节／文件数，并检查实际持续时长、并发交付、回收后的身份和清理确认；参数与证据口径见[运行基线](runtime-baseline.md#并发与持续负载)。配置工作流不代表长时间运行已经验收。
