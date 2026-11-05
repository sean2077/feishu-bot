# 运行诊断

前台 `run` 向 stderr 输出逐行 JSON；`daemon` 服务通过 `--log-file` 写入文件（默认 `<install-dir>/logs/service.log`，10 MB × 3），包含 UTC 时间和事件名。配置凭据、模型认证头和受控操作密钥通过统一 logger 脱敏。不要将 debug HTTP 转储、完整原始消息、模型请求或环境变量加入公共日志。

## 服务状态与日志

- `feishu-bot daemon status` 显示 Running/Stopped/Stopping/Stale lock、PID、配置、环境文件、日志和工作目录；`--json` 供脚本读取。
- `feishu-bot daemon logs -n 100` 读取末尾日志，`-f` 跟随并在轮转后重新打开当前文件。
- `~/.feishu-bot/daemon.json` 只保存绝对路径、平台、版本和安装时间，不保存密钥；`FEISHU_BOT_HOME` 可覆盖元数据目录。
- `daemon stop` 写入 `<dataDir>/gateway.stop`，等待 `gateway.lock` 释放和进程退出。不要用 `schtasks /End`、`Stop-Process` 或重命名锁来“加速”停止。
- 未确认关闭会保留 `gateway.lock`；`daemon status` 报告 stale lock。核实本实例 PID 及派生进程均已退出后，才使用 `daemon start --reclaim-stale-lock`。该命令只删除已无进程持有的锁，不杀进程、不重放外部写操作。
- Windows 注销/关机可能直接终止任务进程并留下锁；下次登录后用同一显式恢复流程处理。

## 飞书连接

适配层记录连接、重连、握手／心跳失败、回调处理及拒绝事件。API 错误保留安全的操作阶段、HTTP 状态和数值平台错误码，不输出提供方原始正文、请求头或 WebSocket URL。超时不能证明远端没有执行写操作。

长连接由 context 管理，关闭时终止 socket、心跳和重连等待。服务器返回的 ping／重连设置在有界范围内使用；失去入站活动后关闭旧连接并重新取得 endpoint。重连不自动重新执行未知模型输入、卡片创建或文件上传。

普通消息可达但卡片无响应时，分别检查平台回调订阅、客户端缓存、投递去重、请求是否仍有效、卡片和操作者归属。不能单凭没有回复推断平台未投递。先用授权测试账号验证，避免通过放宽权限解决诊断问题。

## 原生会话

`worker_ready` 等事件用于观察初始化和进程生命周期，不是用户首字延迟。原生恢复失败保留原 session ID；先检查 CLI 版本、cwd、配置身份和历史目录，不通过删除状态来掩盖故障。

`shutdown_unconfirmed` 或 `worker_stop_unconfirmed` 表示尚不能证明本实例的原生进程已退出。服务保留 `gateway.lock` 或会话名额，防止并行接管。核实派生进程后再处置，不能仅检查主 PID。

## 交付

`/outputs` 查询持久化未确认交付，`/retry-output <ID>` 是明确补发入口。已知消息 ID 更新可以重试，创建和上传结果未知必须先核对远端。表情失败不重做模型任务；必要答案或文件未交付不标 Done。

`doctor` 默认只作本地检查。`--probe-feishu` 检查应用认证和机器人身份；`--probe` 可能计费，仅检查基础 Messages 响应。它们不修改应用权限，不验证实际卡片、文件、手机／桌面体验或工具安全性。

## 分阶段 doctor 报告

`doctor` 的 `native` 同时给出 `actual`、`tested` 和 `status`。当前协议回归固定在 Claude Code **2.1.316**；匹配时为 `tested`，其他可识别原生版本为 `untested`，不能识别为 `unavailable`。`tested` 只表示版本与回归基准一致，`verification.nativeProtocol` 仍是 `not_checked`；本机的 `--version` 没有运行权限、追问、MCP、compact 或恢复测试。未测试版本不会仅因版本不同而阻止启动。

每个失败检查带 `remedy`，`nextSteps` 按当前结果提示下一步。`verification` 分开记录配置、可执行文件、基础模型响应、模型流式、模型工具、飞书认证、身份、消息、回调、文件和重连，不能把任意一个 `passed` 推广成完整验收。`ready` 只表示本次请求的检查全部通过。

`storage` 是磁盘占用观察：数据库、WAL、SHM 的字节数，以及 `exports` 的文件数和总字节数。它只读取文件元数据，不打开生产 SQLite、不做 checkpoint、不扫描聊天内容、不删除文件。导出目录最多扫描 10,000 项、不跟随符号链接；遇到权限、上限或不完整扫描时标明 `partial`／`unavailable`。这些数字不能用于证明进程 RSS 泄漏；数据库行数明确保持 `not_checked`。服务运行期间文件大小可能变化，应把报告视为一次快照。

| 现象 | 下一步 |
|---|---|
| 原生版本 `untested` | 对该候选版本运行协议测试；保留原生历史，勿以新建会话掩盖恢复失败 |
| `feishuAuth` 失败 | 检查应用 ID、凭据变量、应用可用性和主机网络；不要输出 secret |
| 认证成功、身份失败 | 检查机器人功能和身份接口；还不能判断事件订阅是否完整 |
| 基础模型成功、工具／流式未测 | 继续做原生 CLI 的本地模型回归及测试群验收 |
| 模型已结束、文件仍待交付 | 查看 `/outputs`；先确认交付，不能重复模型任务来补文件 |
| 平台暂时拒绝 | 查看安全的错误阶段、HTTP 状态、平台码及请求标识，等待有界重试结果 |
| 写入结果未知 | 先检查远端卡片／文件，再显式补发；不能把超时当作未执行 |

`doctor --verify-chat <配置中的群ID>` 或 `doctor --verify-chat dm:<配置中的用户ID>` 是单独的显式联网验收入口。先停止本机服务，命令会占用同一服务锁避免重复接收事件，使用私有临时数据库；按终端提示发送随机挑战消息，再确认测试卡片和文件。它不调用模型、不修改绑定，最多等待两分钟。只接受所选聊天的获准作者、当前卡片 ID 和有效随机值；退出后更新卡片使按钮失效。文件打开结果记为 `confirmed_by_user`，与平台消息接口确认分开。原生工具、模型、重连和原会话恢复仍保持独立验收状态。详见[首次接入](first-connection.md)。
