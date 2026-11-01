# 部署与配置

Linux 和 Windows 直接运行同一个 Node.js 服务。需要 Node.js 22.16+、Git；Windows 使用 Git for Windows。

## 飞书应用与授权

创建飞书自建应用，开启机器人、消息接收和长连接，并配置交互卡片回调。基本交互需要接收群 @／私聊消息、发送回复与操作消息表情的权限。按需历史工具还需要消息历史读取权限；群历史需相应的群消息读取范围，通常包括 `im:message.group_msg`。权限须在控制台申请并发布，最终以应用的实际可用范围为准。

即使拥有读取群消息的权限，也不自动处理所有事件。配置 `groups` 限定群，`members` 限定该群的获准作者，项目维护者也可使用。省略 `members` 仅允许维护者。群里只有明确 @ 本机器人才触发；单纯回复机器人、@全体或写出名字不触发。私聊由 `dms` 授权，不需要 @。所有未授权消息静默丢弃。

`limits.backgroundMessages` 默认 **0**，无非 @ 群消息缓存和自动预注入。需要前文时由 Agent 调用当前聊天限定的历史工具，工具仍过滤未授权作者，缺少 API 权限时返回明确错误。参见[对话规则](conversation-contract.md)和[飞书历史接口](https://open.feishu.cn/document/server-docs/im-v1/message/list)。

开发、测试和常驻部署使用独立应用及数据目录。同一应用多个长连接可能分流事件，不能按广播订阅使用。

## 安装和启动

```sh
npm ci --ignore-scripts
npm run init
```

编辑 `config.json`。`init` 会构建程序并创建配置父目录，拒绝覆盖已有配置。更新代码后执行 `npm ci --ignore-scripts && npm run build`，再重启；测试使用 `npm run check`。

Linux：

```sh
export FEISHU_APP_SECRET='replace-with-app-secret'
export MODEL_AUTH_TOKEN='replace-with-model-token'
npm run doctor
npm start
```

PowerShell：

```powershell
$env:FEISHU_APP_SECRET = "replace-with-app-secret"
$env:MODEL_AUTH_TOKEN = "replace-with-model-token"
npm run doctor
npm start
```

不要提交密钥。`appSecretEnv` 和 `model.tokenEnv` 可显式指定其他变量，已有显式设置继续有效。模型默认变量是 `MODEL_AUTH_TOKEN`，不会扫描其他凭据。旧配置省略 `tokenEnv` 时，应补上实际变量名或设置新默认变量。

## 首次启动检查

`npm run doctor` 检查本地配置和运行条件；`ready: true` 只表示报告列出的检查通过，不代表已验证飞书身份、模型工具调用或真实协作。

模板中的示例 ID、保留示例域名、模型占位名和占位凭据必须替换。群与私聊至少配置一项；空列表不会接收任何请求。`run` 在连接飞书和打开运行状态前拒绝这些未完成配置。配置语法错误只报告位置，不回显可能含密钥的原文。

使用独立配置时填写完整参数，例如 `node dist/cli.js doctor --config ./config.local.json`。缺少路径、重复 `--config` 或拼错参数会直接报错，不偷偷使用默认配置。

数据目录检查使用独立临时 SQLite 文件，结束后删除，不打开现有 `gateway.sqlite`，也不留下固定 `doctor.sqlite`。目录不可写或配置的资料目录不存在时仍输出其余检查项；先处理报告中的失败项，再启动。

只有显式 `doctor --probe` 才发送一个可能计费的模型请求。未完成配置时跳过请求；HTTP 200 的登录页或非 Messages JSON 不算通过。诊断不打印模型回复或凭据。

## 模型、网络和时区

`model.baseUrl` 使用 Claude Code 可调用的 Anthropic Messages 兼容地址，`model`／`smallModel` 填实际模型 ID。可直连服务商或兼容网关；不做协议转换，不保证任意兼容模型都具备同样工具与多模态能力。

地址支持路径前缀，末尾 `/v1` 去重。默认 HTTPS；`allowHttp` 仅用于明确选择的本地测试。`extraHeaders` 可引用 `env:VARIABLE_NAME`，缺失引用和头注入在运行前拒绝。

HTTP_PROXY、HTTPS_PROXY、ALL_PROXY、NO_PROXY、NODE_EXTRA_CA_CERTS 等必要网络配置传入 CC。自定义 CA 应在 Node 启动前设置，不关闭证书校验。

时区跟随环境，不绑定地区。需要一致的时间解释时可设置 `TZ=UTC`，同时影响原生进程与输入元数据；未设置时使用主机时区。模型回复跟随请求语言，内置提示主要为中文。

`doctor --probe` 发送一个小请求，可能产生费用；只检查基础连接，不代表流式、工具、子代理、图片或长上下文已验收。模型额度由上游控制，`runtime.maxWorkers` 只限制活跃会话数。

## 常驻运行

Linux 可使用 [systemd 示例](../deploy/feishu-bot.service)。按安装位置修改服务账户、工作目录和 Node 路径。使用专用非 root 账户，配置与环境文件仅授予必要读取权限。环境文件不写 `export`：

```text
FEISHU_APP_SECRET=replace-with-app-secret
MODEL_AUTH_TOKEN=replace-with-model-token
TZ=UTC
```

`dataDir` 使用固定持久目录，例如 `/var/lib/feishu-bot`。数据库和其中原生 HOME／transcript 必须一起保留，不能只备份 session ID 而删除历史文件。服务启动不预开所有会话；首次任务才启动对应 CC。

```sh
node dist/cli.js doctor --config /etc/feishu-bot/config.json
sudo systemctl enable --now feishu-bot
journalctl -u feishu-bot -f
```

Windows 先以前台模式检查配置与网络。CC 找不到 Git Bash 时，再设置 `CLAUDE_CODE_GIT_BASH_PATH` 为实际路径；JSON 路径用正斜杠或转义反斜杠，工具经 PATH 查找。Ctrl+C 请求关闭本实例的会话进程树；停止失败会记录 `shutdown_failed`，不能据此认为所有派生进程均已退出。

## 容量与恢复

`runtime.maxWorkers` 默认 8；`idleMs` 默认 900000（15 分钟），0 关闭定时回收。容量满时即使关闭定时回收，也可释放确认空闲的旧进程。

回收只释放进程，不改 session ID、工作文件或记忆。下条消息、服务重启及 `/stop` 后都续接原会话。`/new` 才主动新建；`/compact` 原生压缩也保持 session。`/status` 在进程已回收时仍显示保存的会话。

任务、未确认输入、后台工作、Monitor、原生会话定时工作、审批和最终回复交付未结束时不回收；未知状态保守保留。`worker_ready.startupMs` 只代表进程初始化，不是模型首字延迟。

服务关闭时，若 Worker 停止失败（包括关闭期间才完成初始化的 Worker），会设置非零退出码并保留 `gateway.lock`，阻止新实例接管同一数据目录。正常关闭仍自动释放锁。异常退出后先核实锁记录 PID 及本实例派生进程均已结束，再清理旧锁；仅确认主进程 PID 已消失还不够。恢复历史不复活旧后台进程，旧审批失效，结果未知的写操作不重放。原生恢复失败会报错并保留映射，不静默清空上下文。

表情清理与输出更新可重试，但不会因回执失败重做任务。超长答案的卡片和完整 `reply.md` 文件共同组成一次交付，文件未确认送达时不标记 Done。`/outputs` 会区分卡片和文件阶段。文件发送结果未知时不自动重发；核对后使用 `/retry-output <ID>` 继续同一交付，可能重复之前已被远端接受的文件。补发读取持久化答案，不重新调用模型或重做外部操作。

背景与去重有内置清理；原生 transcript、附件、导出、状态库与未确认结果按部署数据策略备份归档。

## 升级说明

升级前备份 `dataDir` 并停止旧服务。SQLite 增量迁移保留会话映射，恢复阶段会重新授权待交付消息；不要删除数据库来完成升级。

旧配置显式非零 `backgroundMessages` 不会自动变为 0，应按新行为设为 0。既有 CC 历史不会因关闭背景而消失；需要干净上下文时显式 `/new`。群 `members` 省略时不再默认放行所有人，普通成员须显式列出。

原生运行不需要 `runtime.mode/image/memory/cpus`；历史 `mode: local` 仍可读取。`autoApproveWorkspace: false` 保持原逐次权限语义，可显式迁移到 `permissionMode`／`allowedTools`；不会静默提权。`model.maxConcurrent` 已停用，模型限流在上游配置。

[场景配置](usage.md) · [对话规则](conversation-contract.md) · [权限与数据边界](../SECURITY.md) · [验收](validation.md)
