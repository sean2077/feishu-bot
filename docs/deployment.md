# 部署与配置

本页是安装、配置、飞书应用权限、常驻服务、升级与恢复的操作说明。对话与会话行为以[对话行为](conversation-contract.md)为准，模块与并发模型见[架构](architecture.md)。

Linux、macOS 和 Windows 运行同一个 Go 服务。部署只需网关二进制、Git 和原生 Claude Code；Windows 使用 Git for Windows。协议测试固定使用 Claude Code 2.1.316，升级 CLI 后须重新验收。内置 `daemon` 管理仅支持 Linux／Windows；macOS 使用前台 `run` 或自行配置服务管理。

下文命令以源码构建的 `./bin/feishu-bot` 为例；使用发布包时换成解压目录中的 `./feishu-bot`，Windows 对应 `.\feishu-bot.exe` 或 `.\bin\feishu-bot.exe`。

## 获取程序

### 发布包安装

从 [GitHub Releases](https://github.com/sean2077/feishu-bot/releases) 选择目标 Go 版本，下载与本机匹配的便携压缩包或安装包，以及同一版本的 `SHA256SUMS.txt`。Linux、macOS 分别提供 amd64／arm64，Windows 提供 amd64；macOS Intel 选择 `darwin-amd64`，Apple Silicon 选择 `darwin-arm64`。Windows MSI 只提供 amd64，Linux deb 提供 amd64／arm64。完整产物命名见[版本发布](releasing.md#发布产物)。`v0.2.0` 及更早的通用 ZIP 是旧 TypeScript 版本，升级见 [Go 迁移](go-migration.md)。

执行与平台对应的命令，核对结果与 `SHA256SUMS.txt` 中该文件一行的 SHA-256 一致。`vX.Y.Z` 是版本占位符，替换成实际下载版本：

```sh
# Linux 示例
sha256sum feishu-bot-vX.Y.Z-linux-amd64.tar.gz
# macOS Apple Silicon 示例
shasum -a 256 feishu-bot-vX.Y.Z-darwin-arm64.tar.gz
```

Windows PowerShell：

```powershell
Get-FileHash -Algorithm SHA256 .\feishu-bot-vX.Y.Z-windows-amd64.zip
```

校验后解压并进入包内同名目录。以 macOS Apple Silicon 为例：

```sh
tar -xzf feishu-bot-vX.Y.Z-darwin-arm64.tar.gz
cd feishu-bot-vX.Y.Z-darwin-arm64
./feishu-bot --version
```

Windows 解压 ZIP 后使用 `.\feishu-bot.exe`。包内 `INSTALL.md` 给出该版本的安装入口，`BUILDINFO.json` 记录 tag、提交和目标平台；`config.example.json` 是参考模板，实际配置由 `init` 生成（见[配置文件与凭据](#配置文件与凭据)）。配置、凭据、状态库、会话历史和日志应放在固定部署目录，不放在带版本号的解压目录中；升级时只更换已核对的二进制（见[升级与回滚](#升级与回滚)）。

macOS 产物未提供 Developer ID 签名或 Apple 公证。若首次运行被系统阻止，在确认下载来自本仓库且 SHA-256 一致后，按 [Apple 的按程序放行说明](https://support.apple.com/en-us/102445)在“系统设置 → 隐私与安全性”中对这个程序选择“仍要打开”。保持系统全局保护启用；此操作只针对已核对的程序。

### 安装版

Windows MSI 和 Linux deb 只安装程序与文档，不注册后台服务、systemd unit 或任务计划。需要常驻运行时，安装后再执行 `feishu-bot daemon install`。安装包与便携包使用同一版本和同一 `SHA256SUMS.txt`。

Windows MSI 按当前用户安装到 `%LOCALAPPDATA%\feishu-bot`，不需要管理员权限；可双击或静默安装：

```powershell
Start-Process msiexec.exe -ArgumentList '/i', '.\feishu-bot-vX.Y.Z-windows-amd64.msi', '/qn' -Wait
& "$env:LOCALAPPDATA\feishu-bot\feishu-bot.exe" --version
```

当前 MSI 没有代码签名，Windows SmartScreen 可能提示。卸载通过“设置 > 应用”中的 `feishu-bot`，卸载不会删除自行创建的配置、状态库或日志。

Linux deb 把二进制安装到 `/usr/bin/feishu-bot`，文档安装到 `/usr/share/doc/feishu-bot/`：

```sh
sudo dpkg -i feishu-bot-vX.Y.Z-linux-amd64.deb
feishu-bot --version
sudo dpkg -r feishu-bot
```

`dpkg -r` 不会删除配置和数据目录。

### 从源码构建

需要 Go 1.25+。在仓库目录执行：

```sh
go build -trimpath -o ./bin/feishu-bot ./cmd/feishu-bot
```

Windows 输出 `./bin/feishu-bot.exe`。生产构建使用 `CGO_ENABLED=0`；运行二进制不需要 Go 编译器、Node.js、npm 或 CGO 动态库。源码更新后重新构建，再按[升级与回滚](#升级与回滚)替换运行中的二进制。开发检查见[开发与贡献](../CONTRIBUTING.md)。

## 配置文件与凭据

```sh
./bin/feishu-bot init --config ~/.feishu-bot/config.json
```

`init` 创建配置父目录，以 0600 权限写入模板，拒绝覆盖已有文件。`config example` 打印同一模板，`config path` 打印解析后的配置路径，`config format` 把合法 JSON 原位改写为两空格缩进（只检查 JSON 语法，不校验字段）。

**配置路径**：`init`、`doctor`、`run` 和 `session run` 未传 `--config` 时只使用当前目录的 `config.json`；`daemon install` 与 `config path|format|explain` 依次查找 `--config`、当前目录 `config.json`、`~/.feishu-bot/config.json`（Windows 为 `%USERPROFILE%\.feishu-bot\config.json`）。缺少路径值、重复 `--config` 或拼错参数会直接报错，不会悄悄改用默认配置。

**部署目录**：`dataDir`（默认 `state`）、项目 `cwd`／`seedDir`／`trustedConfigDir` 和 `runtime.claudeExecutable` 的相对路径都以配置文件所在目录为基准；固定操作的 `actions[].cwd` 沿用旧版语义，相对路径以服务进程的工作目录为基准。`daemon install` 默认也把配置所在目录作为安装目录。因此应为配置选择一个固定目录，例如 `~/.feishu-bot`，不要放在源码 checkout 或带版本号的解压目录中。

**凭据**：飞书应用密钥从 `appSecretEnv`（默认 `FEISHU_APP_SECRET`）指定的环境变量读取，模型凭据从 `model.tokenEnv`（默认 `MODEL_AUTH_TOKEN`）读取；`model.extraHeaders` 的 `env:VARIABLE_NAME` 值和固定操作的 `env` 来源变量同样从环境读取。已有显式变量名继续有效，未配置时不会扫描其他账号或凭据；旧配置省略 `tokenEnv` 时，应补上实际变量名或设置新的默认变量。不要把密钥写进配置、命令参数或聊天，也不要提交 `service.env`。

**解析规则**：字段名区分大小写，未知字段直接拒绝；除旧版允许以 `null` 取默认值的少数可选容器外，`null` 也会被拒绝，可选字段请省略。配置语法错误不回显可能含密钥的原文。模板中的示例 ID、保留示例域名、模型占位名和占位凭据必须替换；群与私聊至少配置一项。`doctor` 报告这些未完成项，`run` 在连接飞书和打开运行状态前拒绝它们。

常用字段及默认值（取值范围由配置加载器校验，超出即拒绝启动）：

| 字段 | 默认 | 说明 |
|---|---|---|
| `tenantId`、`appId` | 无 | 飞书租户与应用 ID |
| `dataDir` | `state` | 状态库、工作区与隔离模式原生历史；须持久保存 |
| `model.baseUrl`、`model.model` | 无 | 见[模型、网络和时区](#模型网络和时区)；`smallModel` 省略时等于 `model` |
| `projects[]` | 至少一项 | 资源／权限单元；`maintainers` 必填，`permissionMode` 默认 `acceptEdits`，`nativeConfig` 默认 `isolated` |
| `groups[]`、`dms[]` | 空 | 群与私聊授权，见[飞书应用与授权](#飞书应用与授权) |
| `runtime.maxWorkers`、`runtime.idleMs` | 8、900000 | 活跃原生进程上限（1–128）与空闲回收时间，见[容量与恢复](#容量与恢复) |
| `limits.backgroundMessages`／`backgroundMs` | 0／900000 | 非 @ 群消息缓存条数（0–200）与保留时间；默认不缓存 |
| `limits.textChars` | 20000 | 单条输入正文字符上限（100–200000），超出时明确拒绝 |
| `limits.inboxPerRoom` | 64 | 每个聊天的待交付输入上限（1–64）；话题范围群按全群计算 |
| `limits.messagesPerMinute` | 30 | 每个聊天中每位作者的每分钟消息上限（1–1000） |
| `limits.attachmentBytes` | 20 MiB | 单个附件上限，最多 100 MiB |
| `limits.approvalMs` | 300000 | 审批与追问等待时间（1 秒–1 小时） |
| `limits.updateMs` | 1000 | 回复卡流式更新间隔（250–30000 ms） |
| `limits.imageBatchMs` 等 | 0 | 连续图片合批，见[连续图片](image-batches.md) |

`modelProfiles`、`runtime.claudeExecutable` 与容量参数分别在下文说明。场景化配置片段见[使用场景](usage.md)。

## 飞书应用与授权

创建飞书自建应用，开启机器人、消息接收和长连接，并配置交互卡片回调。基本交互需要接收群 @／私聊消息、发送回复与操作消息表情的权限。表情写入结果未知时的核对及完成表情回滚会读取本应用的消息表情列表，需要具有 `im:message.reactions:read` 或可用的 `im:message:readonly` 权限；没有读取权限时不应强行重发未知表情。按需历史工具还需要消息历史读取权限；群历史需相应的群消息读取范围，通常包括 `im:message.group_msg`。权限须在控制台申请并发布，最终以应用的实际可用范围为准。

原生打字效果需要应用可调用 CardKit 卡片创建及内容更新接口，通常须开通并发布 `cardkit:card:write`。明确的权限拒绝发生在发送消息之前时，可降级为同卡整卡更新；超时或结果未知不会触发重复发送。CardKit 的消息与卡片 ID、递增序列和最后确认快照保存在 `dataDir` 的状态库内，重启后的更新继续使用原卡片。实际手机和桌面效果仍需分别验收。

开发、测试和常驻部署使用独立应用及数据目录。同一应用的多个长连接可能分流事件，不能按广播订阅使用。

即使拥有读取群消息的权限，网关也只处理已授权的事件。授权与触发规则（显式群绑定、`chatId: "*"` 兜底绑定、`members`、`allowAllMembers`、维护者、明确 @、私聊 `dms`）以[对话行为](conversation-contract.md#授权与触发)为准；以下只列配置要点。

```json
{
  "groups": [
    { "chatId": "oc_example_group", "projectId": "assistant", "members": ["ou_example_member"] },
    { "chatId": "oc_example_team_group", "projectId": "assistant", "allowAllMembers": true, "sessionScope": "thread" },
    { "chatId": "oc_example_open_group", "projectId": "assistant", "allowAllMembers": true },
    { "chatId": "*", "projectId": "assistant" }
  ]
}
```

以上是合并到完整配置的群片段。

- 省略 `members` 或设为 `[]` 只允许项目维护者。`chatId: "*"` 至多出现一次，只覆盖未显式绑定的群，默认也只允许维护者；在兜底条目上添加 `members` 会为所有这些群追加获准作者，设置 `allowAllMembers: true` 则放开这些群的所有用户。`allowAllMembers` 不改变管理命令和审批所需的维护者身份，也不作用于私聊。
- `mentionOptional: true` 让该群所有获准作者的普通消息和命令无需 @；默认 `false`，只免除 @，不改变群与作者授权，修改后需重启。应用还需能收到未 @ 机器人的群消息（通常为 `im:message.group_msg`，以控制台实际发布范围为准），否则事件不会到达网关。仅建议用于成员很少且互信的小群。
- `sessionScope: "thread"` 让该群每个飞书话题使用独立会话；默认 `"chat"` 保持全群一条当前会话。可写在 `chatId: "*"` 兜底条目上，显式绑定优先；修改后需重启，原范围的会话保留，切换前请先处理完待交付输入。该模式不需要额外的消息权限；飞书拒绝话题回复的群（错误码 230071）应保持 `"chat"`。同一群的话题共用工作区、原生 HOME 和自动记忆，每个活跃话题占一个 `runtime.maxWorkers` 名额，话题较多的群应相应调大或设置 `runtime.maxWorkersPerChat`；`messagesPerMinute` 与 `inboxPerRoom` 仍按全群计算。行为细节见[话题会话](conversation-contract.md#话题会话)。
- `limits.backgroundMessages` 默认 **0**：不缓存、不预注入非 @ 群消息。需要前文时由 Agent 调用当前聊天限定的历史工具，参见[飞书历史接口](https://open.feishu.cn/document/server-docs/im-v1/message/list)。

本地排查授权可用 `config explain --chat oc_example_group --user ou_example_member --mentioned --json`：它复用真实房间、作者和触发规则，解释显式／兜底／私聊绑定、是否允许触发、会话管理与工具审批权限。私聊加 `--chat-type p2p`，模拟机器人消息加 `--bot`。该命令不联网、不打开状态库、不创建工作区、不读取密钥；结论只说明配置策略，不代表真实成员或飞书权限已经验证。

## 模型、网络和时区

`model.baseUrl` 使用 Claude Code 可调用的 Anthropic Messages 兼容地址（对应 `ANTHROPIC_BASE_URL`），`model`／`smallModel` 填实际模型 ID。可直连服务商或兼容网关；不做协议转换，不保证任意兼容模型都具备同样的工具与多模态能力。地址支持路径前缀，末尾 `/v1` 去重，不能包含凭据、查询串或片段。默认要求 HTTPS；`allowHttp` 仅用于明确选择的本地测试。`extraHeaders` 可引用 `env:VARIABLE_NAME`，缺失引用和头注入在运行前拒绝。模型额度与限流由上游控制，`runtime.maxWorkers` 只限制活跃原生进程数。

可选 `model.effort` 指定原生默认 effort；`modelProfiles` 是管理员定义的命名模型配置，只允许 `model`、`smallModel`、`effort`，不能覆盖端点、凭据或工具权限。聊天内 `/model` 查看并选择，`/profile` 同义；选择按会话保存，等待安全轮次边界后生效并保持原生 ID。未配置时使用默认模型。effort 支持由原生 CLI／模型服务决定，不用额外提示词模拟推理控制。[配置与行为](usage.md#会话模型配置)

Go 飞书 HTTP／WebSocket 客户端使用系统证书及 Go 的代理环境规则（`HTTP_PROXY`、`HTTPS_PROXY`、`NO_PROXY`）；原生 Claude 还接收获准的代理和证书环境。为 Go 服务安装的系统 CA 与 Claude 的 `NODE_EXTRA_CA_CERTS` 等配置需要分别验证，不能以一方连接成功推断另一方成功。不要关闭证书校验。

时区跟随环境，不绑定地区。需要一致的时间解释时可设置 `TZ=UTC`，它同时影响原生进程与输入元数据；未设置时使用主机时区。无法取得 IANA 时区名的主机（如 Windows）按当前偏移提供 `UTC±hh:mm` 形式；需要具体地区名（含夏令时规则）时请设置 `TZ`，发布二进制内置时区数据库，Windows 上同样可用。模型回复跟随请求语言，内置提示主要为中文。

## 直接使用本地目录与原生配置

已有本地工作流可以显式设置项目 `cwd`，直接在该目录运行，不复制到聊天工作区。相对路径以配置文件目录为基准；目录必须已存在。`cwd` 不能与 `seedDir` 或 `trustedConfigDir` 同时设置，避免混淆直接使用和复制配置。附件仍由各聊天分别保存；不同聊天指向同一目录时会共享文件，不提供文件写入隔离。

话题范围默认仍共用该群的项目目录。需要并行修改代码时，管理员可以先准备互不重叠的目录或 Git worktree，再用显式群绑定的 `threadWorkspaces` 将话题根消息 ID 绑定到目录：

```json
{
  "groups": [{
    "chatId": "oc_example_development",
    "projectId": "local-assistant",
    "sessionScope": "thread",
    "threadWorkspaces": {
      "om_example_feature_a": "./worktrees/feature-a",
      "om_example_feature_b": "./worktrees/feature-b"
    }
  }]
}
```

该片段合并到已有完整配置，保留原有成员策略。在目标话题内发送 `/status` 可查看话题根消息 ID 和当前配置的工作区绑定，`/status --json` 对应 `threadRoot`／`workspaceBinding`（空绑定表示使用群默认目录）。键是根消息的 `message_id`（话题内事件的 `root_id`），不是 `thread_id`。绑定仅适用于 `sessionScope: "thread"` 的显式群，不能配置在 `chatId: "*"` 上。相对路径以配置文件目录为基准，目标必须已存在且不能与网关房间状态目录重叠；`doctor` 检查目录是否存在。聊天消息不能新增或修改绑定。

绑定的话题直接使用目标目录，优先于项目 `cwd`，不执行该项目的 `seedDir`／`trustedConfigDir` 复制，不初始化或改写已有 `.git`；未绑定的话题继续原有项目目录规则。修改配置后须正常停止并重启服务。已有会话若曾在另一目录启动，或旧会话缺少可核对的启动身份，会拒绝续接并提示恢复原配置或在该话题显式 `/new`；原生会话 ID、历史和旧目录保留，不自动搬迁。新增会话沿用该话题当前绑定。

绑定改变原生工作目录及该目录加载的项目规则，不改变房间键、权限模式、原生 HOME 或话题附件范围；同一群的原生配置和会话记录仍可能共享，原生记忆按 Claude 的项目路径规则使用。需要不同文件互不覆盖时，必须绑定互不重叠的目录；把多个话题绑定到同一目录仍然共享文件。网关不自动创建、调度或清理 worktree，目录绑定也不构成同账号安全沙箱。

共享目录也会影响原生持久定时任务。Claude Code 2.1.317 的 `CronCreate` 在 `durable: true` 时将任务保存在原生项目根目录的 `.claude/scheduled_tasks.json`，其协调锁同样按项目划分；创建进程退出后，同项目的另一原生会话可以接收该任务。网关独立的聊天映射不能保证这些任务仍由原聊天执行或交付，`CronList` 也可能看到同项目的持久任务。因此不能把共享 `cwd` 下的持久任务当作已隔离到聊天的能力；需要这种归属保证时，应使用互不重叠的原生项目目录，并单独验收恢复和交付。会话内的非持久任务仍使用原生调度，不因保存了 session ID 就承诺进程退出后继续运行。网关不会复制、接管或自动重放原生定时任务。

`nativeConfig` 默认 `isolated`，各聊天使用独立原生配置。设为 `inherit` 时，使用服务账户的原生 Claude 配置，保留 `CLAUDE_CONFIG_DIR` 原本的设置或未设置状态：未设置时，用户 settings 在默认 `.claude` 目录，全局 MCP 等配置仍使用用户主目录的 `.claude.json`，不改为 `.claude/.claude.json`。用户配置、hooks、技能、MCP 和插件按原生 Claude 的加载规则生效；这也会加载对应工作目录中的项目规则。`inherit` 可配合 `cwd`，也可在独立聊天工作目录中使用。先审核这些配置及其执行权限，再开放给获准聊天。

```json
{
  "projects": [{
    "id": "local-assistant",
    "maintainers": ["ou_example_maintainer"],
    "cwd": "./local-work",
    "nativeConfig": "inherit",
    "permissionMode": "bypassPermissions"
  }]
}
```

以上是合并到完整配置的项目片段。`permissionMode` 可选 `default`、`acceptEdits`（默认）和 `bypassPermissions`。`bypassPermissions` 是显式选择的原生 YOLO 权限模式：它跳过常规工具权限确认，但不取消显式 deny／ask 规则或原生规则；追问仍需交互，网关的受控操作、发送文件等独立确认也不因此全部放行。不要把该选项理解为聊天成员获得管理员权限。权限边界见[权限与信任范围](../SECURITY.md)。

`nativeConfig: "inherit"` 时，原生文件钩子会拒绝范围覆盖网关状态目录（`dataDir` 下的房间目录）的 Grep／Glob，例如对用户主目录整体搜索；请搜索项目目录或更具体的子目录。Grep／Glob 的路径还须是普通绝对路径或相对路径：首尾不可见字符、`~` 展开、Windows 无盘符的根路径、驱动器相对路径、UNC 与设备路径（`\\?\`、`\\server\share`）均被拒绝，共享目录请映射盘符后使用；Windows 上路径经过目录联接（junction）或其他重解析点时也会被拒绝。这只是纵深防护，同账号目录不是安全沙箱，Bash 仍可读取同账号可访问的文件。

`runtime.claudeExecutable` 指定原生 Claude Code 可执行文件，相对路径以配置文件目录为基准；省略时使用服务账户 PATH 中的 `claude`。Go 版不内置或自动下载 CLI。`doctor` 执行选定程序的 `--version`，确认其能启动并标识为 Claude Code；继承模式还检查原生配置目录存在，但不执行 hooks、MCP 或插件，不证明它们已可用。升级 CLI 后按[验证与验收](validation.md)重跑协议回归。

隔离模式的原生历史随 `dataDir` 保存；继承模式的原生历史位于共享 Claude 配置目录的 `projects` 树中，按原生 cwd 归档。因此继承部署须同时备份网关状态与实际 Claude 历史目录。共享历史位置不代表其他聊天可以通过 `/sessions` 或 `/resume` 访问这些会话，也不会自动导入其他桥接服务的历史映射。

会话首次创建后固定其工作目录与原生配置身份。修改 `cwd` 或 `nativeConfig` 后，应明确新建会话；已有会话不能通过重启或回收悄悄转到另一个目录或配置身份，恢复失败保留原映射并报错。

## 首次启动检查

`doctor` 默认离线，检查本地配置和运行条件；`ready: true` 只表示报告列出的检查通过，不代表已验证飞书身份、模型工具调用或真实协作。逐层联网检查和测试聊天验证见[首次连接](first-connection.md)。

- 使用独立配置时传完整参数，例如 `doctor --config ./config.local.json`；`--env-file <KEY=VALUE 文件>` 可在检查前载入凭据。
- 数据目录检查使用独立临时 SQLite 文件，结束后删除，不打开现有 `gateway.sqlite`，也不留下固定 `doctor.sqlite`。目录不可写或配置的资料目录不存在时仍输出其余检查项；先处理报告中的失败项，再启动。
- `native` 字段报告实际版本、固定已测版本与 `tested`／`untested` 状态；未知版本不被伪装成已验证，也不单凭版本不同阻止启动。`storage` 只读取数据库、WAL、SHM 和导出目录文件元数据，不打开生产 SQLite 或执行 checkpoint。
- Windows 上 CC 找不到 Git Bash 时，可设置 `CLAUDE_CODE_GIT_BASH_PATH` 指向实际 `bash.exe`；`doctor` 会执行该程序并检查版本，不只凭文件存在判断就绪。未显式设置时不强制要求 Git Bash，原生工具支持情况仍需实际验证。JSON 中的 Windows 路径使用正斜杠或转义反斜杠。
- 显式 `--probe-feishu` 只查询应用认证与机器人身份；显式 `--probe` 发送一个可能计费的基础模型请求，未完成配置时跳过，HTTP 200 的登录页或非 Messages JSON 不算通过，只检查基础连接，不代表流式、工具、子代理、图片或长上下文已验收。诊断不打印模型回复或凭据。
- 显式 `--verify-chat <已配置群ID>` 或 `--verify-chat dm:<已配置用户ID>` 验证入站、卡片、回调与小文件；须先停止同一数据目录的服务，不添加成员、不启动模型。

## 内置 daemon 服务

本节适用于 Linux／Windows。内置命令不生成任何启动脚本；macOS 没有内置 launchd 管理器，见[外部服务管理](#外部服务管理)。

```sh
./bin/feishu-bot daemon install --config ~/.feishu-bot/config.json
./bin/feishu-bot daemon status
./bin/feishu-bot daemon logs -f
./bin/feishu-bot daemon stop
./bin/feishu-bot daemon start
./bin/feishu-bot daemon restart
./bin/feishu-bot daemon uninstall
```

### 安装

`daemon install [--config PATH] [--env-file PATH] [--work-dir DIR] [--log-file PATH] [--log-max-size MB] [--log-max-backups N] [--binary PATH] [--replace] [--no-start]`：

1. 按[配置路径](#配置文件与凭据)规则解析 `--config`。安装目录默认为配置所在目录，可用 `--work-dir` 覆盖。
2. 准备 `<install-dir>/service.env`：优先使用 `--env-file`（`KEY=VALUE` 行，不写 `export`），其次沿用已有 `service.env`；都没有时从当前进程环境生成，只包含配置所需凭据和白名单运行变量（`PATH`、`TZ`、`LANG`／`LC_ALL`、代理、CA 证书、`CLAUDE_CONFIG_DIR`、`CLAUDE_CODE_GIT_BASH_PATH` 等）。已有 `service.env` 不会被当前环境中的新值覆盖；需要更新凭据时传 `--env-file`。缺少必需凭据会拒绝安装。`service.env` 权限为 0600，含凭据，不要提交或公开。
3. 运行离线 `doctor` 检查；失败时打印脱敏报告并拒绝安装，不调用飞书或模型，也不改动现有安装和 `service.env`。
4. 检查同名任务／unit：与本次将生成的定义不同（包括不属于本项目的同名定义）时拒绝，需要显式 `--replace`。
5. 把源二进制（默认当前可执行文件，可用 `--binary` 指定）复制到 `<install-dir>/bin/feishu-bot[.exe]`，停止正在运行的旧实例，注册平台服务，并默认启动；`--no-start` 只安装。启动后最多等待 30 秒出现 `gateway.lock`。升级时的暂存和回滚见[升级与回滚](#升级与回滚)。
6. 成功后写入 `~/.feishu-bot/daemon.json`（schema 1；`FEISHU_BOT_HOME` 可覆盖该目录），记录绝对 config／env／log／data 路径、平台、版本和安装时间，不保存密钥。其他 `daemon` 子命令读取该文件，不需要重复传 `--config`。

使用相同参数重复执行 `daemon install` 是幂等的：停止旧实例、替换二进制、重写服务定义并重新启动。任务或 unit 定义中不包含密钥。

### 平台

- **Windows**：任务计划程序在当前用户登录时直接运行安装目录中的二进制，任务动作为绝对路径的 `run --service --config ... --env-file ... --log-file ...`，不经过 wscript、PowerShell 或 VBS；`run --service` 在日志打开后关闭控制台窗口。登录任务不是 Windows SCM 服务：未登录不运行，以当前交互用户身份运行，这样可以保留 `nativeConfig: inherit` 的原生配置和用户环境。需要开机即运行或独立服务账户时，另行使用专用部署，不能假设同一账户目录是安全沙箱。需要继承配置时，应使用拥有该原生配置的账户；自动启动环境与交互终端可能不同。
- **Linux**：root 使用 `/etc/systemd/system/feishu-bot.service`，非 root 使用 `~/.config/systemd/user/feishu-bot.service`；`ExecStart` 带绝对 config／env／log 参数，失败时由 systemd 自动重启。非 root 常驻建议执行 `loginctl enable-linger <user>`，否则用户注销后服务停止。

### 日志、停止与 stale lock

- `--log-file` 默认 `<install-dir>/logs/service.log`，`--log-max-size` 默认 10 MB，`--log-max-backups` 默认 3。`daemon logs` 默认显示末尾 100 行，`-n N` 指定行数，`-f` 跟随并处理轮转。
- `daemon status` 显示 Running／Stopped／Stopping／Stale lock、PID、配置、环境文件、日志和工作目录；`--json` 供脚本读取。
- `daemon stop` 写入 `<dataDir>/gateway.stop`，等待 `gateway.lock` 释放和进程退出（最多 45 秒），不使用强杀。停止会中断正在执行的前台任务，未确认的外部写操作不会自动重放。不要用 `schtasks /End`、`Stop-Process` 或重命名锁来加速停止。
- 未确认关闭会保留 `gateway.lock`。Windows 注销／关机也可能直接终止任务进程并留下锁，下次启动会失败。核实锁记录的 PID 及本实例派生进程均已退出后，才可显式使用 `daemon start --reclaim-stale-lock`；该命令只删除已无进程持有的锁，不杀进程。
- `daemon uninstall` 停止服务并删除任务／unit 和 `daemon.json`，保留 config、`service.env`、日志和 state。

## 外部服务管理

macOS，或需要专用账户、开机即运行的 Linux system 部署，可以由外部服务管理器运行 `feishu-bot run`。明确配置工作目录、`--config`、`--env-file`、`--log-file`（可配合 `--log-max-size`、`--log-max-backups`）和原生 Claude Code 路径。网关自身的配置、授权、状态持久化和停止确认规则与其他平台相同；内置 `daemon install/start/stop` 不适用于 macOS。

Linux 的 [systemd 示例](../deploy/feishu-bot.service)适合专用账户的 system 部署；按安装位置修改服务账户、工作目录和二进制路径。使用专用非 root 账户，配置与环境文件仅授予必要读取权限。环境文件不写 `export`：

```text
FEISHU_APP_SECRET=replace-with-app-secret
MODEL_AUTH_TOKEN=replace-with-model-token
TZ=UTC
```

```sh
./bin/feishu-bot doctor --config /etc/feishu-bot/config.json
sudo systemctl enable --now feishu-bot
journalctl -u feishu-bot -f
```

`dataDir` 使用固定持久目录，例如 `/var/lib/feishu-bot`。数据库和其中的原生 HOME／transcript 必须一起保留，不能只备份 session ID 而删除历史文件。服务启动不预开所有会话；首次任务才启动对应 CC。

前台 `run` 收到 Ctrl+C 或终止信号时请求关闭本实例的会话进程树；停止失败会记录 `shutdown_unconfirmed`，不能据此认为所有派生进程均已退出。切换同一飞书应用的运行方式前，确认旧服务及其自动启动入口已停用，避免两个长连接分流事件。

## 升级与回滚

升级前备份 `dataDir`（继承模式另备份实际 Claude 历史目录）。SQLite 增量迁移在启动时保留会话映射，恢复阶段会重新授权待交付消息；不要删除数据库来完成升级，也不要让两个实例同时使用同一数据目录。配置和行为变化见下文及[对话行为](conversation-contract.md#升级)；从 TypeScript 版切换时先完成 [Go 迁移](go-migration.md)。

### 用 daemon 升级二进制

Linux／Windows 用新二进制重新执行安装：直接运行新二进制的 `daemon install`，或用 `daemon install --binary <新二进制>`。保持与原安装相同的 `--config`、`--work-dir` 和日志参数；参数变化会改变服务定义，需要 `--replace`。

升级按以下顺序进行，尽量让失败发生在旧服务停止之前：

1. 预检、所有权检查和凭据检查全部在旧服务仍运行时完成。
2. 新二进制和新 `service.env` 先完整读取并暂存为安装目录内的私有临时文件；旧二进制、`service.env`、`daemon.json` 及服务定义（Linux 为 unit 正文、文件权限和 `enabled`／`disabled` 状态，Windows 为任务定义）保存到安装目录下的 `.feishu-bot-upgrade-*` 私有恢复目录。
3. 再停止旧服务。停止失败时不替换任何文件；旧网关可能已收到停止请求，用 `daemon status` 核实，若已退出再 `daemon start`。
4. 依次发布二进制和 `service.env`、注册服务定义、保存 `daemon.json`。任一步失败都会尝试恢复旧文件和服务定义，旧服务原本在运行时重新启动它；恢复成功后删除恢复目录，错误末尾注明已恢复。恢复不完整时保留恢复目录，并在错误中给出路径。
5. 新安装全部提交后才调用启动。启动失败或 30 秒内未出现 `gateway.lock` 时不回滚，因为新进程可能已经运行；此时保留一致的新安装和恢复目录，用 `daemon status`、`daemon logs` 核实后再 `daemon start`。

恢复目录含凭据，按原安装数据的权限保管；处理完失败安装后由维护者删除错误中列出的目录。`gateway.lock` 出现只说明进程已启动，飞书连接与模型能力仍按[首次连接](first-connection.md)验收。

两类平台限制会在停止旧服务、发布文件之前拒绝安装，`--replace` 也不跳过：

- Linux 旧 unit 为符号链接、位于受管路径以外、处于 `masked`、`enabled-runtime`、`static` 等 `enabled`／`disabled` 以外的状态，或无法完整列出 unit 文件时，无法自动回滚；需要维护者先明确处理这些服务定义。
- Windows 通过 Task Scheduler 的结构化错误码确认任务是否不存在；查询取消、权限不足或调度器异常会阻止安装，不作为缺失任务继续覆盖或回滚删除。

降级到较旧版本没有回归验证：旧版本不识别新版附加表中的状态（如原生身份未决、停止暂停），可能绕过这些保护。确需回退时先停止服务并备份完整 `dataDir`。

前台 `run` 或外部服务管理器部署：先按原方式正常停止并确认原生子进程退出，替换二进制，再启动。

### 从脚本式启动迁移

从旧的 VBS／PowerShell／Node launcher 迁移时，先停止旧服务、确认其原生子进程退出，并删除旧的自启动任务和脚本，然后运行 `daemon install`。服务命令不会猜测或接管其他工具的任务；发现同名但不是本项目安装的任务时会拒绝覆盖，需人工确认或显式 `--replace`。

### 配置兼容

- 显式非零的 `backgroundMessages` 不会自动变为 0，应按新行为设为 0。既有 CC 历史不会因关闭背景而消失；需要干净上下文时显式 `/new`。
- 群 `members` 省略时只允许维护者，普通成员须显式列出，或用 `allowAllMembers: true` 放开该群。`chatId: "*"`、`mentionOptional`、`sessionScope`、图片合批与容量参数都可省略，缺省保持原行为。
- 原生运行不支持容器字段：`runtime.mode` 只接受历史值 `"local"`，`runtime.image`／`memory`／`cpus` 和 `model.maxConcurrent` 已移除，出现即拒绝启动，须删除；模型限流在上游配置。
- `autoApproveWorkspace: false` 按原逐次权限语义读取为 `permissionMode: "default"`，`true` 被拒绝；可显式迁移到 `permissionMode`／`allowedTools`，不会静默提权。

## 容量与恢复

`runtime.maxWorkers` 默认 8（1–128），限制同时存在的原生进程；`idleMs` 默认 900000（15 分钟），0 关闭定时回收。容量满时即使关闭定时回收，也可释放确认空闲的旧进程。

以下三个可选参数均默认 0，保留原有行为：

| 参数 | 含义 |
|---|---|
| `runtime.maxWorkersPerChat` | 每个聊天可占用的原生进程上限，范围 0–`maxWorkers`；同群各话题共用额度，0 不另设限制。额度满时优先回收本聊天确认空闲的进程，其余输入保留排队，其他聊天仍可使用剩余全局名额。 |
| `runtime.maxRssMiB` | 新进程准入时检查网关与当前父子关系下后代进程的 RSS 合计，达到阈值先尝试回收确认空闲进程；仍不足或观测不可用则等待。Linux 使用 procfs，Windows 使用 Working Set，macOS 使用系统 `ps` 并排除采集进程。0 不采集、不限制。 |
| `runtime.idleMinMs` | 设为正数时，首次成功输入后的空闲保温时间从该值开始，每次成功复用加倍，最多 `idleMs`；取值不得大于 `idleMs`，`idleMs` 为 0 时只能为 0。0 使用固定 `idleMs`。 |

例如在完整配置的 `runtime` 中使用 `{"maxWorkers":8,"maxWorkersPerChat":2,"maxRssMiB":2048,"idleMs":900000,"idleMinMs":60000}`。应按实际机器与负载选值，示例不代表容量保证。RSS 合计可能重复计算共享页，快照期间进程也会变化；这是限制继续新建的信号，不是硬内存上限，已有进程仍可增长。超过阈值不会中断活动输入、后台工作、审批或已有输入流。设置 `maxRssMiB` 后，服务的定期维护也会在可观测内存超限时回收确认空闲的进程。因容量等待的输入会保存，并在聊天中提示等待原因。

容量等待、复用与回收的诊断事件见[运行诊断](runtime-diagnostics.md#容量等待复用与回收)。

回收只释放进程，不改 session ID、工作文件或记忆；任务、未确认输入、后台工作、Monitor、原生会话定时工作、审批和最终回复交付未结束时不回收，未知状态保守保留。会话续接、`/stop`、`/new` 和 `/compact` 的语义见[对话行为](conversation-contract.md#连续会话与进程生命周期)。以下是运维需要关注的恢复状态：

- **关闭未确认**：Worker 停止失败（包括关闭期间才完成初始化的 Worker）时，服务以非零退出码结束并保留 `gateway.lock`，阻止新实例接管同一数据目录。正常关闭等待进程树退出和最后事件处理完毕后释放锁。异常退出后先核实锁记录 PID 及本实例派生进程均已结束，再清理旧锁；仅确认主进程 PID 已消失还不够。恢复历史不复活旧后台进程，旧审批失效，结果未知的写操作不重放。
- **原生身份未决**：原生 ID 写库失败时先重试，再写入聊天工作区的标记文件，下次启动补存并续接；两者都失败时该会话保持未决，需维护者用 `/resume` 或 `/new` 明确选择。不要用旧版本绕过该状态。规则见[恢复与原生身份](conversation-contract.md#恢复与原生身份)，机制见[原生进程生命周期](architecture.md#原生进程生命周期)。
- **停止暂停**：状态库不可写时，停止暂停改存为 `dataDir/stop-pauses` 下的标记文件；标记损坏或不可读时拒绝恢复。两者都无法写入时仍优先终止进程并报告错误，但无法跨重启保存停止意图，应先修复存储再成功执行一次 `/stop`。用户可见规则见[停止](conversation-contract.md#停止)。
- **遗留输入**：原生退出时遗留输入状态暂时不可写，会保留协调状态；存储恢复后扫描并标记为 uncertain，释放队列占用，不重放已发送输入。
- **回执与输出**：表情清理与输出更新可重试，但不会因回执失败重做任务；明确失败的表情创建按普通创建重试，权限等不可重试的拒绝直接放弃，结果未知的创建只核对、不重建，一小时后仍未观察到则停止核对。超长答案的卡片和完整 `reply.md` 文件共同组成一次交付，`/outputs` 区分卡片和文件阶段。文件发送结果未知时不自动重发；核对后使用 `/retry-output <ID>` 继续同一交付，可能重复已被远端接受的文件。补发读取持久化答案，不重新调用模型或重做外部操作。

背景与去重记录有内置清理；原生 transcript、附件、导出、状态库与未确认结果按部署的数据策略备份归档。

### 附件与传输预算

普通附件使用一次 GET；只有平台明确返回资源码 `234037` 才回退到顺序 Range 下载，每段最多 4 MiB，边下载边写入私有文件。校验 Content-Range、长度、总大小和 ETag 一致性；中途取消、资源变化、截断或续传被忽略会失败并清理部分文件。下载不扩大原来的消息／作者／资源授权，整次操作仍受 60 秒截止以及 `limits.attachmentBytes` 约束（默认 20 MiB、最多 100 MiB）。大于上限的文件仍拒绝，不因为支持 Range 就绕过限制。

飞书 HTTP 请求在一个客户端实例内共享最多 8 个活动请求、64 个等待者；卡片预览最多 2 个，文件／资源另有分组上限。429 形成同端点组共享的最早可重试时间，排队被拒与发出后未知分开处理。上限控制请求数与等待数，不代表平台配额保证，也不使未知创建／上传自动可重试。运行诊断见[传输与存储预算](runtime-diagnostics.md#传输与存储预算)。

## 本地终端交接

先从 `/sessions` 复制完整网关会话 ID，再停止当前服务。命令从已有 `gateway.sqlite` 查找会话，不接受原生 ID、名称或模糊短 ID；若选择了历史会话，后续服务也继续该选择。

```sh
feishu-bot daemon stop
feishu-bot session run --session 00000000-0000-4000-8000-000000000001 --config /path/to/config.json --env-file /path/to/service.env
feishu-bot daemon start
```

前台 `run` 部署先按原方式正常停止，终端交接结束后再运行原启动命令。使用真实交互终端和相同服务账户；继承模式也需相同用户配置、代理／证书环境及 `CLAUDE_CONFIG_DIR`。`--env-file` 可省略，但模型凭据须已在环境中。命令不修改 cwd 或配置来适配历史，会严格核对保存的启动身份及当前路由授权。

全局服务锁在整个本地交互期间保留，不能同时开启飞书服务。存在该聊天的 queued／sent／consumed 输入、未确认输出、待审批或运行中的操作时拒绝交接；结果未知（uncertain）的旧输入或操作只在启动时提示数量，保留为证据且不重放，不永久阻止交接。启动前检查（终端、凭据、模型配置、可执行文件）失败时不切换该聊天的当前会话；待切换模型配置也须先在飞书处理。直接退出或 Ctrl+C 后，只有进程树清理得到确认才释放锁；未确认时按 stale lock 流程核查，不自动启动替代实例。

原生 TUI 使用保存的 native ID、工作目录、配置模式、已应用模型配置和权限模式。权限提示在终端回答；飞书专用 MCP、固定操作 relay 与飞书输出交付不在本地开启。该期间的原生用量不会写入网关用量记录，覆盖缺口会显示在 `/usage` 中。此入口服务于同一受信任账户下的接续，目录和锁不构成多租户沙箱。

[场景配置](usage.md) · [对话规则](conversation-contract.md) · [权限与数据边界](../SECURITY.md) · [验收](validation.md)
