# feishu-bot

把 Claude Code 接到项目组的飞书群和私聊。多人可以在处理过程中继续补充消息；不同群、每个私聊分别保留自己的会话和工作区。

网关只负责交付、身份、审批与运行托管。任务理解、工具执行、并行子代理、压缩和后台监控仍交给 Claude Code。模型通过公司提供的 Anthropic Messages 兼容入口调用。

## 能做什么

- 群内 @ 或回复机器人触发处理；其他获准接收的群消息仅作为有限、增量背景，不自动启动模型。
- 普通消息直接进入持续开放的 CC 输入流，不等待整个上一轮结束，也不额外做意图分类。
- 保留发送者、引用、话题和附件；同一原生结果可以确认多条输入，不按“一条输入一个结果”计数。
- 同群共享主会话；群与私聊分别隔离历史、记忆、工作区、附件和审批。
- 流式回复、提问卡片、一次性审批、状态查询、会话恢复、停止和结果补发。
- Linux 使用无网络的隔离 worker 容器；公司模型密钥和业务写凭据留在宿主。
- Windows 原生可开发和测验。它不是生产容器隔离的替代品。

## 开始使用

要求 Node.js **22.16+**，推荐 Node.js 24；Git；生产环境另需 Linux 和 Docker。CC 运行时由锁定的官方 Agent SDK 提供，无需继承个人 Claude 登录。

```sh
npm ci --ignore-scripts
npm run check
node dist/cli.js init
```

编辑生成的 `config.json`，填写测试飞书应用、群／用户 ID、维护者和公司模型入口。模板默认是本地测试模式。密钥通过环境变量提供，不写进仓库：

```powershell
# Windows PowerShell；使用独立的测试飞书应用。
$env:FEISHU_APP_SECRET = "测试应用密钥"
$env:COMPANY_MODEL_TOKEN = "公司测试网关凭据"
node dist/cli.js doctor
node dist/cli.js run
```

Linux 使用相同 CLI；生产部署见 [部署说明](docs/deployment.md)。`doctor --probe` 会发起一次极小的模型请求，有 token 开销，默认不会执行。

## 聊天方式

正常说话即可，例如“@机器人 帮忙看这个构建失败”，处理中其他人仍可补充“只在 Windows 出现”。群背景不构成授权，权限决定不交给模型。

| 命令 | 作用 |
|---|---|
| `/status` | 当前运行、后台活动、待交付输入和审批数 |
| `/sessions` | 只列本聊天的历史会话 |
| `/new` | 停止当前运行后新建本聊天会话 |
| `/resume <ID>` | 选择本聊天的历史会话，下条消息恢复上下文 |
| `/stop` | 停止本聊天 worker、后台进程、审批和待交付输入 |
| `/answer <请求ID> <回答>` | 回答指定追问；不会把普通消息误当作审批 |
| `/outputs` | 查看未确认送达的最终回复 |
| `/retry-output <ID>` | 人工确认补发结果不明确的回复，可能出现重复消息 |

群内新建、恢复和停止限维护者；私聊本人也可以管理自己的会话。敏感操作仍只由维护者批准。非维护者私聊发起敏感操作时，维护者收到**操作摘要**审批卡，不接收私聊历史。

`/stop` 不承诺撤销已经发出的外部操作。下一条普通消息使用新会话；需要之前的上下文时显式 `/resume`。进程回收和新建会话是两回事；默认不因闲置换会话。

## 项目能力

预先授权的代码阅读、本地编辑与测试由 CC 在工作区完成。高权限远端操作使用固定、受控的宿主操作；不把发布凭据交给可执行任意代码的 worker。

[项目操作说明](docs/project-actions.md) 附有 Jenkins 查询／构建、GitLab 提交 MR、飞书文档和多维表格示例。已有可信 CLI 可直接接入，无需重写业务系统。原生 Bash／Monitor 也可以调用这些**同一套**受控操作，网关不增加 cron。

## 运行与验证边界

`npm run check` 包含类型检查、回放／安全测试、构建，以及**真实 CC 二进制＋本地模拟模型服务**测试。CI 在 Linux、Windows 和两个 Node 版本上运行，并另做 Linux Docker 边界验证。

这些测试不等于真实飞书租户或公司 DeepSeek 网关验收。接入前请按 [验收清单](docs/validation.md) 做双账号、双群和私聊联调。模型能否正确理解并行任务、图片及复杂工具调用，也需要实际模型验证。

## 文档

[架构与原生能力边界](docs/architecture.md) · [部署](docs/deployment.md) · [权限与隔离](SECURITY.md) · [项目操作](docs/project-actions.md) · [验收](docs/validation.md) · [开发](CONTRIBUTING.md)
