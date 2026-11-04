# 运行基线

Go 版的生产结构是一个常驻网关加每个活跃会话一个原生 Claude 进程，不再存在每会话 Node worker。结构变化不能直接换算成内存百分比。

```sh
go build -trimpath -o ./bin/benchmark-native ./cmd/benchmark-native
./bin/benchmark-native --claude /absolute/path/to/claude --samples 3 --warm-turns 2 --idle-ms 1000
```

命令只使用临时状态、本地确定性模型与内存飞书替身，不读取部署配置。每样本启动一个原生会话并执行一冷两热轮次，确认同一进程／原生 ID 和最终 Done。输出 JSON 包含每次结果与失败阶段；交付或退出确认失败会返回非零退出码。内存采样目前支持 Linux `/proc`，其他平台明确 unavailable；它不影响原生功能验证的成功与失败。

## Go 迁移的本地观察

相同 Linux 主机、Claude 2.1.316、`deterministic-text-v1`、一个暖会话，双方各 3 个样本、9 轮全部成功。下面是闲置 1000 ms 后快照的中位数，单位 MiB：

| 项目 | TypeScript v0.2.0 | Go 重构 |
|---|---:|---:|
| 宿主（含 fixture／数据库） | 50.44 | 18.20 |
| 可观察子进程 | 279.32 | 200.10 |
| 完整进程树 | 326.86 | 217.94 |
| 子进程数量 | 2 | 1 |

该样本完整进程树 RSS 减少约 **33.3%**。分项中位数之和不一定等于逐次总量的中位数。Go 宿主减少约 63.9%，原生 Claude 仍占主要部分；不能将宿主比例用于整套服务。

[完整原始数据](benchmarks/go-migration-baseline.json)保留样本、耗时、版本、结果与统计限制。[TypeScript 采样器补丁](benchmarks/typescript-proc-sampler.patch)只校正容器 PID 命名空间与采集方法，没有修改旧版生产逻辑。旧版使用 Node 24.22.1，新版使用 Go 1.27.3。Go 数据取自本 PR 重构工作树、最终生命周期审查之前；它提供迁移方向证据，不是所有后续提交的精确基准。

本地飞书传输为内存替身，没有真实 WS、真实模型、插件、MCP 或多会话负载。运行时另有后台构建任务，因此原始数据中的延迟只作描述，不作为隔离后的加速结论。真实部署应按下述口径再测。

## 可重复的比较

固定同一机器、操作系统、原生 Claude 版本、配置、权限、输入和暖会话数。比较 TypeScript 版时可从 Git 中检出 v0.2.0 的测量脚本；当前 Go 树只运行 Go 实现，旧脚本不再属于生产路径。

先用 `CLAUDE_NATIVE_PATH` 和本地模型 fixture 验证协议，再测量：

| 状态 | 需要记录 |
|---|---|
| 网关启动、无会话 | 网关 RSS／PSS／USS、进程数、是否意外启动 CLI |
| 单个热会话 | 网关与 Claude 分项、总进程树、第一次响应和热续轮 |
| 多个热会话 | 固定 2／4／8 个会话的增量，不混入不同插件／MCP |
| 工具／附件／长回答 | 峰值、传输缓冲、完成后的保留量 |
| 空闲回收后 | 已退出进程、会话 ID 保留、再次唤醒时间 |

记录每次观察、样本数量和 min／median／max；少量样本不报告 p95／p99。不用不同 runner 的数字宣称语言迁移收益，也不把同时改变 idleMs 或 maxWorkers 的结果计为语言收益。

## 内存口径

Linux 可采集 `/proc/<pid>/status` 和 `smaps_rollup`；Windows 使用 Working Set、Private Bytes 或相应系统指标。RSS 相加会重复共享页，进程树结果也可能遗漏已脱离父进程的工具。优先同时保留分项和 PSS／USS，明确单位。

容器中的 `process.pid`／`os.Getpid()` 与挂载的 `/proc` 可能属于不同 PID 命名空间。必须先核对 `/proc/self/status` 的 Pid／NSpid 与采样器视图，再关联后代。无法识别主进程时标记 **unavailable**，不得记为 0 或悄悄换一个同名进程。

真实模型的延迟、飞书网络、工具执行和 CLI 初始化应分别记录。本地 fixture 只提供确定性回复，其合成 token 不代表真实成本或上下文规模。测量工具不加入生产服务的定时循环。
