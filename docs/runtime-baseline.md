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

`--sessions` 为 0–8，`--text-bytes` 最大 102400，`--update-ms` 为 20–5000，`--transport-ms` 为 0–1000。20 ms 是基准驱动的压力测试参数；生产配置 `limits.updateMs` 仍要求至少 250 ms。`--reclaim` 使用网关已有 `Sweep`，确认原进程释放后再发消息；网关会话 ID 和原生 ID 必须同时保持，恢复失败不会新建会话。没有会话时不应产生 CLI 或模型请求。

JSON schema 3 保留每阶段进程树、100 ms 间隔观察到的完整进程树采样峰值及采集开销、宿主／原生 CLI／其余工具或 MCP 后代分项、每轮首字／原生结果／模拟交付耗时、卡片创建与更新次数、序列化卡片字节、入站文件与导出文件字节，以及进程级 Go 分配，并新增下面的工作量和连续趋势。采样峰值保留同一次完整快照，不把不同时刻各分项的最大值拼成一个峰值；100 ms 之间的瞬时峰值仍可能漏测。宿主分项包含网关、SQLite、采样器和本地服务；任意工具与 MCP 无法仅靠 PID 可靠区分，因此合并保留，不能把全部后代都记成 Claude 本体。Linux 私有页与 Windows Private Bytes 含义不同，跨操作系统不直接相减。

### 并发与持续负载

`--concurrent` 让不同聊天同时提交并等待原生轮次，每个聊天内部仍串行。`--soak-seconds` 在冷启动及热身完成后继续提交，观察 SQLite／WAL、goroutine、进程内存与维护耗时是否随轮次积累；它只支持 `warm` 场景，不改变默认资源矩阵的顺序和比较口径。

```sh
./bin/benchmark-native --claude /absolute/path/to/claude --samples 1 \
  --scenario warm --sessions 4 --warm-turns 2 --concurrent \
  --soak-seconds 600 --max-turns 10000 --sample-ms 1000 \
  --idle-ms 1000 --reclaim > benchmark-soak.json
```

`--soak-seconds` 为 0–3600，`--max-turns` 为 1–20000，包含冷启动、热身以及可选恢复轮次；先达到时长或轮次上限便停止新提交。每个已提交轮次仍有最长 45 秒的有界交付等待，随后必须确认退出。`--sample-ms` 为 100–10000；采集器本身有开销，应在比较中固定。持续负载模式每 5 秒通过已有 `Sweep` 记录一次维护，不修改生产服务周期。

`workload` 记录尝试／完成轮数、同时等待轮次峰值、持续阶段实际时长与停止原因，范围为初始轮次及持续阶段，可选回收恢复单独列在 `turns` 中。`completedMockTurnsPerSecond` 是本地确定性模型和模拟交付下的观测量，不能当作真实模型吞吐。`trends` 记录数据库／WAL 文件字节、goroutine、活跃进程、在途轮次、内存聚合与维护候选／变更／耗时；不可观测项为 `null`，尚未发生的维护也不记为零耗时，不存在的 WAL 则为实际零文件字节。

运行时会把完整趋势、轮次与维护事件持续写入临时目录的 `observations.jsonl`，路径在 stderr 显示。最终 JSON 保留最初采样、最近最多 2000 个趋势点和丢弃点数；成功后清理临时目录，需保留超过窗口的全量记录时应在运行中另存 JSONL。失败保留数据库、JSONL 与 `benchmark-evidence.json`，并在结果中给出 `evidenceDirectory`。进程退出未确认会停止后续样本，不把残留进程带进下一组。

渲染细项使用生产渲染路径的 Go 分配基准：

```sh
go test ./internal/gateway -run '^$' -bench 'BenchmarkRender|BenchmarkReplyCard' -benchmem
```

CI 在 Linux／Windows 的 Go 1.27 记录资源矩阵，并额外运行 2 个聊天、6 秒持续阶段的并发检查；带平台后缀的 `native-resource-observations-*` artifact 同时保留矩阵与并发 JSON。Linux 另记录三组、每组重复三次的有界渲染分配观察。短时 CI 观察用于发现回归与核对机制，不代表上面的 10 分钟命令已执行，也不用于承诺性能比例；正式比较仍需固定机器、多样本和相同输入。

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

## 原生版本与实际产物

普通 CI 使用固定 Claude Code 2.1.316。`Verify Go gateway` 的手动运行可同时指定 `candidate_sdk_version` 与 `candidate_native_version`，安装官方测试包、核对实际 `--version` 身份，再运行权限／追问、持续输入、MCP、compact、停止、恢复和长文本附件回归。候选成功不会自动改写仓库已测版本。

CI 给无 CGO 生产二进制写入版本并绑定准确提交、BUILDINFO 与 SHA-256，直接运行基本命令、完整本地 TLS／WebSocket／REST／原生 CLI 旅程和固定旧二进制升级恢复。发布工作流对解包及安装后的最终文件重复同一旅程；Windows／macOS 假 CA 仅在隔离 runner 明确启用、按指纹清理，生产路径保持系统证书校验。具体步骤与限制由[实际构建产物回归](validation.md#实际构建产物回归)维护；这些 fixture 不是实际飞书或真实模型验收。
