# 部署与配置

Linux 和 Windows 直接运行同一个 Node.js 服务。需要 Node.js 22.16+、Git；Windows 使用 Git for Windows。

## 飞书应用

创建飞书自建应用，开启机器人、消息接收和长连接，并配置交互卡片回调。使用群背景功能时，需要额外获得接收非 @ 群消息的权限。运行配置仍只允许指定群和私聊用户，不因应用权限扩大而自动接入其他聊天。

开发、测试和常驻部署使用独立应用及数据目录。同一应用的多个长连接可能分流事件，不能按广播订阅使用。

## 安装和启动

```sh
npm ci --ignore-scripts
npm run init
```

编辑 `config.json`。`init` 会构建程序并创建配置文件的父目录，拒绝覆盖已有配置。更新代码后执行 `npm ci --ignore-scripts && npm run build`，再重启服务。

示例使用两个环境变量：

```sh
export FEISHU_APP_SECRET='replace-with-app-secret'
export MODEL_AUTH_TOKEN='replace-with-model-token'
npm run doctor
npm start
```

PowerShell 的等价设置：

```powershell
$env:FEISHU_APP_SECRET = "replace-with-app-secret"
$env:MODEL_AUTH_TOKEN = "replace-with-model-token"
npm run doctor
npm start
```

不要把密钥提交到仓库。`appSecretEnv` 和 `model.tokenEnv` 均可显式指定其他环境变量名；已有显式设置继续有效。未指定模型变量名时使用 `MODEL_AUTH_TOKEN`，不会扫描其他凭据。如果旧配置省略了 `tokenEnv`，升级时应补上实际变量名或设置新默认变量。

## 模型、网络和时区

`model.baseUrl` 使用能被 Claude Code 调用的 Anthropic Messages 兼容地址，`model` 和 `smallModel` 填服务实际提供的 ID。可直接连接供应商或经过兼容网关；本程序不做模型协议转换，也不保证任意兼容模型都具备相同工具和多模态能力。

地址支持路径前缀，末尾 `/v1` 会去重。默认要求 HTTPS，`allowHttp` 仅用于明确选择的本地测试。`extraHeaders` 可引用 `env:VARIABLE_NAME`；引用缺失和头注入在运行前拒绝。

HTTP_PROXY、HTTPS_PROXY、ALL_PROXY、NO_PROXY、NODE_EXTRA_CA_CERTS 等必要网络设置传入 CC。自定义 CA 应在启动 Node 前设置，不通过关闭证书校验绕过问题。

时区跟随服务环境，不绑定地区。需要一致的时间解释时，在启动前设置例如 `TZ=UTC`；该值同时影响原生进程和交给模型的时区信息。未设置时使用主机时区。模型回复跟随当前请求语言，用户也可明确指定语言。

`doctor --probe` 会发送一个小模型请求，可能产生费用；它只验证基础连接，不代表流式、工具调用、子代理、图片或长上下文已通过验收。模型请求额度由上游服务控制，`runtime.maxWorkers` 只限制活跃会话数。

## 常驻运行

Linux 可使用 [systemd 示例](../deploy/feishu-bot.service)。按安装位置修改服务账户、工作目录和 Node 路径。使用专用非 root 账户，配置及环境文件仅授予必要读取权限。

环境文件内容示例，不写 `export`：

```text
FEISHU_APP_SECRET=replace-with-app-secret
MODEL_AUTH_TOKEN=replace-with-model-token
TZ=UTC
```

`dataDir` 建议使用固定绝对路径，例如 `/var/lib/feishu-bot`。服务启动只连接飞书，会话在首次任务到来时启动。

```sh
node dist/cli.js doctor --config /etc/feishu-bot/config.json
sudo systemctl enable --now feishu-bot
journalctl -u feishu-bot -f
```

Windows 可先以前台模式检查配置和网络。CC 找不到 Git Bash 时再设置 `CLAUDE_CODE_GIT_BASH_PATH` 为本机实际路径。JSON 路径使用正斜杠或转义后的反斜杠；工具通过 PATH 查找。Ctrl+C 会关闭本实例的会话进程树。

## 容量与恢复

`maxWorkers` 默认 8；`idleMs` 默认 900000（15 分钟），0 关闭定时回收。容量满时即使关闭定时回收，也可释放确认空闲的旧进程。会话 ID、工作文件和记忆保留，下次消息恢复历史。

处理中的任务、未确认输入、后台任务、Monitor、会话定时工作和审批不回收；未知状态也不主动回收。`worker_ready.startupMs` 是进程初始化耗时，不是模型首字延迟。

异常退出后先核实单实例锁记录的 PID 及本实例派生进程已结束，再清理旧锁。恢复 transcript 不会恢复已经结束的后台进程，旧审批失效，结果未知的外部写不能盲目重复执行。

背景缓存和去重记录有内置清理；原生 transcript、附件、导出和未确认结果需按部署的数据保留要求归档。

## 旧配置兼容

当前原生运行不需要 `runtime.mode/image/memory/cpus`；历史模板的 `mode: local` 仍可读取。`autoApproveWorkspace: false` 保留逐次权限语义，可显式迁移到 `permissionMode` 和 `allowedTools`。不会静默提升权限。

`model.maxConcurrent` 已停用；模型限流应在上游设置。部署更新前停止旧实例，不自动删除未知进程或资源。聊天数据目录布局保持不变。

[场景配置](usage.md) · [权限与数据边界](../SECURITY.md) · [验收清单](validation.md)
