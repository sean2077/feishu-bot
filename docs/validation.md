# 验证记录与上线验收

日期：2026-10-31（Asia/Singapore）。版本：0.1.0 初版实现。此记录严格区分真实运行时、模拟渠道和外部服务联调。

## 本地已经执行

Linux、Node.js 22.16.0：`npm run check`，包括严格类型检查、单元／集成测试、构建以及真实 Claude Code 协议测试。依赖来自锁文件：Claude Agent SDK 0.3.316（内含 CC 2.1.316）、Feishu Channel SDK 0.7.1、Zod 4.1.12。

`tests/native-runtime.test.mjs` 启动的是实际 SDK 和实际 Claude Code 二进制，而不是 fake worker。仅模型服务使用本地可控的 Anthropic Messages 响应 fixture；没有发送公司数据。它验证：

- 原生 Write 审批悬而未决时仍能接收另一人的输入。
- 审批后实际写入中文／空格路径文件。
- 一个 result 正确归属多条输入 UUID，而非一条输入必等于一轮结果。
- 第三条输入复用相同原生 session 和进程，并携带先前上下文。
- 原生 Agent、Skill、ScheduleWakeup 工具没有被网关替代。

使用假渠道／worker 的集成用例另外覆盖：群共享、两个群和两个私聊并发、增量背景、真实作者归属、审批和普通输入并行、越权／过期／私聊转交审批、会话越界、停止后的旧输出、重复投递、崩溃时不盲目重放、附件按需下载、持久化 outbox、不可变文件审批快照、受控操作入口和独立 Git baseline。

## 远端自动化

`.github/workflows/ci.yml` 在 Ubuntu／Windows 原生环境、Node 22.16.0／24 上运行同一检查，并在 Linux 构建实际 worker 镜像。`tests/docker-smoke.mjs` 通过真实 Docker＋CC＋本地模型 fixture 检查：禁网、只读根目录、只读附件、宿主文件／密钥／Docker socket 不可见以及宿主模型凭据转发。

核心源码提交 `93eb82b23c1050d754e55e1969a74aadf3282c47` 的 [CI 运行](https://github.com/sean2077/feishu-bot/actions/runs/41163928755) 已通过四组平台的检查步骤和 Linux Docker 测试。Linux 每组 35 项核心测试通过；Windows 每组 34 项通过、1 项 POSIX 专用用例按条件跳过。四组均另外执行并通过 1 项真实 CC 协议测试。Windows runner 的结果不是用户 Windows 11 工作机实机验收；Docker 用例也不能代替真实飞书联调。最终提交状态以 PR／main 对应 CI 为准。

## 仍需要你的环境验证

本次没有公司网关、真实飞书应用和项目服务身份的凭据，不把这些项目标成已验收：

| 场景 | 验收方法 |
|---|---|
| 实际飞书身份／卡片回调 | 两个账号、两个群、两个私聊验证 @、引用、附件和多次／转发／无权卡片点击 |
| 公司 DeepSeek | 实际消息流、工具执行、忙时补充、子代理、长上下文压缩、图片能力与费用统计 |
| 真正后台交互 | 同群任务后台运行时继续提问，Monitor 通知正确回到原群；停止后无孤儿进程 |
| 业务操作 | 隔离 GitLab／Jenkins／飞书测试资源，验证读写范围、审批和结果未知时的核对流程 |
| Windows 工作机 | 使用独立测试 app，验证实际 Git Bash、企业代理／证书、中文路径和进程退出 |
| 故障 | 重启网关、断网重连、消息重投、卡片超时；不无声丢消息、不自动重做状态未知的写操作 |

新配置先 `doctor`；`doctor --probe` 只发一个小模型请求，不代表完整验收。测试与生产不得复用同一个飞书 app 长连接。

## 性能与功能边界

当前没有真实 DeepSeek／飞书延迟或资源占用的对照数据，不宣称比 cc-connect 更快／更省 token。已做结构性减负：不额外调用路由 LLM、不每条消息冷启、不重复复制完整历史、不为闲聊唤醒模型、流式输出限频。

Native SDK 的 `client_composed` 输入用于避免已组装的群消息被当作 CLI slash command 或路径展开；它跳过该 SDK 首条输入的部分附件／技能提醒构造。项目指令、Skill 工具和后续原生工具仍存在，但不宣称与交互式终端所有 UI 细节完全相同。

Monitor／原生调度和后台进程依赖活跃 worker；恢复 transcript 不复活这些进程。权限隔离默认按群／DM，属于同一群的成员共享该群知识与操作结果。普通 Windows local 模式只做逻辑隔离，不能承载不受信任多人生产执行。
