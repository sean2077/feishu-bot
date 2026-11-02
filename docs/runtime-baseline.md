# 原生运行基线

这是一项按需运行的有限测量，不向生产网关增加采样器、调度或模型调用。

```sh
npm run build
node scripts/benchmark-native.mjs --samples 3 --warm-turns 2 --idle-ms 1000
```

输出 JSON 可重定向保存。参数有范围检查：样本 1–10，每样本热续轮 1–5，闲置观察 100–10000 毫秒。不读取部署配置，不连接真实飞书，不使用真实模型凭据；模型响应来自仅监听回环地址的确定性 HTTP fixture，飞书传输使用内存替身。

## 口径

每个样本创建独立临时状态、工作区和原生会话，使用生产 Gateway、SQLite、launcher 与锁定 SDK 中的 CC。第一条输入标记为 `cold-native`，后续输入必须复用同一个 worker 和原生 session。这里的冷启动是原生进程／会话首次启动，不是系统冷启动；模块、文件系统和 OS 缓存可能已热。

| 字段 | 含义 |
|---|---|
| `serviceInitMs` | 当前进程中配置／SQLite／Gateway 恢复准备时间；不含 Node 启动与模块导入 |
| `workerStartupMs` | 生产 launcher 的原生 ready 计时；不是模型首字或消息送达时间 |
| `firstOutputMs` | 从提交消息到第一个可展示 text／thinking 事件，含首轮工作区准备 |
| `nativeResultMs` | 从提交消息到原生 result |
| `mockDeliveryMs` | 从提交消息到替身确认 Done；不是飞书网络延迟 |
| `usage` | SDK result 实际报告的四种 token 字段；缺失为 null。fixture 自报数字是合成数据，不是价格、真实 token 成本或当前上下文占用 |
| `idle.hostCpuMs` | 有限闲置窗口中测量进程的 CPU 时间；不包含子孙进程 CPU，不是 CPU 百分比 |
| `idle.memory` | 闲置后的一次进程树快照，主机和可观察后代分别报告，排除采样命令及其后代 |
| `stopMs` | 生产 Gateway 关闭耗时，连同退出确认结果报告，不代替异常孤儿进程测试 |

Linux 使用 `ps` RSS（KiB 转字节），Windows 使用 `Win32_Process.WorkingSetSize`。主机进程包含测量脚本、Gateway、SQLite 和 fixture HTTP 服务，不能称为纯网关开销；后代包含可观察到的 Worker／CC。各进程 RSS／working set 相加可能重复计算共享页，不是独占物理内存。采样失败标记 `unavailable`，不能记为 0。断链或快照前已结束的后代不在覆盖范围。

内存采样开销单独记录，不加入回合延迟。Windows 采样本身会启动 PowerShell，因此停止计时不能用来证明首次 PowerShell 冷启动或旧 CIM 故障已经解决。脚本不改变生产清理算法，也不按进程名结束任务。

## 比较与失败

报告保留每个样本和失败阶段，汇总包括尝试／成功／失败数量。任一样本的原生交付或关闭失败均设置非零退出码；无法采集内存单独标记。摘要只提供有效观察的数量、最小值、中位数与最大值，不用三个样本估算 p95/p99 或生产失败概率。

比较时固定代码树、SDK、OS、Node、CPU、参数、权限、fixture 版本及后台负载；不同 GitHub runner 的值不能直接当作优化前后收益。现有 CI 在完整回归结束后串行执行该测量，避免与同作业的测试文件竞争。原有 PID、心跳、授权、交付和进程超时断言不变。

需要真实模型和飞书基线时，应在另行授权的独立环境记录相同阶段，并明确网络、工具、模型、usage 来源及价格口径；不要把本脚本的合成 token 或本地延迟外推为线上成本和体验。

统计实现参考 [Node process 数据口径](https://nodejs.org/api/process.html) 和 [Win32_Process 字段](https://learn.microsoft.com/en-us/windows/win32/cimwin32prov/win32-process)。对照 cc-connect 的用量展示时，仍区分整轮累计使用量、当前上下文与不同模型，不额外扫描 transcript 或增加网关压缩策略。
