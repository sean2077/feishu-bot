# 迭代状态

只记录可复现问题、验证范围和下一项验收；不把待部署检查写成线上故障。

| 项目 | 当前结果 | 证据 / 下一步 |
|---|---|---|
| 首次启动诊断 | 已修复 | 模板、无聊天绑定和不完整参数被拒绝；数据目录错误聚合，临时探针清理，配置错误不回显原文。 |
| Windows 显式 Git Bash 路径误报 | 已修复（Windows 真实可执行文件与跨平台 CI） | `doctor` 原先只检查指定文件存在，可能把 `node.exe` 误判为 Git Bash。现要求 `bash.exe` 并实际执行 `--version`，拒绝不能启动或不返回 GNU bash 版本的路径；仅检查显式配置，不改写用户设置，也不代表真实 CC 工具联调。 |
| 超长答案完整交付 | 已修复 | 文件与卡片共同持久化；文件未送达不标 Done；重启可补发，不重做模型或业务动作。 |
| 已完成导出文件清理 | 已修复（平台 CI 已验证） | 完成超过 7 天的回复与主动文件快照在现有 housekeeping 中限量清理；只有确认送达且路径处于受管 exports 内才删除文件。清理失败保留数据库关联，未确认文件与受管目录外文件不删除。历史上已失去数据库关联的旧文件不盲删。 |
| 终止交付的磁盘保留 | 已修复（平台 CI 已验证） | 取消且超过 7 天的受管快照与数据库行一并清理；未知上传继续保留，受管目录外仍禁止删除。主动 `send_file` 写入快照但尚未持久登记前若失败，清理无归属的暂存文件。 |
| Agent 主动文件交付 | 已修复（平台 CI 已验证） | `send_file` 创建有归属的持久文件交付记录；未知上传结果只允许明确补发，待交付文件阻止同原生会话的完成表情。文件快照不依赖原工作区后续改动。 |
| Windows 异常父进程退出 | 已修复（平台 CI 已验证） | Windows 上 detached 子孙在根进程退出后存活，旧清理路径遗漏；现在按原根进程的创建/退出时间、Win32_Process 父子关系及实际创建时间定位并终止存活后代，异常退出完成清理后再释放会话。中间祖先也已退出的断链情形不能靠单次快照可靠追溯。 |
| 启动期间停止竞态 | 已修复（平台 CI 已验证） | stop 与异步 action relay 初始化重叠时不再启动 CC 或遗留监听；Feishu 连接成功晚于 shutdown 时会再次断开，不误报 ready；启动恢复完成前不关闭数据库。真实飞书连接尚未联调。 |
| 项目操作异常退出清理 | 已修复（平台 CI 已验证） | 外部项目操作 launcher 退出但后代存活且继续持有 stdout 时，取消指令原本在 Windows 遗漏清理；已给操作进程登记出生与退出时间，复用已有 CIM 归属检查清理可追溯后代。无常驻轮询、不按进程名终止；中间祖先丢失仍无法安全归属。 |
| Windows 原生权限与停止 | 已验证 | 工作区路径规范化后，原有 acceptEdits 无额外审批断言通过；三代真实进程的 PID 退出与心跳停止也已在 Windows CI 通过。未放宽权限。 |
| 恢复提醒事务与首次投递前崩溃 | 已修复（SQLite 故障注入／平台 CI） | 旧审批过期、输入标记未确认和恢复提醒持久登记位于同一事务；提醒已提交但尚未开始外部创建时，下次启动可安全投递。发送已开始而结果未知仍保持 `uncertain`，不自动重发。不新建原生会话；真实飞书待验收。 |
| 重启恢复提醒的投递丢失 | 已修复（真实 SQLite／模拟飞书／跨平台 CI） | 审批／追问失效和未确认输入的合并提醒在调用飞书前先登记 Outbox；消息创建结果未知时保留 `uncertain`，不自动重复外部写入；当前聊天有权成员可通过 `/outputs` 查看、维护者通过 `/retry-output` 显式补发。不会重启旧审批、创建新 CC 会话或改变授权。真实飞书服务的实际投递语义仍待验收。 |
| 意外重启的审批／追问回执 | 已修复（平台 CI 已验证） | 重启前仍为 pending 的请求在恢复时过期；如果没有挂起输入，原先不会提示。新增按当前聊天授权范围的一次性告知，并与同房间未确认输入合并，不恢复旧审批或重新执行外部操作。 |
| 运行中原生 worker 意外退出告知 | 已修复（平台 CI 已验证） | 原生进程异常退出且仍有未完成输入、审批或后台活动时，在当前授权聊天告知一次；审批取消、输入转 uncertain、执行结果不重放，不改变原会话。正常停止和闲置回收不额外提醒。 |
| 空闲停止失败时的会话与进程名额 | 已修复（模拟停止失败与平台 CI） | `stop()` 抛错时不再释放未确认退出的 Worker 名额；后续聊天不能偷偷启动第二原生进程，`/status` 显示 `termination_unconfirmed`，`/new` 和 `/resume` 不再在失败时切换历史。真实 Windows CIM 超时下仍待验证；整体服务关闭的错误上报见 PR #19。 |
| 关闭时等待原生控制回复 | 已修复（故障注入与平台 CI 已验证） | 完全关闭不再等待 `interrupt()` 回执才调用 `close()`；主机输入 EOF 或重复 stop 时直接清理传输，保留取消待批请求、关闭 relay 与会话 ID 的既有行为。注入 SDK 不响应的用例不是实际 CC 故障复现。 |
| 服务关闭失败与数据锁 | 已修复（故障注入与平台 CI 已验证） | 活跃或初始化中的 Worker 停止失败，在其余清理收尾后明确报错；CLI 设置非零退出码并保留原数据锁，不允许第二实例直接接管。正常关闭仍释放锁。未改变原生进程终止算法，不能据此保证全部后代已退出。 |
| 异常退出后代清理失败 | 已修复（故障注入与跨平台 CI 已验证） | `killTree()` 失败不再发可安全重用的普通退出事件；保留会话进程名额、取消旧审批、把未确认输入标记为 uncertain，并禁止替代 Worker 和会话切换。初始化失败且未取得 Worker 句柄也保持锁定。真实 CIM 失败、断链后代和 Windows 11 本机仍待验证。 |
| 项目操作停止失败未被上报 | 已修复（故障注入与平台 CI 已验证） | 项目操作取消、超时或输出超限时，只有 `killTree()` 确认完成且子进程输出关闭才报告常规终止；清理报错或 5 秒收尾期限仍未确认时改报 `process termination unconfirmed`。`action_runs` 保持 `uncertain` 并有独立审计；不会自动重试外部操作。真实 Windows CIM 与断链归属仍待验证。 |
| 已确认 Done 的回执回滚 | 已修复（模拟飞书 API 与跨平台 CI） | 旧清除路径只清理 OnIt；当原 Done API 已确认但目标回执随后转 clear 时，按本应用归属查找并删除 Done reaction_id，其他应用不受影响。普通处理路径不增加 API 查询；真实飞书服务仍待验收。 |
| 结果未知的表情创建与恢复 | 已修复（模拟 API 与跨平台 CI） | 添加 OnIt／Done 前持久登记未确认尝试；响应丢失或重启后查询本应用的 reaction_id，存在时采纳，暂不可见时保持 `attempt:*` 并继续查询，绝不因一次空列表重做未知写入；后续 clear 可移除已找到的本应用 Done。成功路径不额外查询。真实 Feishu 一致性、未知写入的清理以及长期悬置状态仍待验收。 |
| 重启后未知 OnIt 的清理 | 已修复（真实 SQLite／模拟飞书／跨平台 CI） | `Receipts.recover()` 不再清除 `attempt:processing` 证据；恢复后第一次查询尚不可见时保持待确认，再由已有重试找到并删除本应用的 OnIt，不重复创建或触碰其他应用表情。真实 Feishu 列表可见性、长期悬置仍需联调。 |
| 持续失败的旧输出阻塞后续交付 | 已修复（SQLite／模拟飞书／跨平台 CI） | 旧 `Outbox.retryable()` 每次只选最早 10 条，失败但可安全重试的已知卡片更新会永久遮挡较新待发输出。现按每聊天 `(at,id)` 游标轮转、到末尾回绕，仍每轮最多 10 条；未知创建／上传仍不自动补发，文件交付屏障和当前授权检查不变。游标仅驻内存，真实 Feishu 延迟与失败率待测。 |
| 待确认表情阻塞后续回执重试 | 已修复（SQLite／模拟飞书／跨平台 CI） | 旧 `retry()` 每次只取最早 50 条，长期未确认写入可能使后续回执在重启后一直得不到重新核对。现按 `(at,id)` 游标轮转，每轮仍最多 50 条，保留未知结果且不盲重放外部操作；真实 Feishu 可见性仍待验收。 |
| 重启后已确认 OnIt 的清理 | 已修复（模拟飞书列表延迟与跨平台 CI） | `applied=processing` 的 OnIt 已获 API 确认但服务重启丢失内存 reaction_id 后，首次远端列表为空不再当作已清除；保留原回执，等待既有重试观察并删除本应用的表情。其他应用不受影响。真实飞书可见性及长期悬置待测。 |
| 处理中表情转完成时的清理阶段 | 已修复（故障注入与平台 CI 已验证） | `desired=done` 但旧 `OnIt` 仍为 `processing`／`attempt:processing` 时，先确认清理原表情，再持久登记真正的 `attempt:done` 并创建 Done；暂不可见或清理失败不能伪装成已经尝试 Done。外部未知写依旧不可盲重试，真实飞书可见性仍待验收。 |
| 未确认 Done 的撤销与可见性延迟 | 已修复（模拟列表延迟与跨平台 CI） | `attempt:done` 后转 clear 时，若首次列表未看到本应用 Done，不再错误记为 clear；保留待确认状态，后续既有 retry／重启可找到并删除自己的表情，不重复写入、不删除其他应用。真正长期不可见或远端删除结果未知时仍可能悬置，真实飞书待验证。 |
| 旧 CI 启动前失败 | 原因尚未证实 | 历史运行没有 runner/步骤；注释读取受工具限制。新运行已分配 runner，不通过猜测或重跑旧任务推断原因。 |
| Windows 孤儿进程清理偶发超时 | 未修复生产路径；CI 冷启动阶段已定位 | [PR #18 分阶段测量](https://github.com/sean2077/feishu-bot/actions/runs/41267952734) 显示首次 PowerShell 脚本执行前约 10.95–11.07 秒，CIM 查询约 0.68–1.61 秒，进程终止阶段远短于冷启动。限制测试文件并行度为 2 后，在[对照 CI](https://github.com/sean2077/feishu-bot/actions/runs/41268313285) 中启动前阶段约 3.42–4.19 秒；这是不同 runner 上的测试调度观察，不代表生产 Windows 11 的进程清理故障已经修复。PR #13/#14 的 WMI 替换未证明收益，保持未合入。 |

## 本轮验证

PR #34 复现恢复提醒跨事务崩溃窗口：只加回归的提交 `21fb11b` 在 [CI 41330752555](https://github.com/sean2077/feishu-bot/actions/runs/41330752555) 的 Ubuntu／Windows × Node.js 22.16／24 四组均实际失败：提醒写入故障后旧审批已被过期，且已保存但从未开始外部创建的提醒在下次恢复中不再投递。代码提交 `3dc5bef` 将审批／输入状态与当前授权聊天的恢复提醒统一在 SQLite 事务内登记，按 `kind=recovery` 区分可安全补发的首次外部尝试；已开始创建但响应未知仍只保留 `uncertain`，不自动重复。 [CI 41330967532](https://github.com/sean2077/feishu-bot/actions/runs/41330967532) 四组完整通过，各 134 项核心通过、1 项平台专用跳过、12 项原生子进程测试通过。新增故障注入使用真实 SQLite／Gateway、模拟飞书，实际飞书服务、真实异常重启时序及端到端 exactly-once 均未验收。最终文档版和合并后 CI 单独核验。

PR #33 发现异常重启时恢复提醒本身缺少持久交付保障：原待批请求已被 SQLite 标为失效，但 `notice()` 一次性创建消息的响应丢失后，后续启动既无法再次检测旧请求，也无待补发记录。仅新增回归的提交 `e89806e` 在 [CI 41325493187](https://github.com/sean2077/feishu-bot/actions/runs/41325493187) Ubuntu／Windows × Node.js 22.16／24 **四组均复现失败**，断言缺少持久化输出。最小修复提交 `c2b523c` 复用现有 Outbox，在向飞书发恢复卡前保存记录；未知外部创建保持 `uncertain`，仅允许原聊天有权操作显式补发，第二次恢复与 housekeeping 不会盲重试，模拟测试并验证跨聊天补发被拒绝、会话不切换及旧审批不执行。[代码与回归 CI 41325739581](https://github.com/sean2077/feishu-bot/actions/runs/41325739581) 四组均完整通过。使用真实 SQLite／网关代码、模拟飞书 API；真实飞书及 `Store.recover()` 后到 Outbox 登记前的进程崩溃窗口未验证，不宣称端到端 exactly-once。最终文档和合入 main CI 分别核验。

PR #32 针对旧卡片更新持续失败时的待交付输出饥饿：测试先构造 SQLite 中 12 条有已知 target 的待重试输出，前 10 条每次模拟更新失败，较新两条可以成功；只加回归的 `bd8dc879` 在 [CI 41319739331](https://github.com/sean2077/feishu-bot/actions/runs/41319739331) 的 Ubuntu／Windows × Node.js 22.16／24 四组均实际失败，较新两条一次也未被尝试。修复提交 `fd5eee85` 将原来的固定 `LIMIT 10` 改为每聊天 `(at,id)` 游标轮转，新增相同时间戳及未知外部创建不得自动重试的边界；[代码与回归 CI 41319963860](https://github.com/sean2077/feishu-bot/actions/runs/41319963860) 四组完整通过。使用真实 SQLite 与模拟飞书 API；不代表实际服务性能或外部写 exactly-once。最终文档和合并后版本须分别核验。

PR #31 复现回执重试饥饿：SQLite 中 55 条较早的 `attempt:processing` 永久待确认，新一条能成功核对的回执却被 `ORDER BY at LIMIT 50` 持续排除。仅新增回归的提交 `a08a436` 在 [CI 41315103830](https://github.com/sean2077/feishu-bot/actions/runs/41315103830) 的 Ubuntu／Windows × Node.js 22.16／24 四组均因相同断言实际失败。修复提交 `256c28c` 按 `(at,id)` 游标轮转既有 50 条批次；[代码 CI 41315264399](https://github.com/sean2077/feishu-bot/actions/runs/41315264399) 四组完整通过。最终测试将所有回执设置为同一时间戳以覆盖游标的 ID 次序。使用真实 SQLite 和回执串行代码，飞书 API 为故障替身；不代表真实服务一致性或性能提高。最终文档版本与合入 main 的 CI 另行核验。

PR #30 区分 `attempt:processing`（写入结果未知）与 `processing`（原添加 API 已返回成功）在重启时的不同边界。仅加测试提交 `53ca391` 的 [CI 41312073363](https://github.com/sean2077/feishu-bot/actions/runs/41312073363) 四组 Ubuntu／Windows × Node.js 22.16／24 均实败：真实 SQLite 中 `applied=processing` 的已确认 OnIt，在重启后首次列表暂不可见时被错误改为 `clear`，远端表情仍存在。最小修复提交 `d9e2906` 将冷启动时已确认或未确认的 OnIt 一并要求观察到本应用的 reaction_id 才能确认删除；原有内存 reaction_id 路径不变。[修复代码 CI 41312242417](https://github.com/sean2077/feishu-bot/actions/runs/41312242417) 四组完整通过。最终文档头和合入 main 的 CI 仍须独立核验。本项使用真实回执处理与 SQLite、模拟飞书列表延迟，不代表真实服务联调，远端表情若已被其他操作移除则可能保守地继续待确认。

PR #28 针对 `OnIt`→`Done` 状态转换阶段混淆建立两项回归：仅新增测试的提交 `96d1e46` 在 [CI 41309942960](https://github.com/sean2077/feishu-bot/actions/runs/41309942960) 的 Ubuntu／Windows × Node.js 22.16／24 四组均实败；第一次远端 OnIt 已接受但列表暂不可见，第二次已确认 OnIt 删除失败，旧代码都错误把 `applied` 提前记成 `attempt:done`，导致后续不敢创建实际未尝试的 Done。源码修复 `e3e2fc4`／`29d397a` 将清理 OnIt 与新增 Done 分为现有串行队列里的两个持久状态阶段，并避免确认清除后的重复 OnIt 查询。代码版 [CI 41310093827](https://github.com/sean2077/feishu-bot/actions/runs/41310093827) Ubuntu 两组和 Windows Node 22 通过，Windows Node 24 因未改动的 PowerShell/CIM 15 秒超时失败；两个新增回归在该 Windows 作业中均已通过。合并 PR #29 的 Windows 测试调度（不覆盖彼此改动）后，组合版 [CI 41310956718](https://github.com/sean2077/feishu-bot/actions/runs/41310956718) 四组完整成功。最终文档头与合入 main 的 CI 仍需独立核验。新增用例使用真实 SQLite／回执串行代码及模拟飞书响应丢失、列表延迟和删除故障，不代表真实 Feishu 联调。

PR #29 仅调整测试文件调度，不修改生产 PowerShell/CIM 或清理策略。已有 [PR #18 对照](https://github.com/sean2077/feishu-bot/actions/runs/41268146872) 显示 Windows 串行时 PowerShell 首次脚本前耗时约 1.83–2.38 秒、Ubuntu 全局串行则明显变慢；[main CI 41306079883](https://github.com/sean2077/feishu-bot/actions/runs/41306079883) 与 [PR #28 CI 41310093827](https://github.com/sean2077/feishu-bot/actions/runs/41310093827) 在原并行度 2 上分别出现独立 Windows 15 秒 PowerShell 超时。此次 Windows 单独使用测试文件并行度 1，Linux 保留 2；[代码版 CI 41310430060](https://github.com/sean2077/feishu-bot/actions/runs/41310430060) 四组 Ubuntu／Windows × Node 22.16／24 通过，Windows 真进程树孤儿回收用例耗时约 3.78／3.92 秒，Linux 继续跑完整核心与原生回归。不同 runner 的一次性结果不足以证明失败率改善，生产 Windows 11 性能及断链进程归属未解决。最终文档版 [CI 41310652686](https://github.com/sean2077/feishu-bot/actions/runs/41310652686) 与合入后 [main CI 41310896999](https://github.com/sean2077/feishu-bot/actions/runs/41310896999) 四组均通过；仍不能证明真实 Windows 11 的长期稳定性。

PR #27 对进程重启期间 OnIt 已远端接受但响应丢失、恢复时列表暂不可见的边界建立回归。仅新增测试的提交 `15c7e10` 在 [CI 41305411580](https://github.com/sean2077/feishu-bot/actions/runs/41305411580) 四组 Ubuntu／Windows × Node 22.16／24 中均实败，SQLite `applied` 被过早记为 `clear`（应为 `attempt:processing`），导致迟到的 OnIt 不能再清除。修复提交 `a3c1fc9`／`5d43e94` 的 [CI 41305612009](https://github.com/sean2077/feishu-bot/actions/runs/41305612009) 四组完整通过（每组 125 项核心通过＋1 项平台跳过，12 项子进程套件通过）：仅保留恢复前未知尝试并要求观察本应用 OnIt 后才确认删除，使用既有重试，无新后台调度、外部写重试或权限变更。最终文档提交与合并后 main CI 单独核验。此测试使用真实状态库、模拟飞书 API 的可见性延迟，不代表实际飞书联调。

PR #26 对结果未知的 Done 在撤销时暂不可见建立回归：仅新增测试的提交 `6ccf3d8` 在 [CI 41300826620](https://github.com/sean2077/feishu-bot/actions/runs/41300826620) 四组均发生新增断言失败：SQLite 的 `applied` 被过早设为 `clear`（预期仍为 `attempt:done`），后续没有再次删除可能迟到的 Done；Windows Node 22.16 同轮还独立触发未改动的 PowerShell/CIM 进程树用例超时，不能归因本项。修复提交 `f4d19de` 的 [CI 41300963923](https://github.com/sean2077/feishu-bot/actions/runs/41300963923) 已通过 Ubuntu／Windows × Node 22.16／24 四组完整回归：只对 `attempt:done` 的清除要求先观察到本应用的 Done，首次查不到则保留待确认并由既有 retry 继续，不重写未知外部操作；已有确认 Done 清除路径不变。真实飞书列表可见性、长期悬置和远端删除结果未知仍待验收；最终文档提交与 main push CI 独立核验。

PR #25 检查了表情创建结果未知且首次查询暂时不可见的边界：仅加回归的提交 `b13421d` 在[修复前 CI 41296518442](https://github.com/sean2077/feishu-bot/actions/runs/41296518442) 的 Ubuntu Node 22.16／24 中两项新增测试实败，均因额外调用一次 `addReaction`（预期 1 次，实际 2 次）；Windows red-phase 被后续提交取消，不声称已复现。修复提交 `cfe3665` 的[四组 CI 41296606553](https://github.com/sean2077/feishu-bot/actions/runs/41296606553) 全部通过。测试使用 SQLite／回执代码和模拟飞书响应丢失、列表延迟可见；未知写入若始终不可见会保留待确认状态，不自动重做，也不保证真实 API 的即时一致性。最终文档提交与 main CI 另行核验。

PR #24 对首次 Windows 部署诊断的误判建立回归：测试专用提交 `5e8506e` 的[CI 41292169792](https://github.com/sean2077/feishu-bot/actions/runs/41292169792) 在 Windows Node.js 22.16／24 均因已有 `node.exe` 被误认 Git Bash 而实败；Ubuntu 两组正常通过，Windows 新测试之外的核心用例未发现回归。代码提交 `bb3dc50` 的[CI 41292422273](https://github.com/sean2077/feishu-bot/actions/runs/41292422273) 四组完整成功：Ubuntu 每组 121 通过＋1 Windows 专用跳过，Windows 每组 121 通过＋1 既有 POSIX 跳过；各组原生子进程套件 12 项通过。新增负例还覆盖仅命名为 `bash.exe` 的无效文件；有标准 Git Bash 安装路径时执行正例。未配置路径时维持原生可选行为，没有真实 Feishu／模型联调，不宣称解决 PowerShell/CIM 清理超时。最终文档版及合并后 CI 单独核验。

PR #23 通过真实 SQLite／回执串行处理及模拟飞书表情 API 复现响应丢失后盲增的问题：仅新增回归的提交 `b9a48a5` 在[修复前 CI 41287868230](https://github.com/sean2077/feishu-bot/actions/runs/41287868230) 四组均实际出现两项新增失败（OnIt／Done 重复创建），其余核心回归通过。代码修复提交 `c848c33` 的[CI 41288000598](https://github.com/sean2077/feishu-bot/actions/runs/41288000598) 已通过 Ubuntu／Windows × Node 22.16／24 四组完整回归（Ubuntu 每组 121 核心通过，Windows 每组 120 通过＋1 既有 POSIX 跳过，各组 12 原生子进程测试）。保存未确认尝试后，重试先校验本应用的已有表情；仅在未找到时创建，并保留其他应用表情。新增断言核对持久登记发生于外部写入之前。最终文档版本和 main push CI 需单独核验；真实 Feishu 的列表可见性与网络结果未知仍待实际验收。

PR #22 针对已确认 Done 与后续 clear 的表情回执竞态：仅加回归的提交 `30ea80d` 在[四组修复前 CI 41284302810](https://github.com/sean2077/feishu-bot/actions/runs/41284302810) 全部实败，SQLite 的 desired/applied 最终均为 clear，但替身 Feishu 通道仍保留本应用 Done。修复提交 `3623cf0` 的[四组 CI 41284472226](https://github.com/sean2077/feishu-bot/actions/runs/41284472226) 全部通过。修复只在上一回执状态为 done、又需清除时查询本应用 Done reaction_id；其他应用同 emoji 不清理，日常成功/取消不新增接口查询。最终 PR 与合入后 CI 需独立核验。此次使用真实 SQLite／回执串行代码与 Feishu API 替身，不是实时飞书联调；网络结果未知或跨进程中断仍需实际验收。

PR #21 针对 `runCommand()` 吞掉项目操作终止失败建立故障注入回归。[修复前 CI 41275807289](https://github.com/sean2077/feishu-bot/actions/runs/41275807289) 四组均失败：核心测试全部正常，新测试中真实 Node 子进程触发停止，模拟 `killTree()` 拒绝，原逻辑却报告普通取消。修复提交 `95e78db` 保留实际清理 Promise 的结论并等待其完成；5 秒既有收尾期限未确认时保守报错，`ActionBroker` 记录不含原始异常的审计。扩充同一测试验证数据库 `action_runs=uncertain`。代码版本 [CI 41276009831](https://github.com/sean2077/feishu-bot/actions/runs/41276009831) 已通过 Ubuntu／Windows × Node 22.16／24 四组完整检查（Ubuntu 118 核心，Windows 117 通过＋1 既有 POSIX 跳过；各组 12 项原生套件）。测试替换进程清理模块，不能声称真实 CIM、模型或飞书联调；性能未测量。最终提交和合入后 CI 须独立核验。

PR #20 针对异常退出清理失败的原生会话所有权补足防线。仅新增模拟事件回归的提交 `321b408` 在[四组修复前 CI 41272269103](https://github.com/sean2077/feishu-bot/actions/runs/41272269103) 均实际失败：曾允许替代 Worker 启动；初始化清理未确认却把状态显示为 `suspended`。修复后提交 `8f8f3b1` 让运行时向网关传递清理未确认状态，保留名额并阻止新会话。增加使用真实 Node 子进程、显式替换进程清理模块的运行时故障注入后，代码提交 `8c81d62` 在[完整 CI 41272463257](https://github.com/sean2077/feishu-bot/actions/runs/41272463257) 通过 Ubuntu／Windows × Node 22.16／24；Ubuntu 每组 118 项核心测试，Windows 117 项通过与 1 项既有 POSIX 跳过，四组各 11 项原生子进程套件。新增测试不等同真实 Windows CIM 故障；未宣称性能收益、真实模型或飞书联调。最终文档版本以 PR #20 与合并后的 CI 为准。

PR #19 的测试专用提交 `63f9a68` 在 [CI 41269346907](https://github.com/sean2077/feishu-bot/actions/runs/41269346907) 四组失败；已核对 Ubuntu 与 Windows Node 24 日志，活跃和初始化中 Worker 的停止失败均被 `Gateway.close()` 吞掉，两项新增用例实际报 `Missing expected rejection`。此时构建／原生套件未执行，不能算 CLI 数据锁复现。

先修复 Gateway 的提交 `13982e6` 在 [CI 41269693348](https://github.com/sean2077/feishu-bot/actions/runs/41269693348) 继续定位 CLI 边界：已核对 Ubuntu 与 Windows Node 24 日志，核心回归通过，正常关闭用例和既有原生／SDK 用例通过，但失败关闭后的 `gateway.lock` 被错误删除，新增 CLI 用例实际报 ENOENT。补齐锁释放条件的提交 `6bf46fb` 已通过 [CI 41269842428](https://github.com/sean2077/feishu-bot/actions/runs/41269842428) 的 Ubuntu/Windows × Node.js 22.16/24 四组完整检查，无重跑、超时增加或断言削弱。最终文档版本的 PR／main CI 单独在 [PR #19](https://github.com/sean2077/feishu-bot/pull/19) 核验记录。

新增验收覆盖另一 Worker 仍获清理、待批取消、会话 ID 保留、无虚假 Done、日志脱敏、锁保留及第二次启动被拒绝；正常关闭仍自动解锁。Gateway／SQLite／构建后 CLI 为真实代码，Worker 与飞书为显式替身，CLI 通过 Node 信号事件触发关闭，不是实际系统信号、真实 CC 故障或飞书联调。既有 6 项 SDK/CC fixture 与 2 项 SDK 故障注入继续保留，本轮再增加 2 项 CLI 边界 fixture；不把这 10 项全部称为真实 CC 测试。本轮未进行新的本地完整回归、真实部署或性能测量。

## 近期验证

PR #17 针对空闲容量回收的停止失败建立了模拟 Worker 验收。仅添加测试的提交 `8a0c68e` 在[修复前 CI](https://github.com/sean2077/feishu-bot/actions/runs/41264338648) 的 Ubuntu Node 24 中实际出现两个 Worker（预期仅一个）；修复提交 `1c81f21` 的[四组完整 CI](https://github.com/sean2077/feishu-bot/actions/runs/41264429007) 已通过 Ubuntu/Windows × Node.js 22.16/24：Ubuntu 每组 114 项核心测试，Windows 每组 113 项通过加 1 项既有 POSIX 跳过；各组另有 6 项真实 CC 进程 fixture。新增测试使用模拟 `stop()` 失败，不是 Windows 孤儿进程清理超时的真实复现；没有测量性能收益。PR 最终文字提交的 [CI 41264617483](https://github.com/sean2077/feishu-bot/actions/runs/41264617483) Windows Node 22.16 遇到既有 PowerShell 启动超时，其他三组通过；在 #18 调整测试调度后重新进行组合回归。

PR #18 针对 Windows PowerShell/CIM 15 秒超时做了三组受控 CI 对照（均未改变 PID、心跳或进程归属断言，也未增加超时）：[默认测试并行 CI 41267952734](https://github.com/sean2077/feishu-bot/actions/runs/41267952734) 的首次 PowerShell 进程从 Node 启动到脚本首行耗时 Node 22.16 为 11.07 秒、Node 24 为 10.95 秒；脚本内 CIM 查询分别约 1.61 和 0.68 秒，结束到子进程关闭约 20 毫秒。[并行度 1 CI 41268146872](https://github.com/sean2077/feishu-bot/actions/runs/41268146872) 的对应启动前耗时约 1.83 和 2.38 秒，但 Ubuntu 核心套件耗时从原 5–6 秒增至约 12–13 秒。[并行度 2 CI 41268313285](https://github.com/sean2077/feishu-bot/actions/runs/41268313285) 的对应启动前耗时约 4.19 和 3.42 秒；Ubuntu 核心约 7 秒；四组检查成功。各组使用不同托管 runner，样本不足以推断失败率或实际部署性能，仅支持将测试调度竞争作为主要可疑因素。实验性的源码计时已从交付补丁撤回，最终仅保留测试文件并行度上限 2；合入后的 [main CI 41268691852](https://github.com/sean2077/feishu-bot/actions/runs/41268691852) 已通过，仍不代表生产路径修复。

PR #15 的测试专用提交 `0392949` 在[修复前 CI](https://github.com/sean2077/feishu-bot/actions/runs/41260809778) 的 Ubuntu 与 Windows Node 24 日志中，均实际出现两项新增用例超时：主机输入 EOF、重复 stop 指令无法完成 worker 退出；既有核心回归与 6 项原生进程 fixture 通过。测试运行真实的已构建 worker 子进程，只替换 SDK 边界，注入永不返回的 `interrupt()`，并预先建立模拟后台心跳与待批权限请求。

修复提交 `7e91361` 的[四组完整 CI](https://github.com/sean2077/feishu-bot/actions/runs/41260963509) 已通过 Ubuntu/Windows × Node 22.16/24：Ubuntu 每组 112 项核心回归；Windows 每组 111 项通过、1 项既有 POSIX 专用跳过；各组原生测试共 8 项（6 项既有 fixture + 2 项 SDK 故障注入）。新增断言要求正常退出、`close()` 恰好一次、权限被拒绝、心跳停止、不伪造成功或故障事件，不靠父进程强杀通过验收。没有增加等待超时、依赖或常驻进程。真实 CC 的恢复／压缩等既有 fixture 使用模拟模型，新增两项更不能称为真实模型／飞书联调；本轮没有新的本地完整回归或性能收益结论。

PR #16 的测试先在 [Ubuntu Node.js 22.16／24 修复前 CI](https://github.com/sean2077/feishu-bot/actions/runs/41260827490) 复现原生进程意外退出时没有同群通知（Windows 修复前作业因后续提交取消）。修复后的 [PR CI](https://github.com/sean2077/feishu-bot/actions/runs/41260891662) 四组通过：Ubuntu 各 113 核心测试，Windows 各 112 通过＋1 既有 POSIX 专用跳过，每组另外 6 项真实 CC 进程 fixture。新增用例核对意外退出通知仅一次、旧审批不可再批准、未确认输入不重放、同一会话可续接且无虚假 Done。飞书传输仍为模拟，实际服务通知可达性待联调。

PR #16 在 PR #15 验证期间合入 main；两项源码没有重叠，状态文档合并保留双方记录。以上各自 CI 不能替代组合版本的最终 PR／main CI。

## 既往验证

PR #12 已用[修复前 CI](https://github.com/sean2077/feishu-bot/actions/runs/41256047020) 的 Ubuntu Node 22.16/24 验证新增两项恢复回归会失败：请求独立过期时缺少通知，以及有未确认输入时警告缺少审批／追问信息。Windows 两组因后续修复提交触发自动取消，不能声称修复前 Windows 已复现。修复后的 [PR CI](https://github.com/sean2077/feishu-bot/actions/runs/41256182969) 四组已通过：Ubuntu × Node 22.16/24 各 112 项核心回归，Windows 各 111 项通过 + 1 项既有 POSIX 专用跳过，各组另有 6 项真实 CC 进程 fixture。Windows Node 24 首轮在未改动的进程树清理测试中 PowerShell CIM 查询超过 15 秒被终止，定向重跑该失败作业一次通过；该偶发超时未被归因为本次代码，也未放宽测试。本项仅验证数据库恢复和模拟消息通知，不代表真实飞书卡片更新或原生后台任务重启验收。

PR #5 阶段的历史本地验证（Linux / Node.js 22.16）：完整 `npm run check` 通过，97 项核心回归及 5 项真实 Claude Code 进程测试。模型响应与飞书传输使用测试替身，不代表真实服务验收，也不是本轮验证。

PR #5 初次 CI（`f1b6bf9`）的 Windows 原生 acceptEdits 用例失败；修正后的源码提交 `ed74711` 已通过[四组 CI](https://github.com/sean2077/feishu-bot/actions/runs/41222283705)：Ubuntu/Windows × Node.js 22.16/24。Ubuntu 每组 97 项核心回归通过；Windows 每组 96 项通过，1 项 POSIX 专用用例按条件跳过；四组各有 5 项真实 CC 测试通过。Windows runner 是 Windows Server 2025，不代替特定桌面设备验收。后续版本仍应查看其自己的运行结果。

PR #6 的真实代码提交 `a3fd618` 已通过[四组 CI](https://github.com/sean2077/feishu-bot/actions/runs/41228719416)：Ubuntu/Windows × Node.js 22.16/24。Ubuntu 每组 102 项核心回归通过，Windows 保留既有 POSIX 专用跳过；各组包含真实 CC 进程 fixture，但文件传输为模拟飞书。新增覆盖 Agent 发送失败、快照不可变、重启后的明确补发及已确认上传不重复发送。

PR #7 的源码提交 `54270a9` 已通过[四组 CI](https://github.com/sean2077/feishu-bot/actions/runs/41233582219)：Ubuntu/Windows × Node.js 22.16/24。Ubuntu 每组 106 项核心回归通过；Windows 每组 105 项通过、1 项既有 POSIX 专用跳过；每组 6 项真实进程 fixture 通过。新增验证异步原生初始化中止与延迟连接取消。Feishu WebSocket 仍使用可控替身。

PR #8 的回归用例先在 [Windows Node 22/24 CI](https://github.com/sean2077/feishu-bot/actions/runs/41238135618) 实际复现遗漏（Linux 两组成功、Windows 两组失败）；修复后在 [四组 CI](https://github.com/sean2077/feishu-bot/actions/runs/41238302626) 全部通过。Ubuntu 每组 107 项核心测试；Windows 每组 106 项通过、1 项既有 POSIX 跳过。Windows 孤儿树使用真实三代 Node 进程与持续心跳验证，非真实模型／飞书联调。

PR #9 的实际修复提交 `d33ed4d` 已通过[四组 CI](https://github.com/sean2077/feishu-bot/actions/runs/41242799686)：Ubuntu/Windows × Node.js 22.16/24，Ubuntu 每组 108 项核心通过，Windows 107 项通过、1 项既有 POSIX 专用跳过；每组另有 6 项原生进程 fixture 通过。回归确认过期且已送达的导出快照会随数据库记录一并清理，未确认文件继续保留、受管目录之外的文件不触碰。没有真实飞书文件服务联调或历史孤立文件全盘扫描。

PR #10 的新增实际进程测试先在[修复前 Windows Node 22／24](https://github.com/sean2077/feishu-bot/actions/runs/41247771646) 双双复现存活子进程无法清理，Ubuntu 两组通过；修复 `runCommand` 出生时间登记后，在[四组 PR CI](https://github.com/sean2077/feishu-bot/actions/runs/41247955159) 全部通过。Ubuntu 每组 109 项核心回归；Windows 每组 108 项通过、1 项既有 POSIX 专用跳过；各组 6 项原生进程 fixture 通过。新增用例等待 launcher 明确退出，并以子进程 PID 和持续心跳验证清理。此结果不代表未知祖先链、真实 Feishu 或模型连接的验收。

PR #11 的代码提交 `a794a35` 已通过[四组 CI](https://github.com/sean2077/feishu-bot/actions/runs/41251654561)：Ubuntu/Windows × Node.js 22.16/24。Ubuntu 每组 110 项核心回归通过，Windows 每组 109 项通过、1 项既有 POSIX 专用跳过，各组 6 项真实原生进程 fixture 通过。新增验证过期取消文件回收、未确认上传不删除、受管目录外不删除，以及 `send_file` 的本地持久登记失败不遗留孤立快照。最初的纯回归提交因后续补丁触发并发 CI 自动取消，不能声称修复前 CI 已复现；源代码查询已确认旧实现只清理 `done`。

## 下一轮优先核查

1. Windows 生产负载下的清理可靠性、异常退出后代实际归属是否可靠，以及初始化失败的真实进程树清理。PR #18 已定位 CI 冷启动阶段，不能用测试调度改善替代生产修复；一次性快照仍不能安全归属中间祖先已消失的断链进程。PR #17／#19 已分别覆盖运行中容量和服务关闭的停止错误，不重复开发。
2. 对新增主动文件投递进行真实飞书上传／断线／重启后的人工补发验收；模拟测试只能证明持久状态机与授权检查。未确认文件会保守地阻止同会话后续输出标 Done，直到处理成功或明确停止；评估实际使用中是否需要更精准的原生任务归属。
3. 建立可重复的资源基线，区分冷启动、热续轮、闲置占用和推理成本。已有 fixture 耗时不是实时模型或飞书延迟基准。

## 真实环境待验证

使用独立测试飞书应用、模型入口及测试群/账号。凭据只配置在运行主机，不放入 issue、聊天或测试材料。先完成单群/私聊基本往返，再验证双群、多人补充、审批、文件失败与恢复；具体清单见[验收](validation.md)。尚未进行真实部署。
