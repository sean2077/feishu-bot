# 运行基线

本页维护性能与内存的测量方法和比较口径。Go 版的生产结构是一个常驻网关加每个活跃会话一个原生 Claude 进程，不再有每会话 Node worker；结构变化不能直接换算成内存百分比。各类结果能作为哪种证据，见[验证与验收](validation.md#性能与回归边界)。

## 测量工具

```sh
go build -trimpath -o ./bin/benchmark-native ./cmd/benchmark-native
./bin/benchmark-native --claude /absolute/path/to/claude --samples 3 --warm-turns 2 --idle-ms 1000
```

`--claude` 缺省读取 `CLAUDE_NATIVE_PATH`。命令只使用临时状态、本地确定性模型（当前 fixture `deterministic-text-v2`）与内存飞书替身，不读取部署配置；输出 JSON 的 `scope.liveServiceAccepted` 恒为 `false`。每个样本启动原生会话，执行一次冷轮次和 `--warm-turns` 次热续轮，确认同一进程／原生 ID 和最终 Done。JSON 包含每次结果与失败阶段；交付或退出确认失败返回非零退出码。

内存采样支持 Linux `/proc`（RSS 与可读时的私有页）和 Windows Win32 Working Set／Private Bytes；macOS 当前明确 `unavailable`。不可观察内存不记作 0，也不影响原生功能验证本身的成败。

## 可重复的比较

固定同一机器、操作系统、原生 Claude 版本、基准 fixture、配置、权限、输入和暖会话数。比较 TypeScript 版时可从 Git 检出 v0.2.0 的测量脚本；当前 Go 树只运行 Go 实现，旧脚本不属于生产路径。修改原生 CLI 版本时先按[协议 fixture](validation.md#真实-claude-协议-fixture) 验证，再测量。

| 状态 | 需要记录 |
|---|---|
| 网关启动、无会话 | 网关 RSS／PSS／USS、进程数、是否意外启动 CLI |
| 单个热会话 | 网关与 Claude 分项、总进程树、第一次响应和热续轮 |
| 多个热会话 | 固定 2／4／8 个会话的增量，不混入不同插件／MCP |
| 工具／附件／长回答 | 峰值、传输缓冲、完成后的保留量 |
| 空闲回收后 | 已退出进程、会话 ID 保留、再次唤醒时间 |

记录每次观察、样本数和 min／median／max；少量样本不报告 p95／p99。不用不同 runner 的数字宣称语言迁移收益，也不把同时改变 `idleMs`、`maxWorkers` 等容量参数的结果计为语言收益。

## 内存口径

Linux 可采集 `/proc/<pid>/status` 和 `smaps_rollup`；Windows 使用 Working Set、Private Bytes 或相应系统指标。RSS 相加会重复计算共享页，进程树结果也可能遗漏已脱离父进程的工具。优先同时保留分项和 PSS／USS，并明确单位。Linux 私有页与 Windows Private Bytes 含义不同，跨操作系统不直接相减。

容器中的 `os.Getpid()` 与挂载的 `/proc` 可能属于不同 PID 命名空间。先核对 `/proc/self/status` 的 Pid／NSpid 与采样器视图，再关联后代；无法识别主进程时标记 **unavailable**，不得记为 0 或换一个同名进程。

真实模型延迟、飞书网络、工具执行和 CLI 初始化应分别记录。本地 fixture 只提供确定性回复，合成 token 不代表真实成本或上下文规模。测量工具不加入生产服务的定时循环。

## 可执行资源矩阵

```sh
./bin/benchmark-native --claude /absolute/path/to/claude --samples 3 --warm-turns 2 --idle-ms 1000 --scenario matrix
```

矩阵依次运行无会话、1 个热会话、4 个同时保留的热会话、20 KiB／100 KiB 中文回答的快／慢卡片更新、1 MiB 入站附件和完整回答导出、空闲回收后恢复原生会话 ID。长文本按 **UTF-8 字节** 构造；慢卡片场景使用本地替身延迟，不代表真实飞书网络。多会话逐个完成冷／热轮次后同时保留进程，用于稳定的容量比较，不是并发吞吐测试。

单独定位场景：

```sh
./bin/benchmark-native --samples 1 --warm-turns 0 --sessions 0 --idle-ms 100
./bin/benchmark-native --samples 3 --sessions 4 --idle-ms 1000
./bin/benchmark-native --samples 3 --text-bytes 102400 --update-ms 20 --transport-ms 100 --attachments --reclaim
```

| 参数 | 范围（默认） |
|---|---|
| `--samples` | 1–10（3） |
| `--warm-turns` | 0–5（2） |
| `--idle-ms` | 100–10000（1000） |
| `--scenario` | `warm`（默认）或 `matrix` |
| `--sessions` | 0–8（1） |
| `--text-bytes` | 0–102400（0 使用短回复） |
| `--update-ms` | 20–5000（1000） |
| `--transport-ms` | 0–1000（0） |

20 ms 更新间隔是基准驱动的压力参数；生产配置 `limits.updateMs` 仍要求至少 250 ms。`--reclaim` 使用网关已有 `Sweep`，确认原进程释放后再发消息；网关会话 ID 和原生 ID 必须同时保持，恢复失败不会新建会话。没有会话时不应产生 CLI 或模型请求。

JSON `schema: 3` 保留每阶段进程树、按 100 ms 间隔采样的完整进程树峰值及采集开销、宿主／原生 CLI／其余工具或 MCP 后代分项、每轮首字／原生结果／模拟交付耗时、卡片创建与更新次数、序列化卡片字节、入站与导出文件字节、进程级 Go 分配，以及下节的工作量和趋势。采样峰值取同一次完整快照，不把不同时刻各分项的最大值拼成峰值；100 ms 之间的瞬时峰值仍可能漏测。宿主分项包含网关、SQLite、采样器和本地服务；工具与 MCP 无法仅靠 PID 可靠区分，因此合并保留，不能把全部后代记成 Claude 本体。

渲染细项使用生产渲染路径的 Go 分配基准：

```sh
go test ./internal/gateway -run '^$' -bench 'BenchmarkRender|BenchmarkReplyCard' -benchmem
```

### 并发与持续负载

`--concurrent` 让不同聊天同时提交并等待原生轮次，每个聊天内部仍串行。`--soak-seconds` 在冷启动及热身完成后继续提交，观察 SQLite／WAL、goroutine、进程内存与维护耗时是否随轮次积累。两者都只支持 `warm` 场景且要求 `--sessions` 大于 0，不改变默认资源矩阵的顺序和口径。

```sh
./bin/benchmark-native --claude /absolute/path/to/claude --samples 1 \
  --scenario warm --sessions 4 --warm-turns 2 --concurrent \
  --soak-seconds 600 --max-turns 10000 --sample-ms 1000 \
  --idle-ms 1000 --reclaim > benchmark-soak.json
```

| 参数 | 范围（默认） | 说明 |
|---|---|---|
| `--soak-seconds` | 0–3600（0） | 持续阶段时长；先达到时长或轮次上限即停止新提交 |
| `--max-turns` | 1–20000（1000） | 含冷启动、热身及可选恢复轮次；必须容纳全部初始轮次 |
| `--turn-interval-ms` | 0–60000（0） | 每个生产者的持续轮次结束后等待该间隔（不超过剩余时长），让长时间观察覆盖实际时长而不提前耗尽轮次预算 |
| `--sample-ms` | 100–10000（1000） | 趋势采样间隔；采集本身有开销，比较中应固定 |

每个已提交轮次最多有 45 秒有界交付等待，随后必须确认退出。并发或持续负载模式每 5 秒通过已有 `Sweep` 记录一次维护，不修改生产服务周期。

- `workload` 记录 `attemptedTurns`／`completedTurns`、同时等待轮次峰值 `peakInFlightTurns`、持续阶段实际时长 `soakElapsedMs` 与 `stopReason`（`planned_turns`、`duration`、`turn_limit`、`turn_failed`）。范围是初始轮次及持续阶段，可选回收恢复单独列在 `turns` 中。`completedMockTurnsPerSecond` 是本地确定性模型和模拟交付下的观测量，不是真实模型吞吐。
- `trends` 的每个点记录数据库／WAL 文件字节、goroutine、Go 当前堆 `goHeapBytes`／占用页 `goHeapInUseBytes`、活跃进程、在途轮次、内存聚合与维护候选／变更／耗时。数据目录总字节 `dataDirectoryBytes` 与常规文件数 `dataDirectoryFiles` 最多每 10 秒扫描一次，以 `dataDirectoryObservedMs` 标明观察时间，不跟随符号链接；目录包括 fixture 的数据库、原生历史、附件和导出。不可观测项为 `null`，尚未发生的维护不记为零耗时，不存在的 WAL 记为实际零字节。
- 运行期间完整趋势、轮次与维护事件持续写入临时目录的 `observations.jsonl`，路径打印在 stderr。最终 JSON 保留初始采样、最近最多 2000 个趋势点和丢弃点数 `droppedEarlierPoints`；成功后清理临时目录，需要全量记录时应在运行中另存 JSONL。失败时保留数据库、JSONL 与 `benchmark-evidence.json`，并在结果中给出 `evidenceDirectory`。进程退出未确认会停止后续样本，不把残留进程带进下一组。

## CI 与 endurance 观察

两个工作流记录的是对应提交的短时或限时观察，工作流定义见[平台 CI](validation.md#平台-ci)。它们用于发现回归与核对机制，不承诺性能比例；正式比较仍需固定机器、多样本和相同输入。

| 来源 | 参数 | 保留 |
|---|---|---|
| `Verify Go gateway`，Ubuntu／Windows Go 1.27 | 资源矩阵 `--samples 1 --warm-turns 0 --idle-ms 100 --scenario matrix`；并发检查 `--sessions 2 --warm-turns 1 --concurrent --soak-seconds 6 --max-turns 1000 --sample-ms 500 --idle-ms 100`，要求重叠轮次且全部完成 | artifact `native-resource-observations-<OS>-<ARCH>`，7 天 |
| `Verify Go gateway`，Ubuntu Go 1.27 | 渲染分配基准 `-benchtime=3x -count=3`，只打印到日志 | 运行日志 |
| `Native endurance observations`，Linux x64／Windows x64 | 见下文 | artifact `native-endurance-<OS>-<ARCH>`，14 天 |

Endurance 每周日 02:00 UTC 在默认分支运行 30 分钟，手动可选 600／1800／3600 秒；修改该工作流或 `cmd/benchmark-native/**` 的 PR 只做 30 秒冒烟。先运行共用验证 action，再以 `--scenario warm --sessions 4 --warm-turns 1 --concurrent --turn-interval-ms 1000 --max-turns 20000 --sample-ms 2000 --idle-ms 100 --reclaim --attachments --text-bytes 4096 --update-ms 250 --transport-ms 10` 运行，外层进程监督器截止为时长加 120 秒、回收预算 35 秒，job 上限 100 分钟。以下任一不成立即失败：样本 `passed`、`cleanupConfirmed`、`reclaimConfirmed`、`sessionStable`、`stopReason` 为 `duration`、`soakElapsedMs` 达到设定时长、`peakInFlightTurns` 至少 2、完成轮数等于尝试轮数。内存和磁盘增长只记录为观测，不设未经校准的阈值，也不把模拟服务表现等同于真实模型／飞书容量。同一事件类型和 ref 的新运行会取消进行中的旧运行，定时与手动运行互不取消。长时间配置须以实际完成的 workflow run 为证据。

## 历史观察

以下是带日期和源修订的历史证据，保留原样用于对照，不代表当前或后续提交；它们使用当时的工具版本，与当前 `schema: 3`／`deterministic-text-v2` 的输出不能直接比较。

### 2026-11-04 Go 迁移的本地观察

相同 Linux 主机（原始数据记录的可用逻辑 CPU 数为 TypeScript 8、Go 9）、Claude 2.1.316、`deterministic-text-v1`、一个暖会话，双方各 3 个样本、9 轮全部成功。下面是闲置 1000 ms 后快照的中位数，单位 MiB：

| 项目 | TypeScript v0.2.0 | Go 重构 |
|---|---:|---:|
| 宿主（含 fixture／数据库） | 50.44 | 18.20 |
| 可观察子进程 | 279.32 | 200.10 |
| 完整进程树 | 326.86 | 217.94 |
| 子进程数量 | 2 | 1 |

该样本完整进程树 RSS 减少约 **33.3%**。分项中位数之和不一定等于逐次总量的中位数。Go 宿主减少约 63.9%，原生 Claude 仍占主要部分；不能把宿主比例用于整套服务。

[完整原始数据](benchmarks/go-migration-baseline.json)保留样本、耗时、版本、结果与统计限制。TypeScript 基线提交为 `26c4858b9460739e0ccb8712932224609ce22e47`，使用 Node 24.22.1；[TypeScript 采样器补丁](benchmarks/typescript-proc-sampler.patch)只校正容器 PID 命名空间与采集方法，没有修改旧版生产逻辑。Go 数据使用 Go 1.27.3，取自 Go 迁移变更（`c0b780c`）合入前、最终生命周期审查之前的工作树，提供迁移方向证据，不是后续提交的精确基准。

本地飞书传输为内存替身，没有真实 WS、真实模型、插件、MCP 或多会话负载。运行时另有后台构建任务，因此原始数据中的延迟只作描述，不是隔离后的加速结论。真实部署应按上文口径再测。

### 2026-11-05 的 CI 观察

以下来自提交 `9eb43e8ea39676513290b0d9e4b920d1dae98385` 的 [Linux amd64 CI 日志](https://github.com/sean2077/feishu-bot/actions/runs/41988792550/job/127115808957)：Go 1.27.3、Claude Code 2.1.316、4 个逻辑 CPU。它是该提交的限定测试证据，不代表后续提交，也没有与 cc-connect 在同机、同配置下比较。

生产文本累积路径与测试中保留的旧算法，在同一次任务内使用相同的小块中文输入；每组执行 3 次，重复 3 组。基准以 24 字节增量块补齐最后一块，两个标称规模的实际文本分别为 20,016／100,008 字节。下面是各组观察到的范围，`B/op` 是每次操作分配的字节数，不是常驻内存或完整服务峰值：

| 基准标称 UTF-8 字节数 | 旧算法 B/op | 缓冲实现 B/op | 旧算法 allocs/op | 缓冲实现 allocs/op |
|---:|---:|---:|---:|---:|
| 20,000 | 8,802,744–8,804,608 | 85,336 | 833–836 | 26 |
| 100,000 | 405,976,296–405,976,818 | 515,416 | 5,838–5,843 | 32 |

这说明反复拼接和扫描已有文本的分配开销已经减少；不据此推断模型耗时、飞书网络、整套服务 RSS 或其他工具的性能。

同一任务的资源矩阵使用 `--samples 1 --warm-turns 0 --idle-ms 100`，9 个场景全部通过，进程树观察全部为 `observed`。其中 0／1／4 会话在空闲 100 ms 后的快照如下，单位均为字节：

| 已完成首轮并保留的会话 | 宿主 RSS（含 fixture／SQLite／采样器） | 原生 CLI RSS 合计 | 宿主私有页 |
|---:|---:|---:|---:|
| 0 | 20,078,592 | 0 | 18,722,816 |
| 1 | 21,262,336 | 220,418,048 | 19,595,264 |
| 4 | 21,729,280 | 866,336,768 | 20,062,208 |

这次矩阵没有测热续轮延迟，四会话也不是并发吞吐测试。100 KiB 中文快／慢场景均保持一张回复卡，并各导出完整 102,400 字节文件；附件场景确认 1,048,576 字节入站和 102,400 字节导出；回收后保持网关与原生会话 ID。未运行外部工具或 MCP 子进程，因此该分项为 0。单场景仅一个样本，RSS 合计还会重复共享页，不能据此给出生产容量、跨系统差值或性能承诺。
