# 部署

## 飞书应用

测试和生产使用**不同 app ID／secret**。同一个应用的多条 WebSocket 长连接可能分流事件，不是广播；Windows 测试进程不应抢生产应用的连接。

使用企业自建应用，开启机器人和事件长连接，订阅消息接收、交互卡片回调。业务操作按需添加云文档／多维表格权限。选择了群背景模式，需要实际开通获取非 @ 群消息的权限；仅 `group_at_msg` 类权限不足。后台权限名称和可申请范围以企业飞书控制台为准。

先建两个测试群、两个测试账号／私聊。只把这些 ID 配入测试配置；不要用“所有群开放”代替配置。机器人只有在相应项目资料被授予服务身份访问权限后，才能读取资料。

## Linux 生产

使用独立非 root 服务账户和本机常规 Docker daemon。首版采用宿主 UID/GID 对应挂载，rootless／userns-remap／远程 daemon 尚未验收；不要不经检查直接替换部署模式。网关运行在宿主，worker 运行在容器；不需要把 Docker socket 交给模型。

```sh
npm ci --ignore-scripts
npm run check
docker build -f deploy/Dockerfile.worker -t feishu-bot-worker:0.1.0 .
node dist/cli.js init --config /etc/feishu-bot/config.json
```

确保配置目录已存在且服务账户可写初始化目标。将 `runtime.mode` 改为 `docker`，设置短且专用的 `dataDir`，例如 `/var/lib/feishu-bot`；该目录只给服务账户读写。项目可以设置 `seedDir` 为干净源码，`trustedConfigDir` 为审核过的 `.claude` 内容目录。初次复制后形成独立 Git baseline；不会每次请求覆盖修改。

将密钥放入权限 0600 的 `/etc/feishu-bot/environment`，使用 `deploy/feishu-bot.service` 托管；按实际安装目录修改 `WorkingDirectory`、`ExecStart` 和账户。systemd 环境文件不是 shell 脚本，不写 `export`。

```text
FEISHU_APP_SECRET=...
COMPANY_MODEL_TOKEN=...
```

```sh
node dist/cli.js doctor --config /etc/feishu-bot/config.json
# 明确确认后再做基础联网检查；会调用公司模型：
node dist/cli.js doctor --config /etc/feishu-bot/config.json --probe
node dist/cli.js run --config /etc/feishu-bot/config.json
```

源代码编辑和测试在无网络 worker 内完成。生产依赖应预装到项目定制镜像或使用离线包／编译缓存；不要把构建脚本放到持有业务凭据的宿主执行。宿主操作只用于审核过的固定业务操作，见项目操作文档。

## 公司模型网关

提供 Anthropic Messages 兼容接口和流式响应，配置示例见 `examples/config.example.json`。`baseUrl` 可包括路径前缀，支持末尾已有 `/v1`。默认只接受 HTTPS；`allowHttp` 仅供隔离的本地测试。

主模型和小模型必须写网关实际支持的 ID，所有原生子代理也走这些映射。`extraHeaders` 支持 `env:NAME` 引用，例如额外的 `x-api-key` 或租户凭据；不要把密钥写成明文常量。宿主 Node 的企业 CA 可通过启动时的 `NODE_EXTRA_CA_CERTS` 配置。不要用关闭证书校验解决代理问题。Feishu SDK 支持代理环境变量；公司模型请求的代理转发应在宿主网络层验证，不能把一次 HTTP 成功当作完整流式验收。

`doctor --probe` 只验证一个小请求，未验证所有工具、模型上下文窗口、thinking、缓存、子代理或图片兼容性。没有自动切换到个人账号或其他供应商的兜底。

## Windows 原生测试

安装 Node.js、Git for Windows，使用独立的测试 app 和 `local` 模式。PowerShell：

```powershell
npm ci --ignore-scripts
npm run check
node dist/cli.js init
$env:FEISHU_APP_SECRET = "测试密钥"
$env:COMPANY_MODEL_TOKEN = "测试网关凭据"
# Git Bash 未被 CC 找到时才显式设置：
$env:CLAUDE_CODE_GIT_BASH_PATH = "C:\Program Files\Git\bin\bash.exe"
node dist/cli.js doctor
node dist/cli.js run
```

配置内 Windows 路径使用 JSON 双反斜杠，或 `C:/...`。不要把个人工作区、生产 token 或凭据文件放进测试 seed。原生 Windows 与 Linux 使用相同业务代码，子进程清理由各平台处理；WSL／Docker 通过不等于 Windows 原生通过。

## 停机、恢复和保留

正常停止用 Ctrl+C／systemd stop。数据目录单实例锁防止同一状态被两个进程同时写。崩溃后遗留锁需要管理员先核实记录 PID 已不存在，再删除 `gateway.lock`；不自动猜测并抢锁。启动时只清理带本实例专属标签的旧容器。

原生会话历史可恢复，但旧监控和后台进程不会复活。正在等待的审批失效；送达不明确的输入／外部操作需要人工核对。不要把整个数据库备份恢复后自动重放当作灾难恢复方案。

背景按时间／条数限制；终止输入原文一天后清理并保留 30 天去重 tombstone，成功 outbox 七天后清理。原生 transcript、附件、导出文件和未确认结果不会擅自删除；设置磁盘告警并在会话停止后按公司数据策略归档清理。服务日志使用 journald 自身的限额与轮转。
