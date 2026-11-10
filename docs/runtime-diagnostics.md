# 运行诊断

本页面向运维者，说明日志、诊断命令和状态字段的含义。配置项与部署步骤见[部署与配置](deployment.md)，测试与验收证据的边界见[验证与验收](validation.md)。

前台 `run` 向 stderr 输出逐行 JSON；`daemon` 服务通过 `--log-file` 写入文件（默认 `<install-dir>/logs/service.log`，`--log-max-size` 10 MB、`--log-max-backups` 3），每行包含 UTC 时间和事件名。网关审计事件同时写入日志和状态库的 `audit` 表（保留 90 天）。配置凭据、模型认证头和受控操作密钥经统一 logger 脱敏；不要把 debug HTTP 转储、完整原始消息、模型请求或环境变量加入公共日志。

## 服务状态与日志

`daemon status`／`logs`／`stop` 的用法、日志轮转和 stale lock 的显式恢复步骤见[日志、停止与 stale lock](deployment.md#日志停止与-stale-lock)。排查时注意：

- `daemon status` 的 `Stale lock` 表示上次关闭未确认，`gateway.lock` 被保留以阻止新实例接管同一数据目录；Windows 注销／关机直接终止任务进程时也会出现。
- 只有核实锁记录的 PID 及本实例派生进程均已退出后，才使用 `daemon start --reclaim-stale-lock`；不要用 `schtasks /End`、`Stop-Process` 或重命名锁来“加速”停止。

## 分阶段 doctor 报告

`doctor` 默认只作本地检查。`--probe-feishu` 检查应用认证和机器人身份；`--probe` 可能计费，只检查基础 Messages 响应。它们不修改应用权限，不验证实际卡片、文件、手机／桌面体验或工具安全性。逐层接入流程见[首次接入](first-connection.md)。

- **`native`** 给出 `actual`、`tested` 和 `status`。协议回归固定在 Claude Code **2.1.321**：版本一致为 `tested`，其他可识别版本为 `untested`，无法识别为 `unavailable`。`tested` 只表示版本与回归基准一致，`verification.nativeProtocol` 仍是 `not_checked`——本机 `--version` 没有运行权限、追问、MCP、compact 或恢复测试。未测试版本不会仅因版本不同而阻止启动。
- **`verification`** 按项记录 `passed`／`failed`／`blocked`／`not_checked`：`local`、`configuration`、`nativeExecutable`、`nativeProtocol`、`model`、`modelStreaming`、`modelTools`、`feishuAuth`、`feishuIdentity`、`messageDelivery`、`cardCallbacks`、`fileDelivery`、`nativeTools`、`feishuReconnect`、`storageRows`。本地检查只设置 `local`、`configuration`、`nativeExecutable`；`--probe` 设置 `model`，`--probe-feishu` 设置 `feishuAuth`／`feishuIdentity`，`--verify-chat` 再写入 `incomingMessage`、`messageDelivery`、`fileDelivery`、`cardCallbacks` 和 `fileDownload`。`nativeProtocol`、`modelStreaming`、`modelTools`、`nativeTools`、`feishuReconnect`、`storageRows` 始终是 `not_checked`。不能把任意一个 `passed` 推广成完整验收；`ready` 只表示本次请求的检查全部通过。
- **`checks`** 中每个失败项带 `remedy`；`nextSteps` 按当前结果提示下一步。
- **`storage`** 是磁盘占用观察：数据库、WAL、SHM 的字节数，以及 `exports` 的文件数和总字节数。它只读文件元数据，不打开生产 SQLite、不做 checkpoint、不扫描聊天内容、不删除文件。导出目录最多扫描 10,000 项、不跟随符号链接；权限、上限或扫描不完整时标明 `partial`／`unavailable`。这些数字不能证明进程 RSS 泄漏；服务运行时文件大小可能变化，应视为一次快照。

| 现象 | 下一步 |
|---|---|
| 原生版本 `untested` | 对该候选版本运行[协议测试](validation.md#真实-claude-协议-fixture)；保留原生历史，勿以新建会话掩盖恢复失败 |
| `feishuAuth` 失败 | 检查应用 ID、凭据变量、应用可用性和主机网络；不要输出 secret |
| 认证成功、身份失败 | 检查机器人功能和身份接口；还不能判断事件订阅是否完整 |
| 基础模型成功、工具／流式未测 | 继续做原生 CLI 的本地模型回归及测试群验收 |
| 模型已结束、文件仍待交付 | 查看 `/outputs`；先确认交付，不能重复模型任务来补文件 |
| 平台暂时拒绝 | 查看安全的错误阶段、HTTP 状态、平台码及请求标识，等待有界重试结果 |
| 写入结果未知 | 先检查远端卡片／文件，再显式补发；不能把超时当作未执行 |

`doctor --verify-chat <配置中的群ID>` 或 `doctor --verify-chat dm:<配置中的用户ID>` 是单独的显式联网验收入口。先停止本机服务；命令占用同一服务锁以免重复接收事件，并使用私有临时数据库。按终端提示发送随机挑战消息，再确认测试卡片和文件；不调用模型、不修改绑定，最多等待两分钟。只接受所选聊天的获准作者、当前卡片 ID 和有效随机值；退出后更新卡片使按钮失效。文件打开结果记为 `confirmed_by_user`，与平台消息接口的确认分开。原生工具、模型、重连和原会话恢复仍保持独立验收状态。

## 飞书连接

适配层记录连接、重连、握手／心跳失败、回调处理及拒绝事件。API 错误保留安全的操作阶段、HTTP 状态和数值平台错误码，不输出提供方原始正文、请求头或 WebSocket URL。超时不能证明远端没有执行写操作。

长连接由 context 管理，关闭时终止 socket、心跳和重连等待。服务器返回的 ping／重连设置在有界范围内使用；发出心跳后持续 30 秒没有任何入站活动会关闭旧连接并重新取得 endpoint，即使 TCP 仍可写。正常空闲时 Pong 仍算入站活动。消息和回调的有界队列跨重连保留，已经接收的处理中消息不会因 socket 断开而被取消；诊断队列在重连期间继续可见，服务停止后清理。重连不自动重新执行未知模型输入、卡片创建或文件上传。

普通消息可达但卡片无响应时，分别检查平台回调订阅、客户端缓存、投递去重、请求是否仍有效、卡片和操作者归属。不能单凭没有回复推断平台未投递。先用授权测试账号验证，不要通过放宽权限解决诊断问题。

## 原生会话

`worker_ready`（含 `startupMs`、`resumed`）观察初始化和进程生命周期，不是用户首字延迟。原生恢复失败保留原 session ID；先检查 CLI 版本、cwd、配置身份和历史目录，不通过删除状态来掩盖故障。

原生 CLI 的 `[claude-code:unrecognized_model]` 是已知的 stderr 告警：在 2.1.321 的本地假 Messages 流程中不终止会话、不拒绝请求；模型名带 `[1m]` 时仍报告 1,000,000 上下文窗口，不带时为 200,000。`CLAUDE_CODE_AUTO_COMPACT_WINDOW` 实测在 `/context` 中生效（例如 800000 显示 800k 预算）。`worker_stderr` 保留这条告警原文供排障，不过滤、不降级；以上是模拟证据，真实流量仍需验收。

`shutdown_unconfirmed` 或 `worker_stop_unconfirmed` 表示尚不能证明本实例的原生进程已退出。服务保留 `gateway.lock` 或会话名额，防止并行接管。核实派生进程后再处置，不能只检查主 PID。

`/status --json` 的 `titleSync` 描述会话名称同步到原生标题的状态：`pending` 表示目标已保存、待原生确认，`confirmed` 才表示对应修订获 ACK；`not_sent`、`rejected`、`unconfirmed` 分别是未发出、明确拒绝和结果未知。`confirmed` 字段保留最后一次已确认值，不随目标保存提前更新。原生标题超时不会杀死健康任务，也不会据此重复提交原任务。

## 容量等待、复用与回收

容量参数 `runtime.maxWorkers`、`maxWorkersPerChat`、`maxRssMiB`、`idleMs`、`idleMinMs` 的含义见[容量与恢复](deployment.md#容量与恢复)。限额只决定能否新建原生进程，不中断已有输入流、后台工作或审批。

| 信号 | 含义 |
|---|---|
| `worker_admission_wait.reason` | 新进程暂不能启动：`worker_limit`（全局额度）、`chat_limit`（本聊天额度，同群话题共用）、`memory_limit`（进程树 RSS 达到 `maxRssMiB`）、`memory_unavailable`（无法观测内存，暂停新建）。附带 `workers`、`chatWorkers`、`residentBytes`、`maxRssMiB`；同一房间仅在开始等待或原因变化时记录 |
| `worker_capacity_reclaimed.reason` | 为腾出额度或因内存超限回收了一个确认空闲的进程；会话 ID 和历史保留 |
| `worker_slot_reserved.queueWaitMs` | 从首次容量等待到取得名额的时间；`resume` 标明是否按原生 ID 续接。不含进程初始化与模型首字延迟 |
| `worker_input_dispatched` | `reused` 表示输入交给了已服务过输入的热进程；`idleBudgetMs` 是此后的空闲保温预算（设置 `idleMinMs` 时随成功复用加倍至 `idleMs`）。复用次数不等于模型轮数 |

等待期间输入已持久保存，聊天收到一次“输入已保存”提示，`/inputs` 中对应输入显示“等待原生进程名额”。`memory_unavailable` 不会通过回收来猜测容量，应先检查进程树观测（Linux procfs、Windows Working Set、macOS `ps`）。服务定期维护在可观测内存超限时也会回收确认空闲的进程，并记录 `worker_capacity_reclaimed`（`reason: memory_limit`）。RSS 合计可能重复计算共享页，不是硬内存上限。

## 交付

`/outputs` 查询持久化的未确认交付，`/retry-output <ID>` 是明确补发入口。已知消息 ID 的更新可以重试，创建和上传结果未知时必须先核对远端。表情失败不重做模型任务；必要答案或文件未交付不标 Done。

## 传输与存储预算

每 30 秒的 `feishu_transport_metrics` 包含 HTTP 活动／等待／拒绝／共享退避，以及 WS 连接次数、最后入站时间、队列深度／观察峰值、拒绝、ACK 次数／失败／累计及最大耗时。维度使用固定端点组和消息 lane，不以 chat ID、message ID 或 URL 建立指标标签，不记录正文。ACK 指标描述适配层对回调的回复，不表示模型完成或最终文件送达。

`/status --json` 的 `storage` 仅统计当前聊天：表名、行数、最早时间、该时间列的含义与保留说明。空表或旧卡片缺少可用时间字段时 `oldestAt` 为 null；查询最多 2 秒，超时返回已完成的部分和 `storageError`，不伪造 0。维护者还可见客户端整体的 `transport` 指标；普通成员不能通过本聊天状态查看其他聊天共用的客户端负载。离线 `doctor` 只读文件元数据，不打开生产数据库。

服务每 30 秒执行一次 `Sweep`。其中普通数据库维护总截止 500 ms，每种策略最多检查 128 个候选、修改 64 行，游标和下一种策略持久保存。受保护的旧记录不会堵住其后的可清理记录；失败回滚该策略的变更与游标，已提交的前序策略仍计入报告。超时是单轮软上限：已提交游标保留，下一轮从被中断的策略继续，`store_maintenance` 以 `deadline` 和 `progress` 记录；只有连续多轮没有任何策略提交时才写 `maintenance_stalled` 审计。`/status` 显示最近一次完整遍历全部策略的时间。已清空的正文不再重复 UPDATE。`store_maintenance` 记录持续时间、候选、修改数、各策略及脱敏错误；它不保证数据库文件立即缩小，也不等于 RSS 回收。

| 记录 | 正常保留政策与例外 |
|---|---|
| 背景／context 输入 | 配置的背景窗口；聊天内仍有活动工作时保留，包括尚未绑定会话的附件上下文 |
| 终态输入 | 1 天后清正文、30 天后清普通去重标记；活动工作及用户取消／放弃相关去重标记受保护 |
| 已结算回执 | 7 天；仍有活动工作的会话的关联记录保留 |
| 输出消息索引 | 30 天；未完成 outbox 引用保留 |
| 终态 outbox | 导出文件清理后至少 7 天；未知文件结果、用户放弃决定保留；已放弃正文可单独清理 |
| 审计 | 90 天 |
| 已结束审批请求 | 到期后 30 天；仍有活动工作的会话的关联记录保留 |
| 已结束固定操作 | 90 天；running／uncertain 保留 |
| 用量与覆盖记录 | 保留，因为结果 ID 同时承担去重；不直接删除后重新累计 |
| 用户操作决定、输出来源放弃标记 | 保留，用于阻止旧输入重放或迟到最终回复伪装成成功 |
| 结果未知（uncertain）的输入与固定操作 | 记录本身保留为证据且不重放；它没有后续结算，因此不阻止同一会话或聊天中其他终态记录按上述期限清理 |

输出重试扫描有独立预算：每聊天一次最多查看 128 个按年龄／ID 排序的候选、返回 10 个可重试项目，500 ms 可取消；尚未到 `retry_after` 的整批候选也会推进游标。限流或未知结果不会迫使每次 Sweep 重新扫描同一大批旧记录。
