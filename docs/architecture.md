# 架构

一个 Node.js 服务，按需派生原生 Claude Code 会话。只服务 Feishu＋Claude Code，不实现通用渠道框架、业务 planner 或 cron。

```text
飞书群／私聊
  → 官方 Channel SDK
  → 身份、短期背景、持久收件、审批
  → 本聊天的常驻原生 CC 输入／输出流
       → 公司模型网关（直接连接）
       → CC tools / skills / subagents / compaction / Monitor
  ← 流式卡片、提问、审批、文件
```

## 部署与运行时

Linux、Windows 使用相同 native launcher。`runtime.ts` 启动会话进程，`worker.ts` 使用官方 Agent SDK 驱动实际 CC 子进程，保留跨轮输入流与后台输出。没有 per-chat 容器、镜像、Unix socket 模型代理、Docker socket 或启动时全量预热。

`Gateway` 只限制同时活跃的会话数，不拆解业务任务。已有进程热复用；首次启动只准备一次聊天工作区。达到上限时释放确定空闲的最久未活动进程并保留 session ID，原生后台任务／审批／待处理输入不被回收。新房间在容量释放时立即重新尝试交付，不必等下一轮固定轮询。

短暂的全局 admission 锁只保护容量预留／回收，不包住 CC 初始化、模型运行或等待审批；不同房间的启动和普通输入仍可并行。

## 会话与消息

房间键包含 tenant、app、project、聊天类型、chat ID 和私聊用户 ID。同群成员共享主会话，不同群和 DM 分开，话题只用于定位讨论和回复。

普通真人消息用原生 `priority: next`，保留独立 UUID 和身份，不另作意图分类。SDK 的有损跨消息合并关闭；未 @ 的获准群消息仅进有界缓存，下次触发时交付增量背景。CC 仍可能在当前工具完成后才消费新输入，不保证强制即时打断。

收件区分 queued／sent／consumed／done；传输 ACK 不等于模型已读。按 result 的输入 UUID 归属确认，不把无归属的后台通知当成完成所有任务。outbox 保存最终回复；未知创建结果不盲重试，已知卡片 ID 可以重试更新。

## 权限与项目工具

默认 `acceptEdits`，常用测试等按 `allowedTools` 原生规则预授权；其余请求通过飞书维护者卡片决定。不用自写的“安全命令识别器”取代 CC，也不默认绕过权限。

只有配置外部操作时才注册 `project_action` 工具和 Monitor 使用的本地操作入口，不为空项目启动无用 listener。这个入口只是原生 Bash／Monitor 对同一受控操作的访问方式，不是另一套调度器。业务写操作仍逐次审批，文件审批仍绑定快照。

## 数据与边界

每聊天独立 workspace、HOME、附件和原生记忆是应用层分离；所有进程属于同一系统账户，不是相互不信任者的执行沙箱。模型凭据进入 CC 的原生请求环境，业务凭据不主动继承，但同账户任意代码不能依靠环境白名单实现强密钥隔离。

原生 transcript 由 CC 自己管理；网关只持久化交付／审批和会话映射，不重复向模型灌入完整历史。恢复 transcript 不复活旧监控；服务故障后状态不明确的外部写不会自动重放。

`client_composed` 避免组装后的群内容被当作本地 CLI slash／路径展开；部分首轮技能提醒与终端 UI 不完全相同。这不是 PTY 镜像，也不宣称替代全部交互式终端功能。

## 参考

官方接口与已有项目的消息／审批场景是参考，不引入它们的 agent loop：
- https://github.com/larksuite/channel-sdk-node
- https://code.claude.com/docs/en/agent-sdk/permissions
- https://code.claude.com/docs/en/agent-sdk/streaming-vs-single-mode
- https://github.com/anthropics/claude-agent-sdk-typescript
- https://github.com/chenhg5/cc-connect
