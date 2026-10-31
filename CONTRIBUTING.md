# 开发

Node.js 22.16+，Git；`npm ci --ignore-scripts` 后运行 `npm run check`。依赖使用精确版本和 lockfile，不使用 `--force` 或 `--legacy-peer-deps` 掩盖 peer 冲突。

核心原则：只支持 Feishu＋Claude Code；不增加模型意图分类、通用 adapter 框架、业务 cron 或重复的 agent loop。新增代码必须明确属于交付、身份、审批、隔离或运行托管。

关键回归：忙时输入合并归属；不同群／DM 不串；旧审批／复制卡片失效；停止后不复活旧任务；不盲目重放外部写；附件和发布补丁的 fd／快照边界；原生 Windows 和 Linux 分别测试。

`tests/native-runtime.test.mjs` 使用真实 SDK 携带的 CC 进程和本地模拟模型，不需要真实密钥。`tests/docker-smoke.mjs` 需先构建 worker 镜像，只在 Linux 非 root 账户运行。`docs/validation.md` 区分自动化证据与真实渠道验收。

改动不应把服务凭据传入 worker，也不应以 allowlist 提示词代替宿主校验。高权限执行器路径、环境和资源目标来自管理员配置。配置只在重启时加载；运行中的工具参数不能更改项目或会话归属。
