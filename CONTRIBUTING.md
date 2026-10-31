# 开发

Node.js 22.16+、Git；`npm ci --ignore-scripts`，然后 `npm run check`。Linux 和 Windows 同一套 native 运行，不需要容器。依赖使用现有精确版本及 lockfile。

只服务飞书和 CC，不增加路由 LLM、planner、压缩器、业务 cron 或通用 adapter。新增代码应改善真实交互、权限、交付或原生运行；不要把简单部署重新包成基础设施平台。

回归重点：忙时输入／结果归属、热进程复用、容量竞争、后台不被误回收、会话恢复、群／DM 所属关系、一次性审批、停止、附件、outbox 和结果未知的外部写。

`tests/native-runtime.test.mjs` 使用真实 launcher／CC 和本地模型 fixture，不需要真实密钥。CI 验证 Ubuntu／Windows 和两个 Node 版本；实际飞书、公司网关和用户工作机结果单独记录。

团队运行使用专用系统账户。业务凭据不主动注入 agent，不在携带凭据的执行器中运行不可信项目脚本；也不要把同账户原生进程描述为相互隔离的安全沙箱。
