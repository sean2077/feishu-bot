# feishu-bot

在飞书群聊和私聊中使用 Claude Code。可用于个人助手、资料整理、协作开发，以及经授权的外部系统操作。支持 Linux 和 Windows 原生运行。

一个常驻服务接收消息，按需启动并复用会话。消息处理、子代理、上下文压缩和后台工作由 Claude Code 完成；feishu-bot 负责聊天路由、消息交付、权限交互和运行状态。

## 快速开始

需要 Node.js 22.16+ 和 Git。Windows 使用 Git for Windows。克隆本仓库后，在仓库目录执行：

```sh
npm ci --ignore-scripts
npm run init
```

`init` 会构建程序并生成 `config.json`，不会覆盖已有文件。填写飞书应用、允许接入的群或私聊、维护者 ID，以及模型服务配置。只使用私聊时可将 `groups` 设为 `[]`，无需建立群组或绑定代码仓库。

模型入口需要兼容 Anthropic Messages；`baseUrl` 对应 Claude Code 的 `ANTHROPIC_BASE_URL`，模型 ID 按服务实际支持的值配置。项目不绑定特定供应商，也不提供协议转换层。兼容接口不等于所有模型能力均已验证。

Linux：

```sh
export FEISHU_APP_SECRET='replace-with-app-secret'
export MODEL_AUTH_TOKEN='replace-with-model-token'
npm run doctor
npm start
```

Windows PowerShell：

```powershell
$env:FEISHU_APP_SECRET = "replace-with-app-secret"
$env:MODEL_AUTH_TOKEN = "replace-with-model-token"
npm run doctor
npm start
```

模型凭据的环境变量名可通过 `model.tokenEnv` 指定；已有显式配置保持有效，不需要改用示例中的名称。未配置时默认读取 `MODEL_AUTH_TOKEN`，不会尝试其他账号或凭据。

[部署与配置](docs/deployment.md) · [不同使用场景](docs/usage.md)

## 对话与会话

群聊中 @ 机器人或回复它的消息即可触发处理；私聊中的消息直接进入该私聊会话。处理中发送的新消息交给同一 CC 输入流，由 CC 判断如何继续，不额外增加任务分类模型。

获准接收的非 @ 群消息可作为有条数和时间限制的增量背景，不单独触发模型。将 `limits.backgroundMessages` 设为 `0` 可关闭背景缓存。每条消息保留作者、引用和附件信息。

同群成员共享会话；不同群、不同私聊分别管理历史、目录和原生记忆。首次任务启动进程，后续消息热复用。默认真正空闲 15 分钟后回收进程但保留会话；存在后台工作、监控、审批或未处理输入时不回收。

| 命令 | 作用 |
|---|---|
| `/status` | 查看当前会话、后台工作、输入与审批状态 |
| `/sessions`、`/resume <ID>` | 查看和恢复当前聊天的历史会话 |
| `/new`、`/stop` | 新建会话；停止当前聊天的运行及待交付输入 |
| `/answer <请求ID> <回答>` | 回答指定追问 |
| `/outputs`、`/retry-output <ID>` | 查看未确认送达的回复；明确补发 |

群内管理操作限维护者，私聊本人也可管理自己的会话。`/stop` 不能撤销已发生的外部操作；恢复历史不会重新启动已经结束的后台进程。

模型回复跟随当前请求的语言；内置命令和审批提示目前主要为中文。时间解释跟随服务运行环境，可通过启动前设置 `TZ` 统一时区。

## 按场景配置

`projects` 是资源与权限配置单元，不要求一定有代码项目。`seedDir`、`trustedConfigDir` 和外部 `actions` 都可按需配置。

| 场景 | 配置重点 |
|---|---|
| 个人助手 | 仅开启自己的私聊，不必配置源码或外部操作 |
| 资料整理 | 提供获准使用的资料目录或附件；按需接入文档服务 |
| 协作开发 | 配置干净的源码目录、可信项目技能和原生工具规则 |
| 业务查询与操作 | 配置固定资源范围的可信执行器；外部写操作逐次审批 |

默认使用原生 `acceptEdits`，常规文件编辑不逐次询问；需要更多确认时可使用 `default`。预授权工具由 `allowedTools` 配置，不默认开放任意 shell。[权限与信任范围](SECURITY.md)

Jenkins、GitLab、飞书文档和多维表格只是[可选操作示例](docs/project-actions.md)，不是运行依赖，也不代表任何特定部署环境。

## 运行边界与验证

原生进程使用服务账户的系统权限。不同聊天的目录和路由是逻辑分离，不是相互不信任租户之间的安全沙箱。个人部署和共享部署都应按实际需要授予文件、网络和外部 API 权限。

```sh
npm run check
```

测试包括类型检查、交互回放、构建，以及真实 CC 进程配合本地模拟模型的测试。实际飞书应用、所选模型和外部服务仍需分别验证；自动化测试不代表所有部署已通过验收。[验证清单](docs/validation.md)

[架构](docs/architecture.md) · [部署](docs/deployment.md) · [使用场景](docs/usage.md) · [开发](CONTRIBUTING.md) · [公开发布检查](docs/publication.md)
