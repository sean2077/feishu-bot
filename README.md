# feishu-bot

在飞书群聊和私聊中使用 Claude Code。可用于个人助手、资料整理、协作开发，以及经授权的外部系统操作。支持 Linux 和 Windows 原生运行。

一个常驻服务接收消息，按需启动并复用会话。任务理解、子代理、上下文压缩和后台工作由 Claude Code 完成；feishu-bot 负责聊天路由、消息交付、权限交互和运行状态。

## 快速开始

需要 Node.js 22.16+ 和 Git。Windows 使用 Git for Windows。克隆本仓库后，在仓库目录执行：

```sh
npm ci --ignore-scripts
npm run init
```

`init` 会构建程序并生成 `config.json`，不会覆盖已有文件。填写飞书应用、允许接入的群、群内获准使用者、私聊用户、维护者 ID，以及模型服务配置。只使用私聊时可将 `groups` 设为 `[]`，无需建立群组或绑定代码仓库。

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

模型凭据的环境变量名可通过 `model.tokenEnv` 指定；已有显式配置保持有效。未配置时默认读取 `MODEL_AUTH_TOKEN`，不会尝试其他账号或凭据。

[部署与配置](docs/deployment.md) · [不同使用场景](docs/usage.md)

## 对话与会话

**群聊须同时满足：已授权群、已授权发言者、明确 @ 本机器人。**仅回复或引用机器人而没有 @，不会触发。群配置中的 `members` 列出获准使用者，项目维护者也可使用；省略或留空 `members` 只允许维护者，不会放开全群。私聊不要求 @，但用户仍须出现在 `dms` 中。

未授权消息静默丢弃，不回复、不加表情、不进入上下文。正常忙时补充直接交给当前 CC 输入流，由 CC 判断如何继续，不增加任务分类模型。

**非 @ 群消息默认不缓存、不预注入：`limits.backgroundMessages = 0`。**Agent 需要前文时调用 `read_chat_history`，按需读取当前聊天的一页历史；历史、引用和附件入口同样检查作者和聊天权限。工具需要应用具备对应消息读取权限，缺少权限会明确返回工具错误，而不是假装没有历史。

一群一条当前会话，不按线程或发言者拆分；每个私聊独立。空闲回收进程、服务重启和 `/stop` 都保留原 session ID，下次消息续接原生历史。只有 `/new` 主动新建；恢复失败不会静默清空上下文。原生历史文件需要持续保存在 `dataDir`。

| 命令 | 作用 |
|---|---|
| `/status` | 查看当前会话、后台工作、输入与审批状态 |
| `/sessions`、`/resume <ID>` | 查看和恢复当前聊天的历史会话 |
| `/compact [保留重点]` | 使用 CC 原生压缩；忙时在当前工作轮次结束后执行，不新建会话 |
| `/new` | 停止当前运行并新建会话，旧历史仍保留 |
| `/stop` | 停止运行和待交付输入，保留当前会话供下次继续 |
| `/answer <请求ID> <回答>` | 回答指定追问 |
| `/outputs`、`/retry-output <ID>` | 查看未确认送达的回复；明确补发 |

群内命令也须 @；影响共享会话的管理操作限维护者，私聊本人可管理自己的会话。网关不增加自动压缩策略，CC 自带的原生压缩照常工作。`/compact` 会区分实际压缩、无需压缩和失败。恢复历史不会复活旧后台进程，`/stop` 也不能撤销已经发生的外部操作。

## 回复展示

通过校验并接收后，在触发消息上添加临时 `OnIt` 表情。本轮成功且最终回复已交付时移除临时表情并添加 `Done`；失败、取消或未确认送达时不标成功。

一轮回复使用同一张卡片，引用最初的触发消息。模型公开返回的思考内容和答案分区更新，结束时折叠思考；工具调用轨迹默认不展示，审批和追问仍可交互。没有可展示思考时只显示处理状态，不生成额外的思考摘要。

[完整对话规则与升级说明](docs/conversation-contract.md)

## 按场景配置

`projects` 是资源与权限配置单元，不要求一定有代码项目。`seedDir`、`trustedConfigDir` 和外部 `actions` 都可按需配置。

| 场景 | 配置重点 |
|---|---|
| 个人助手 | 仅开启自己的私聊，不必配置源码或外部操作 |
| 资料整理 | 提供获准使用的资料目录或附件；按需接入文档服务 |
| 协作开发 | 配置干净源码、可信项目技能、群成员与原生工具规则 |
| 业务查询与操作 | 配置固定资源范围的可信执行器；外部写操作逐次审批 |

默认使用原生 `acceptEdits`，常规文件编辑不逐次询问；需要更多确认时可使用 `default`。预授权工具由 `allowedTools` 配置，不默认开放任意 shell。[权限与信任范围](SECURITY.md)

Jenkins、GitLab、飞书文档和多维表格只是[可选操作示例](docs/project-actions.md)，不是运行依赖，也不代表任何特定部署环境。

## 运行边界与验证

原生进程使用服务账户的系统权限。不同聊天的目录和路由是逻辑分离，不是相互不信任租户之间的安全沙箱。按实际需要授予文件、网络和外部 API 权限。

```sh
npm run check
```

测试包括类型检查、交互回放、构建，以及真实 CC 进程配合本地模拟模型的测试。真实飞书应用、所选模型和外部服务仍需分别验证。[验证清单](docs/validation.md)

模型回复跟随当前请求语言；内置命令和审批提示主要为中文。时区跟随运行环境，可在启动前设置 `TZ`。

[架构](docs/architecture.md) · [部署](docs/deployment.md) · [使用场景](docs/usage.md) · [开发](CONTRIBUTING.md) · [公开发布检查](docs/publication.md)
