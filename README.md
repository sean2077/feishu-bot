# feishu-bot

在飞书群聊和私聊中使用 Claude Code。可用于个人助手、资料整理、协作开发，以及经授权的外部系统操作。Linux、macOS 和 Windows 原生运行同一个服务。

一个 Go 常驻服务（网关）接收飞书消息，通过双向 `stream-json` 协议直接启动并复用 Claude Code 原生进程。任务理解、子代理、上下文压缩和后台工作由 Claude Code 完成；网关负责聊天路由、授权、消息交付、权限交互和运行状态。运行时不依赖 Node.js 或 TypeScript Agent SDK。

## 快速开始

### 1. 准备环境

- Git 与原生 Claude Code；Windows 使用 Git for Windows。协议回归固定使用 Claude Code 2.1.321，其他版本须另行验收。
- 一个开启机器人、长连接和卡片回调的飞书自建应用，权限清单见[飞书应用与授权](docs/deployment.md#飞书应用与授权)。
- 一个兼容 Anthropic Messages 的模型入口。项目不绑定特定供应商，也不提供协议转换层；兼容接口不等于所有模型能力均已验证。

### 2. 获取程序

- **发布包**：从 [Releases](https://github.com/sean2077/feishu-bot/releases) 下载与系统和架构匹配的附件（如 `feishu-bot-vX.Y.Z-darwin-arm64.tar.gz`），按[发布包安装](docs/deployment.md#发布包安装)核对 SHA-256 后使用，无需安装 Go。同一版本还提供 Windows MSI 与 Linux deb 安装版，它们只复制程序和文档，不注册后台服务。`v0.2.0` 及更早的通用 ZIP 是旧 TypeScript 版本。
- **源码构建**：需要 Go 1.25+。在仓库目录执行 `go build -trimpath -o ./bin/feishu-bot ./cmd/feishu-bot`；Windows 输出 `./bin/feishu-bot.exe`。

以下命令以源码构建的 `./bin/feishu-bot` 为例；使用发布包时换成解压目录中的 `./feishu-bot`（Windows 为 `.\feishu-bot.exe`）。

### 3. 生成并填写配置

建议把配置放在与源码、解压目录分开的固定部署目录中，例如 `~/.feishu-bot`：

```sh
./bin/feishu-bot init --config ~/.feishu-bot/config.json
```

`init` 写入模板，不会覆盖已有文件。填写飞书应用、允许接入的群、群内获准使用者、私聊用户、项目维护者及模型入口；只使用私聊时可将 `groups` 设为 `[]`，无需建立群组或绑定代码仓库。凭据只放在运行主机的环境变量中，变量名、`dataDir` 等字段说明见[部署与配置](docs/deployment.md#配置文件与凭据)。

### 4. 检查并前台运行

Linux／macOS：

```sh
export FEISHU_APP_SECRET='replace-with-app-secret'
export MODEL_AUTH_TOKEN='replace-with-model-token'
./bin/feishu-bot doctor --config ~/.feishu-bot/config.json
./bin/feishu-bot run --config ~/.feishu-bot/config.json
```

Windows PowerShell：

```powershell
$env:FEISHU_APP_SECRET = "replace-with-app-secret"
$env:MODEL_AUTH_TOKEN = "replace-with-model-token"
.\bin\feishu-bot.exe doctor --config "$HOME\.feishu-bot\config.json"
.\bin\feishu-bot.exe run --config "$HOME\.feishu-bot\config.json"
```

`doctor` 默认离线，`ready` 只表示所列检查通过。随后按[首次连接](docs/first-connection.md)逐层确认飞书身份、测试聊天中的卡片／回调／文件，再做真实协作验收。

### 5. 常驻运行

Linux／Windows 使用内置服务命令，不需要编写 `.ps1`、`.vbs`、`.cmd` 或 launcher 脚本：

```sh
./bin/feishu-bot daemon install --config ~/.feishu-bot/config.json
./bin/feishu-bot daemon status
./bin/feishu-bot daemon logs -f
```

`daemon install` 执行离线预检，把二进制复制到配置所在目录的 `bin/`，从当前环境生成 `service.env`（不把密钥写进任务或 unit），再注册 Windows 登录任务或 Linux systemd 服务并启动。Windows 登录任务以当前用户身份运行，不是开机即运行的系统服务。macOS 使用前台 `run` 或自行配置服务管理器。停止、升级、回滚和 stale lock 恢复见[内置 daemon 服务](docs/deployment.md#内置-daemon-服务)与[升级与回滚](docs/deployment.md#升级与回滚)；从 TypeScript 版升级另见 [Go 迁移](docs/go-migration.md)。

## 在飞书中使用

**群聊默认须同时满足：已授权群、已授权发言者、明确 @ 本机器人**。仅回复或引用机器人而没有 @ 不会触发。`members` 列出获准使用者，项目维护者始终可用；省略 `members` 只允许维护者，`allowAllMembers: true` 才放开全群。`chatId: "*"` 的唯一兜底绑定覆盖未显式配置的群，显式绑定优先。成员很少且互信的小群可设置 `mentionOptional: true` 免除 @，群和作者授权仍然生效。私聊不要求 @，但用户须出现在 `dms` 中。未授权消息静默丢弃，不回复、不加表情、不进入上下文。

**非 @ 群消息默认不缓存、不预注入（`limits.backgroundMessages = 0`）**。Agent 需要前文时调用 `read_chat_history` 按需读取当前聊天的一页历史，同样检查聊天和作者；应用缺少消息读取权限时返回明确的工具错误。

默认一群一条当前会话，每个私聊独立；群配置 `sessionScope: "thread"` 后，每个飞书话题使用独立会话。空闲回收、服务重启和 `/stop` 都保留原 session ID，下次消息续接原生历史；只有 `/new`（或话题范围群中新的顶层触发）开启新会话，恢复失败不会静默清空上下文。忙时普通输入直接交给当前原生输入流；网关不另加任务分类模型，也不做自动压缩。完整规则见[对话行为](docs/conversation-contract.md)。

| 命令 | 作用 |
|---|---|
| `/help` | 查看命令、权限和恢复提示 |
| `/status` | 查看当前会话、后台工作、输入与审批状态 |
| `/btw <正文>`、`/ps <正文>` | 向正在执行的前台任务补充信息；两者同义，不是旁路问答，[补充语义](docs/supplements.md) |
| `/sessions [关键词]`、`/resume <名称或ID>`、`/rename <名称>` | 搜索、命名和恢复本聊天的会话；支持唯一短 ID |
| `/model [配置名]`、`/profile [配置名]` | 查看或选择管理员定义的模型与 effort 配置，在安全轮次边界生效 |
| `/artifacts [文件ID]` | 列出或取回本聊天已完成的文件，不启动模型 |
| `/compact [保留重点]` | 原生压缩；忙时在当前工作轮次结束后执行，不新建会话 |
| `/new` | 停止当前运行并新建会话，旧历史仍保留 |
| `/stop` | 停止运行和待交付输入，保留当前会话供下次继续 |
| `/answer <请求ID> <回答>` | 回答指定追问；卡片按钮失效时的恢复入口 |
| `/inputs`、`/cancel-input <ID>` | 查看输入及等待原因；核实尚未提交后取消 |
| `/outputs`、`/retry-output <ID>`、`/discard-output <ID>` | 查看、明确补发或放弃同轮依赖交付；放弃不停止模型、不标 Done |
| `/usage`、`/usage --json` | 查看所选会话已报告的原生用量；缺失值显示未知，不估算费用 |

群内命令同样需要 @（`mentionOptional` 群除外）；影响共享会话的管理操作限维护者，私聊本人可管理自己的会话。状态、追问卡片、模型配置、文件取回和用量口径见[使用场景](docs/usage.md)。

## 回复、图片与附件

通过授权并接收的消息先加临时 `OnIt` 表情；本轮成功且最终回复和必要文件均已交付后才改为 `Done`，失败、取消或未确认送达时不标成功。

一轮回复使用同一张卡片并引用最初的触发消息：正文通过 CardKit 流式更新，思考区默认折叠，工具调用轨迹不展示，审批和追问仍可交互，结束时显示用时、模型、用量和工作目录。追问支持整张表单选择及自定义回答；超出字节或表格预算的答案保留单卡预览并另附完整原文。卡片按钮绑定当前聊天、消息、会话及版本，普通成员不能借按钮绕过维护者权限。展示与交付规则见[对话行为](docs/conversation-contract.md#回复卡与交付)。

文本、富文本、独立图片和文件由 Go 入站适配层解析；不完整或未支持的主输入会明确提示，合并转发不自动展开，见[原始消息格式](docs/raw-messages.md)。连续多图合批默认关闭（`limits.imageBatchMs = 0`），开启方式与限制见[连续图片](docs/image-batches.md)。

## 按场景配置

`projects` 是资源与权限配置单元，不要求一定有代码项目；`seedDir`、`trustedConfigDir`、`cwd` 和外部 `actions` 都按需配置。

| 场景 | 配置重点 |
|---|---|
| 个人助手 | 仅开启自己的私聊，不必配置源码或外部操作 |
| 资料整理 | 提供获准使用的资料目录或附件；按需接入文档服务 |
| 协作开发 | 配置干净源码、可信项目技能、群成员与原生工具规则；多人并行可用话题会话 |
| 业务查询与操作 | 配置固定资源范围的可信执行器；外部写操作逐次审批 |

默认使用原生 `acceptEdits`，常规文件编辑不逐次询问；需要更多确认时使用 `default`。预授权工具由 `allowedTools` 配置，不默认开放任意 shell。Jenkins、GitLab、飞书文档和多维表格只是[可选操作示例](docs/project-actions.md)，不是运行依赖。示例配置见[使用场景](docs/usage.md)。

排查授权时可运行 `feishu-bot config explain --chat <聊天ID> --user <用户ID> --mentioned --json`，它只读取配置并复用实际路由规则。需要在终端继续现有会话时，见[本地终端交接](docs/deployment.md#本地终端交接)；需要把当前本地会话交给飞书私聊话题时，见[认领本地会话到飞书私聊话题](docs/deployment.md#认领本地会话到飞书私聊话题)；任务结果通知和终端存活时的话题交互见[任务通知与话题内交互](docs/deployment.md#任务通知与话题内交互)。

## 运行边界

原生进程使用服务账户的系统权限。不同聊天的目录和路由是逻辑分离，不是互不信任租户之间的安全沙箱；按实际需要授予文件、网络和外部 API 权限，详见[权限与信任范围](SECURITY.md)。

模型回复跟随请求语言；内置命令和审批提示主要为中文。时区跟随运行环境，可在启动前设置 `TZ`。

开发检查使用 `go test -race ./...` 和 `go vet ./...`；设置 `CLAUDE_NATIVE_PATH` 后才运行真实 Claude Code 进程的协议回归。模拟测试、平台 CI 与真实飞书／模型验收分别记录，见[开发与贡献](CONTRIBUTING.md)和[验证与验收](docs/validation.md)。

## 文档

[文档索引](docs/README.md) · [部署与配置](docs/deployment.md) · [首次连接](docs/first-connection.md) · [使用场景](docs/usage.md) · [对话行为](docs/conversation-contract.md) · [架构](docs/architecture.md) · [运行诊断](docs/runtime-diagnostics.md) · [版本发布](docs/releasing.md) · [版本记录](CHANGELOG.md) · [依赖声明](THIRD_PARTY_NOTICES.md)
