# 部署

Linux 和 Windows 直接运行同一个 Node.js 服务，不安装 Docker，不构建镜像，不为聊天额外创建运行环境。

## 飞书测试应用

创建企业自建应用，开启机器人、消息事件和长连接，启用交互卡片回调。需要群背景时，还要开通接收非 @ 群消息的权限；只接收 @ 的 scope 不够。只将允许的群和用户写入配置。

Windows 测试和 Linux 常驻服务使用不同的 app ID、secret 和数据目录；同一飞书应用的多个长连接可能分流事件，不是广播。

## 安装与启动

安装 Node.js 22.16+（推荐 24）及 Git，执行：

```sh
npm ci --ignore-scripts
npm run init
```

编辑 `config.json`，填写 app、群／私聊、维护者和公司网关信息。`npm run init` 自动构建且不覆盖已有配置。以后更新代码执行 `npm ci --ignore-scripts && npm run build`，再重启服务；测试用 `npm run check`。

Linux：

```sh
export FEISHU_APP_SECRET='测试应用密钥'
export COMPANY_MODEL_TOKEN='公司网关凭据'
npm run doctor
npm start
```

Windows PowerShell：

```powershell
$env:FEISHU_APP_SECRET = "测试应用密钥"
$env:COMPANY_MODEL_TOKEN = "公司网关凭据"
npm run doctor
npm start
```

Windows 安装 Git for Windows。如果 CC 找不到 Git Bash，再设置 `CLAUDE_CODE_GIT_BASH_PATH` 为实际 `bash.exe` 的绝对路径。JSON 中用 `C:/...` 或双反斜杠。Node／Git／构建工具通过 PATH 使用，不要求重建镜像来安装依赖。

## 常驻 Linux 服务

使用项目专用的非 root 账户。提供了 `deploy/feishu-bot.service`，按实际位置修改账户、工作目录和 Node 路径，密钥放入权限 0600 的 `/etc/feishu-bot/environment`：

```text
FEISHU_APP_SECRET=...
COMPANY_MODEL_TOKEN=...
```

systemd 环境文件不写 `export`。配置中 `dataDir` 使用固定绝对路径，例如 `/var/lib/feishu-bot`。服务启动只连飞书，不预开所有 CC；第一次任务到来才启动对应会话。

```sh
node dist/cli.js doctor --config /etc/feishu-bot/config.json
sudo systemctl enable --now feishu-bot
journalctl -u feishu-bot -f
```

启动配置、启用服务由管理员按公司的部署权限执行。Windows 首先以前台运行便于调试；Ctrl+C 会关闭会话进程树，不需要 WSL。

## 公司网关、代理和证书

`model.baseUrl` 使用在 Claude Code 中工作的 `ANTHROPIC_BASE_URL`，`model`／`smallModel` 使用公司实际提供的 ID。主模型与原生子代理都使用公司入口；不添加本地模型代理或协议转换。配置末尾的 `/v1` 会去重，避免原生 SDK 拼成 `/v1/v1/messages`。

`extraHeaders` 可引用 `env:VARIABLE_NAME`，运行前校验，不会把缺失的头悄悄发送为空值。HTTPS_PROXY、HTTP_PROXY、ALL_PROXY、NO_PROXY 和 NODE_EXTRA_CA_CERTS 等必要网络设置显式传入 CC；企业 CA 应在启动 Node 前配置，不要关闭证书检查。

`doctor --probe` 会付费调用一次很小的模型请求，只检查基础连通性，不代表流式、复杂工具、子代理、图片和缓存已经验收。公司额度／模型请求限流由公司网关控制；`runtime.maxWorkers` 只限制活跃 CC 会话数，不限制原生子代理每次模型请求。

## 进程复用

`runtime.maxWorkers` 默认 8，示例为 4。`idleMs` 默认 900000（15 分钟），0 关闭定时回收。容量满时，即使关闭定时回收，也优先释放真正空闲的旧进程，给新聊天让位。

回收不新建会话：原生 session ID、工作文件和记忆保留，下次消息恢复。正在处理、存在待交付输入、后台任务、Monitor、会话定时工作或审批的会话不回收；后台状态未知也保守保留。全部忙时明确告知输入已保存，空闲后自动交付。

`worker_ready` 日志记录实际进程初始化耗时和是否恢复会话；不要把它当成模型首字延迟。真实体验还受模型、网关、工具执行和网络影响。

## 从初版配置更新

删除 `runtime.mode/image/memory/cpus`。旧模板的 `mode: local` 仍能读取，但不再需要区分生产／测试运行方式。`autoApproveWorkspace: false` 保留原来的逐次权限语义；推荐改成 `permissionMode: acceptEdits`，按项目添加 `allowedTools`。旧的自动全权限配置不会被静默迁移。

移除 `model.maxConcurrent`，在公司网关配置模型限流。首次采用过旧容器部署的管理员应先停旧服务并自行清理其已确认的旧 worker；新版不带 Docker 命令，也不会抢占或删除未知容器。原有聊天状态目录布局不变。

## 停机与恢复

数据目录有单实例锁。异常退出后核实记录 PID 和本实例的派生进程已结束，再清理旧锁；不要盲目删除锁或用进程名批量终止其他 CC。原生会话历史可恢复，已经死亡的监控／后台进程不会复活，旧审批也失效。

背景和去重记录按配置保留；原生 transcript、附件、导出及未确认结果由管理员按公司策略归档。不能把同账户下的独立文件夹当成强安全沙箱。
