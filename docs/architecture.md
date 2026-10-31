# 架构

## 职责

`feishu.ts` 使用官方 `@larksuite/channel` 管理 Feishu WebSocket、消息归一化、卡片、引用和资源下载。`gateway.ts` 做确定性的身份／会话路由。`runtime.ts` 托管每个聊天的 worker；`worker.ts` 通过官方 Claude Agent SDK 保持一个连续的输入输出流。

SQLite 保存聊天归属、输入投递状态、审批、外部操作状态及最终输出。CC 自己的 transcript 仍是原生对话历史，不在网关再维护一份可供模型重放的完整历史。

```text
飞书群／私聊
  → 官方 Channel SDK
  → 身份校验、短期背景、可靠收件
  → 每个聊天独立的 Claude worker
       → 原生工具、subagents、压缩、Monitor、会话定时能力
       → 房间限定的 MCP／本地操作通道
  ← 输出流、追问、审批、附件
```

## 输入与会话

房间键包含 tenant、app、project、聊天类型、chat ID，以及私聊用户 ID。项目绑定变化不会恢复旧项目的原生会话。恢复只能使用网关保存并归属于当前聊天的 ID，不能枚举宿主上所有 CC 会话。

普通输入使用 `priority: next`、真人来源及独立 UUID。网关不按任务类型分类。正在执行的工具可能仍需完成后 CC 才消费补充消息；这不是即时抢占保证。跨群并行是独立 worker，同群并行由 CC 原生子代理处理。

收件状态区分 `queued → sent → consumed → done`。`accepted` 仅表示 worker 收到，不冒充模型已读。当前 SDK 的 `user_message_uuids` 给出本轮合并／中途消费的输入归属；后台结果不能顺手确认所有待处理输入。持久输入缓冲用于可靠交付和容量限制，不是第二套业务任务调度器。

Channel SDK 的有损跨消息合并被关闭。正文、发送者、消息 ID 和引用逐条保留。非 @ 消息只进入有时限、条数与总体字符限制的缓存；触发时发送增量背景，不逐条唤醒模型。正文较长时截取上限，附件按需读取；没有跨群背景合并。

## 回复与交互

持续读取输出流，不在第一条 `result` 后结束整个 reader。流式文本合并更新；不显示 thinking 内容。一个结果可以覆盖多人输入，默认引用最初触发消息，后续不因为某人刚说话就把已有卡片移动到另一个话题。

原生 `AskUserQuestion` 与权限回调走单独的飞书卡片回路。回调只做短操作；消息回调不等待 agent 执行，卡片通道也不排在整个任务之后。指定请求的回答与普通补充消息分开。

最终结果先保存到 outbox。已知消息 ID 的更新可以重试；创建请求失败但无法确认是否已经送达时标为 `uncertain`，只有显式补发才允许可能的重复。这不是端到端 exactly-once 承诺。

## Linux 执行边界

网关以服务账户运行。每个聊天的 Docker worker 有独立可写 workspace 和 HOME；只读挂载经过管理员选择的 `.claude` 配置和宿主拥有的附件目录。容器使用 `--network none`、只读根文件系统、资源／进程限制、去掉 capabilities、禁止提权；不挂载 Docker socket、宿主 HOME 或全局密钥。

公司模型请求通过只读挂载的 UNIX socket 转交宿主。该代理只接受 Messages／count_tokens、已配置模型和临时房间凭据，不能变成通用 HTTP 代理。真实公司 token 留在宿主。业务操作也由宿主的固定目标执行器处理，不继承模型自己提供的目标 URL 或 shell 命令。

`action-relay.ts` 让原生 Bash／Monitor 调用相同的 `project_action`，不是另一个 agent、定时器或调度器。它只接受本项目允许的操作；所有写审批仍由宿主执行。

## 原生能力的明确边界

- SDK、Channel SDK 和全部传递依赖由 lockfile 锁定。更新必须重跑真实 CC fixture，不只检查 TypeScript 类型。
- 组成后的输入使用 `client_composed`，避免把聊天资料中的 `@path`／斜杠当作本地文件或管理命令。当前 CLI 会同时跳过 turn-start 附加上下文的一部分，部分 skill 列表／提醒会到第一轮工具调用后才出现。不是完整终端 UI 镜像；不宣称每个斜杠命令原样透传。
- 保留 CC 默认系统提示和工具，没有另写 planner、compactor、subagent 调度器或 cron。
- 活跃后台任务由原生 level events 追踪；Stop hook 的原生后台／会话定时摘要用于决定是否可以回收。无法判定时保守保留进程，而不误杀监控。
- 恢复 transcript 不会复活旧进程、Monitor 或外部操作。重启先停止属于本实例的遗留容器，过期审批失效，投递结果不明的输入不自动重放。
- Docker 禁网意味着项目依赖和编译工具需预装到定制 worker 镜像；WebFetch、直接联网的 shell／MCP 不会神奇可用。可使用已批准的项目操作读取外部系统，不为方便而偷偷放开 egress。
- 公司非 Claude 模型的能力和兼容性由实际网关决定；原生权限机制不等于模型理解质量得到保证。

## 参考接口

- https://github.com/larksuite/channel-sdk-node
- https://platform.claude.com/docs/en/agent-sdk/streaming-vs-single-mode
- https://platform.claude.com/docs/en/agent-sdk/permissions
- https://github.com/anthropics/claude-agent-sdk-typescript
- https://github.com/chenhg5/cc-connect

借鉴的是消息／审批／恢复场景和边界设计，没有把其他项目的 agent loop 或通用渠道框架加入本项目。
