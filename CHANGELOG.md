# 版本记录

## Unreleased — Go 原生网关

- `actions[].timeoutMs` 上限调整：`readOnly` 保持 10 分钟，非只读放宽到 30 分钟；`doctor` 对超过 10 分钟的 action 给 info。超时、取消或输出超限时先发中断信号并最多等待 5 秒（Unix `SIGTERM`，Windows 尽力 `Ctrl+Break`），再强制结束进程树，给 wrapper 的 finally 释放租约机会。原生 Bash/Monitor 的 `feishu-bot action` 客户端等待上限同步提高到 160 分钟，避免客户端提前退出后网关仍执行外部写操作；`project_action` MCP 调用仍需把 `MCP_TOOL_TIMEOUT` 设到 action timeout 加审批窗口之上。
- 网关对 `actions[].approval: "none"`／`"open"` 增加机器人 sender 拒绝：本轮活动 senders 中任一 `SenderIsBot`（包括 `allowOtherBots` 群里的其他机器人）时返回 `{"ok":false,"error":"机器人消息不能触发写操作"}`，`action_direct_rejected` 审计记 `bot_sender=true`；`card` 不受影响，仍由真人点击审批。`RequestContext` 增加 `hasBotSender`；机器人来源的中断输入不提供续接，点击「继续」提示由真人重新发起。
- 新增 `actions[].approval: "open"`：非只读固定操作在请求上下文 active 且 `senders` 非空时直接执行，不检查 maintainer，权限由动作自身（例如 wrapper 的 roles）判定；调用时没有活动请求（inactive）才返回 `{"ok":false,"error":"需要活跃的用户请求"}`，延迟调用会归属调用时的活动轮次。执行时注入 `FEISHU_REQUEST_*`，不设置审批人环境变量，`action_runs.approval` 记为 `open`；污染轮次直接执行并记 `tainted=true`。`doctor` 对 `open` 给出 info，`readOnly` 与 `open` 同时配置时给出冗余 warning；`card` 和 `none` 行为不变。

## 0.6.1 — 2026-11-13

- 恢复 v0.6.0 的发布：修复项目提示词失败分类测试把夹具文件命名为 `nul.prompt`（Windows 保留设备名，`windows-2022` 上读写落到 NUL 设备）导致发布构建失败的问题；本版本包含 v0.6.0 的全部变更，生产行为不变。

## 0.6.0 — 2026-11-13

- 固定操作子进程的基础环境增加 Windows 用户／系统目录变量 `LOCALAPPDATA`、`APPDATA`、`USERPROFILE`、`SystemDrive`、`ProgramData`、`ALLUSERSPROFILE`；`actions[].vars` 同名值覆盖继承值，其他服务环境变量仍不继承。
- 回复卡状态行改用飞书内置图标：思考中／处理中显示 `loading_outlined`，已回复 `succeed_filled`，失败 `error_filled`，停止 `stop_outlined`，等待审批／回答 `alarm-clock_outlined`；不引入图片上传或客户端动画配置。
- 新增 `actions[].approval`（默认 `"card"`）：`"none"` 的非只读固定操作在本轮请求上下文 active 且全部 senders 都是项目 maintainers 时直接执行，不弹审批卡；否则返回 `{"ok":false,"error":"需要管理员发起"}`，不回退。直接执行仍注入 `FEISHU_REQUEST_*`，不设置审批人环境变量；`action_runs.approval` 区分 `card`、`none` 和 `readonly`。污染轮次维持直接执行，`action_started` 审计记 `tainted=true`。`readOnly` 与 `approval: "none"` 同时配置时 doctor 给 warning；项目配置 `approval: "none"` 而 maintainers 为空时 doctor 失败，非法取值在配置加载时拒绝。
- 回复卡按钮行只保留「查看状态」，「待回答」「重试交付」「停止本轮」收进「更多」overflow；重试需 30 秒内连续选择两次确认，避免误触补发可能重复的输出。旧卡片上的「复制」按钮已下线，点击只提示，不再发送纯文本；`/copy` 命令同步移除。
- 新增逐群 `groups[].allowOtherBots`（默认 `false`，仅显式群可用）：开启后其他机器人的消息可以入站、进入背景和 `read_chat_history`，内容一律视为不可信输入。其他机器人只有明确 @ 本机器人才触发，`mentionOptional` 不适用；同一话题、同一机器人连续触发最多 2 轮，超过后保存为背景并提示，人类消息打破计数。新增 `mention_bot` 工具：只有本轮所有 senders 都是项目 maintainer 且请求文本点名目标时可用，目标必须在 `groups[].otherBots` 名单中并配置 `openId`（飞书 open_bot_id；post at 元素不接受 appId），每轮最多一个，正文可选、截断到 2000 字但不改写；发送一条结构化 post 后本轮结束，不等待或汇总结果。对方回复该消息或同话题后续消息只进历史，不触发新一轮。每次调用写脱敏审计；不影响 actions、审批或维护者身份。
- 回复卡「停止本轮」改为本轮提问人或本聊天 maintainer 可点，其他成员收到「只有提问人或管理员可以停止本轮。」提示且轮次继续。提问人 open_id 来自原生轮次 anchor，写入 HMAC 签名的 quick control payload；客户端不能伪造。拿不到提问人时退化为仅 maintainer 可停止；retry 和会话 resume 仍只限 maintainer。
- 修复「继续」按钮可能接错历史中断点：关闭状态卡现在把续接按钮绑定到产生它的中断输出，点击时使用该输出对应原始输入的摘要（截断 300 字）构造续接提示，并明确要求只继续这条请求、不要切换到其它历史中断点；恢复提醒卡仍以该会话最新的 uncertain 输入为锚点。续接轮次的回复目标改为被点击的卡片，不再使用可能很旧的原始输入消息；被再次中断时，通过输入的 interactionMessageId 继续解析锚点。停止进行中的状态卡仍显示按钮，点击在停止完成前会被拒绝。关闭状态卡即使原输入已被 `/stop` 取消，只要仍能解析原始请求就显示「继续」。
- 修复项目 `settingsFile` 含顶层 `autoMode` 时原生进程拒绝启动的问题：现在允许 `autoMode` 为 JSON 对象并原样透传到生成的临时 settings（isolated 和 inherit 都覆盖），已知 `environment`/`allow`/`soft_deny` 和未知子字段均不逐项校验；`autoMode` 不是对象时仍拒绝。`hooks`、`defaultMode`、`apiKeyHelper`、`additionalDirectories` 等其他未知顶层字段继续 fail-closed，错误日志和 doctor 显示具体字段名。
- 新增 `projects[].permissionMode: "auto"`：网关只对 auto 项目注入 `CLAUDE_CODE_AUTO_MODE_SERVER=0`，强制使用 Claude Code 本地安全分类器；不改变 API_KEY 或非必要流量开关。分类器使用现有模型凭据和 `baseUrl`，固定 `claude-sonnet-5`，每次最多两段、约 40 秒且计费。分类器拒绝写 `auto_mode_denied`，不可用／无法评估写 `auto_mode_unavailable`（`kind=unavailable|unevaluable`），并在回复卡追加一条固定中文提示；工具输入不进入卡片或审计。init 回显 `permissionMode`，不一致写 `permission_mode_mismatch`；`/status` 显示请求与生效值，doctor/config explain 标注运行时 `not_checked`。deny 仍是硬边界，ask 仍进入维护者审批。默认值不变，线上配置未切换。
- 修复回复反馈按钮从未上卡的问题：CardKit 的 `element_id` 上限为 20 字符，原反馈控件 ID 过长，服务端按重复 ID 以 300301 拒绝最终控件刷新。反馈控件改用短 ID；发送前统一校验所有 `element_id` 长度，超限在本地明确拒绝。正常一轮不再产生 `output_controls_refresh_failed`，控件内容不变时仍由客户端快照比较跳过更新，永久拒绝仍只按外观失败处理。
- `worker_exit` 增加 `session`，并在宿主提供房间键时带 `room`，多个 worker 同时退出时可区分来源。
- 追问改用 Card 2.0 整张表单：每题支持单／多选及自定义文字，非空自定义覆盖选项、空白回退选项；一次提交避免刷新丢失输入和跨作者拼接草稿，保留 `/answer`。回调校验请求、会话、卡片、版本、作者及截止，持久提交时再确认会话仍有效。
- 回复 Markdown 增加围栏与表格结构处理：单组件前四个表格正常展示，第五个起降级为围栏文字并附完整原文；流式与预算截断补齐围栏，持久答案和导出原文不变，文件未确认前不标 Done。
- 修复 WebSocket 重连取消已接收消息处理、慢 handler 阻塞重连的问题：有界消息／回调队列跨连接保留，旧 ACK 仍绑定旧 socket。增加保持 TCP 可写但无入站活动、健康空闲、处理中重连和停止清理的真实本地连接回归。
- 新增显式话题目录配置 `groups[].threadWorkspaces`，可将话题根消息 ID 绑定到已有目录或 Git worktree，未绑定话题继续共享默认目录；保持原生 HOME、附件与房间键不变。已有会话改变目录时拒绝静默续接并提示恢复配置或 `/new`，本地终端交接同样核对。`/status` 显示绑定，`doctor` 检查目录；网关不创建或清理 worktree。
- 未注入版本的源码构建不再显示过期的 `0.3.0-dev`，改为 `dev-g<提交>`（有未提交改动时加 `-dirty`）；部署文档给出注入 `git describe --tags --long --dirty` 版本（如 `0.5.0-3-g1a2b3c4`）的本地构建命令。发布产物仍注入 `X.Y.Z`。
- 修复前台轮次中启动的后台命令在轮次结束前完成时，同一条 @ 收到多张回复卡且多出的卡片一直停在“思考中”的问题：原生把任务通知并入当前轮次时，后续输出留在原回复卡片；原生结果到达时一并收尾本轮其余回复卡片，从未发出的片段只关闭、不再新发卡片。
- 新增项目级环境变量 `projects[].env` 与 `projects[].envPassthrough`：`env` 保存非敏感字面值，`envPassthrough` 从服务环境复制凭据；加载时按大小写不敏感拒绝保留名、重复名和两者交叠。变量在下次原生进程启动时生效，不改变会话身份、工作区或权限规则；`doctor` 检查缺失的 passthrough 和疑似密钥字面量，`config explain` 只显示变量名，`daemon install` 生成的 `service.env` 会捕获当前已存在的 passthrough 值。
- 新增 `projects[].settingsFile`：挂载管理员维护的原生 settings 快照，只允许 `env` 和 `permissions.allow/ask/deny`；每次启动原生进程时读取并校验，与模型环境合并为 0600 临时 `--settings` 文件，进程结束删除。文件缺失或不合规时拒绝启动该进程，输入保留并提示启动失败；同一条待交付输入因启动失败只提示一次，新输入会再提示一次。`doctor` 检查结构，`config explain` 只显示路径。isolated 下文件 env／permissions 原样传入且模型凭据不落盘，inherit 下合并模型环境。
- 新增请求上下文：固定操作子进程获得 `FEISHU_REQUEST_*` 发起人／消息变量，`action_runs` 增加 `requester` 和 `request_context`，非只读审批卡显示发起人；上下文在请求到达时捕获，审批等待期间变化不影响记录。原生进程始终启动只读 loopback `context` 端点，Bash 子进程可用 `"$FEISHU_ACTION_CLIENT" context` 读取当前输入上下文，无输入时返回 inactive；没有配置 `actions` 的项目不接受 action 调用。
- 新增审批策略：`projects[].owners`（maintainers 子集）、`projects[].approvals.forbidSelfApproval` 和 `actions[].approvers`。指定 approvers 后该操作只认这些审批人；开启禁止自批后发起人不能批准自己的请求，owner 例外，拒绝不受限制；请求上下文为空时只有 owner 可以批准。原生工具 ask 同样执行自批规则，问题卡保持现状；审批卡显示发起人和指定审批人，私聊转发到对应审批人；`requests` 增加 requester/request_context，自批拒绝写审计事件。
- 新增按项目开启的回复反馈 `projects[].feedback.enabled`：只在最终成功交付并标记 Done 的回复卡上显示「有用」「无用」，每人每回复一票、最后一次为准，点击只提示不重写卡片。`feedback` 表保存问题与回复摘要快照（截断并脱敏）、open_id、会话和消息 ID，保留 90 天；新增只读 `feedback export` 命令按 value/since/project 导出 JSONL，WAL 下可与运行中的服务并发读取。
- 修复交付成功后的控件刷新失败在回复卡上残留「重试交付」或追加卡片的噪音：进行中的交付不再提前显示重试，最终刷新失败只记外观未更新；维护超时改为可续跑状态，`/status` 增加最近一次完整清理时间；新增 `projects[].displayWorkspace` 控制回复卡工作目录显示，群聊默认项目 id，私聊默认完整路径。
- 修复维护游标跨轮次完成一整圈时未写入 `last_full`、`/status` 一直显示“尚未完成一次完整清理”的问题；现在最后一条策略提交时在同一事务写入，非 deadline 错误回滚时不会误记。
- 新增每轮阶段耗时诊断：`turn_result` 记录排队、启动／复用、发送、首个模型输出和工具累计耗时；`turn_delivery` 记录每次交付尝试及 done 时的总时长。`dispatch_failed`、`worker_failure` 和 `output_delivery_failed` 增加 phase 与固定分类原因，`worker_exit` 增加停止意图 `reason` 和 `inTurn`；只写日志和审计，不改变卡片内容。
- 新增默认关闭的定时回复反馈归档 `feedback.export`：开启后每轮维护清理前按项目和 UTC 日期写 `feedback-<project>-<YYYY-MM-DD>.jsonl`，复用 `feedback export` 的 JSONL 记录与只读查询；临时文件同步后 rename、0600，当日文件不覆盖，`feedback_exports` 保存 `(at,id)` 水位。`feedback` 保留策略只删除 `(at,id)` 不高于本项目水位的旧行；归档失败暂停整个策略并只审计一次，修复后自动恢复。归档是至少一次语义，写文件后水位保存失败会在下一天重复导出，消费方按 `(messageId, user, at)` 去重；`doctor` 检查目录存在、非符号链接且可写。手工 `feedback export` 行为不变。
- 原生协议回归固定版本更新为 Claude Code 2.1.321（CI fixture 使用 Agent SDK 0.3.321）。本地假 Messages 流程确认 `[1m]` 模型名只产生已知 stderr 告警，不终止会话；1M 上下文窗口和 `CLAUDE_CODE_AUTO_COMPACT_WINDOW` 均按配置生效。该结论是模拟证据，未复现真实流量下的 SIGKILL。
- 新增反馈审计：点击保存成功后写 `feedback_recorded`（room、session、output、value），定时导出成功后写 `feedback_exported`（project、date、rows、文件 basename）；只记录元数据，不含问答内容、open_id 或其他用户标识。
- 新增默认关闭的 `projects[].bash.rejectCwdPrefix`：开启后在 PreToolUse Bash hook 中拒绝以当前原生 cwd 的 `cd <cwd> &&` 或 `cd <cwd>;` 开头的命令，返回可直接照做的说明，减少重复审批噪音；支持引号、正反斜杠、Windows 大小写和 MSYS 路径、`~` 展开。它只检查开头一个 cd，不处理环境变量/`cd -`/`cd ..`/`pushd`，不是安全边界。每次触发写 `bash_cwd_prefix_rejected` 审计（room、session），不记录命令全文。
- 新增项目系统提示词 `projects[].appendSystemPrompt` / `appendSystemPromptFile`（二选一，16 KiB，UTF-8，无 NUL）：在初始化 appendSystemPrompt 中按 bridge → action → `Project instructions:` 顺序拼接，interactive 通过 `--append-system-prompt` 传入。文件每次启动读取；缺失/不可读/超长/编码错误时 fail-open，不带提示词并写 `project_prompt_unavailable` 审计（project、原因分类，不写内容），doctor 报告但不阻止启动；doctor/config explain 只显示配置、来源和字节数。提示词是指引，不改变授权、范围或权限规则。

- `auto_mode_denied` 在分类器原因为 `Stage 2 classifier error` 时增加 `stage2Error: true` 审计标记；表示 Stage 2 出错后 CLI 按 Stage 1 结论拦截，可能误拦且耗时偏长。卡片文案不变。
- `read_chat_history` 对当前房间中本机器人已交付的回复卡片，在飞书返回空文本或「请升级至最新版本客户端，以查看内容」占位时，用 outbox 中 `state=done`、`kind=reply` 的保存正文回填；只匹配卡片 target，不回填文件消息，不跨房间；7 天后 outbox 清理则保留原样；回填查询失败不影响历史返回，只写 `history_backfill_failed` 审计。
- 放宽 gateway fixture 的统一等待上限（仅测试工具，不改变生产行为）：worker 启动、首条输入、标题、条件轮询和关闭清理共享同一上限，避免 `-race` 慢速 runner 上把启动耗时误判为失败；图片批次超时相对该上限设置，保持原测试语义。
- 稳定 Windows 偶发测试（仅测试工具，不改变生产行为）：进程树 RSS 采样在短暂不完整快照时重试；benchmark 趋势采样改为等待捕获和维护事件后再读取进度文件；快捷回调测试显式同步启动 worker；升级回滚测试只在已确认回滚后重试 Windows 文件共享冲突；artifact TLS fixture 对 PowerShell 超时重试，并在证书导入超时后按指纹清理可能已半导入的证书再重试，清理失败时保留恢复记录。
- 新增 `projects[].modelProfile`：值须引用已声明的 `modelProfiles` 或 `default`，否则配置加载失败。解析顺序为会话显式选择（包括显式 `default`）优先，其次项目默认，最后部署默认；项目默认只影响从未显式选择的会话，`/model default` 固定使用部署默认。改变项目默认后，会话在下次原生进程启动或按同一原生 ID 续接时使用新的 `--model`，保留已有历史；凭据、`baseUrl` 和权限仍来自部署配置。`/status` 显示生效配置及来源（`session`／`project`／`default`），`/sessions --json` 按会话显示 `profile` 和 `profileSource`，`/model --json` 增加 `currentSource` 和 `projectDefault`；doctor 与 config explain 显示项目默认配置名，不显示凭据。
- 新增默认关闭的项目预检索 `projects[].preContext`：引用本项目中 `readOnly: true` 的固定操作，仅在新一轮人类输入前执行；参数为触发文本折叠空白/控制字符并按 `maxLength` 截断后，作为字面量 `--` 后的单个 argv 传入，不经过 shell；`timeoutMs` 默认 5000 且不超过 action 上限，`maxChars` 默认 6000。检索与工作区准备、原生进程启动并行，结果以带 `source`/`text`/`truncated` 的独立字段注入本轮输入，作为不可信背景材料，不规划步骤、替代工具或门控输入。超时、非零退出、空输出或参数失败时照常发送并写 `pre_context_failed`（project、action、reason、durationMs），`turn_result` 增加 `preContext`（used/failed/skipped）和 `preContextMs`。该 action 仍会通过 `project_action` 暴露给模型；AGENTS 契约同步澄清只读预检索不构成第二套规划器。
- 新增默认关闭的话题群按需历史 `groups[].topicHistory: "chat"`：`read_chat_history` 增加 `scope`（默认 `topic`）和 `expandThreads`；`chat` 可读本群时间线或已观察到的同群话题，支持时间范围、并发展开最多 20 个话题（每话题最多 10 条回复）、40000 字预算按“先减回复、再减话题”裁剪、`threads`／`expanded`／`omittedThreads`／`omittedReplies`／`failedThreads` 统计（单个话题失败时跳过并降级，全部失败才报错）、`background: true` 来源标注和 `history_chat_scope_read` 审计；游标绑定 scope，绝不跨群。历史回填扩展到同群其他房间，但仍只回填已完成的卡片回复。真实飞书测量确认 chat 容器只返回话题根消息、不返回回复，thread 容器不支持时间过滤；因此展开按最近 3 天发现窗口逐个读取话题，更早创建且近期有回复的话题仍可能遗漏。
- 修复 `read_chat_history` 的 `scope` 和 `expandThreads` 被原生层参数校验拒绝（`unknown history argument`）：原生层现在按 schema 校验 `scope` 只能是 `topic`／`chat`、`expandThreads` 只能是布尔值，组合与配置开关继续由网关负责；新增 schema 防漂移测试和真实 CLI 集成覆盖，确认参数能到达网关并写入 `history_chat_scope_read`。
- 修复 Claude Code 2.1.322 auto 模式文案变化导致 `auto_mode_unavailable` 漏报：新增对 `<model> is temporarily unavailable ... so auto mode cannot determine the safety of ...` 的识别，同时保留 2.1.321 的 `Auto mode is unavailable:` 文本。2.1.322 的 Stage 2 原因尾部增加 `— retrying often succeeds`，既有 `Stage 2 classifier error` 前缀和 `stage2Error` 识别不变；测试夹具补充两个版本的原文。
- 修复配置字符串含真实 NUL 控制字符时被笼统报告为 `must be a nonempty string` 的问题：现在空字符串保留原错误，含 NUL 的字段单独提示 `contains a NUL control character; write it as an escaped sequence in the JSON string instead of a raw character`；部署文档补充 `pattern`／`argv` 中正则转义应写成 JSON 转义文本（如 `"\\x00"`）的说明。其他校验不变。
- 历史结果和输入 envelope 增加本地时间：`core.Message` 新增 `localTime`（RFC3339，带数字偏移，如 `2026-11-11T15:05:00+08:00`），覆盖 `read_chat_history` 的 topic、chat 时间线、threadId 和 expandThreads 返回，以及 envelope 的 trigger／background／quote；历史结果顶层增加 `timezone`。时区解析统一到 `core`，与 envelope 的 `timezone` 使用同一来源；`time` 仍为 Unix 毫秒，`localTime` 在时间缺失时为空。
- 新增默认关闭的私聊跨群历史 `dms[].historyChats`：`read_chat_history` 增加 `chat` 参数（配置名或 chatId），只在私聊白名单中解析显式绑定的目标群，复用 chat 时间线、threadId 和 expandThreads。每次调用（含游标续页）要求活动人类请求，发起人是该 DM 用户且为目标群 maintainer 或显式 member；`allowAllMembers` 不算授权，目标不能是兜底群或私聊。跨群结果只读文本、不返回附件，写 `history_cross_chat_read` 审计且不含正文。读取成功后本轮标记为污染：写工具、`send_file`、非只读 `project_action` 和未知工具需要 DM 用户确认，只读工具放行；非只读 action 先做污染确认，再按原有 approvers 和 `forbidSelfApproval` 审批，污染确认不替换原审批。后台任务仍在运行时标记保持，下一轮人类输入且后台结束后清除，`/stop`、超时和 worker 结束 fail-closed；worker 侧另有 approvalMs+10s 截止，避免 hook 超时放行。污染审批写 `tainted_turn_prompt`，只解除本层拦截，不替代原生 permission 规则。残余风险：跨群正文仍留在原生 transcript，可能影响后续轮次；真实飞书跨群读取尚未验收。
- 回复反馈改为每人每张回复只能反馈一次：`feedback` 以 `(output,user)` 去重，重复点击不覆盖并提示已有选择；按钮文字显示实时计数“有用 · N”／“无用 · M”，每次点击后按 CardKit 元素接口只更新两个按钮，复用 `nextCardSequence` 的严格递增序号和 `feishu_cards.snapshot` 同步；同一张卡串行刷新并在发送前重新读计数，多人并发时最终显示收敛。刷新失败不影响反馈记录，写 `feedback_count_refresh_failed`；旧消息卡没有 CardKit 实体时写 `feedback_count_refresh_skipped`。反馈导出格式不变。
- 新增顶层 `cardTitle` 配置，默认 `飞书助手`；trim 后须为 1–40 字符且不含控制字符。回复卡状态行使用该名称并对 markdown 特殊字符转义；旧版无 `element_id` 的状态行仍按旧标题识别，只在状态行被重写时替换为新标题，不批量回填旧卡。
- 固定操作审批通过后向子进程注入 `FEISHU_ACTION_REQUEST_ID`、`FEISHU_ACTION_APPROVER_OPEN_ID`、`FEISHU_ACTION_APPROVED_AT`，污染轮次另注入 `FEISHU_ACTION_TAINT_CONFIRMER_OPEN_ID`；只读操作不注入。`action_runs` 追加可空 `request`、`approver`、`taint_request` 列，分别记录最终审批请求/审批人和污染确认请求；`actions[].env`／`vars` 新增拒绝 `FEISHU_ACTION_` 前缀。审批标识是审计数据，不是授权。
- 新增异常中断一键续接：恢复提醒卡和状态卡在存在 `uncertain` 输入时显示「继续」按钮，只有原提问人或项目 maintainer 可点，同一张卡只能成功续接一次；点击后在同一话题/会话排队固定续接提示，基于已有上下文继续，不重放中断前的写操作。未点击时仍需在原话题 @机器人 才会恢复；恢复提示会写明直接回复不带 @ 不会触发。

## 0.5.0 — 2026-11-07

- 原生启动前持久记录身份未决，停止／回收／关闭等待最终身份事件，防止迟到 ready 与下一次启动竞争；退出时数据库收尾失败继续协调，不自动重放；停止暂停与用量待补写支持恢复，统计缺口保持可见。
- 卡片补充、快捷动作和审批的绝对截止贯穿数据库等待与声明；群级待交付检查和入队在同一事务完成，活动／排队输入使用部分索引；回执清除及明确失败与最终交付并发的状态可以收敛。
- 普通消息与模型主动取附件共用原子发布缓存，短内部文件名保留展示名称，支持崩溃恢复与长中文文件名；旧附件迁移失败明确阻断，旧目录不放宽附件范围；话题根引用按实际交付事实追踪，首次读取失败后可补充。
- 新增默认关闭的每聊天原生进程额度、RSS 准入阈值和按成功复用延长的空闲预算，以及容量等待／复用／回收诊断。新增独立 Linux／Windows 周期 endurance 观察，保留 Go 堆与数据目录趋势，PR 使用短时冒烟。
- `daemon install` 在停旧服务前完成二进制暂存与私有恢复副本；注册／元数据失败恢复旧文件和服务定义；完整安装提交后的启动未确认保留一致的新安装与明确恢复说明，避免替换可能正在运行的进程。
- 修复一批可靠性问题：最终回复与用量记录保存失败时短暂重试（用量仍失败则照常交付回复），仍失败则释放该轮并以中断内容收尾卡片，不再让同一聊天后续每轮回复都被吞掉；新会话原生 ID 写库失败时改写工作区标记并在下次启动补存续接，`/stop` 与原生初始化交错时（包括停止后才到达的初始化帧）也保留原生 ID，避免下次启动静默换成新上下文；`/stop` 遇到数据库错误仍完成进程停止并释放 Worker 名额，且在重新 `/stop` 成功前不再交付本应取消的排队输入；附件准备失败明确提示“未提交给模型”，同一附件可重复获取；明确失败的表情按普通创建重试，结果未知的表情一小时后停止核对；卡片回调时限从收到事件起算，超时后不再提交补充；本地终端启动失败恢复原会话选择；`daemon install` 检查失败不再覆盖 `service.env`；Windows 等无 IANA 时区名的主机按 `UTC±hh:mm` 偏移提供时区而非 UTC，并内置时区数据库使 `TZ` 在 Windows 发布版中生效；文件上传不再被共享客户端的 20 秒超时截断。修复 Windows CI 中进程回收测试在后代已被及时回收时的误报。
- 修复话题会话的五处问题：卡片快捷补充在话题模式下被静默取消（改为绑定已验证的话题房间、会话和轮次）；新开话题可绕过同一作者的限流和全群待交付上限（改为按群计算）；根消息是否已提供改为看当前会话是否实际收到输入，附件准备失败或话题内 `/new` 后会再次提供；入站附件按话题分目录，工具读取与 `send_file` 不跨话题；后台重试改为扫描数据库中真正有待发输出的房间，网关不再在内存里累积所有历史房间。共享工作区准备锁改为可响应取消。
- 新增群配置 `groups[].sessionScope`：默认 `"chat"` 行为与房间键不变；`"thread"` 让群主时间线上的每个新 @ 以飞书话题回复并开启独立原生会话，话题内 @ 续接同一会话，归属只看 `thread_id`／`root_id`（普通引用回复不并入被引用话题）。回复、状态卡、通知和文件留在话题内；`read_chat_history` 只读本话题；顶层管理命令只返回提示；同一群的话题共享工作区与原生 HOME，并发冷启动只串行工作区准备。飞书拒绝话题回复（230071／230072）时明确报错，不退回整群会话。不改变 SQLite schema。
- 修复按 `threadId` 读取历史时默认附带 `end_time` 的问题：飞书话题容器不支持时间范围，现在不再发送默认截止时间，显式时间范围返回工具错误。
- 最终便携／安装产物验证绑定 BUILDINFO、准确提交与 SHA-256，执行完整本地 TLS／WS／REST／真实 CLI 旅程及固定旧二进制升级恢复；Windows 精确撤销测试 CA，macOS 明确采用一次性 runner job 的 OS 信任生命周期。外层监督器确认硬超时后的进程回收，安装／卸载未确认均阻断发布。
- 原生标题控制增加有界 writer／ACK 等待、真实截止时间和持久化目标／确认／修订；改名与首条输入异步，旧响应不会确认新名称，维护者卡片实际送达前不宣称审批已转发。
- 普通 SQLite 维护增加可取消的候选／写入预算、持久游标、范围索引与清理诊断；保留未知操作、用量去重和用户取消／放弃证据（未知记录只保护自身，不冻结整个会话或聊天的清理），输出重试候选也分批扫描。
- `/sessions [关键词]` 支持人工名称／原生标题／ID 的当前聊天搜索与签名分页，以新接收人类输入排序；原生标题不改变 `/resume` 的精确解析规则。
- 新增 `/inputs`、`/cancel-input` 和 `/discard-output`：仅撤回可证明尚未交接的输入，图片批次整体处理；放弃同轮依赖交付保留持久决定，不停止模型或伪报 Done。
- 飞书传输增加客户端／端点组并发预算、共享 429 退避与 WS／HTTP 指标；附件仅在明确 `234037` 时使用严格顺序 Range，保持授权、总截止和大小上限。
- 原生基准新增跨聊天并发、时长／轮次受限的持续负载与实时 SQLite／WAL／goroutine／内存／维护趋势，失败保留诊断；Linux／Windows CI 单独记录短时并发观察，默认资源矩阵保持原口径。
- 新增只读 `config explain` 和服务停止后的 `session run --session <完整ID>`；本地终端复用原生身份、目录与模型配置，持有服务锁直到进程树退出确认。
- 回复卡改为紧凑信息流：去掉彩色标题栏，顶部以状态行显示助手与当前状态；思考过程默认折叠并改为中文标题；快捷操作按钮紧凑左排，补充表单改为两行输入和右下实心“发送”；成功交付后移除“重试交付”，只保留查看状态与待回答。不改变 CardKit 流式更新、控件签名／绑定、卡片预算与交付语义。
- 统一紧凑回复卡的文件待送达、未完整送达、取消和放弃状态，避免旧“已回复”状态残留；兼容已保存的旧版卡片，保留同卡收尾及完整交付确认规则。
- 卡片表单补充在原生接收后回执提交者和完整正文（引用格式，转义 HTML／链接标记），`/btw` 回执只标注提交者；移除通用“已交给原生输入流”免责声明提示。
- “查看状态”改为每个聊天一张可折叠状态卡片：消息 ID 持久保存，重复点击原地刷新同一卡片，不追加聊天消息；旧卡片确定不可用时才重建。新增 `status_cards` 表和对应回归测试。
- 新会话启动时用首条人类消息（群聊含作者显示名）设置原生会话显示名，`/rename` 同步更新 Claude Code 会话列表；通过原生 `rename_session` 控制请求实现，不改变 session ID、transcript、输入排队或恢复行为。自动标题与聊天内名称分开保存，`/rename -` 恢复自动标题，此前建立的会话在下一次启动时补设。
- 新增 `groups[]` 兜底绑定 `chatId: "*"`：未显式配置的群默认只允许项目维护者 @ 触发；`members` 追加获准作者，`allowAllMembers: true` 放开该群所有用户。显式群绑定优先，私聊不受影响，作者策略变化不轮换房间键或原生会话。

## 0.4.1 — 2026-11-05

- 恢复 v0.4.0 引入的便携包与安装版发布：修复 deb 安装冒烟在临时目录中使用相对安装包路径导致构建失败的问题。

## 0.4.0 — 2026-11-05

- 发布同时提供便携压缩包和安装版：Windows MSI 按当前用户安装到 `%LOCALAPPDATA%\feishu-bot`，Linux amd64／arm64 deb 安装到 `/usr/bin/feishu-bot`；安装器只复制程序和文档，不注册后台服务。安装包固定时间戳并可复现构建，工作流会重复构建、实际安装运行和卸载。

## 0.3.0 — 2026-11-05

- 新增持久化交付步骤：消息 UUID、冻结请求与首次尝试时间在网络操作前保存，CardKit 创建、文件上传和消息关联分别确认；结果未知保持显式核对，明确限流有界退避，永久拒绝与重试耗尽单独显示。
- 回复卡按嵌套 JSON 的实际字节数预算，覆盖中文、emoji 与转义开销；超限正文导出完整文件，文件待交付时显示独立状态，所有必要交付完成后才添加 Done。
- 审批等待暂停流式状态，流过期转为同卡全量更新；停止、新会话、重启和崩溃恢复保留原卡收尾。增加终态卡片及交付日志保留策略，保护活跃与未确认记录。
- 新增会话范围的命名模型配置（含原生 effort）、会话名称与唯一短 ID、已完成产物取回和带消息／会话／版本绑定的快捷操作；不改变原生 session ID、不启动额外模型任务。
- 扩展 doctor 的已测／未测原生版本、分阶段诊断、数据库／WAL／导出文件占用及显式 `--verify-chat` 首次连接验收。
- 增加 Windows Working Set／Private Bytes 进程树采样、0／1／4 会话与中文长回复／附件／回收恢复资源场景，以及实际构建二进制的独立本地服务验收。
- 新增逐群 `groups[].mentionOptional` 配置：设为 `true` 的群免除 @，群内所有获准作者直接触发；默认仍要求明确 @，群和作者授权不变，修改后需重启。
- 新增仅稳定版本 tag 触发的 release CI：在 Linux amd64／arm64、Windows amd64、macOS amd64／arm64 上验证并打包，生成 SHA-256 与构建信息，完整附件校验后公开 GitHub Release；同一版本重跑核对提交和内容，保留已公开附件。
- macOS 提供原生网关二进制与前台运行说明；使用 Darwin 进程表记录子进程组身份、检查已观测的 PID 复用并区分僵尸，内置 daemon 管理仍限 Linux／Windows。
- 用 Go 替换生产 TypeScript 服务、会话 worker、飞书适配、审批、交付、诊断及固定操作示例；一个服务直接管理持久 Claude 原生进程，运行时不再依赖 Node.js 或 TypeScript Agent SDK。
- 保留既有 JSON 配置、SQLite schema 5、聊天命名空间、原生 session ID 和工作区布局；停止、恢复及 compact 不主动新建会话。
- 通过双向 stream-json 实现原生权限、追问、MCP、hooks、忙时输入与后台来源，固定 CLI 2.1.316 做真实进程／本地模型协议回归。
- 使用 Go 的有界流式文件传输、可取消飞书长连接、持久 CardKit 序号和不确定外部写入恢复。Windows 原生进程使用 Job Object 确认派生进程退出。
- CI 改为 Ubuntu／Windows 的 Go 构建、竞态检测、vet 和独立固定原生 CLI 测试；新增同 fixture 的 Go 基准命令。
- 部署需要独立安装原生 Claude，可通过 `runtime.claudeExecutable` 指定；固定操作正则使用 RE2。详见 [迁移与回滚](docs/go-migration.md)。
- 新增 `daemon install/uninstall/start/stop/restart/status/logs` 与 `config path/example/format`。Windows 任务计划程序直接执行安装目录中的二进制，Linux 使用 systemd；不再需要 launcher/stop 脚本。
- `run` 新增 `--env-file`、`--log-file`、日志轮转和 `--service`；`daemon install` 复制二进制、生成或校验 `service.env`（不把密钥写入任务/unit）、执行离线 doctor 预检，并支持幂等升级与同名任务所有权检查。
- 新增 `<dataDir>/gateway.stop` 控制文件优雅停止；`daemon stop` 等待 `gateway.lock` 释放，不强制杀进程；stale lock 需要显式 `daemon start --reclaim-stale-lock`。
- 修复飞书 post 消息的 `at` 元素使用 mention key（如 `@_user_1`）时机器人名未被剥离，导致 `@机器人 /status` 被当作普通模型输入的问题；群内命令现在按预期识别。

内存收益需要同版本、同配置和同样本测量。真实飞书、真实模型及客户端验收单独进行；历史 TypeScript 测试结果不计作 Go 覆盖。

## 0.2.0 — 2026-11-04

### 本地原生接入

- 新增项目 `cwd`，可直接使用已有目录；与复制资料的 `seedDir`、`trustedConfigDir` 互斥。
- 新增 `nativeConfig: inherit`，沿用服务账户的原生配置、技能、MCP、插件和 hooks。默认仍使用独立工作区与 `acceptEdits`。
- 支持显式原生 `bypassPermissions`，保留真正的追问交互；新增可选 `runtime.claudeExecutable`。
- 固定会话的规范工作目录及配置身份，恢复不匹配时明确报错；共享原生历史位置不扩大聊天恢复权限。

### 回复与交付

- 使用 CardKit 在同一张卡片更新思考与答案，提供状态色标题、紧凑思考区及用时、实际模型、已报告用量、末次请求输入规模和工作目录。
- 持久保存卡片标识、更新序列与快照；不通过重发新卡片处理结果未知的创建请求。
- 修复追问卡片空提交后的再次提交和重复选择，串行刷新同一卡片并保留 `/answer` 恢复方式。
- 可回传当前聊天拥有的入站附件；规范路径与交付快照继续阻止跨聊天或任意外部文件导出。

### 可靠性与资源

- 按原生来源和后台运行标识区分输出，避免后台通知借用后续前台回复；保留原生明确确认消费的输入归属。
- 增加经过脱敏的连接与回调诊断，使用原生 WebSocket 超时检查；支持本地父进程通过 IPC 请求正常关闭。
- 使用同一飞书 SDK 的公开 CommonJS 入口减少模块加载开销；修复后台任务双标识归属缓存超过既定上限后继续增长的问题。
- 保持既有进程保留、后台任务保护和并发配置语义；未通过降低并发、缩短保留时间或强制回收工作集节省内存。

### 验证与限制

本地 Windows 完整 `npm run check` 为 349 项通过、2 项平台条件跳过，包含实际 CC／SDK 配合模拟模型和飞书的回归。真实模型、飞书及客户端验收与模拟证据分别记录；这些结果不代表所有部署、全部模型能力或所有客户端旅程均已验证。

共享 `cwd` 的文件访问属于同一系统账户的信任范围。原生持久定时任务按项目目录保存，可能由同目录的另一原生会话接管，不能作为隔离到飞书聊天的持久调度能力。详情见[部署说明](docs/deployment.md#直接使用本地目录与原生配置)。
