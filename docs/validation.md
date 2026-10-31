# 验证与验收

## 当前架构

2026-10-31（Asia/Singapore）：按使用需求撤掉容器运行，统一 Linux／Windows 原生服务。此前 #1 的 Docker 测试只属于旧版本，不再作为当前部署证明。

## 自动化

`npm run check` 包括类型检查、核心／安全回放、构建和真实 CC 测试。CI 使用 Ubuntu／Windows × Node 22.16／24，不要求 Docker，也不使用真实服务凭据。

新增 native-service 回归覆盖：无容器默认配置、旧原生模板的权限保持、公司请求头、网络配置白名单、闲聊不启动进程、忙时输入热复用、容量竞争、空闲回收后原 session 恢复、等待房间即时唤醒，以及审批／后台活动／监控／未知状态不被回收。

`native-runtime.test.mjs` 使用生产的原生 launcher、实际 SDK 和 Claude Code 二进制；只有模型响应是本地确定性 fixture。分别验证 `default` 模式下 Write 审批、忙时另一人补充，以及 `acceptEdits` 下相同编辑不再弹审批。两者均验证：公司自定义请求头、原生直连、第二轮复用同一会话、中文路径附件、原生 Agent／Skill／ScheduleWakeup 可用、空项目不注册无用业务操作。

本地新测试已运行；远端完整结果见本次修正 PR 的对应 CI。此前未修改的模块按 main 的 blob hash 校验后用于本地验证；远端 CI 运行的是完整仓库，包括已有群／私聊／审批／outbox 回归。

## 性能记录方式

`worker_ready.startupMs` 记录实际 worker 初始化时间，不是模型首字时间。native fixture 另外输出首轮／热续轮耗时，用来检查有没有每条消息冷启动，但本地模拟模型的毫秒数不能外推成 DeepSeek 或飞书延迟，也不是对 cc-connect 的基准比较。

结构性减负：删除每聊天容器启动与模型代理；首次工作区只准备一次；后续消息复用进程；空闲群不占满活跃名额；空项目不启动本地业务 listener；不增加路由 LLM、业务 cron 或重复历史注入。

## 实际环境仍需验收

使用独立的飞书测试 app 和公司测试网关，检查两个账号、两个群、两个私聊：忙时补充、引用附件、审批回调归属、未知结果不重放、Monitor／子代理回传、实际权限和配额。Windows 工作机另验 Git Bash、证书／代理、中文路径和进程退出。

没有真实飞书／公司模型／业务服务凭据时，不将这些项目标为完成。Windows CI 不等于用户 Windows 11 实机验收；fixture 不证明 DeepSeek 的任务理解能力。无容器的正常系统账户进程也不等于安全沙箱。
