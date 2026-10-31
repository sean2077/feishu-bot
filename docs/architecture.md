# 架构

一个 Node.js 服务按需启动原生 Claude Code 会话。飞书消息接入与业务使用场景分开：网关处理交付、身份和审批，CC 处理工具、上下文与后台工作。

```text
飞书群聊／私聊
  → Channel SDK
  → 身份、有限背景、持久收件、审批
  → 当前聊天的原生 CC 输入／输出流
       → 配置的模型服务
       → tools / skills / subagents / compaction / Monitor
  ← 流式回复、提问、审批、文件
```

## 运行与容量

Linux 和 Windows 使用相同 launcher。`runtime.ts` 管理会话进程，`worker.ts` 通过官方 Agent SDK 驱动 CC，持续接收跨轮输入和后台输出。

已有会话复用进程，首次启动准备工作区。容量满时回收确定空闲、最久未活动的进程，同时保留 session ID。后台工作、审批和待处理输入不回收，状态未知时保守保留。容量释放后重新交付等待房间。

短暂的全局准入锁只保护容量预留和回收，不覆盖模型执行或等待审批。不同聊天可以并行。

## 会话与消息

房间键包含 tenant、app、project、聊天类型、chat ID 和私聊用户 ID。`projects` 表示资源和权限配置单元，可用于代码、资料或业务操作，不必对应代码仓库。

同群成员共享主会话，不同群与私聊分别管理。话题和引用帮助定位讨论和回复，不另设业务任务分类器。普通真人消息使用原生 `priority: next`，保留独立 UUID；CC 可能在当前工具结束后才读取新输入，不承诺即时抢占。

关闭 Channel SDK 的有损跨消息合并，保留每条消息的作者与资源。非 @ 群消息仅进入有界缓存，触发时交付增量背景；缓存可关闭。时间元数据跟随进程解析出的运行时区，管理员可在启动前设置 `TZ`。

收件状态区分 queued、sent、consumed、done。传输确认不等于模型已读，完成状态依据原生结果的输入 UUID；无归属的后台通知不确认其他输入。outbox 持久化最终回复；未知创建结果不盲目重试，已知卡片 ID 可重试更新。

## 权限与扩展

使用 CC 原生 `default`／`acceptEdits` 及 `allowedTools`。额外权限请求通过飞书卡片交给维护者，不用自写命令分类器取代原生判断。

只有配置固定操作时才注册 `project_action` 和供 Bash／Monitor 使用的本地入口。该入口复用相同的参数校验与审批，不是第二套调度器。业务写操作按请求批准，文件参数绑定快照。

## 数据与能力边界

会话目录、HOME、附件和记忆是应用层分离；同系统账户进程不是不可信租户的安全沙箱。模型凭据按配置交给 CC，业务凭据不主动继承，但环境变量白名单不能保证同账户任意代码彼此保密。

原生 transcript 由 CC 管理，网关不重复注入完整历史。恢复历史不复活旧进程或监控，状态未知的外部写不会自动重放。

`client_composed` 避免组装后的群内容触发终端路径或 slash 展开；部分首轮技能提醒与终端 UI 不同。本项目不是 PTY 镜像，不承诺全部终端功能逐项一致。

模型回复默认跟随当前请求语言；内置卡片和命令文本目前主要为中文。不同模型服务的工具调用、多模态和长上下文能力需独立验证。

## 相关接口

- [Feishu Channel SDK](https://github.com/larksuite/channel-sdk-node)
- [Claude Agent SDK](https://github.com/anthropics/claude-agent-sdk-typescript)

[权限说明](../SECURITY.md) · [使用场景](usage.md) · [验证](validation.md)
