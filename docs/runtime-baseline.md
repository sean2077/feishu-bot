# 运行基线

Go 版的生产结构是一个常驻网关加每个活跃会话一个原生 Claude 进程，不再存在每会话 Node worker。结构变化不能直接换算成内存百分比。

```sh
go build -trimpath -o ./bin/benchmark-native ./cmd/benchmark-native
./bin/benchmark-native --claude /absolute/path/to/claude --samples 3 --warm-turns 2 --idle-ms 1000
```

命令只使用临时状态、本地确定性模型与内存飞书替身，不读取部署配置。每样本启动一个原生会话并执行一冷两热轮次，确认同一进程／原生 ID 和最终 Done。输出 JSON 包含每次结果与失败阶段；交付或退出确认失败会返回非零退出码。内存采样支持 Linux `/proc`（RSS 与可读时的私有页）和 Windows Win32 Working Set／Private Bytes；macOS 当前明确 `unavailable`。不可观察内存不记作 0，也不影响原生功能验证本身的成功与失败。

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


## 可执行资源矩阵

```sh
./bin/benchmark-native --claude /absolute/path/to/claude --samples 3 --warm-turns 2 --idle-ms 1000 --scenario matrix
```

矩阵依次运行无会话、1 个热会话、4 个同时保留的热会话、20 KiB／100 KiB 中文回答的快／慢卡片更新、1 MiB 入站附件和完整回答导出、空闲回收后恢复原生会话 ID。长文本按 **UTF-8 字节** 构造；慢卡片场景使用本地替身延迟，不代表真实飞书网络。多会话逐个完成冷／热轮次后同时保留进程，用于稳定的容量比较，并非并发模型吞吐测试。

单独定位场景可用以下参数：

```sh
./bin/benchmark-native --samples 1 --warm-turns 0 --sessions 0 --idle-ms 100
./bin/benchmark-native --samples 3 --sessions 4 --idle-ms 1000
./bin/benchmark-native --samples 3 --text-bytes 102400 --update-ms 20 --transport-ms 100 --attachments --reclaim
```

`--sessions` 为 0–8，`--text-bytes` 最大 102400，`--update-ms` 为 20–5000，`--transport-ms` 为 0–1000。`--reclaim` 使用网关已有 `Sweep`，确认原进程释放后再发消息；网关会话 ID 和原生 ID 必须同时保持，恢复失败不会新建会话。没有会话时不应产生 CLI 或模型请求。

JSON schema 2 记录每阶段进程树、宿主／原生 CLI／其余工具或 MCP 后代分项、每轮首字／原生结果／模拟交付耗时、卡片创建与更新次数、序列化卡片字节、入站文件与导出文件字节，以及进程级 Go 分配。宿主分项包含网关、SQLite、采样器和本地服务；任意工具与 MCP 无法仅靠 PID 可靠区分，因此合并保留，不能把全部后代都记成 Claude 本体。Linux 私有页与 Windows Private Bytes 含义不同，跨操作系统不直接相减。

渲染细项使用生产渲染路径的 Go 分配基准：

```sh
go test ./internal/gateway -run '^$' -bench 'BenchmarkRender|BenchmarkReplyCard' -benchmem
```

CI 在 Linux／Go 1.27 记录一轮资源矩阵和有界的渲染分配观察，并保留 `native-resource-observations` artifact。单次 CI 观察用于发现回归与核对机制，不用于承诺性能比例；正式比较仍需固定机器、多样本和相同输入。进程树退出未确认时，采样器停止后续样本，避免把残留进程计入下一组。

## 原生版本与实际产物

普通 CI 使用固定 Claude Code 2.1.316。`Verify Go gateway` 的手动运行可同时指定 `candidate_sdk_version` 与 `candidate_native_version`，安装官方测试包、核对实际 `--version` 身份，再运行权限／追问、持续输入、MCP、compact、停止、恢复和长文本附件回归。候选成功不会自动改写仓库已测版本。

CI 还给无 CGO 生产二进制写入当前提交身份，直接运行该文件的 `version`、`init`、`doctor` 和本地模型探针；Unix 平台通过严格限定到本地 TLS 服务的代理验证飞书认证／身份探针。Windows 使用系统证书存储，因此这段假 CA 飞书探针明确不执行；其他实际二进制步骤仍运行。这些测试与源码内的真实 CLI 网关旅程共同提供发布前证据，但不是实际飞书或真实模型验收。
