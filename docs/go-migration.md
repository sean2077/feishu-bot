# 从 TypeScript 版迁移到 Go

> **适用范围**：仅用于把 `v0.2.0` 及更早的 TypeScript 部署升级到 Go 版。迁移本身已在 `v0.3.0` 完成；新部署和 Go 版之间的升级不需要阅读本页，按[部署与配置](deployment.md)操作即可。当前代码仍保留下述兼容入口（旧配置别名、SQLite schema 5 的原位升级和旧房间目录布局），所以本页仍是有效的一次性升级说明。

Go 版替换网关、飞书适配、原生会话宿主、审批和固定操作执行器。生产进程结构由“Node 网关 + 每个会话一个 Node worker + 原生 Claude”改为“Go 网关 + 每个会话一个原生 Claude”。原生 Claude 的模型、工具、子代理和上下文仍由 Claude 自己执行。

## 保持的状态与行为

| 项目 | 迁移方式 |
|---|---|
| JSON 配置 | 保留字段、默认值和按配置文件目录解析的路径；保留 `runtime.mode: "local"`、`autoApproveWorkspace: false` 兼容入口 |
| SQLite | 继续使用 `gateway.sqlite` 和 schema 5；启动时只添加附加表和可缺省列，保留会话、收件、审批、交付、回执、CardKit 和用量数据 |
| 聊天身份 | 保留 tenant／app／project／聊天／私聊用户组成的房间键，整群范围下线程不拆会话 |
| 工作区 | 保留房间键哈希目录、独立 HOME／配置目录、附件目录及已有启动身份 |
| Claude 历史 | 使用保存的原生 session ID 续接，恢复失败报错；不会悄悄改为新会话 |
| 不确定操作 | sent／consumed 输入、未知创建和文件上传不自动重放；已知消息 ID 的更新可以重试 |
| 控制命令 | `/stop` 保留会话；`/new` 才新建；`/compact` 使用原生控制边界；补充绑定原前台输入 |

## 需要改写的配置

Go 解码保持旧版的字段大小写、默认值和整数校验，但更严格地拒绝已经无效的字段：

- 旧版明确允许以 `null` 取默认值的可选容器继续兼容；权限、布尔值和单项限制不接受 `null`。可选字段建议直接省略。
- `runtime.mode` 只接受 `"local"`；`runtime.image`／`memory`／`cpus` 与 `model.maxConcurrent` 出现即拒绝启动，须删除。
- `autoApproveWorkspace: false` 读取为 `permissionMode: "default"`；`true` 被拒绝，请改用 `permissionMode`／`allowedTools`。
- 固定操作的 `parameters.*.pattern` 使用 Go RE2 语法；带反向引用或环视的 JavaScript 正则需要改写，否则启动会明确拒绝。普通字符类、分组和重复次数继续有效。

迁移后的其他配置变化（`backgroundMessages`、`members` 等）与 Go 版之间的升级相同，见[配置兼容](deployment.md#配置兼容)。

## 迁移步骤

1. **安装原生 Claude Code**。生产版不再依赖 Agent SDK 代为携带 CLI。将可执行文件放入服务账户 PATH，或设置 `runtime.claudeExecutable`。协议回归固定 Claude Code **2.1.316**（对应旧 SDK 0.3.316 携带的版本）；自行升级后应重跑原生测试。
2. **获取 Go 网关**：下载发布包或从源码构建，见[获取程序](deployment.md#获取程序)。运行二进制不需要 Go 编译器、Node.js、npm 或 CGO 动态库。
3. **停止旧服务及自动启动入口**：确认其原生子进程已退出，删除旧的 launcher／stop 脚本和自启动任务。不要让旧版和 Go 版同时连接同一应用或使用同一数据目录。
4. **备份完整状态**：保存配置、SQLite 及 WAL／SHM、工作区和原生历史；继承模式另保存实际 Claude 配置和历史目录。正常停止后再备份数据库能获得一致快照。
5. **保留相同路径与账户**：保留 `dataDir`、项目绑定、cwd 和原生配置身份。若使用相对 `dataDir`，配置文件也应保持原位置。
6. **先运行诊断**：`feishu-bot doctor --config <path>` 默认离线，使用独立临时数据库；按需显式添加 `--probe-feishu` 或 `--probe`。
7. **在授权测试聊天验收**：按[首次连接](first-connection.md)检查会话 ID、工具审批、追问、图片／文件、同卡流式、停止续接和异常交付。旧审批在恢复时过期；旧卡片不能继续授权操作。
8. **切换常驻服务**：Linux／Windows 使用 `feishu-bot daemon install --config <绝对路径>`，不再需要 launcher 脚本，见[内置 daemon 服务](deployment.md#内置-daemon-服务)。用 `daemon stop`／`daemon start`／`daemon restart` 验证停止和重启后仍是同一原生 session ID。

如果停止无法确认，网关保留 `gateway.lock`。必须先调查本实例的原生进程及派生工具，再决定是否清理锁；不要只根据网关 PID 已消失来删除。

## 回滚到 TypeScript 版

迁移设计保持同一 schema 版本号（5）和既有持久字段，Go 版只添加附加表和可缺省列，不删除旧表或改写会话映射。但仓库没有针对“当前 Go 版回退到 TypeScript 版”的回归测试，而且旧版不识别 Go 版后续新增的状态，例如原生身份未决、停止暂停、待补存用量和话题范围群的会话，可能因此绕过这些保护。回滚应尽量在迁移验收阶段进行；已在 Go 版长期运行的部署优先回退到较早的 Go 版本，或恢复迁移前的完整备份（会丢失迁移后的会话进展）。

回滚时：

1. 执行 `feishu-bot daemon stop`（或 `daemon uninstall` 删除任务／unit），确认 Go 进程及其原生子进程均已退出；不要让两版同时连接同一飞书应用。`daemon uninstall` 保留 config、`service.env`、日志和 state，便于再次切回。
2. 保留当前完整备份；不要把较旧数据库单独覆盖回已产生新历史的目录。
3. 切回原版启动命令。对结果未知的外部写操作先核实；回滚不代表这些操作被撤销。

## 迁移背景（历史）

迁移移除了每个热会话的 Node worker、TypeScript SDK 和重复的飞书依赖装载，减少了进程及运行时层数。同机、单个暖会话的本地 fixture 中，完整进程树 RSS 中位数由 326.86 MiB 降至 217.94 MiB，减少约 33.3%；样本、测量方法与限制见[运行基线](runtime-baseline.md)。该结果不代表所有真实部署的降幅：原生 Claude、工具子进程、原生历史和工作文件依然存在，继承配置中的插件和 MCP 也可能占主要内存。单纯降低 `maxWorkers`／`idleMs` 或启用容量参数带来的变化不能计作语言重构收益。

`core.Transport` 与 `core.WorkerFactory` 保留清晰边界，便于参考其他 Go 项目的 WebSocket、进程管理、流式解析和诊断做法。引用或移植实现时逐个核实许可证、协议版本和行为测试；不能因语言相同就假设会话、审批和消息投递规则相同。飞书和原生 Claude 仍是本项目的范围，不引入额外聊天平台或第二套调度器。
