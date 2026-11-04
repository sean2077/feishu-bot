# 验证与验收

测试、平台 CI 和实际飞书／模型验收是不同证据。工作流存在不代表已经通过；本地模拟成功也不代表手机、桌面和真实模型均可用。

## Go 检查

```sh
go test -race ./...
go vet ./...
go build -trimpath -o ./bin/feishu-bot ./cmd/feishu-bot
```

Go 1.25+ 可构建；CI 配置 Ubuntu／Windows 与 Go 1.25／1.27。生产构建支持 `CGO_ENABLED=0`，竞态检测使用 runner 的 C 工具链。Go 测试使用临时 SQLite／目录、虚构身份、本地 HTTP／WebSocket 和可控 Worker，不读取部署配置或调用真实服务。

测试与被测包同目录，使用 `*_test.go`；平台专用测试使用 `_windows_test.go` 等文件名或构建约束。原生协议 fixture 位于 `internal/native`、`internal/integration` 和 `cmd/benchmark-native`。当前 CI 不设覆盖率门槛；新增行为应覆盖可观察的契约结果，而不是追求覆盖率数字。

| 范围 | 检查重点 |
|---|---|
| config／cli | 严格字段与限制、默认值、配置路径、无覆盖 init、离线 doctor、显式探针、关闭顺序 |
| store／workspace | 旧 schema、原生 ID、不可跨聊天 resume、launch identity、未知写入恢复、路径与快照 |
| gateway | 授权及零副作用、忙时输入／补充／compact、审批、历史范围、图片批次、完整交付、会话控制 |
| feishu | 原始事件、明确 @、CardKit 序号、同卡更新、反应所有权、有界文件与可取消连接 |
| native | stream-json、公开内容归属、权限／MCP 控制、原生配置、热续轮、停止与恢复 |
| daemon | install/uninstall/start/stop/restart/status/logs；env 文件、二进制复制、离线预检、幂等、同名任务所有权、stale lock 恢复、日志轮转、任务动作无脚本 |
| integration | 真实 Claude + Gateway + SQLite 的审批写入、追问、完整交付、空闲回收与重启续接 |

新增或修改具体行为时，使用能观察外部结果或状态不变量的测试，不以“调用了 stop”代替进程已退出、不以“发送函数返回”代替完整交付、不以“模型说记得”代替相同 session ID。

## 真实 Claude 协议 fixture

协议测试使用原生 **Claude Code 2.1.316** 和仅监听回环地址的确定性模型服务；不使用真实模型凭据，不连接真实飞书。指向已安装的固定版本 CLI：

```sh
CLAUDE_NATIVE_PATH=/absolute/path/to/claude go test -race -count=1 -timeout 180s -v ./internal/native ./internal/integration ./cmd/benchmark-native
```

PowerShell：

```powershell
$env:CLAUDE_NATIVE_PATH = "C:/tools/claude.exe"
go test -race -count=1 -timeout 180s -v ./internal/native ./internal/integration ./cmd/benchmark-native
```

没有设置变量时，真实 CLI 用例明确 skip，普通单元测试继续运行。CI 在单独测试步骤中安装固定 SDK 包所携带的原生 CLI；这是获取 fixture 的构建依赖，生产服务不运行该 SDK。不得将 skip 写为真实协议已验证。

组合回归通过网关实际审批卡和 `/answer` 完成原生 Write／AskUserQuestion，核对临时文件内容与超长回复导出，确认文件交付回执之前没有 Done。随后触发空闲回收、停止并重开数据库，检查三代原生进程保持同一会话 ID 和历史。测试中的飞书传输和模型仍为本地替身。

原生协议是版本敏感边界。更改 CLI 版本、控制帧、环境继承或工具设置时，应验证初始化、连续两轮、原生 ID、权限／追问控制、MCP、compact 和停止后 resume，并记录实际覆盖范围。测试中的合成 token 只检查采集／去重，不是账单。

## 飞书与模型真实验收

在独立应用、授权测试群及私聊中执行。至少准备维护者、普通获准成员和未授权成员，记录 OS、网关／Claude／客户端版本、配置摘要及结果；凭据留在运行主机，避免保存完整原始事件。

1. **入口与权限。**分别测试明确 @、无 @ 引用、@全体、未授权作者、未授权群和私聊。拒绝应无正文入库、下载、回执或模型调用。测试 text、post、image、file、引用和线程；不完整输入明确拒绝。
2. **连续对话与补充。**同群不同作者／线程保持同一会话；普通忙时输入进入原生流。`/btw`／`/ps` 不启动空闲任务，字面 slash 正文不变成管理命令。目标结束或 compact 等待时不越过边界。
3. **审批与追问。**普通成员不能批准共享外部写操作；维护者可批准／拒绝。多选可取消、空提交后可补齐、单选可再次选择；状态按操作者分离。过期或旧按钮不能复活请求，`/answer` 保留恢复入口。
4. **流式与完整交付。**同卡显示公开思考／答案，隐藏工具和签名；最终折叠思考。超长回复或必要附件未完成时没有 Done。人工核对未知上传结果后，才显式使用 `/retry-output`；不得通过盲目重发制造验收。
5. **图片。**默认逐条；开启窗口后检验相同作者／线程／引用的连续独立图片合批。每条群图片都必须独立明确 @。数量、单文件、总量或下载失败时整批取消。
6. **停止与恢复。**任务、审批、图片窗口、慢下载和最终交付时分别测试 `/stop`、重启；检查未知外部写入不重放。停止／回收／恢复保留原生 ID，仅 `/new` 新建；改变 cwd／配置身份会拒绝旧会话。
7. **用量。**无结果时未知；连续轮次、工具轮次、重启和重复结果不重复累计。查询不调用模型，不混入其他聊天、子代理或累计 modelUsage。
8. **继承与后台。**先用无副作用 hook／技能／MCP 验证配置来源，再测试实际需要的原生后台能力。只清理本次创建的测试任务，记录工具返回的 ID。后台输出归属不能借用随后出现的前台输入。
9. **服务生命周期。** Windows 登录任务直接执行安装目录中的二进制，不出现脚本或控制台窗口；`daemon status` 报告 PID/配置/日志；`daemon logs -f` 能跟上轮转；`daemon stop` 优雅退出并释放 `gateway.lock`；`daemon start`／`daemon restart` 后 `/status` 仍是同一 session ID 和原生 session ID。未经核实不要删除 stale lock；显式 `daemon start --reclaim-stale-lock` 只清理已无进程持有的锁。`daemon uninstall` 删除任务/unit 但保留 config、service.env、日志和 state。

手机和桌面分别记录。模型流式、多模态、工具和长上下文能力分别验收；基础 Messages 探针成功不替代这些结果。共享 cwd 的原生持久定时任务仍遵循 Claude 的项目级规则，不能宣称跨聊天隔离。

## 性能与回归边界

Go 迁移移除了每会话 Node worker，但没有预设内存降幅。比较需要固定原生 CLI、输入、模型 fixture、暖会话数和后台负载，并区分网关与完整进程树。详见[运行基线](runtime-baseline.md)。历史 TypeScript 测试的通过记录是迁移参照，不计作 Go 回归覆盖；旧报告保留在历史迭代文档和 Git 历史中。
