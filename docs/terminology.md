# 术语

本页是项目术语的唯一定义位置。定义以当前 Go 实现、配置和[对话行为](conversation-contract.md)为准；新增持久概念时在此补充，并同步相关文档。代码、配置和协议中的标识符保持原样。

**网关**

Go 常驻服务，负责飞书事件接入、授权、会话路由、审批、持久交付、诊断和原生进程生命周期。模型循环、工具、子代理、上下文压缩和后台工作由原生 Claude Code 执行；网关不实现第二套 agent loop。
_英文_: gateway
_避免_: 用 bot、server 指代整个系统

**原生运行时**

Claude Code CLI 进程及其双向 `stream-json` 控制协议。模型、工具、子代理、compact 和后台工作由它执行。
_英文_: native runtime
_避免_: worker（旧 TypeScript 版本的每会话 Node worker）

**会话**

一个房间与一条原生 Claude session 的当前映射。默认（整群范围）群聊一群一条，线程只影响引用和回复位置，不拆分会话；话题范围的群每个话题一条；每个私聊独立。
_英文_: conversation
_避免_: thread、进程（会话不是进程本身）

**会话范围**

`groups[].sessionScope` 决定群聊按什么划分房间：`chat`（默认）全群共用一条当前会话；`thread` 每个飞书话题一个房间和一条会话。私聊不受影响。修改后需重启，原范围的会话保留。
_英文_: session scope

**话题会话**

话题范围群里的会话。群主时间线上的一条新触发消息开启新话题并成为其根消息；话题内的触发消息续接同一会话。只有带 `thread_id` 的消息属于已有话题，普通引用回复不会并入被引用的话题。同一群的话题默认共享工作目录，始终沿用该群的原生 HOME 布局，入站附件按话题分开。
_英文_: topic session
_避免_: 把“线程”泛指为会话

**话题历史范围**

`groups[].topicHistory` 控制 `read_chat_history` 在话题会话中能读什么：默认 `topic` 只读当前话题；`chat` 允许读取本群时间线或其他已观察到的同群话题，仍绝不跨群。群时间线只返回话题根消息和顶层消息，展开其他话题需要逐个读取话题容器；其他话题内容在结果中标为背景。
_英文_: topic history scope

**跨群历史**

私聊通过显式 `dms[].historyChats` 白名单读取目标群的历史。目标必须是 `groups[]` 中显式绑定的群；发起人必须是该 DM 用户，并且是目标群 maintainer 或显式 member，`allowAllMembers` 不算授权。第一版只读文本、不提供附件，每次读取都审计；私聊永远不能被其他会话读取。
_英文_: cross-chat history

**其他机器人**

显式群通过 `groups[].allowOtherBots: true` 允许非本应用机器人入站的设置。入站内容是不可信输入；只有明确 @ 本机器人才触发，同一话题同一机器人最多连续触发 2 轮，人类消息打破连续计数。连续计数在当前进程内维护，服务重启重新开始。它不改变群成员授权、action 或审批；`otherBots` 名单只用于出站提及白名单。
_英文_: other bots
_避免_: 把 `allowOtherBots` 当成机器人信任或管理员授权

**机器人提及**

群内 maintainer 通过 `mention_bot` 向 `otherBots` 白名单中的一个机器人发送一条结构化 post：at 元素使用目标机器人的 `openId`（open_bot_id），可附发起人提供的正文。每轮最多一个目标；正文截断到 2000 字但不改写。网关不等待或汇总对方结果，也不在消息里替用户声明授权。对方对该消息或同话题的回复只进入历史，不触发。
_英文_: bot mention
_避免_: 把 `mention_bot` 当作通用机器人编排或审批通道

**轮次污染标记**

跨群历史读取成功后附加在当前轮次上的写操作限制。写工具、`send_file`、非只读 action 和未知工具需要 DM 用户确认，只读工具放行；非只读 action 的污染确认之后仍按原有 approvers 和禁止自批规则审批；`approval: "none"` 或 `"open"` 的非只读 action 例外，维持直接执行，`action_started` 审计记 `tainted=true`。新的人类输入在后台任务结束后清除标记，`/stop`、超时或 worker 结束会拒绝挂起的污染审批。它只解除本层拦截，不替代原生权限规则。
_英文_: tainted turn

**原生会话 ID**

Claude Code 持久化的 session 标识，用于停止、空闲回收、服务重启和 compact 后的 resume。只有 `/new` 主动轮换；话题范围群中，新的顶层触发开启另一个话题的新会话，不轮换已有话题。恢复失败不会静默替换。
_英文_: native session ID
_避免_: conversation ID、room key

**话题根消息交付**

根消息本身或其引用随某条输入进入原生会话，并由消费确认或对应结果确认的持久事实。成功读取根消息、收到其他输入或执行 compact 不构成该事实；停止、重启和 compact 保留它，新会话独立记录。
_英文_: topic root delivery

**房间键**

由 tenant、app、project、聊天类型、chat ID 和私聊用户 ID 组成的路由键。不包含原生 session ID；整群范围不包含线程。话题范围的群在同样字段后追加话题根消息 ID，整群房间键保持不变。
_英文_: room key

**工作区**

为一个房间准备的工作目录、原生 HOME、附件目录和配置身份；话题范围群默认共用该群的工作目录和原生 HOME，附件目录按话题分开。可由 `seedDir`、`trustedConfigDir`、显式 `cwd` 或话题工作区绑定提供工作目录。
_英文_: workspace

**项目环境变量**

`projects[].env` 的非敏感字面值与 `projects[].envPassthrough` 从服务环境复制的变量，注入该项目的原生进程及其 Bash 子进程。只在下一次原生进程启动时生效，不进入房间键或会话身份；对该项目所有获准聊天可见，不是权限或信任隔离边界。
_英文_: project environment variable
_避免_: 用“项目密钥”指代 `env`（它不保存密钥）

**项目设置文件**

`projects[].settingsFile` 指定的管理员维护的原生 settings 快照。只允许 `env` 和 `permissions.allow/ask/deny`；每次原生进程启动时读取并与模型环境合并成一份临时 `--settings` 文件，读取或校验失败时拒绝该次启动。它是管理员信任输入，不是聊天可控制的规则，也不是安全沙箱。
_英文_: project settings file
_避免_: 把 settings 文件称为隔离边界

**话题工作区绑定**

管理员在显式群的 `threadWorkspaces` 中将话题根消息 ID 映射到已有目录。仅改变该话题的原生 cwd，不改变房间键、原生 HOME、附件范围或项目权限；已有会话需通过持久启动身份核对，目录变化须显式新建会话。绑定可以指向人工准备的 Git worktree，网关不管理其创建与清理。
_英文_: topic workspace binding

**热进程**

已完成初始化、可在后续消息中复用的原生进程。空闲回收后会话历史仍保留，下一条消息按原生 session ID 恢复。
_英文_: warm process

**原生身份未决**

新原生进程启动前保存的持久状态，表示该会话可能已创建原生上下文，但尚未保存其 ID。保存原生 ID 后解除；只有确认进程及事件处理均结束、且从未交接输入或观察到原生身份的启动（包括交接前自行退出的进程），才可按未发送解除。服务重启不会把未决会话当作新会话启动。
_英文_: unresolved native identity

**停止暂停**

停止操作开始时保存的房间暂停。取消待交付内容失败后继续阻止派发，服务重启仍有效；维护者（私聊为本人）重新 `/stop` 并成功取消暂存输入后解除。优先保存在状态库，状态库不可写时使用数据目录的标记文件。
_英文_: stop pause

**回收**

停止已确认空闲的原生进程以释放容量。回收不是 `/new`，不删除会话历史，也不重放未确认输入。
_英文_: reclamation

**聊天进程额度**

`runtime.maxWorkersPerChat` 限定一个聊天可同时占用的原生进程数，同群各话题共用；0 表示只受全局 `maxWorkers` 限制。额度只约束新建进程，不中断已有输入流。
_英文_: per-chat worker quota

**内存准入**

设置 `runtime.maxRssMiB` 后，新建原生进程前检查网关及其后代进程的 RSS 合计；超限先回收确认空闲的进程，仍不足或无法观测时输入排队等待。它是准入信号，不是操作系统硬内存上限。
_英文_: memory admission
_避免_: 内存上限、内存配额

**空闲保温预算**

热进程在空闲回收前保留的时长。默认固定为 `runtime.idleMs`；设置 `runtime.idleMinMs` 后从该值开始，每次成功复用加倍，最多到 `idleMs`。
_英文_: idle budget

**补充**

`/btw` 或 `/ps` 把正文送入正在运行的前台原生输入流；已有排队的 compact 是边界。补充不是旁路问答，不能新建会话或代替审批回答。
_英文_: supplement

**审批**

原生权限请求或固定外部写操作的管理者确认。绑定请求、会话、操作、参数和操作者，一次性生效；停止、超时或重启后失效。
_英文_: approval

**auto 模式**

`projects[].permissionMode: "auto"` 使用的原生权限模式：白名单外操作由 Claude Code 的本地安全分类器判断，不再弹维护者审批卡。分类器使用当前模型凭据和 `baseUrl`，固定模型 `claude-sonnet-5`，每次最多两段请求；deny 规则仍是硬边界，ask 规则仍进入审批。分类器不是安全沙箱，也不替代项目外部权限。
_英文_: auto permission mode
_避免_: 把 auto 称为“无权限”或“自动批准所有操作”

**自动模式分类器事件**

`auto_mode_denied`（分类器拒绝工具）和 `auto_mode_unavailable`（分类器不可用或无法评估，`kind=unavailable|unevaluable`）两种审计。只记录 session、工具名和脱敏短原因；工具输入不进入审计或卡片。
_英文_: auto-mode classifier event

**追问**

Claude Code `AskUserQuestion` 交互。每题支持选项和可选自定义文字，整张表单一次提交；非空自定义文字覆盖该题选项，不跨操作者拼接草稿。客户端缓存失效时使用 `/answer <请求ID> <回答>` 恢复。
_英文_: question

**固定操作**

配置中声明的受控外部执行器及其资源、argv 范围。`readOnly: true` 是管理员对可信执行器的声明，不是自动安全证明；`approval: "card"`（默认）逐次审批，`approval: "none"` 仅在本轮 senders 全为项目 maintainers 时直接执行，`approval: "open"` 在 active senders 非空时直接执行且权限由动作自身判定。
_英文_: project action
_同义_: 项目操作（如[可选项目操作](project-actions.md)的标题）

**直接执行审批**

`actions[].approval` 为 `"none"` 或 `"open"` 的非只读固定操作模式：`"none"` 要求请求上下文 active 且本轮 senders 全部是项目 maintainers；`"open"` 只要求 active 且 senders 非空，权限由动作自身判定。两者都不弹审批卡；senders 只包含请求到达时活动 deliveries 的发送者，不包含缓存背景、引用内容或跨轮次后台进程的延迟调用。本轮 senders 中只要含机器人（包括 `allowOtherBots` 群里的其他机器人），两种直接模式都拒绝并记 `bot_sender=true`；`card` 仍由真人点击。污染轮次维持直接执行，`action_started` 审计记 `tainted=true`。它不是权限提升，也不替代原生权限规则。
_英文_: direct action approval

**预检索上下文**

网关在新一轮人类输入交给原生进程前，按项目 `preContext` 配置执行一次只读固定操作，并把带 `source`、`text`、`truncated` 的结果注入该轮输入。它是不可信的背景材料，不是用户输入、授权、规划步骤或原生工具的替代。
_英文_: pre-context retrieval

**请求上下文**

网关在请求到达时从当前 slot 的 deliveries 和 anchor 得到的发起信息：chat、thread、消息、anchor 发送者 open_id 与显示名、去重后的 senders 和 messageIds。由网关根据飞书事件给出，不是模型报告；没有进行中的输入时视为无上下文。用于固定操作环境、审计和只读 context 端点。
_英文_: request context
_避免_: 用“发起人”指代整个上下文（发起人只是 anchor 发送者）

**本轮提问人**

启动当前轮次的原生 anchor 消息的发送者。回复卡上的「停止本轮」允许该用户或本聊天 maintainer 点击；supplement 或恢复轮次可能让 anchor 继承原请求人，后台/无 anchor 轮次没有提问人时退化为仅 maintainer。提问人写入 HMAC 签名的 quick control，不能由客户端指定。
_英文_: turn requester
_避免_: 把 anchor 发送者当成永久会话所有者

**交付**

把最终回复、卡片更新和必要附件完整送达并确认的过程。本轮成功且全部交付后才标记 Done。
_英文_: delivery
_避免_: send（单次发送不等于完整交付）

**待补存用量**

原生结果已报告、但尚未成功写入用量统计的持久观测。只保留结果 ID、报告字段和观测时间；补存沿用结果 ID 去重，不重做任务或重发答案。存在待补存观测时累计总量未知，已保存小计仍可查询。
_英文_: pending usage observation

**回复卡**

一轮回复使用的同一张飞书卡片，包含思考、答案和状态栏。工具轨迹默认不展示，审批和追问仍可交互。
_英文_: reply card

**回复反馈**

在最终 Done [回复卡](#回复卡)上记录的「有用」「无用」投票。按 room、回复输出和 open_id 记录，每人每回复只能一票，不能改票；按钮显示实时计数，点击只记录、提示并按元素更新两个按钮，不重写整张卡片。快照存于 `feedback` 表，保留 90 天，可导出为评估集候选。
_英文_: reply feedback
_避免_: 用“点赞”指代需要导出的反馈

**反馈归档**

开启 `feedback.export` 后，维护清理前按项目和 UTC 日期写出的 JSONL 文件，以及 `feedback_exports` 中记录已归档位置的 `(at,id)` 水位。归档失败会暂停 `feedback` 保留策略，避免先删除后导出；手工 `feedback export` 不改变该水位。
_英文_: feedback archive
_避免_: 把手工导出称为“归档”，或把归档水位当作反馈处理进度

**状态卡片**

每个聊天最多一张、由“查看状态”刷新同一消息的可折叠卡片。消息 ID 持久保存在 `status_cards` 表；重复点击原地刷新，只有确定旧消息不可用时才重建。
_英文_: status card

**不确定写入**

已发起但结果未确认的外部写操作。不得自动重放；需人工核对或显式 `/retry-output`。
_英文_: uncertain write

**背景预注入**

非 @ 群消息作为下一条提问隐藏前缀的行为。默认关闭（`limits.backgroundMessages = 0`）；需要前文时通过 `read_chat_history` 按需读取。
_英文_: background pre-injection

**免 @ 群**

配置 `groups[].mentionOptional: true` 的群。该群所有获准作者（`members` 与项目 `maintainers`）的每条消息直接触发，无需明确 @；群和作者授权不变。默认关闭，仅启动时读取。
_英文_: mention-optional group
_避免_: 开放群、免授权群

**兜底群绑定**

`groups[]` 中 `chatId: "*"` 的唯一绑定，覆盖未显式绑定的群。默认仅项目维护者可在这些群触发；`members` 增加获准作者，`allowAllMembers: true` 允许该群任意用户。显式群绑定优先，私聊不受影响。默认不配置，仅启动时读取。
_英文_: default group binding
_避免_: 开放群、通配符成员

**允许全部群成员**

群绑定上的 `allowAllMembers: true`。该群任意用户都是获准作者，仍需明确 @，除非同时设置 `mentionOptional`。仅建议用于成员很少且互信的小群。
_英文_: allow-all-members
_避免_: 免授权群

**维护者**

配置中获准管理共享会话、执行管理命令和批准审批的用户。与普通获准成员不同。
_英文_: maintainer

**项目 owner**

`projects[].owners` 中的 maintainer 子集。唯一额外含义是：开启 `forbidSelfApproval` 后仍可批准自己发起的请求；不获得其他管理或审批权限。
_英文_: project owner

**指定审批人**

`actions[].approvers` 中为该固定操作指定的审批人集合。配置后替代项目 maintainers，不隐式加入 maintainers 或 owners；群内点击者还必须是该群授权作者。`approval: "none"` 或 `"open"` 的 action 不使用这个集合。
_英文_: designated approver
_避免_: 把指定审批人等同于 maintainer

**自批**

请求发起人批准自己的请求。`forbidSelfApproval` 默认关闭；开启后非 owner 的发起人不能被批准自己的请求，但可以拒绝；请求上下文为空时只有 owner 可以批准。判断依据是请求上下文中的 open_id，不是显示名。
_英文_: self-approval

**模型配置**

管理员定义的一组原生模型、轻量模型和 effort 选项。会话保存当前与待应用选择，在安全轮次边界续接同一原生 ID；不包含端点、凭据、目录或权限覆盖。
_英文_: model profile

**项目默认模型配置**

项目通过 `projects[].modelProfile` 指定的命名模型配置或 `default`。只对从未显式选择配置的会话生效；会话显式选择（包括显式 `default`）优先。改变项目默认不改变原生会话 ID，未选中配置的会话在下次原生进程启动或续接时使用新模型。
_英文_: project default model profile

**会话名称**

当前聊天内唯一、可修改的会话标签，同时作为原生会话在 Claude Code 会话列表中的显示名。名称与唯一短 ID 只查找本房间已有映射，不替代原生 ID 或导入其他聊天历史；未命名时使用首条消息派生的自动标题，清除名称后恢复该标题。
_英文_: session name

**交付步骤**

一次逻辑交付中的独立外部操作，例如 CardKit 创建、文件上传或消息关联。请求、幂等标识（接口支持时）、首次尝试和确认结果分别持久化，不以一步成功代替完整交付。
_英文_: delivery step

**快捷操作绑定**

签名控件与实际消息、当前房间／会话、轮次或输出修订及期限的关联。回调时重新授权，停止与补发声明一次生效，旧修订不能操作新轮次。
_英文_: quick control binding

**便携版**

以压缩包发布的网关二进制及文档。解压后在所选目录运行，不写入系统安装记录；升级时替换已核对的二进制并保留配置与数据。
_英文_: portable package

**安装版**

以平台安装包发布的同一网关二进制及文档：Windows MSI 按当前用户安装到 `%LOCALAPPDATA%\feishu-bot`，Linux deb 安装到 `/usr/bin/feishu-bot`。安装器不注册后台服务；常驻运行仍由 `daemon install` 管理。
_英文_: installer package
_避免_: 用安装版指代 daemon 服务安装

**会话目录活动时间**

最近一次新接收的人类输入时间，用于当前聊天的会话搜索排序。原生输出、管理查询与输入重放不更新它；无记录的旧会话以创建时间回退。
_英文_: last admitted input time

**标题同步修订**

一次已保存标题目标的版本。目标值和原生 ACK 的确认值分开，旧修订的迟到响应不能确认新目标。
_英文_: title sync revision

**交付放弃**

维护者（私聊为本人）明确停止交付来源绑定可核实的同轮回复及必要文件，包含后到的同轮补交；原轮结束后才完成的后台结果不受影响。该决定保留，不代表送达、模型停止或远端消息撤回，也不产生 Done。
_英文_: delivery discard

**安装恢复副本**

`daemon install` 在停止旧服务前保存的旧二进制、`service.env`、`daemon.json` 与服务定义，位于安装目录下的 `.feishu-bot-upgrade-*` 私有目录。安装失败时据此回滚，回滚成功后删除；回滚不完整时保留并在错误中给出路径。含凭据，按 `service.env` 同等保护。
_英文_: installation recovery copy

**本地终端交接**

服务停止后，持同一个服务锁在本地原生终端恢复已保存的会话身份；进程树退出确认后才释放锁。没有两个写入者同时接管，不额外建立网关 agent loop。
_英文_: local terminal handoff

**认领话题**

用户在本地 Claude Code 终端显式发起、由 `dms[].adoptRoots` 允许并绑定到本人私聊的飞书话题。一张 `adopted_topics` 记录保存房间键、原生 session、`thread_id`、根消息、登记工作目录和状态。只有 open 状态且 `thread_id` 与根消息同时精确匹配的入站进入该话题；pending／uncertain 只报告状态，不启动原生进程、不重放外部写入。工作目录取自本地 transcript 并每次按 adoptRoots 重新校验。
_英文_: adopted topic
_避免_: 把认领话题称为普通话题会话或群话题；它只存在于私聊

**终端占用**

认领话题登记的本机 Claude Code 进程身份（PID 与可得时的启动时间）。进程仍存活时，话题消息只排队并提示等待，网关不启动第二个原生进程；进程退出或启动时间确认不同后释放。`/new` 通过切换到新会话解除当前占用，不结束本机进程。
_英文_: terminal holder
_避免_: 把终端占用称为锁或租约；它不是文件锁，也不阻止其他管理命令
