# feishu-bot

项目组的飞书机器人，直接运行 Claude Code。支持 Linux 和 Windows，不需要 Docker。

一个常驻服务接收飞书消息；各群和私聊的 CC 会话按需启动并复用。网关负责身份、消息交付、审批和状态，任务理解、工具使用、子代理、压缩和监控交给 CC。

## 开始使用

安装 Node.js 22.16+（推荐 24）和 Git。Windows 安装 Git for Windows。

```sh
npm ci --ignore-scripts
npm run init
```

`init` 会构建程序并生成 `config.json`，不会覆盖已有配置。填写飞书 app、允许的群／私聊、维护者，以及公司模型网关地址和模型 ID。`baseUrl` 与你在 Claude Code 中使用的 `ANTHROPIC_BASE_URL` 相同，不添加本地模型转换服务。

设置密钥后运行。Windows PowerShell：

```powershell
$env:FEISHU_APP_SECRET = "测试应用密钥"
$env:COMPANY_MODEL_TOKEN = "公司网关凭据"
npm run doctor
npm start
```

Linux 使用对应的 `export` 设置环境变量，执行同样命令。首次先用独立测试飞书应用；测试与生产不要复用同一个 app 的长连接。[飞书配置与后台部署](docs/deployment.md)

## 日常使用

群里 @ 机器人或回复它的消息即可。处理中有人继续 @，新消息直接送入当前 CC 输入流，不等待整项任务完成；不需要学习 queue／steer 等输入模式。

未 @ 的获准群消息只作为有限的增量背景，不启动模型。每条消息保留作者、引用和附件；同群成员共享主会话，不同群和私聊分别管理会话、目录和原生记忆。

- **少冷启动**：服务启动和普通群闲聊不启动 CC。首次任务启动，后续消息复用进程和上下文。
- **不让空闲群占满资源**：默认真正空闲 15 分钟后只回收进程，保留历史；容量满时优先释放空闲进程。监控、子代理、执行中的操作和审批不会被当成空闲。
- **少打断**：默认使用 CC 的 `acceptEdits`，文件编辑不逐次审批。项目的常用测试命令可通过 `allowedTools` 预授权；其余权限请求仍发给维护者。没有默认全权限模式。

### 少量管理命令

| 命令 | 作用 |
|---|---|
| `/status` | 当前会话、后台活动、输入与审批状态 |
| `/sessions`、`/resume <ID>` | 查看和恢复本聊天的历史会话 |
| `/new`、`/stop` | 新会话；停止当前聊天运行及待交付输入 |
| `/answer <请求ID> <回答>` | 回答指定追问 |
| `/outputs`、`/retry-output <ID>` | 查看未确认送达的回复，明确补发 |

群内新建、恢复和停止限维护者；私聊本人也可管理自己的会话。`/stop` 不会撤销已经发生的外部操作，也不会复活旧监控；需要旧上下文时显式 `/resume`。

## 项目配置

```json
{
  "id": "project",
  "maintainers": ["ou_maintainer"],
  "seedDir": "C:/work/project-source",
  "permissionMode": "acceptEdits",
  "allowedTools": ["Bash(npm test *)"],
  "actions": []
}
```

`seedDir` 可选，只在聊天工作区首次建立时复制干净源码并创建独立 Git baseline；后续消息不重复复制，也不覆盖现有修改。项目依赖可在原生工作区正常安装、复用系统工具和包缓存，不再要求离线镜像。

`allowedTools` 使用 CC 原生规则，不是另一个命令分类器。预授权前确认该项目及脚本可信。Jenkins、GitLab MR、飞书文档与多维表格可通过已有可信 CLI 或[受控项目操作](docs/project-actions.md)接入；写操作逐次审批，凭据不默认放进 agent 环境。

## 部署与信任范围

Linux 常驻服务和 Windows 工作机使用同一运行方式。建议团队服务使用独立、非管理员的系统账户，不继承你的个人登录态。

不同会话的目录和路由是**逻辑分离，不是 OS 沙箱**。允许执行代码后，它具有该服务账户的系统权限；同一账户的进程不能承诺彼此保密。此版用于同一信任范围的项目组，不宣称能承载相互不信任的租户。[权限说明](SECURITY.md)

## 验证

```sh
npm run check
```

包含类型检查、单元／交互回放、构建，以及真实 CC 进程对本地模型 fixture 的测试。CI 在 Windows／Linux、Node 22.16／24 上运行，不需要容器或真实密钥。

真实飞书和公司 DeepSeek 网关仍需使用你的测试环境联调；本地模型 fixture 不代表真实模型性能或企业 API 验收。[验证清单](docs/validation.md)

[架构](docs/architecture.md) · [部署](docs/deployment.md) · [项目操作](docs/project-actions.md) · [开发](CONTRIBUTING.md)
