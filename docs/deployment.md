# 部署与配置

本页是安装、配置、飞书应用权限、常驻服务、升级与恢复的操作说明。对话与会话行为以[对话行为](conversation-contract.md)为准，模块与并发模型见[架构](architecture.md)。

Linux、macOS 和 Windows 运行同一个 Go 服务。部署只需网关二进制、Git 和原生 Claude Code；Windows 使用 Git for Windows。协议测试固定使用 Claude Code 2.1.321，升级 CLI 后须重新验收。内置 `daemon` 管理仅支持 Linux／Windows；macOS 使用前台 `run` 或自行配置服务管理。

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

上述构建未注入版本，`--version` 显示 `dev`，并附 Go 工具链记录的提交，例如 `dev-g1a2b3c4`（工作区有未提交改动时为 `dev-g1a2b3c4-dirty`）。用于常驻服务的本地构建建议注入 `git describe` 版本，便于 `--version`、`daemon status` 区分构建来源；需要 Git Bash 或其他 POSIX shell，且仓库已获取 tag：

```sh
version="$(git describe --tags --long --dirty --match 'v[0-9]*')"
CGO_ENABLED=0 go build -trimpath -ldflags "-X github.com/sean2077/feishu-bot/internal/cli.Version=${version#v}" -o ./bin/feishu-bot ./cmd/feishu-bot
```

结果形如 `0.5.0-3-g1a2b3c4`：最近的版本 tag、其后的提交数和提交短 SHA，未提交改动追加 `-dirty`。`--long` 使 tag 所在提交的本地构建显示为 `0.5.0-0-g…`，与发布产物的 `0.5.0` 区分；发布版本的注入见[版本发布](releasing.md)。版本字符串只用于显示和 `daemon.json` 记录，不参与兼容判断。

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
| `projects[].permissionMode` | `acceptEdits` | `default`、`acceptEdits`、`bypassPermissions` 或 `auto`；`auto` 使用原生安全分类器，见[auto 权限模式](#auto-权限模式) |
| `projects[].bash.rejectCwdPrefix` | `false` | 拒绝以 `cd <当前原生 cwd> &&`／`;` 开头的 Bash 命令，减少重复审批噪音；不是安全边界，见[Bash cwd 前缀](#bash-cwd-前缀) |
| `projects[].env`、`projects[].envPassthrough` | 空 | 项目级原生进程环境变量；`env` 为非敏感字面值，`envPassthrough` 从服务环境复制凭据，见[项目环境变量](#项目环境变量) |
| `projects[].settingsFile` | 空 | 管理员维护的原生 settings 快照；只允许 `env`、`permissions.allow/ask/deny` 和 `autoMode`，见[项目设置文件](#项目设置文件) |
| `projects[].appendSystemPrompt`、`appendSystemPromptFile` | 空 | 二选一的项目系统提示词；只作为模型指引，不改变授权/权限，见[项目系统提示词](#项目系统提示词) |
| `projects[].preContext` | 空 | 新一轮人类输入前执行一个只读固定操作并注入带出处的背景材料；默认关闭，见[预检索上下文](#预检索上下文) |
| `projects[].owners` | 空 | maintainers 子集；仅用于 `forbidSelfApproval` 时的 owner 例外，不增加其他权限 |
| `projects[].approvals.forbidSelfApproval` | `false` | 开启后发起人不能批准自己的请求，owner 例外；见[审批资格与自批](#审批资格与自批) |
| `actions[].approvers` | 空 | 该操作的指定审批人；配置后替代 maintainers，不隐式加入 maintainers 或 owners |
| `projects[].feedback.enabled` | `false` | 在最终 Done 回复卡上显示「有用／无用」反馈，见[回复反馈](#回复反馈) |
| `feedback.export.enabled`、`feedback.export.dir` | `false`、空 | 维护前按项目、UTC 日期归档反馈；开启时 `dir` 必填且目录须已存在，见[回复反馈](#回复反馈) |
| `projects[].displayWorkspace` | 空 | 回复卡工作目录显示：`full`、`basename`、`project`、`hidden`；空时群聊显示项目 id，私聊显示完整路径 |
| `groups[]`、`dms[]` | 空 | 群与私聊授权，见[飞书应用与授权](#飞书应用与授权) |
| `groups[].topicHistory` | `"topic"` | `"topic"` 默认只读当前话题；`"chat"` 允许 `read_chat_history` 的 `scope: "chat"` 读取同群时间线和其他话题。只适用于 `sessionScope: "thread"`，其他群配置会在加载时拒绝 |
| `dms[].historyChats` | 空 | 私聊跨群读取白名单：`[{"chatId","name"}]`，目标必须是显式 `groups[]` 绑定；发起人须为目标群 maintainer 或显式 member（`allowAllMembers` 不算），只读文本并审计；读取后本轮写操作改为 DM 用户审批 |
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

回复卡页脚的工作目录由 `projects[].displayWorkspace` 控制：`full` 显示绝对路径，`basename` 只显示目录名，`project` 显示项目 id，`hidden` 不显示。省略时群聊默认 `project`、私聊默认 `full`；已有卡片保留生成时的内容。该设置只影响展示，不改变原生工作目录。

## 飞书应用与授权

创建飞书自建应用，开启机器人、消息接收和长连接，并配置交互卡片回调。基本交互需要接收群 @／私聊消息、发送回复与操作消息表情的权限。表情写入结果未知时的核对及完成表情回滚会读取本应用的消息表情列表，需要具有 `im:message.reactions:read` 或可用的 `im:message:readonly` 权限；没有读取权限时不应强行重发未知表情。按需历史工具还需要消息历史读取权限；群历史需相应的群消息读取范围，通常包括 `im:message.group_msg`。权限须在控制台申请并发布，最终以应用的实际可用范围为准。

原生打字效果需要应用可调用 CardKit 卡片创建及内容更新接口，通常须开通并发布 `cardkit:card:write`。明确的权限拒绝发生在发送消息之前时，可降级为同卡整卡更新；超时或结果未知不会触发重复发送。CardKit 的消息与卡片 ID、递增序列和最后确认快照保存在 `dataDir` 的状态库内，重启后的更新继续使用原卡片。实际手机和桌面效果仍需分别验收。

开发、测试和常驻部署使用独立应用及数据目录。同一应用的多个长连接可能分流事件，不能按广播订阅使用。

即使拥有读取群消息的权限，网关也只处理已授权的事件。授权与触发规则（显式群绑定、`chatId: "*"` 兜底绑定、`members`、`allowAllMembers`、维护者、明确 @、私聊 `dms`）以[对话行为](conversation-contract.md#授权与触发)为准；以下只列配置要点。

```json
{
  "groups": [
    { "chatId": "oc_example_group", "projectId": "assistant", "members": ["ou_example_member"] },
    { "chatId": "oc_example_team_group", "projectId": "assistant", "allowAllMembers": true, "sessionScope": "thread", "topicHistory": "chat" },
    { "chatId": "oc_example_open_group", "projectId": "assistant", "allowAllMembers": true },
    { "chatId": "*", "projectId": "assistant" }
  ]
}
```

以上是合并到完整配置的群片段。

- 省略 `members` 或设为 `[]` 只允许项目维护者。`chatId: "*"` 至多出现一次，只覆盖未显式绑定的群，默认也只允许维护者；在兜底条目上添加 `members` 会为所有这些群追加获准作者，设置 `allowAllMembers: true` 则放开这些群的所有用户。`allowAllMembers` 不改变管理命令和审批所需的维护者身份，也不作用于私聊。
- `mentionOptional: true` 让该群所有获准作者的普通消息和命令无需 @；默认 `false`，只免除 @，不改变群与作者授权，修改后需重启。应用还需能收到未 @ 机器人的群消息（通常为 `im:message.group_msg`，以控制台实际发布范围为准），否则事件不会到达网关。仅建议用于成员很少且互信的小群。
- `sessionScope: "thread"` 让该群每个飞书话题使用独立会话；默认 `"chat"` 保持全群一条当前会话。可写在 `chatId: "*"` 兜底条目上，显式绑定优先；修改后需重启，原范围的会话保留，切换前请先处理完待交付输入。该模式不需要额外的消息权限；飞书拒绝话题回复的群（错误码 230071）应保持 `"chat"`。同一群的话题共用工作区、原生 HOME 和自动记忆，每个活跃话题占一个 `runtime.maxWorkers` 名额，话题较多的群应相应调大或设置 `runtime.maxWorkersPerChat`；`messagesPerMinute` 与 `inboxPerRoom` 仍按全群计算。行为细节见[话题会话](conversation-contract.md#话题会话)。

`groups[].topicHistory` 默认 `"topic"`，保持话题会话只读当前话题。设为 `"chat"` 后，`read_chat_history` 接受 `scope: "chat"`：不带 `threadId` 读取本群时间线，带 `threadId` 只能读取已在本聊天观察到、且属于本群的话题；`expandThreads: true` 会并发读取最近 20 个话题、每话题最多 10 条回复，并按最近回复时间倒序。发现话题时会把群时间线窗口放宽到 3 天再按回复时间筛选，因此更早创建但近期有回复的话题可能仍不在本页；返回统计 `threads`、`expanded`、`omittedThreads`、`omittedReplies`、`failedThreads`，总正文预算沿用 40000 字，先减少每话题回复数，再减少话题数；单个话题读取失败时跳过并降级，全部发现话题都失败才报错。`history_chat_scope_read` 的 `threads` 列出所有被发现的话题根，不只是最终展开的那些。其他话题的消息带 `background: true`，仍按当前群的作者规则过滤；绝不跨群。该字段只对 `sessionScope: "thread"` 有意义，配置在普通群上会加载失败，避免被静默忽略。

`dms[].historyChats` 默认空，只在私聊绑定上生效：

```json
{
  "dms": [{
    "userId": "ou_example_owner",
    "projectId": "assistant",
    "historyChats": [
      { "chatId": "oc_example_team_group", "name": "team group" }
    ]
  }]
}
```

目标 `chatId` 必须是 `groups[]` 中显式绑定的群，不能是 `*` 兜底或私聊；同一私聊内 `name` 与 `chatId` 合并后必须唯一。配置后，该私聊的 `read_chat_history` 可传 `chat`（配置名或 chatId），其余参数与 `scope: "chat"` 相同；目标群是话题模式时复用群时间线和 `expandThreads`。每次调用（包括游标续页）都要求存在活动人类请求，发起人必须是该 DM 用户，并且在目标群 `maintainers` 或显式 `members` 中；`allowAllMembers` 不构成跨群授权。跨群结果只含文本，不返回附件，`fetch_attachment` 对目标群消息不可用；`threadId` 仍需先通过时间线或展开观察到。读取成功后本轮标记为污染：写工具、`send_file`、非只读 `project_action` 和未知工具需要 DM 用户确认，只读工具放行；非只读 action 先由 DM 用户确认污染，再按原有 approvers／`forbidSelfApproval` 审批，污染确认不能替换原审批。新的用户输入且后台任务结束后清除标记，`/stop`、超时和 worker 结束一律拒绝挂起的污染审批。每次跨群读取写 `history_cross_chat_read`，每次污染审批写 `tainted_turn_prompt`，都不记录正文。详细边界和残余风险见 [SECURITY](../SECURITY.md)。
- `limits.backgroundMessages` 默认 **0**：不缓存、不预注入非 @ 群消息。需要前文时由 Agent 调用当前聊天限定的历史工具，参见[飞书历史接口](https://open.feishu.cn/document/server-docs/im-v1/message/list)。

本地排查授权可用 `config explain --chat oc_example_group --user ou_example_member --mentioned --json`：它复用真实房间、作者和触发规则，解释显式／兜底／私聊绑定、是否允许触发、会话管理与工具审批权限。私聊加 `--chat-type p2p`，模拟机器人消息加 `--bot`。该命令不联网、不打开状态库、不创建工作区、不读取密钥；结论只说明配置策略，不代表真实成员或飞书权限已经验证。

## 模型、网络和时区

`model.baseUrl` 使用 Claude Code 可调用的 Anthropic Messages 兼容地址（对应 `ANTHROPIC_BASE_URL`），`model`／`smallModel` 填实际模型 ID。可直连服务商或兼容网关；不做协议转换，不保证任意兼容模型都具备同样的工具与多模态能力。地址支持路径前缀，末尾 `/v1` 去重，不能包含凭据、查询串或片段。默认要求 HTTPS；`allowHttp` 仅用于明确选择的本地测试。`extraHeaders` 可引用 `env:VARIABLE_NAME`，缺失引用和头注入在运行前拒绝。模型额度与限流由上游控制，`runtime.maxWorkers` 只限制活跃原生进程数。

可选 `model.effort` 指定原生默认 effort；`modelProfiles` 是管理员定义的命名模型配置，只允许 `model`、`smallModel`、`effort`，不能覆盖端点、凭据或工具权限。`projects[].modelProfile` 可指定该项目未显式选择配置时的默认项，值须引用已声明的配置或 `default`，否则配置加载失败；会话显式选择 `default` 会固定使用部署默认，不跟随项目默认。项目默认改变后，未选中配置的会话在下次原生进程启动或续接时切换模型并保留历史。聊天内 `/model` 查看并选择，`/profile` 同义；选择按会话保存，等待安全轮次边界后生效并保持原生 ID。未配置时使用默认模型。effort 支持由原生 CLI／模型服务决定，不用额外提示词模拟推理控制。[配置与行为](usage.md#会话模型配置)

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

以上是合并到完整配置的项目片段。`permissionMode` 可选 `default`、`acceptEdits`（默认）、`bypassPermissions` 和 `auto`。`bypassPermissions` 是显式选择的原生 YOLO 权限模式：它跳过常规工具权限确认，但不取消显式 deny／ask 规则或原生规则；追问仍需交互，网关的受控操作、发送文件等独立确认也不因此全部放行。不要把该选项理解为聊天成员获得管理员权限。权限边界见[权限与信任范围](../SECURITY.md)。

### auto 权限模式

`permissionMode: "auto"` 使用 Claude Code 的原生安全分类器判断白名单外的操作，不再为这些操作弹维护者审批卡。它不是安全沙箱：`permissions.deny` 仍是硬边界（被 deny 的工具不会出现在原生工具列表中）；`permissions.ask` 仍会把请求交给网关的维护者审批流程；PowerShell、Bash 等其他执行工具如需限制，也要分别配置 deny/ask。分类器只做一次性判断，不替代最小权限、外部系统额度和审计。

网关只为 `auto` 项目注入 `CLAUDE_CODE_AUTO_MODE_SERVER=0`，强制 CLI 使用本地分类器；其他项目、API_KEY 和 `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC` 保持不变（项目 env/settings 不能覆盖该变量，`claude_code_` 前缀已保留）。分类器读取与主模型相同的 `model.tokenEnv` 凭据，请求发往同一个 `model.baseUrl`，模型名固定为 `claude-sonnet-5`（不是 `model.model`）。部署侧必须让该端点支持这个模型名并接受现有凭据，否则分类器请求失败，CLI 会 fail-closed：相关工具不执行并提示自动模式不可用。

每次分类先发一段 `max_tokens=64` 的快速判断，只有需要深度判断时才再发一段 `max_tokens=8192` 的请求；只快筛即放行时只发一段。每次分类最多两段请求，会计费。真实网关上的单次分类约 40 秒，这是外部真实 CLI 测量值；本地假 Messages 服务无法复现该延迟。分类器请求包含当前会话 transcript、CLAUDE.md 和相关工具输入，因此 auto 只应用于可信的项目和模型端点。

本地 2.1.321 + 假 Messages + 本地 HTTPS 代理实测（AUTH_TOKEN only、`DISABLE_TELEMETRY=1`、`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`、`CLAUDE_CODE_AUTO_MODE_SERVER=0`）：分类器请求走配置的 `baseUrl`，`Authorization` 头可用，模型为 `claude-sonnet-5`；代理未观察到 `baseUrl` 之外的出站连接。该测量只覆盖一轮本地 fixture，不是完整抓包，其他可能目的地以 Claude Code 官方说明为准。`settingsFile.autoMode` 会原样透传；2.1.321 本地 fixture 验证 CLI 正常启动且分类器运行，但未在分类器请求中观察到测试标记，CLI 是否使用其中内容未验证。

分类器拦截时直接返回工具错误，不经过 `can_use_tool`；网关写 `auto_mode_denied` 审计，并在当前回复卡追加一条「安全分类器拦截了 <工具名>，操作未执行；如需执行请联系维护者。」。分类器不可用或无法评估时写 `auto_mode_unavailable`（`kind=unavailable|unevaluable`），卡片提示「自动模式安全审查不可用，<工具名> 未执行；请联系维护者。」。每轮最多一条卡片提示；工具输入不进入卡片或审计。`doctor`、`config explain` 和 `/status` 会显示配置/生效的权限模式；auto 分类器是否真的可用只能在运行时由工具结果确认，doctor 标为 `not_checked`。

`nativeConfig: "inherit"` 时，原生文件钩子会拒绝范围覆盖网关状态目录（`dataDir` 下的房间目录）的 Grep／Glob，例如对用户主目录整体搜索；请搜索项目目录或更具体的子目录。Grep／Glob 的路径还须是普通绝对路径或相对路径：首尾不可见字符、`~` 展开、Windows 无盘符的根路径、驱动器相对路径、UNC 与设备路径（`\\?\`、`\\server\share`）均被拒绝，共享目录请映射盘符后使用；Windows 上路径经过目录联接（junction）或其他重解析点时也会被拒绝。这只是纵深防护，同账号目录不是安全沙箱，Bash 仍可读取同账号可访问的文件。

### Bash cwd 前缀

`projects[].bash.rejectCwdPrefix` 默认 `false`。开启后，原生 PreToolUse `tool` hook 对 Bash 命令做一次轻量检查：去掉首尾空白后，命令以 `cd <目标> &&` 或 `cd <目标>;` 开头，且目标规范化后等于该 worker 的原生 cwd（`workspace.Work` 的物理路径）时，直接返回可执行的 deny 说明，提示模型去掉开头的 `cd` 后重试。它只检查开头的那一个 `cd`，不再解析后续命令。

规范化规则：去掉单引号或双引号；`/` 与 `\` 等价；去掉末尾分隔符；Windows 比较不区分大小写，Linux／macOS 区分；Windows 上 MSYS 形式 `/c/Users/...` 先转换为 `C:\Users\...`；`~` 只在未加引号时按传给原生进程的 `HOME` 展开（isolated 下是工作区 home，不是真实用户 home）。不展开环境变量，也不处理 `cd -`、`cd ..`、`pushd`；`cd` 到其他目录、子目录或不带 `&&`／`;` 的命令不拦。

这是审批噪音优化，不是安全边界：模型可以去掉开头的 `cd` 后重新执行，届时仍按原生 allow/ask/deny 规则判断。每次触发写 `bash_cwd_prefix_rejected` 审计（room 为当前房间，detail 只有 `session`），不记录命令全文。开启不需要改动线上配置；由维护者在项目上显式打开。

`runtime.claudeExecutable` 指定原生 Claude Code 可执行文件，相对路径以配置文件目录为基准；省略时使用服务账户 PATH 中的 `claude`。Go 版不内置或自动下载 CLI。`doctor` 执行选定程序的 `--version`，确认其能启动并标识为 Claude Code；继承模式还检查原生配置目录存在，但不执行 hooks、MCP 或插件，不证明它们已可用。升级 CLI 后按[验证与验收](validation.md)重跑协议回归。

隔离模式的原生历史随 `dataDir` 保存；继承模式的原生历史位于共享 Claude 配置目录的 `projects` 树中，按原生 cwd 归档。因此继承部署须同时备份网关状态与实际 Claude 历史目录。共享历史位置不代表其他聊天可以通过 `/sessions` 或 `/resume` 访问这些会话，也不会自动导入其他桥接服务的历史映射。

会话首次创建后固定其工作目录与原生配置身份。修改 `cwd` 或 `nativeConfig` 后，应明确新建会话；已有会话不能通过重启或回收悄悄转到另一个目录或配置身份，恢复失败保留原映射并报错。

## 项目环境变量

`projects[].env` 与 `projects[].envPassthrough` 把变量注入该项目的原生 Claude 进程及其 Bash／工具子进程，用于区分同一部署中的多个项目，或为隔离模式补充工具配置和凭据。两者按项目生效，不改变其他项目、工作区、原生 HOME 或会话身份。

- `env` 是变量名到非敏感字面值的映射；值必须非空且不含 NUL。URL、profile 名、目录等非敏感值放这里；不要把密钥写在这里：变量名疑似凭据（`TOKEN`、`SECRET`、`PASSWORD`、`PASSWD`、`API_KEY`、`AUTHORIZATION`、`CREDENTIAL`、`PRIVATE_KEY` 等按段匹配）时，`doctor` 报告失败并提示改用 `envPassthrough`。
- `envPassthrough` 是变量名列表，只放密钥；运行时从网关服务进程环境（包括 `service.env`）按名复制。缺失或空值时该变量不注入，`run` 仍会启动；但声明的名字视为部署要求：`doctor` 失败，`daemon install` 和升级会在改动现有安装前拒绝。下游工具应把缺失报告为未配置，而不是假设变量一定存在。
- 加载配置时按大小写不敏感拒绝重复名、`env` 与 `envPassthrough` 的交叠，以及保留名。保留名包括 `HOME`、`USERPROFILE`、`HOMEDRIVE`、`HOMEPATH`、`CLAUDE_CONFIG_DIR`、`CLAUDECODE`、`NODE_OPTIONS`、`DEBUG`，前缀 `ANTHROPIC_`、`CLAUDE_CODE_`、`CLAUDE_AGENT_SDK`、`FEISHU_ACTION_`、`FEISHU_REQUEST_`，以及 `appSecretEnv`、`model.tokenEnv`、`model.extraHeaders` 中 `env:NAME` 引用的变量和各项目 `actions[].env` 引用的源变量。固定操作的凭据只应进入操作子进程，不能通过项目 passthrough 交给原生 Agent；违规时加载失败，错误只包含项目 ID、变量名和原因，不包含值。
- 项目变量在隔离／继承边界和代理、CA 变量之后叠加，因此可以覆盖同名继承值或代理设置；Windows 上写入前会删除仅大小写不同的同名键。
- 环境只在下一次原生进程启动时生效，不进入房间键、会话身份或 resume 判断。修改配置后，等待空闲回收或显式 `/new`，新进程才使用新值。
- 项目环境不进入固定操作子进程；固定操作仍只使用 `actions[].env`。
- 这些变量对该项目所有获准聊天的原生工具和 Bash 可见，不是权限或信任隔离边界；权限仍由 `permissionMode`、`allowedTools` 和原生规则决定。见[权限与信任范围](../SECURITY.md)。

原生 settings 的 `env` 块会覆盖进程环境中的同名变量；隔离模式会加载工作目录的 project settings，继承模式还会加载 user／local settings。因此不要在原生 settings 的 `env` 中定义同名变量来“覆盖项目值”：项目值只在 settings 未定义该变量时生效。该优先级已用 Claude Code 2.1.319 的显式 `--settings` 和用户 settings 验证；固定回归版本 2.1.321 未单独复验。

替换 `HOME`／`USERPROFILE` 后，部分工具不再读取默认配置目录。优先用工具自身的配置目录变量指向真实目录，例如 `GLAB_CONFIG_DIR`、`LARKSUITE_CLI_CONFIG_DIR`，不要为了工具配置放宽原生 HOME 边界。不同工具、版本和登录态存储方式的兼容性不同，需按实际版本验证；SSH 客户端是否读取替换后的 HOME 也随平台和调用方式而异，需要时使用显式 `-F`。

`doctor` 对每个 passthrough 变量做存在性检查（只报存在，不显示值，占位值算失败）；`doctor` 可以配合 `--env-file` 检查部署环境。`config explain` 在项目视图中列出 `env` 名和 passthrough 名，只给名字不给值；它不加载 `--env-file`，因此不判断变量是否存在。`daemon install` 在没有 `--env-file` 且没有已有 `service.env` 时，会把当前环境中已存在的 passthrough 值写入生成的 `service.env`；已有 `service.env` 不会被当前环境的新值覆盖，新增 passthrough 后须用 `--env-file` 提供完整变量。

passthrough 值中长度不少于 8 个字符的值会加入日志、回复卡和通知中的脱敏；更短的值不加入。这个阈值不区分内容，因此较长的普通 URL 等非密钥值也可能在回复卡或日志中被替换，而短凭据不会被脱敏。非敏感值应放在 `env`，只把密钥放进 `envPassthrough`；脱敏是尽力而为，不能替代外部凭据的权限和轮换管理。

```json
{
  "projects": [{
    "id": "team-tools",
    "maintainers": ["ou_example_maintainer"],
    "nativeConfig": "isolated",
    "env": {
      "PROFILE": "team",
      "GLAB_CONFIG_DIR": "/srv/example/tool-config/glab"
    },
    "envPassthrough": ["TEAM_GITLAB_TOKEN"],
    "actions": []
  }]
}
```

以上是合并到完整配置的项目片段。同一部署中的其他项目可以各自设置不同的 `env` 值；共用的 `cwd`、原生 HOME 和权限规则不受影响。

## 项目设置文件

`projects[].settingsFile` 指向一份由管理员维护的原生 Claude settings 文件。网关在每次启动原生进程时读取并校验它，把其中的 `env`、`permissions.allow/ask/deny` 与模型环境（仅 inherit）合并成一份 0600 私有临时文件，通过 `--settings` 传入；进程结束后删除。相对路径以配置文件目录为基准，可以和 `cwd` 一起使用。settingsFile 不进入会话身份或 resume 判断。

只允许顶层 `env`、`permissions` 和 `autoMode`。`permissions` 只允许 `allow`、`ask`、`deny`；每个值必须是非空字符串数组，元素非空且不含 NUL。`env` 的变量名、值、保留名和与 `projects[].env`／`envPassthrough` 的交叠按[项目环境变量](#项目环境变量)的规则校验。`autoMode` 必须是 JSON 对象，网关不逐项校验其内容，已知字段（如 `environment`、`allow`、`soft_deny`）和未知子字段都原样透传给原生 CLI；它是管理员信任的分类器提示输入，不是普通聊天的可配置项。`permissionMode` 不是 `auto` 时也允许透传，CLI 会忽略。`defaultMode`、`hooks`、`additionalDirectories`、`apiKeyHelper`、`model` 等其他顶层键一律拒绝，不会静默忽略或透传，因为其中一些会执行代码或覆盖网关管理的模式。文件必须是 UTF-8 JSON 对象，最大 1 MiB。错误只报告项目、文件路径、字段或变量名，不报告值、规则内容或 `autoMode` 子字段值；服务日志和 `doctor` 会显示具体失败字段。上游 settings 生成器新增顶层字段前，应先确认 feishu-bot 已支持，否则该项目的原生进程会拒绝启动。

文件按快照读取：修改后，下一次原生进程启动（空闲回收或 `/new`）才生效；网关不监视文件变化。文件缺失、不可读、超限、JSON 损坏或字段不合规时，该原生进程拒绝启动；输入保留、不重放，聊天收到泛化的启动失败提示，服务诊断只记录脱敏后的路径／字段，已有会话映射不变。同一条待交付输入因启动失败只提示一次；新输入会再提示一次。`doctor` 对每个配置了 `settingsFile` 的项目做同样的存在、可读和结构检查，不显示规则内容；`run` 启动本身不读取该文件，但 `daemon install`／升级预检使用完整 doctor 报告，会因此拒绝。

合并规则：

- 未配置 `settingsFile` 时行为不变：isolated 不传 `--settings`，inherit 仅传模型环境。
- isolated 配置时，临时文件只含文件的 `env`、`permissions` 和 `autoMode`；模型凭据仍在进程环境，不写入临时文件。
- inherit 配置时，模型环境最后合并到文件 `env`；保留名校验保证两者不重名。`autoMode` 在两条路径都原样写入临时 settings。
- `--allowedTools`（网关固定只读工具与 `projects[].allowedTools`）照常生成，与文件的 allow 规则叠加；ask／deny 按原生规则生效。
- `--settings` 的 `env` 覆盖进程环境和 user／project settings 的同名变量。该优先级用 Claude Code 2.1.319 验证；固定回归版本 2.1.321 未单独复验。
- deny 已在 2.1.319 用本地假 Messages 服务验证：allow 命中时 Bash 执行，同一规则加入 deny 后命令被拒绝；`acceptEdits` 和 `bypassPermissions` 两种模式都生效。

isolated 模式仍会加载 cwd 下的 `.claude/settings.json` project settings；其 allow、hooks 等按原生规则与 `--settings` 合并（deny 优先是原生文档行为；本项目只验证了 `--settings` 自身的 deny，未单独验证跨来源 deny 冲突）。管理员必须把 cwd 仓库自带的 project settings 与这份文件一起审核，不能只审核 `settingsFile`。

## 项目系统提示词

`projects[].appendSystemPrompt`（内联）和 `appendSystemPromptFile`（文件）二选一；两个同时配置时配置加载失败。提示词上限 **16 KiB**，必须是 UTF-8 且不含 NUL。文件路径按配置文件目录解析，不能位于 `dataDir/rooms` 房间目录或该会话的原生 HOME 下。网关在每次启动原生进程时读取一次快照：文件修改在下一次启动（空闲回收或 `/new`）后生效。原生进程收到的拼接顺序是 bridge 提示词 → action 段 → `Project instructions:` 固定标题 + 项目提示词；interactive 本地接管使用原生 `--append-system-prompt`（2.1.321 支持）。

提示词只是给模型的指引，不能突破网关的授权、聊天/作者范围、工具 allow/ask/deny 或 `permissionMode`；它不是安全边界。提示词文件与 cwd 中的 `CLAUDE.md` 一样，如果放在会话可写的目录里，会话可能修改它，管理员必须自行保护文件权限和内容。

提示词读取采用 fail-open：文件缺失、不可读、超长、非 UTF-8 或含 NUL 时，原生进程照常启动但不带项目提示词，并写一次 `project_prompt_unavailable` 审计（project、固定原因分类，不写内容）；每个进程最多一次。`doctor` 对同一问题报失败并提示管理员修复，但不阻止启动；`doctor` 和 `config explain` 只显示是否配置、来源（内联/文件）和字节数，不显示提示词内容。interactive 本地接管无法写网关审计，失败时同样只省略提示词。

规则写法示例（虚构命令）：

```json
{
  "permissions": {
    "allow": ["Bash(uv run --no-sync example-tool sub:*)"],
    "ask": ["Bash(uv run --no-sync example-tool doc *--input*)"],
    "deny": ["Bash(curl:*)", "Read(./.secrets/**)"]
  }
}
```

写全 runner 前缀（`Bash(uv run --no-sync example-tool sub:*)`）可让该子命令直接放行；不要用 `Bash(uv:*)` 等过宽前缀。需要在执行前确认的命令用 `ask` 和中间通配（`Bash(uv run --no-sync example-tool doc *--input*)`），不要用 allow 加提示词。deny 用于硬边界（如 `Bash(curl:*)`、`Read(./.secrets/**)`）；原生 deny 优先于 allow，但同账户代码仍可能通过其他工具或路径访问数据，不是安全沙箱。

## 预检索上下文

`projects[].preContext` 让网关在把新一轮人类输入交给原生进程前，执行一个管理员配置的只读固定操作，并把输出作为带出处的背景材料一并注入，让模型的第一跳不必先调用工具。默认关闭。它复用既有固定操作执行路径，不是第二套调度器或规划器，也不替代原生工具。

```json
{
  "actions": [{
    "id": "search",
    "description": "read-only fixture search",
    "executable": "/opt/example/bin/search",
    "args": ["search", "--limit", "5", "--format", "context", "--", "{{question}}"],
    "readOnly": true,
    "timeoutMs": 10000,
    "parameters": {
      "question": {
        "description": "one user question",
        "pattern": "^[^\\x00-\\x1f\\x7f]+$",
        "maxLength": 500
      }
    },
    "vars": { "EXAMPLE_PROFILE": "demo" }
  }],
  "preContext": { "action": "search", "parameter": "question" }
}
```

在 `pattern` 或 `argv` 中写正则转义时，确保 JSON 实际得到的是转义文本（例如 `"\\x00"`），而不是被 shell 或生成器提前解释成控制字符。`doctor` 会单独报告字符串中的 NUL 控制字符，并提示改写成 JSON 转义序列。

`preContext.action` 必须是本项目中 `readOnly: true` 的固定操作，`parameter` 必须是它声明的参数。操作参数模板只能引用这个参数，并且必须把 `{{parameter}}` 作为一个独立 argv 紧跟在字面量 `--` 之后；这样以 `-` 开头的人类提问不会被当成选项。操作不能声明 `workspaceFile` 参数。`timeoutMs` 默认 **5000**，范围 100 到该 action 的 `timeoutMs`；`maxChars` 默认 **6000**，范围 1–20000。不满足任一条时配置加载失败。

网关通过固定操作路径执行：绝对 executable、固定 argv、参数的 pattern 和 maxLength、`env`／`vars` 白名单、输出脱敏、`action_runs` 与 `action_started`／`action_finished` 审计、`FEISHU_REQUEST_*` 请求上下文；不经过 shell。参数值是触发消息的用户可见文本（机器人 mention 已由输入归一化移除）；网关先将空白和控制字符连续段折叠为一个空格并去掉首尾空白，再按参数 `maxLength` 截断。参数校验不通过时 fail-open，原因记为 `parameter`。

只对新一轮人类输入执行：`/compact`、管理命令、轮次进行中的补充或普通输入、以及没有用户文本的纯图片批次都不执行；图片批次使用第一条消息的文本，渲染出的图片占位符不算用户文本。检索与工作区准备、原生进程启动并行；两者都完成后再发送输入，发送最多等待 `timeoutMs`。超时、非零退出、空输出或执行错误时照常发送不带 `preContext` 的输入，并写 `pre_context_failed` 审计（project、action、固定原因分类、durationMs），不记录提问或输出正文。

注入使用输入 JSON 的独立字段 `preContext: {"source": "<action id>", "text": "...", "truncated": bool}`。它是网关执行的只读检索结果，属于不可信的背景材料，不是用户输入、授权或指令；模型可以直接引用，不够时仍应调用原生工具。输出超出 `maxChars` 时从尾部截断，保留开头，按 UTF-8 字符边界切分并标记 `truncated: true`。`turn_result` 增加 `preContext`（`used`／`failed`／`skipped`）和 `preContextMs`，用于比较预检索与原生首跳的耗时。

这个 action 仍会通过 `project_action` 暴露给模型（`readOnly` 自动执行），这是允许的行为；管理员应按 executor 的实际权限审核它，不能把 `readOnly` 当作自动安全证明。executor 会收到用户提问原文以及配置的凭据和请求上下文，部署方需按数据范围保护。

## 审批资格与自批

`projects[].owners` 必须是 `maintainers` 的子集；它的唯一含义是：开启 `projects[].approvals.forbidSelfApproval` 后，owner 仍可以批准自己发起的请求。owner 不因此获得其他管理或审批权限。`forbidSelfApproval` 默认 `false`，保持原有行为。

审批资格统一按以下规则判断；权限审批的卡片回调和私聊转发卡片使用同一套规则，问题回答不使用审批人集合和自批规则，保持现有行为：

- 原生工具 ask 和未配置 `approvers` 的固定操作使用项目 maintainers 作为审批人。
- 固定操作可以配置 `actions[].approvers`；配置后该操作只认这个列表，不再认 maintainers，也不会隐式加入 maintainers 或 owners。空列表按未配置处理，沿用 maintainers。approvers 不要求是 maintainers，但群内审批卡还要求点击者是该群授权作者；否则点击被拒绝并只能等待超时。`doctor` 无法验证群成员关系。
- `AskUserQuestion` 不使用审批人列表，也不受自批规则限制，保持现有行为。
- `forbidSelfApproval: true` 时，请求到达时上下文中的 `senders` 包含点击者、且点击者不是 owner 的批准请求会被拒绝，审批保持待处理，其他审批人仍可批准；拒绝（deny）不受自批限制，但点击者仍必须是审批人。
- 请求上下文为空（后台任务、没有进行中的输入）时无法确定发起人；此时只有 owner 可以批准，其他审批人批准会被拒绝。拒绝仍允许审批人执行。
- 私聊中的审批请求如果当前用户不是审批人，会转发到审批人的私聊；配置 approvers 时转发给 approvers，否则转发给 maintainers。指定 approvers 的审批卡标题为“需要指定审批人批准”，摘要显示“审批人：指定审批人”，不列出名单。

`doctor` 在 `forbidSelfApproval` 为 true 且 owners 为空时给出一条提示（不算失败）：只有一个维护者时，他将无法审批自己发起的请求。`config explain` 显示 owners 数量、`forbidSelfApproval` 和当前用户是否 owner；`canApproveTools` 仍只表示 maintainers 身份。

```json
{
  "projects": [{
    "id": "team-tools",
    "maintainers": ["ou_example_owner", "ou_example_member"],
    "owners": ["ou_example_owner"],
    "approvals": { "forbidSelfApproval": true },
    "actions": [{
      "id": "publish",
      "description": "Publish",
      "executable": "/srv/example/bin/publish",
      "approvers": ["ou_example_approver"]
    }]
  }]
}
```

以上是合并到完整配置的项目片段。approvers 仍需是群内授权作者才能点击群里的审批卡；私聊转发只发送操作摘要和参数。

## 回复反馈

`projects[].feedback.enabled` 按项目开启回复卡反馈，默认 `false`；关闭时回复卡与之前完全一致。

开启后，只在最终成功交付并标记 Done 的回复卡底部出现「有用」「无用」按钮；未完整送达、已放弃、已取消、文件待送达和中间流式卡片不加。一轮回复拆成多张卡时只在最后一张添加。按钮复用快捷操作的 24 小时卡片绑定与会话检查；会话切换、卡片过期或非授权作者点击会按快捷操作提示失效。任何获准作者（私聊为私聊用户）每人每张回复保留一票，最后一次为准，可以改票；点击只返回「已记录：有用／无用」提示，不重写卡片。频率限制与普通消息共用。

点击时在 `feedback` 表保存快照：问题取自 outbox origins 对应的 inbox 消息，找不到或已被清理时留空；回复正文取自 outbox。两者分别截断为 4000／2000 字符，并使用现有 secrets 脱敏；同时记录 room、project、gateway session、原生 session（有则记录）、outbox ID、卡片消息 ID、open_id、value 和时间。`feedback` 保留 90 天，维护者应定期导出。

部署级 `feedback.export` 默认关闭。开启时必须配置 `feedback.export.dir`，相对路径按配置文件目录解析；目录须预先存在，`doctor` 会检查它存在、不是符号链接且可写。服务在每轮维护清理前先归档，再执行反馈表的 90 天保留策略。归档按项目和 UTC 日期写成 `feedback-<project>-<YYYY-MM-DD>.jsonl`，每行字段与手工导出相同；文件名中的日期是归档执行日，积压记录写入执行当天的文件。每个项目每天最多写一个文件；写完后在状态库 `feedback_exports` 保存已导出的 `(at,id)` 水位和文件路径，已存在的当日文件不会被覆盖，之后的新反馈留到下一个 UTC 日期。文件先写同目录临时文件、同步后 rename，权限 0600，避免读到半行。

归档是至少一次语义：如果文件已经写入、但水位保存失败，下一个 UTC 日期的文件会再次包含这些行。消费方应按 `(messageId, user, at)` 去重，不能假定每个反馈只出现一次。当日文件已存在而水位不匹配时，导出返回 `ErrExists` 并且不覆盖该文件；当天 `feedback` 保留策略保持暂停，等待下一个 UTC 日期重新归档。

任一项目归档失败时，本轮暂停 `feedback` 保留策略并写一次 `feedback_export_failed` 审计；同一项目、日期和错误类别在后续 30 秒轮次中不重复审计。其他维护策略继续。修复目录后，下一轮成功归档并更新水位，随后恢复删除；失败期间不会删除尚未归档的反馈。成功归档不写逐日审计。关闭 `feedback.export` 时行为与旧版完全一致。

手工导出命令只读打开状态库，独立于定时归档；WAL 模式下可与运行中的服务并发读取，不加锁、不修改数据：

```sh
./bin/feishu-bot feedback export --config ~/.feishu-bot/config.json \
  --value useless --since 30 --project team-tools > feedback.jsonl
```

`--value` 为 `useless`（默认）、`useful` 或 `all`；`--since` 接受 RFC3339 时间或天数；`--project` 过滤项目。每行一条 JSON，字段为 `at`（UTC RFC3339Nano）、`project`、`room`、`session`、`nativeSessionId`、`value`、`user`、`question`、`answerSummary`、`messageId`；问题缺失时附 `questionMissing: true`。导出包含用户问答原文和 open_id，按敏感数据处理，不要提交或公开。

## 首次启动检查

`doctor` 默认离线，检查本地配置和运行条件；`ready: true` 只表示报告列出的检查通过，不代表已验证飞书身份、模型工具调用或真实协作。逐层联网检查和测试聊天验证见[首次连接](first-connection.md)。

- 使用独立配置时传完整参数，例如 `doctor --config ./config.local.json`；`--env-file <KEY=VALUE 文件>` 可在检查前载入凭据。
- 数据目录检查使用独立临时 SQLite 文件，结束后删除，不打开现有 `gateway.sqlite`，也不留下固定 `doctor.sqlite`。目录不可写或配置的资料目录不存在时仍输出其余检查项；先处理报告中的失败项，再启动。
- 开启 `feedback.export` 时，`doctor` 还检查归档目录是否存在、是否为符号链接、是否可写；只创建并删除临时探针文件，不读取归档内容。
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

原生 TUI 使用保存的 native ID、工作目录、配置模式、已应用模型配置（未显式选择时使用项目默认）和权限模式。权限提示在终端回答；飞书专用 MCP、固定操作 relay 与飞书输出交付不在本地开启。该期间的原生用量不会写入网关用量记录，覆盖缺口会显示在 `/usage` 中。此入口服务于同一受信任账户下的接续，目录和锁不构成多租户沙箱。

[场景配置](usage.md) · [对话规则](conversation-contract.md) · [权限与数据边界](../SECURITY.md) · [验收](validation.md)
