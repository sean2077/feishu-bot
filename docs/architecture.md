# 架构

一个 Node.js 服务按需启动原生 Claude Code 会话。网关处理交付、身份和审批，CC 处理任务理解、工具、上下文与后台工作。

```text
飞书群聊／私聊
  → Channel SDK
  → 授权与明确触发、持久收件、审批
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

普通真人消息使用原生 `priority: next`、独立 UUID 和 `client_composed` 的结构化输入，不额外分类。CC 可能在工具结束后才读取补充，不承诺即时抢占。关闭 SDK 有损跨消息合并，保留每条作者、引用和资源。

收件状态为 queued、sent、consumed、done；传输确认不等于模型已读。按结果的 UUID 集合确认实际消费的输入，不把后台通知视作所有任务完成。接收顺序使用持久化插入顺序，不让消息时间戳重排 `/compact` 控制边界。

## 历史与压缩

`backgroundMessages` 默认为 0，不缓存和预注入非 @ 消息。`history.ts` 按显式工具请求读取一页，校验聊天、作者、条数、时间和绑定游标，不做全量归档。仅缓存已授权资源的有限元数据供附件访问，不建立新的完整聊天副本。

锁定的 Channel SDK 0.7.1 的 `fetchMessage` 辅助函数未保留 `chat_id`，因此引用和历史查询使用官方原始 API 的所有权字段，再复用官方内容归一化。不自动递归展开合并转发内容。

`/compact` 单独走原生命令路径，不能与普通 JSON 输入混淆。持久化控制边界等待当前轮结束，随后输入暂缓，卡片审批不阻塞。原生 `compact_boundary` 区分实际压缩与无需压缩。网关不另建自动压缩策略，不禁用 CC 自带自动压缩。

## 输出与回执

`native-output.ts` 保留仅在部分原生帧上出现的输入归属；按消息与块索引对齐增量和完整内容。只处理可展示的 text／thinking，不暴露签名、redacted thinking 和工具轨迹。后台任务来源有可靠关联时沿用原触发，未知来源不挂到最新提问。

`render.ts` 将思考与答案放在同一卡片，流式更新限频，结束折叠思考。最终内容先保存到 outbox，再等待网络；outbox 保存完整卡片与输入归属。已知 ID 重试更新，创建结果不明确时留待显式补发，不盲目再建。

`receipts.ts` 持久化每条消息的目标表情状态，串行合并晚到的网络回执。Done 只在成功回复交付后添加，失败／停止清理临时状态。恢复时清理旧处理中标记，并重新检查授权。审批和追问保持独立交互，不属于隐藏的工具轨迹。

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
- [飞书历史消息 API](https://open.feishu.cn/document/server-docs/im-v1/message/list)

[对话规则](conversation-contract.md) · [权限说明](../SECURITY.md) · [使用场景](usage.md) · [验证](validation.md)
