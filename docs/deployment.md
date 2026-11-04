# 部署与配置

Linux、macOS 和 Windows 运行同一个 Go 服务。源码构建需要 Go 1.25+；部署只需网关二进制、Git 和原生 Claude Code。Windows 使用 Git for Windows。协议测试固定使用 Claude Code 2.1.316，升级 CLI 后须重新验收。内置 `daemon` 管理仅支持 Linux／Windows；macOS 使用前台 `run` 或自行配置服务管理。

## 飞书应用与授权

创建飞书自建应用，开启机器人、消息接收和长连接，并配置交互卡片回调。基本交互需要接收群 @／私聊消息、发送回复与操作消息表情的权限。表情写入结果未知时的核对及完成表情回滚会读取本应用的消息表情列表，需要具有 `im:message.reactions:read` 或可用的 `im:message:readonly` 权限；没有读取权限时不应强行重发未知表情。按需历史工具还需要消息历史读取权限；群历史需相应的群消息读取范围，通常包括 `im:message.group_msg`。权限须在控制台申请并发布，最终以应用的实际可用范围为准。

即使拥有读取群消息的权限，也不自动处理所有事件。配置 `groups` 限定群，`members` 限定该群的获准作者，项目维护者也可使用。省略 `members` 仅允许维护者。群里只有明确 @ 本机器人才触发；单纯回复机器人、@全体或写出名字不触发。私聊由 `dms` 授权，不需要 @。所有未授权消息静默丢弃。

`limits.backgroundMessages` 默认 **0**，无非 @ 群消息缓存和自动预注入。需要前文时由 Agent 调用当前聊天限定的历史工具，工具仍过滤未授权作者，缺少 API 权限时返回明确错误。参见[对话规则](conversation-contract.md)和[飞书历史接口](https://open.feishu.cn/document/server-docs/im-v1/message/list)。

开发、测试和常驻部署使用独立应用及数据目录。同一应用多个长连接可能分流事件，不能按广播订阅使用。

原生打字效果需要应用可调用 CardKit 卡片创建及内容更新接口，通常须开通并发布 `cardkit:card:write`。明确的权限拒绝发生在发送消息之前时，可降级为同卡整卡更新；超时或结果未知不会触发重复发送。CardKit 的消息与卡片 ID、递增序列和最后确认快照保存在 `dataDir` 的状态库内，重启后的更新继续使用原卡片。正常结束关闭流式并折叠思考；实际手机和桌面效果仍需分别验收。

## 发布包安装

从 [GitHub Releases](https://github.com/sean2077/feishu-bot/releases) 选择目标 Go 版本，下载与本机匹配的压缩包和同一版本的 `SHA256SUMS.txt`。Linux、macOS 分别提供 amd64／arm64，Windows 提供 amd64；macOS Intel 选择 `darwin-amd64`，Apple Silicon 选择 `darwin-arm64`。完整产物命名见[版本发布](releasing.md#发布产物)。

执行下面与平台对应的命令，核对结果与 `SHA256SUMS.txt` 中该文件一行的 SHA-256 一致。`vX.Y.Z` 是版本占位符，替换成实际下载版本：

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
./feishu-bot init
```

Windows 解压 ZIP 后使用 `.\feishu-bot.exe`。包内 `INSTALL.md` 给出该版本的安装入口，`BUILDINFO.json` 记录 tag、提交和目标平台；`config.example.json` 是参考模板。`init` 创建实际使用的 `config.json`，已有配置会被保留。凭据、状态库、会话历史和日志留在部署目录，升级时只更换已核对的二进制并保留这些数据。TypeScript 旧版升级另见[Go 迁移](go-migration.md)。

macOS 产物未提供 Developer ID 签名或 Apple 公证。若首次运行被系统阻止，在确认下载来自本仓库且 SHA-256 一致后，按 [Apple 的按程序放行说明](https://support.apple.com/en-us/102445)在“系统设置 → 隐私与安全性”中对这个程序选择“仍要打开”。保持系统全局保护启用；此操作只针对已核对的程序。

## 安装和启动

从源码构建：

```sh
go build -trimpath -o ./bin/feishu-bot ./cmd/feishu-bot
./bin/feishu-bot init
```

编辑 `config.json`。`init` 创建配置父目录，拒绝覆盖已有配置。源码更新后重新构建二进制再重启；测试使用 `go test -race ./...` 和 `go vet ./...`。Windows 构建使用 `go build -trimpath -o ./bin/feishu-bot.exe ./cmd/feishu-bot`。下文命令沿用源码构建的 `./bin/feishu-bot` 路径；使用发布包时换成解压目录中的 `./feishu-bot`，Windows 对应 `.\feishu-bot.exe`。

Linux／macOS：

```sh
export FEISHU_APP_SECRET='replace-with-app-secret'
export MODEL_AUTH_TOKEN='replace-with-model-token'
./bin/feishu-bot doctor
./bin/feishu-bot run
```

PowerShell：

```powershell
$env:FEISHU_APP_SECRET = "replace-with-app-secret"
$env:MODEL_AUTH_TOKEN = "replace-with-model-token"
.\bin\feishu-bot.exe doctor
.\bin\feishu-bot.exe run
```

不要提交密钥。`appSecretEnv` 和 `model.tokenEnv` 可显式指定其他变量，已有显式设置继续有效。模型默认变量是 `MODEL_AUTH_TOKEN`，不会扫描其他凭据。旧配置省略 `tokenEnv` 时，应补上实际变量名或设置新默认变量。

## 内置 daemon 服务

本节适用于 Linux／Windows。常驻部署可使用内置命令，不生成任何启动脚本；macOS 使用前台 `run` 或自行配置服务管理，当前没有内置 launchd 管理器。

```sh
./bin/feishu-bot daemon install --config /etc/feishu-bot/config.json
./bin/feishu-bot daemon status
./bin/feishu-bot daemon logs -f
./bin/feishu-bot daemon stop
./bin/feishu-bot daemon restart
./bin/feishu-bot daemon uninstall
```

Windows 将 `./bin/feishu-bot` 换成 `.\bin\feishu-bot.exe`。

`daemon install` 的行为：

1. 解析 `--config`；缺省时先找 `./config.json`，再找 `~/.feishu-bot/config.json`（Windows 为 `%USERPROFILE%\.feishu-bot\config.json`）。
2. 安装目录默认为 config 所在目录，可用 `--work-dir` 覆盖；当前二进制复制到 `<install-dir>/bin/feishu-bot[.exe]`。可用 `--binary` 指定源二进制。
3. 使用 `--env-file`（`KEY=VALUE`，0600）或已有 `<install-dir>/service.env`；都没有时从当前进程环境生成，只包含必需凭据和白名单运行变量（PATH/TZ/代理/Git Bash 等）。缺少必需凭据会拒绝安装。`service.env` 含凭据，不要提交或公开。
4. 运行离线 `doctor` 本地检查；失败时打印脱敏报告并拒绝安装，不调用飞书或模型。
5. 停止正在运行的旧实例（通过 `gateway.lock` 与控制文件），复制二进制，注册平台服务，并默认启动。
6. 成功后写 `~/.feishu-bot/daemon.json`（schema 1），记录绝对 config/env/log/data 路径、平台、版本和安装时间。`daemon status/logs/stop/uninstall` 读取该文件，不需要重复传 `--config`。

平台：

- Windows：任务计划程序在当前用户登录时**直接运行安装目录中的二进制**；任务动作包含绝对 `run --service --config ... --env-file ... --log-file ...`，不经过 wscript、PowerShell 或 VBS。`run --service` 在日志打开后关闭控制台窗口。
- Linux：root 使用 `/etc/systemd/system/feishu-bot.service`，非 root 使用 `~/.config/systemd/user/feishu-bot.service`；`ExecStart` 带绝对 config/env/log 参数。非 root 常驻建议执行 `loginctl enable-linger <user>`，否则用户注销后服务停止。

日志与停止：

- `--log-file` 默认 `<install-dir>/logs/service.log`，`--log-max-size` 默认 10 MB，`--log-max-backups` 默认 3；`daemon logs -n N` 查看末尾，`-f` 跟随并处理轮转。
- `daemon stop` 写入 `<dataDir>/gateway.stop`，等待 `gateway.lock` 释放和进程退出（最多 45 秒），不使用强杀。停止会中断正在执行的前台任务，未确认的外部写操作不会自动重放。
- 未确认关闭会保留 `gateway.lock`。核实派生进程后，才可显式使用 `daemon start --reclaim-stale-lock`；该命令只删除已无进程持有的锁，不杀进程。
- `daemon uninstall` 删除任务/unit 和 `daemon.json`，保留 config、service.env、日志和 state。
- Windows 注销/关机可能直接终止任务进程并留下 `gateway.lock`；下次登录任务会失败，使用上面的显式恢复命令。
- `daemon install` 默认启动；`--no-start` 只安装。重复执行是幂等的：停止旧实例、替换二进制、重写服务定义并重新启动。同名任务/unit 若不属于本项目，需要显式 `--replace`。
- `config path` 打印解析后的配置路径；`config example` 打印模板；`config format` 只格式化合法 JSON。

Windows 登录启动不是 Windows SCM 服务：未登录不运行，且以当前交互用户身份运行。这是为了保留 `nativeConfig: inherit` 的原生配置和用户环境；需要开机即运行或独立服务账户时，另行使用 systemd/system 服务或专用部署，不能假设同一账户目录是安全沙箱。

## 首次启动检查

`./bin/feishu-bot doctor` 检查本地配置和运行条件；`ready: true` 只表示报告列出的检查通过，不代表已验证飞书身份、模型工具调用或真实协作。

模板中的示例 ID、保留示例域名、模型占位名和占位凭据必须替换。群与私聊至少配置一项；空列表不会接收任何请求。`run` 在连接飞书和打开运行状态前拒绝这些未完成配置。配置语法错误不回显可能含密钥的原文。

使用独立配置时填写完整参数，例如 `./bin/feishu-bot doctor --config ./config.local.json`。缺少路径、重复 `--config` 或拼错参数会直接报错，不偷偷使用默认配置。

数据目录检查使用独立临时 SQLite 文件，结束后删除，不打开现有 `gateway.sqlite`，也不留下固定 `doctor.sqlite`。目录不可写或配置的资料目录不存在时仍输出其余检查项；先处理报告中的失败项，再启动。

只有显式 `doctor --probe` 才发送一个可能计费的模型请求。未完成配置时跳过请求；HTTP 200 的登录页或非 Messages JSON 不算通过。诊断不打印模型回复或凭据。

## 模型、网络和时区

`model.baseUrl` 使用 Claude Code 可调用的 Anthropic Messages 兼容地址，`model`／`smallModel` 填实际模型 ID。可直连服务商或兼容网关；不做协议转换，不保证任意兼容模型都具备同样工具与多模态能力。

地址支持路径前缀，末尾 `/v1` 去重。默认 HTTPS；`allowHttp` 仅用于明确选择的本地测试。`extraHeaders` 可引用 `env:VARIABLE_NAME`，缺失引用和头注入在运行前拒绝。

Go 飞书 HTTP／WebSocket 客户端使用系统证书及 Go 的代理环境规则（HTTP_PROXY、HTTPS_PROXY、NO_PROXY）；原生 Claude 还接收获准的代理和证书环境。为 Go 服务安装的系统 CA 与 Claude 的 NODE_EXTRA_CA_CERTS 等配置需要分别验证，不能以一方连接成功推断另一方成功。不要关闭证书校验。

时区跟随环境，不绑定地区。需要一致的时间解释时可设置 `TZ=UTC`，同时影响原生进程与输入元数据；未设置时使用主机时区。模型回复跟随请求语言，内置提示主要为中文。

`doctor --probe` 发送一个小请求，可能产生费用；只检查基础连接，不代表流式、工具、子代理、图片或长上下文已验收。模型额度由上游控制，`runtime.maxWorkers` 只限制活跃会话数。

## 常驻运行

macOS 使用外部服务管理时，由管理器运行 `feishu-bot run`，明确配置工作目录、`--config`、`--env-file`、日志路径和原生 Claude Code 路径。网关自身的配置、授权、状态持久化和停止确认规则与其他平台相同；内置 `daemon install/start/stop` 不适用于 macOS。

Linux 可使用 `daemon install` 生成 user 或 system unit；下面的 [systemd 示例](../deploy/feishu-bot.service) 适合专用账户或开机即运行的 system 部署。按安装位置修改服务账户、工作目录和二进制路径。使用专用非 root 账户，配置与环境文件仅授予必要读取权限。环境文件不写 `export`：

```text
FEISHU_APP_SECRET=replace-with-app-secret
MODEL_AUTH_TOKEN=replace-with-model-token
TZ=UTC
```

`dataDir` 使用固定持久目录，例如 `/var/lib/feishu-bot`。数据库和其中原生 HOME／transcript 必须一起保留，不能只备份 session ID 而删除历史文件。服务启动不预开所有会话；首次任务才启动对应 CC。

```sh
./bin/feishu-bot doctor --config /etc/feishu-bot/config.json
sudo systemctl enable --now feishu-bot
journalctl -u feishu-bot -f
```

Windows 先以前台模式检查配置与网络。CC 找不到 Git Bash 时，可设置 `CLAUDE_CODE_GIT_BASH_PATH` 指向实际 `bash.exe`；`doctor` 会执行已配置的程序并检查版本，不能只凭文件存在判断就绪。未显式设置该路径时不强制要求 Git Bash，原生工具支持情况仍需实际验证。JSON 路径用正斜杠或转义反斜杠，工具经 PATH 查找。Ctrl+C 请求关闭本实例的会话进程树；停止失败会记录 `shutdown_unconfirmed`，不能据此认为所有派生进程均已退出。

## 直接使用本地目录与原生配置

已有本地工作流可以显式设置项目 `cwd`，直接在该目录运行，不复制到聊天工作区。相对路径以配置文件目录为基准；目录必须已存在。`cwd` 不能与 `seedDir` 或 `trustedConfigDir` 同时设置，避免混淆直接使用和复制配置。附件仍由各聊天分别保存；不同聊天指向同一目录时会共享文件，不提供文件写入隔离。

共享目录也会影响原生持久定时任务。Claude Code 2.1.317 的 `CronCreate` 在 `durable: true` 时将任务保存在原生项目根目录的 `.claude/scheduled_tasks.json`，其协调锁同样按项目划分；创建进程退出后，同项目的另一原生会话可以接收该任务。网关独立的聊天映射不能保证这些任务仍由原聊天执行或交付，`CronList` 也可能看到同项目的持久任务。因此不能把共享 `cwd` 下的持久任务当作已隔离到聊天的能力；需要这种归属保证时，应使用互不重叠的原生项目目录，并单独验收恢复和交付。会话内的非持久任务仍使用原生调度，不因保存了 session ID 就承诺进程退出后继续运行。网关不会复制、接管或自动重放原生定时任务。

`nativeConfig` 默认 `isolated`，保留各聊天独立原生配置的既有行为。设为 `inherit` 时，使用服务账户的原生 Claude 配置，保留 `CLAUDE_CONFIG_DIR` 原本的设置或未设置状态：未设置时，用户 settings 在默认 `.claude` 目录，全局 MCP 等配置仍使用用户主目录的 `.claude.json`，不改为 `.claude/.claude.json`。用户配置、hooks、技能、MCP 和插件按原生 Claude 的加载规则生效；这也会加载对应工作目录中的项目规则。`inherit` 可配合 `cwd`，也可在独立聊天工作目录中使用。先审核这些配置及其执行权限，再开放给获准聊天。

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

以上是合并到完整配置的项目片段。`bypassPermissions` 是显式选择的原生 YOLO 权限模式，仓库默认仍为 `acceptEdits`。它跳过常规工具权限确认，但不取消显式 deny／ask 规则或原生规则；追问仍需交互，网关的受控操作、发送文件等独立确认也不因此全部放行。不要把该选项理解为聊天成员获得管理员权限。

`runtime.claudeExecutable` 指定原生 Claude Code 可执行文件，相对路径以配置文件目录为基准；省略时使用服务账户 PATH 中的 `claude`。Go 版不再内置或自动下载 SDK 的 CLI。`doctor` 执行选定程序的 `--version`，确认其能启动并标识为 Claude Code。继承模式还检查原生配置目录存在，不执行 hooks、MCP 或插件，不证明它们已可用。协议回归固定版本与升级步骤见 [Go 迁移](go-migration.md)。

隔离模式的原生历史随 `dataDir` 保存；继承模式的原生历史位于共享 Claude 配置目录的 `projects` 树中，按原生 cwd 归档。因此继承部署须同时备份网关状态与实际 Claude 历史目录。共享历史位置不代表其他聊天可以通过 `/sessions` 或 `/resume` 访问这些会话，也不会自动导入其他桥接服务的历史映射。

会话首次创建后固定其工作目录与原生配置身份。修改 `cwd` 或 `nativeConfig` 后，应明确新建会话；已有会话不能通过重启或回收悄悄转到另一个目录或配置身份。恢复失败保留原映射并报错。

Windows 常驻启动直接使用 `feishu-bot daemon install`（见上节），由程序注册当前用户登录任务并直接执行二进制，不需要手写任务脚本。需要继承配置时，应使用拥有该原生配置的账户；自动启动环境与交互终端可能不同。先在前台完成诊断和验收；切换同一飞书应用前确认旧服务及其自动启动入口已停用，避免两个长连接分流事件。发现同名但不是本项目安装的任务时，`daemon install` 会拒绝覆盖，需人工确认或显式 `--replace`。

## 容量与恢复

`runtime.maxWorkers` 默认 8；`idleMs` 默认 900000（15 分钟），0 关闭定时回收。容量满时即使关闭定时回收，也可释放确认空闲的旧进程。

回收只释放进程，不改 session ID、工作文件或记忆。下条消息、服务重启及 `/stop` 后都续接原会话。`/new` 才主动新建；`/compact` 原生压缩也保持 session。`/status` 在进程已回收时仍显示保存的会话。

任务、未确认输入、后台工作、Monitor、原生会话定时工作、审批和最终回复交付未结束时不回收；未知状态保守保留。`worker_ready.startupMs` 只代表进程初始化，不是模型首字延迟。

服务关闭时，若 Worker 停止失败（包括关闭期间才完成初始化的 Worker），会设置非零退出码并保留 `gateway.lock`，阻止新实例接管同一数据目录。正常关闭仍自动释放锁。异常退出后先核实锁记录 PID 及本实例派生进程均已结束，再清理旧锁；仅确认主进程 PID 已消失还不够。恢复历史不复活旧后台进程，旧审批失效，结果未知的写操作不重放。原生恢复失败会报错并保留映射，不静默清空上下文。

表情清理与输出更新可重试，但不会因回执失败重做任务。超长答案的卡片和完整 `reply.md` 文件共同组成一次交付，文件未确认送达时不标记 Done。`/outputs` 会区分卡片和文件阶段。文件发送结果未知时不自动重发；核对后使用 `/retry-output <ID>` 继续同一交付，可能重复之前已被远端接受的文件。补发读取持久化答案，不重新调用模型或重做外部操作。

背景与去重有内置清理；原生 transcript、附件、导出、状态库与未确认结果按部署数据策略备份归档。

## 升级说明

从 TypeScript 版切换时，先完成 [Go 迁移步骤](go-migration.md)。升级前备份 `dataDir` 并停止旧服务。SQLite 增量迁移保留会话映射，恢复阶段会重新授权待交付消息；不要删除数据库来完成升级。

从旧的脚本式启动（VBS/PowerShell/Node launcher）迁移时，先停止旧服务、确认其原生子进程退出，并删除旧的自启动任务/脚本；然后运行 `feishu-bot daemon install`。服务命令不会猜测或接管其他工具的任务。升级二进制前先 `daemon stop`；可以直接用新二进制的 `daemon install --binary <new-binary>` 完成停止、替换、注册和重启。重复执行 `daemon install` 是幂等的。

旧配置显式非零 `backgroundMessages` 不会自动变为 0，应按新行为设为 0。既有 CC 历史不会因关闭背景而消失；需要干净上下文时显式 `/new`。群 `members` 省略时不再默认放行所有人，普通成员须显式列出。

原生运行不需要 `runtime.mode/image/memory/cpus`；历史 `mode: local` 仍可读取。`autoApproveWorkspace: false` 保持原逐次权限语义，可显式迁移到 `permissionMode`／`allowedTools`；不会静默提权。`model.maxConcurrent` 已停用，模型限流在上游配置。

[场景配置](usage.md) · [对话规则](conversation-contract.md) · [权限与数据边界](../SECURITY.md) · [验收](validation.md)
