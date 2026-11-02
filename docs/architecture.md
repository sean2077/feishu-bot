# 架构

一个 Node.js 服务按需启动原生 Claude Code 会话。网关处理交付、身份和审批，CC 处理任务理解、工具、上下文与后台工作。

```text
飞书群聊／私聊
  → Channel SDK
  → 生产入站适配、授权与明确触发、持久收件、审批
  → 当前聊天的原生 CC 输入／输出流
       → 配置的模型服务
       → tools / skills / subagents / compaction / Monitor
       → 当前聊天限定的按需历史工具
  ← 同卡思考／答案、提问、审批、文件、表情终态
```

## 运行与容量

Linux 和 Windows 使用相同 launcher。`runtime.ts` 管理进程，`worker.ts` 通过官方 Agent SDK 驱动 CC，持续接收跨轮输入和后台输出。没有第二套 agent loop、意图分类模型或业务调度器。

已有会话复用进程，首次启动准备工作区。容量满时回收确认空闲、最久未活动的进程，同时保留 session ID。后台工作、审批、输入、最终回复交付和未知活动不回收。短暂准入锁只保护容量预留与回收，不覆盖模型执行和等待审批，不同聊天可以并行。

## 授权、会话与消息

`policy.ts` 是入口、恢复、引用、历史、附件和输出重试共用的范围判断。群和作者均须授权，并明确 @；私聊只免除 @。拒绝发生在保存正文、回执和工具调用之前。`members` 缺省是维护者专用，不是全员开放。

房间键包含 tenant、app、project、聊天类型、chat ID 和私聊用户 ID，不包含线程。`projects` 是资源／权限配置单元，不必是代码仓库。会话元数据与原生 transcript 一起持久化；按明确 `resume` ID 恢复，原生恢复失败不自动创建新会话。

普通真人消息使用原生 `priority: next`、独立 UUID 和 `client_composed` 的结构化输入，不额外分类。CC 可能在工具结束后才读取补充，不承诺即时抢占。SDK 跨消息合并保持关闭；网关显式图片批次保留每条原始来源，不依赖有损 SDK 合并。

收件状态为 queued、sent、consumed、done，另有 cancelled 和 uncertain；传输确认不等于模型已读。按结果的 UUID 集合确认实际消费的输入，不把后台通知视作所有任务完成。接收顺序使用持久化插入顺序，不让消息时间戳重排 `/compact` 控制边界。

### 前台补充

Gateway 识别 `/btw`、`/ps` 并剥离命令名，仅将正文放进已有 JSON 输入。`Delivery.supplementTo` 持久绑定接收时仍在运行的前台原输入 UUID，不另建 Agent 或旁路查询。空闲热进程、仅有后台活动、compact 和无目标的情形不会创建会话或 Worker。

派发前及引用／附件等待后重新检查目标、会话和授权。已排队 compact 按原顺序阻止后续补充；目标结束、退出或恢复时，仍 queued 的补充取消并保存可恢复通知。sent／consumed 的未知结果沿用原有不重放规则。批准／拒绝使用独立卡片回调，不被补充或 compact 截获。

### 原始消息与连续图片

`feishu-events.ts` 是 CLI 和集成测试共同使用的生产订阅入口；`feishu-input.ts` 对真实 SDK 的归一化结果做完整性检查，不重写富文本渲染。SDK 原始对象仅在该检查中短暂使用，输出 Message 不含 raw；不完整输入带固定说明，获准触发后明确拒绝，不送模型。具体格式见[原始消息](raw-messages.md)。

`image-batches.ts` 只管理 inbox 的有界图片等待窗口，默认关闭。每条原消息拥有独立 inbox 行、UUID、消息 ID 和来源；批次 ID、固定截止时间与限制快照保存在各行 payload，与接收在同一事务提交。按同 room、session、作者、replyTo、threadId 合并连续独立图片，文本及命令封闭窗口。定时器仅唤醒派发，重启依赖数据库而不是内存计时器。

派发顺序从 SQLite 刷新，完整下载并检查总量后，在一个事务中把所有原消息标记 sent，再组成一次 `imageBatch` 原生输入。任何发送前失败都不以部分图片调用模型，并清理本次准备的文件。Worker 保留每条图片的 UUID 和归属；Gateway 的 inputGroups 将原生消费／结果中的批次入口 UUID 映射回所有原消息。完整交付屏障和授权不改变。已跨发送边界的批次重启后整批 uncertain，不自动重放。

### 入站与通知的交接

每个聊天的入站串行锁只维护本地授权、收件与会话控制顺序。只读管理命令在收到时读取本地状态，其飞书答复不等待网络发送完成再释放入站锁；限速、内容不完整、补充无目标、容量上限和 compact 等提示也不持有该锁等待消息接口。慢速或不确定的通知发送不会阻止同一聊天后续的 /stop；其他聊天保持独立。通知发送失败仍经既有安全审计路径处理，不改写原输入状态，也不表示模型完成。涉及会话变更或明确外部补发的管理操作仍保留原来的控制顺序和未知结果边界。

关闭期间，网关不再等待这些**非持久信息提示**的飞书网络结果：已发送的请求不一定能撤销，但迟到的成功或失败不会访问已关闭的状态库、补做消息索引或触发重发。此处理仅针对 notice 路径；最终答案、文件、审批和其他有持久交付语义的操作不借此绕过 outbox、确认、未知结果与原生进程清理。解除本地等待也不证明底层 SDK HTTP 套接字已经结束或远端没有收到该提示。

## 历史与压缩

`backgroundMessages` 默认为 0，不缓存和预注入非 @ 消息。`history.ts` 按显式工具请求读取一页，校验聊天、作者、条数、时间和绑定游标，不做全量归档。仅缓存已授权资源的有限元数据供附件访问，不建立新的完整聊天副本。

锁定的 Channel SDK 0.7.1 的 `fetchMessage` 辅助函数未保留 `chat_id`，因此引用和历史查询使用官方原始 API 的所有权字段，再复用官方内容归一化。该 SDK 还会在策略判断之前展开合并转发：适配器封住自动调用的公开 `message.get` 方法，只将原方法留在显式受限引用读取的闭包中。生产不访问 SDK 私有字段、不新增连接，不递归展开合并转发。升级 SDK 必须重跑这项边界测试。

`/compact` 单独走原生命令路径，不能与普通 JSON 输入混淆。持久化控制边界等待当前轮结束和最终交付，随后输入暂缓，卡片审批不阻塞。原生 `compact_boundary` 区分实际压缩与无需压缩。网关不另建自动压缩策略，不禁用 CC 自带自动压缩。

## 输出与回执

`native-output.ts` 保留仅在部分原生帧上出现的输入归属；按消息与块索引对齐增量和完整内容。只处理可展示的 text／thinking，不暴露签名、redacted thinking 和工具轨迹。后台任务来源有可靠关联时沿用原触发，未知来源不挂到最新提问。

`render.ts` 将思考与答案放在同一卡片，流式更新限频，结束折叠思考。最终内容先保存到 outbox，再等待网络；outbox 保存完整卡片与输入归属。已知 ID 重试更新，创建结果不明确时留待显式补发，不盲目再建。

`receipts.ts` 持久化每条消息的目标表情状态，串行合并晚到的网络回执。Done 只在成功回复和必要文件完整交付后添加，失败／停止清理临时状态。恢复时清理旧处理中标记，并重新检查授权。审批和追问保持独立交互，不属于隐藏的工具轨迹。

## 使用量与兼容迁移

Worker 透传真实 SDK result UUID 与 `usage`，仅从主循环响应元数据提取模型，不拿配置别名或累计 `modelUsage` 代替。`usage.ts` 在结果审计事务中写入 `usage_results`，主键为本地会话＋原生结果 ID；重复结果不重复累计或渲染。用量采集与答案交付分开，不能因文件尚未交付就抹去已发生的模型消耗。

SQLite schema 5 增加 `usage_results` 和 `usage_coverage`，保留旧表和 session 映射。旧会话首次迁移标记 prior_unknown，不从无稳定 ID 的历史审计伪造完整累计；新会话建立时登记统计起点。字段缺失保留 NULL，无法取得稳定结果 ID 时标记覆盖不完整而不猜测去重键。查询仅用当前 room 的选中 session，支持中文与 JSON，不创建会话、不唤醒 Worker、不扫描 transcript、不计算账单或上下文占用。

锁定 SDK 的持续输入实测验证 `result.usage` 的每结果口径：普通两轮、带工具多步轮次和重启续接均与确定性服务逐次声明吻合。主循环用量不包含子代理，升级 SDK 时必须重跑真实原生测试。详细字段见[使用量](usage.md#使用量)。

## 权限与扩展

保留 CC 原生 `default`／`acceptEdits` 和 `allowedTools`。额外权限请求通过维护者卡片处理。仅配置固定操作时才注册 `project_action` 和供 Bash／Monitor 使用的本地入口，复用参数校验与审批，不增加另一套调度器。

## 数据与能力边界

目录、HOME、附件和记忆是应用层分离，同账户进程不是安全沙箱。模型凭据按配置交给 CC，业务凭据不主动继承，环境白名单不能保证同账户任意代码彼此保密。

恢复 transcript 不复活旧进程或监控；未知外部写不会自动重放。普通输入的 `client_composed` 避免终端路径／slash 展开，只有授权管理命令例外。部分首轮技能提醒与终端 UI 不同，本项目不是 PTY 镜像。

模型回复跟随请求语言；内置提示主要为中文。不同模型服务的工具、多模态和上下文能力需独立验证。

## 相关接口

- [Feishu Channel SDK](https://github.com/larksuite/channel-sdk-node)
- [Claude Agent SDK](https://github.com/anthropics/claude-agent-sdk-typescript)
- [原生 Slash Commands](https://code.claude.com/docs/en/agent-sdk/slash-commands)
- [Agent SDK 用量说明](https://code.claude.com/docs/en/agent-sdk/cost-tracking)
- [飞书历史消息 API](https://open.feishu.cn/document/server-docs/im-v1/message/list)

[对话规则](conversation-contract.md) · [权限说明](../SECURITY.md) · [使用场景](usage.md) · [验证](validation.md)
