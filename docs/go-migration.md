# 从 TypeScript 版迁移到 Go

Go 版替换网关、飞书适配、原生会话宿主、审批和固定操作执行器。生产进程结构由“Node 网关 + 每个会话一个 Node worker + 原生 Claude”改为“Go 网关 + 每个会话一个原生 Claude”。原生 Claude 的模型、工具、子代理和上下文仍由 Claude 自己执行。

## 保持的状态与行为

| 项目 | 迁移方式 |
|---|---|
| JSON 配置 | 保留字段、默认值和按配置文件目录解析的路径；保留 `runtime.mode: local`、`autoApproveWorkspace: false` 兼容入口 |
| SQLite | 继续使用 `gateway.sqlite` 和 schema 5；保留会话、收件、审批、交付、回执、CardKit 和用量数据 |
| 聊天身份 | 保留 tenant／app／project／聊天／私聊用户组成的 room key，线程不拆会话 |
| 工作区 | 保留 room key 哈希目录、独立 HOME／配置目录、附件目录及已有 launch identity |
| Claude 历史 | 使用保存的原生 session ID 续接，恢复失败报错；不会悄悄改为新会话 |
| 不确定操作 | sent／consumed 输入、未知创建和文件上传不自动重放；已知消息 ID 的更新可以重试 |
| 控制命令 | `/stop` 保留会话；`/new` 才新建；`/compact` 使用原生控制边界；补充绑定原前台输入 |

Go JSON 解码保持旧版的字段大小写、默认值和整数校验；旧版明确允许以 `null` 取默认值的可选容器继续兼容，权限、布尔值和单项限制不接受无效 `null`。可选字段建议省略。固定操作的 `parameters.*.pattern` 使用 Go RE2 语法；带反向引用或环视的 JavaScript 正则需要改写，否则启动会明确拒绝。普通字符类、分组和重复次数继续有效。

## 需要调整的部署部分

1. **安装原生 Claude Code。**生产版不再依赖 Agent SDK 代为携带 CLI。将可执行文件放入服务账户 PATH，或设置 `runtime.claudeExecutable`。协议回归固定 Claude Code **2.1.316**（对应旧 SDK 0.3.316 携带的版本）；自行升级后应重跑原生测试。
2. **构建网关。**Linux 使用 `go build -trimpath -o ./bin/feishu-bot ./cmd/feishu-bot`；Windows 输出 `./bin/feishu-bot.exe`。运行二进制不需要 Go 编译器、Node.js、npm 或 CGO 动态库。
3. **停止旧服务及自动启动入口。**确认其原生子进程已退出。不要让旧版和 Go 版同时连接同一应用或使用同一数据目录。
4. **备份完整状态。**保存配置、SQLite 及 WAL／SHM、工作区和原生历史；继承模式另保存实际 Claude 配置和历史目录。正常停止后再备份数据库能获得一致快照。
5. **保留相同路径与账户。**保留 `dataDir`、项目绑定、cwd 和原生配置身份。若使用相对 `dataDir`，配置文件也应保持原位置。启动命令改为 `feishu-bot run --config <path>`。
6. **先运行诊断。**`feishu-bot doctor --config <path>` 默认离线，使用独立临时数据库；按需显式添加 `--probe-feishu` 或 `--probe`。
7. **在授权测试聊天验收。**检查会话 ID、工具审批、追问、图片／文件、同卡流式、停止续接和异常交付，再切换常驻服务。旧审批在恢复时过期；旧卡片不能继续授权操作。

如果停止无法确认，网关保留 `gateway.lock`。必须先调查本实例的原生进程及派生工具，再决定是否清理锁；不要只根据网关 PID 已消失来删除。

## 回滚

停止 Go 服务并确认所有派生进程已退出后，可切回原版启动命令。两版保持同一 schema 和持久字段，正常数据可以续用；切换前仍应保留完整备份。不要把较旧数据库单独覆盖回已产生新历史的目录。对未知外部结果先核实，回滚不代表外部写操作被撤销。

## 内存与性能结论的边界

这次改动移除了每个热会话的 Node worker、TypeScript SDK 和重复飞书依赖装载，减少了进程及运行时层数。同机、单个暖会话的本地 fixture 中，完整进程树 RSS 中位数由 326.86 MiB 降至 217.94 MiB，减少约 33.3%；[运行基线](runtime-baseline.md)记录了样本与限制。该结果不代表所有真实部署的降幅。原生 Claude、工具子进程、原生历史和工作文件依然存在；继承配置中的插件和 MCP 也可能占主要内存。

比较时使用同版本 Claude、相同配置、相同暖会话数和相同输入，分别测量纯网关与完整进程树。RSS 相加会重复计算共享页；可用时同时记录 PSS／USS。测量应覆盖空闲、单会话、多个暖会话、工具执行和回收后状态。缺失观察标记 unavailable，不以 0 替代。

生产版不加入周期性内存采样器，不自动调低会话并发或空闲时间。单纯降低 `maxWorkers`／`idleMs` 的结果不能计作语言重构收益。

## 后续参考其他 Go 项目

`core.Transport` 与 `core.WorkerFactory` 保留清晰边界，便于参考 Go 项目的 WebSocket、进程管理、流式解析和诊断做法。引用或移植实现时逐个核实许可证、协议版本和行为测试；不能因语言相同直接假设会话、审批和消息投递规则相同。飞书和原生 Claude 仍是本项目的范围，不引入额外聊天平台或第二套调度器。
