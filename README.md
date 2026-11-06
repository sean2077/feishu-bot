# feishu-bot

在飞书群聊和私聊中使用 Claude Code。可用于个人助手、资料整理、协作开发，以及经授权的外部系统操作。支持 Linux、macOS 和 Windows 原生运行。

一个 Go 常驻服务接收消息，通过双向 `stream-json` 协议直接启动并复用 Claude Code 原生进程。任务理解、子代理、上下文压缩和后台工作由 Claude Code 完成；feishu-bot 负责聊天路由、消息交付、权限交互和运行状态。运行时不依赖 Node.js 或 TypeScript Agent SDK。

## 快速开始

需要 Git 和原生 Claude Code；Windows 使用 Git for Windows。协议回归固定使用 Claude Code 2.1.316，其他版本须另行验收。

预编译 Go 版本从 [Releases](https://github.com/sean2077/feishu-bot/releases) 选择文件名带系统和架构的附件，例如 `feishu-bot-vX.Y.Z-darwin-arm64.tar.gz`，按[发布包安装](docs/deployment.md#发布包安装)核对 SHA-256 后使用，无需安装 Go。同一版本也提供 Windows MSI 与 Linux deb 安装版；安装器只复制程序和文档，不注册后台服务。历史 `v0.2.0` 的通用 ZIP 是旧 TypeScript 版本；若尚无对应平台的 Go 附件，请按下文从源码构建。发布矩阵覆盖 Linux／macOS 的 amd64、arm64，以及 Windows amd64。macOS 使用前台 `run` 或自行配置服务管理，内置 `daemon` 仅支持 Linux／Windows。

从源码构建需要 Go 1.25+。克隆本仓库后，在仓库目录执行：

```sh
go build -trimpath -o ./bin/feishu-bot ./cmd/feishu-bot
./bin/feishu-bot init
```

Windows 构建使用 `go build -trimpath -o ./bin/feishu-bot.exe ./cmd/feishu-bot`。`init` 生成 `config.json`，不会覆盖已有文件。填写飞书应用、允许接入的群、群内获准使用者、私聊用户、维护者 ID，以及模型服务配置。只使用私聊时可将 `groups` 设为 `[]`，无需建立群组或绑定代码仓库。

模型入口需要兼容 Anthropic Messages；`baseUrl` 对应 Claude Code 的 `ANTHROPIC_BASE_URL`，模型 ID 按服务实际支持的值配置。项目不绑定特定供应商，也不提供协议转换层。兼容接口不等于所有模型能力均已验证。

Linux／macOS：

```sh
export FEISHU_APP_SECRET='replace-with-app-secret'
export MODEL_AUTH_TOKEN='replace-with-model-token'
./bin/feishu-bot doctor
./bin/feishu-bot run
```

Windows PowerShell：

```powershell
$env:FEISHU_APP_SECRET = "replace-with-app-secret"
$env:MODEL_AUTH_TOKEN = "replace-with-model-token"
.\bin\feishu-bot.exe doctor
.\bin\feishu-bot.exe run
```

Linux／Windows 的常驻运行可使用内置服务命令，不需要编写 `.ps1`、`.vbs` 或 `.cmd` 启动脚本：

```sh
./bin/feishu-bot daemon install --config ~/.feishu-bot/config.json
./bin/feishu-bot daemon status
./bin/feishu-bot daemon logs -f
./bin/feishu-bot daemon stop
```

Windows 使用 `.\bin\feishu-bot.exe daemon ...`，可省略 `--config`：缺省先找当前目录 `config.json`，再找 `%USERPROFILE%\.feishu-bot\config.json`。`daemon install` 会把当前二进制复制到安装目录的 `bin/`，生成或校验 `service.env`（权限 0600），执行离线 `doctor` 检查，然后注册登录启动服务：Windows 使用任务计划程序直接执行二进制，Linux 使用 systemd user/system unit。没有中间启动脚本，密钥也不会写入任务或 unit 定义。未传 `--env-file` 时从当前环境生成 `service.env`；已有服务可用 `daemon stop`、`daemon restart`、`daemon uninstall` 管理。`daemon status --json` 供脚本读取，`daemon logs -n 100` 查看最近日志。

服务默认在用户登录后运行（Windows），以保留当前用户的 `nativeConfig: inherit`、原生历史和环境。注销/关机留下的 `gateway.lock` 不会被自动删除；核实派生进程后使用 `daemon start --reclaim-stale-lock`。完整命令、路径和限制见[部署与配置](docs/deployment.md)。

模型凭据的环境变量名可通过 `model.tokenEnv` 指定；已有显式配置保持有效。未配置时默认读取 `MODEL_AUTH_TOKEN`，不会尝试其他账号或凭据。

[部署与配置](docs/deployment.md) · [不同使用场景](docs/usage.md) · [从 TypeScript 版迁移](docs/go-migration.md)

## 对话与会话

**群聊默认须同时满足：已授权群、已授权发言者、明确 @ 本机器人。**仅回复或引用机器人而没有 @，不会触发。群配置中的 `members` 列出获准使用者，项目维护者也可使用；省略或留空 `members` 只允许维护者，不会放开全群。只有你和机器人的小群可在该群设置 `mentionOptional: true` 免除 @；群和作者授权仍然生效，默认值与重启生效方式不变。私聊不要求 @，但用户仍须出现在 `dms` 中。

未授权消息静默丢弃，不回复、不加表情、不进入上下文。正常忙时输入直接交给当前 CC 输入流，由 CC 判断如何继续，不增加任务分类模型。只想补充当前前台任务时使用 `/btw` 或 `/ps`；没有运行中的前台任务就明确拒绝，不另起任务。

**非 @ 群消息默认不缓存、不预注入：`limits.backgroundMessages = 0`。**Agent 需要前文时调用 `read_chat_history`，按需读取当前聊天的一页历史；历史、引用和附件入口同样检查作者和聊天权限。工具需要应用具备对应消息读取权限，缺少权限会明确返回工具错误，而不是假装没有历史。

一群一条当前会话，不按线程或发言者拆分；每个私聊独立。空闲回收进程、服务重启和 `/stop` 都保留原 session ID，下次消息续接原生历史。只有 `/new` 主动新建；恢复失败不会静默清空上下文。原生历史文件需要持续保存在 `dataDir`。

| 命令 | 作用 |
|---|---|
| `/help` | 查看命令、权限和恢复提示 |
| `/status` | 查看当前会话、后台工作、输入与审批状态 |
| `/btw <正文>`、`/ps <正文>` | 向正在执行的前台任务补充信息；两者同义，不是旁路问答 |
| `/sessions [关键词]`、`/resume <名称或ID>`、`/rename <名称>` | 搜索、翻页、命名和恢复本聊天历史；支持唯一短 ID |
| `/model [配置名]`、`/profile [配置名]` | 查看或选择管理员定义的模型与 effort 配置，在安全轮次边界生效 |
| `/artifacts [文件ID]` | 列出或取回本聊天已完成的文件，不启动模型 |
| `/compact [保留重点]` | 使用 CC 原生压缩；忙时在当前工作轮次结束后执行，不新建会话 |
| `/new` | 停止当前运行并新建会话，旧历史仍保留 |
| `/stop` | 停止运行和待交付输入，保留当前会话供下次继续 |
| `/answer <请求ID> <回答>` | 回答指定追问 |
| `/inputs`、`/cancel-input <ID>` | 查看输入及等待原因；核实尚未提交后取消 |
| `/outputs`、`/retry-output <ID>`、`/discard-output <ID>` | 查看、明确补发或放弃同轮依赖交付；放弃不停止模型、不标 Done |
| `/usage`、`/usage --json` | 查看所选会话最近结果及累计观测的原生用量；缺失值未知，不估造费用 |

**追问卡片：** 每次选择或未完整提交后，网关刷新同一卡片的按钮版本。回调独立检查请求、消息、会话和操作者归属；多选状态按操作者分别保存。客户端缓存旧按钮或刷新失败时，使用获准聊天内的 `/answer <请求ID> <回答>`，多题使用问题原文为键的 JSON。[处理方式与验证边界](docs/usage.md#追问卡片重复点击)

群内命令默认也须 @；`mentionOptional` 群同样免 @，但影响共享会话的管理操作仍限维护者，私聊本人可管理自己的会话。网关不增加自动压缩策略，CC 自带的原生压缩照常工作。`/compact` 会区分实际压缩、无需压缩和失败。恢复历史不会复活旧后台进程，`/stop` 也不能撤销已经发生的外部操作。

补充正文按文本交给既有原生输入流，`/btw /new` 不会新建会话，也不能代替审批回答。已经排队的 compact 是边界；等待期间目标运行结束，尚未发送的补充会取消。空闲热进程和仅有后台活动不算可补充目标。[补充语义与竞态](docs/supplements.md)

## 图片、附件与用量

文本、富文本、独立图片和文件经 Go 入站适配层解析，保留作者、引用、线程及资源信息。不完整或未支持的主输入明确提示，不静默当作完整任务执行；合并转发不自动展开。[格式支持边界](docs/raw-messages.md)

连续多图合批默认关闭：`limits.imageBatchMs = 0`。显式配置有界窗口后，只合并同聊天、同作者、同会话且引用／线程一致的连续独立图片，每条群图片仍须独立满足触发条件（默认明确 @；`mentionOptional` 群按作者授权）。每张原消息及截止时间先持久化；发送前下载失败或超限则整批取消，不以部分图片执行。窗口最大 5000 ms，图片上限 2–12；单文件与批次总量分别限制。[配置与恢复规则](docs/image-batches.md)

`/usage` 复用原生结果报告，不触发模型。统计范围是主循环的 `result.usage`，不含子代理、不等于当前上下文或账单。按实际结果 ID 去重，重启不重复累计；升级前的历史明确标为不完整，未报告字段显示未知。[字段与统计口径](docs/usage.md#使用量)

## 回复展示

通过校验并接收后，在触发消息上添加临时 `OnIt` 表情。本轮成功且最终回复和必要文件均已交付时移除临时表情并添加 `Done`；失败、取消或未确认送达时不标成功。

一轮回复使用同一张卡片，引用最初的触发消息。模型公开返回的思考内容和答案分区更新，结束时折叠思考；工具调用轨迹默认不展示，审批和追问仍可交互。没有可展示思考时只显示处理状态，不生成额外的思考摘要。多图批次和多人补充保留每条原消息的回执归属，不只完成最后一条消息。

卡片采用状态色标题、带图标的灰底思考记录、独立正文和小字号状态栏。思考区显示最近 10 段的开头各 180 字，结束后默认折叠；这些片段直接来自模型输出，不另行生成摘要。结束时显示用时、实际模型、已报告用量、末次模型请求的输入规模与原生上下文窗口，以及实际工作目录；缺失字段显示未知，不用累计用量推算上下文。正文通过 CardKit 在原卡片内流式更新；应用明确缺少 CardKit 权限时保留整卡更新方式，不通过另发消息代替更新。

[完整对话规则与升级说明](docs/conversation-contract.md)

卡片预算包含 JSON 转义、标题、思考、状态栏和按钮；中文或 emoji 长答案超限时，保留单卡预览并交付完整文件。等待审批时暂停流式，回答后继续原卡；平台流过期时退回同卡全量更新。停止或重启后的中断状态不会伪装成完成。未确认写入仍需先核对 `/outputs`；明确限流按服务端等待时间退避，永久失败暂停自动重试。

卡片提供状态、补充、停止、回答入口和交付重试；快捷操作绑定当前聊天、消息、会话及版本。普通成员不能通过按钮绕过维护者权限，旧轮次按钮也不能停止后续任务。[模型配置、命名和文件取回](docs/usage.md)

离线 `doctor` 区分实际原生版本与协议已测版本，并报告状态库、WAL 和导出目录大小。首次连接可显式运行 `doctor --verify-chat <已配置群ID>` 或 `doctor --verify-chat dm:<已配置用户ID>`，按挑战消息确认单卡、回调与测试文件；不扩大成员列表，也不调用模型。[首次连接流程](docs/first-connection.md)

## 按场景配置

`projects` 是资源与权限配置单元，不要求一定有代码项目。`seedDir`、`trustedConfigDir` 和外部 `actions` 都可按需配置。

| 场景 | 配置重点 |
|---|---|
| 个人助手 | 仅开启自己的私聊，不必配置源码或外部操作 |
| 资料整理 | 提供获准使用的资料目录或附件；按需接入文档服务 |
| 协作开发 | 配置干净源码、可信项目技能、群成员与原生工具规则 |
| 业务查询与操作 | 配置固定资源范围的可信执行器；外部写操作逐次审批 |

默认使用原生 `acceptEdits`，常规文件编辑不逐次询问；需要更多确认时可使用 `default`。预授权工具由 `allowedTools` 配置，不默认开放任意 shell。[权限与信任范围](SECURITY.md)

Jenkins、GitLab、飞书文档和多维表格只是[可选操作示例](docs/project-actions.md)，不是运行依赖，也不代表任何特定部署环境。

本地排查授权可用 `feishu-bot config explain --chat <聊天ID> --user <用户ID> --mentioned --json`，只读取配置并复用实际路由规则。需要在终端继续现有会话时，先停止服务，再用 `feishu-bot session run --session <完整网关会话ID>`；退出后重启服务。该交接独占服务锁，保留原生身份，详见[部署说明](docs/deployment.md#本地终端交接)。

## 运行边界与验证

原生进程使用服务账户的系统权限。不同聊天的目录和路由是逻辑分离，不是相互不信任租户之间的安全沙箱。按实际需要授予文件、网络和外部 API 权限。

```sh
go test -race ./...
go vet ./...
go build ./cmd/feishu-bot
```

测试覆盖配置与旧数据库、授权与交付、协议事件、飞书 HTTP／WebSocket 替身及并发竞态。设置 `CLAUDE_NATIVE_PATH` 后运行真实 CC 进程配合本地模拟模型的协议回归；普通测试没有配置该变量时明确跳过真实 CLI 测试。CI 单独安装固定版本的测试 CLI。真实飞书应用、手机／桌面客户端、所选模型和外部服务仍需分别验证。[验证清单](docs/validation.md)

模型回复跟随当前请求语言；内置命令和审批提示主要为中文。时区跟随运行环境，可在启动前设置 `TZ`。

[文档索引](docs/README.md) · [架构](docs/architecture.md) · [部署](docs/deployment.md) · [使用场景](docs/usage.md) · [开发](CONTRIBUTING.md) · [版本发布](docs/releasing.md) · [运行基线](docs/runtime-baseline.md) · [依赖声明](THIRD_PARTY_NOTICES.md) · [公开发布检查](docs/publication.md)
