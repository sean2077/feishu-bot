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

一个聊天与一条原生 Claude session 的当前映射。群聊一群一条，每个私聊独立；线程只影响引用和回复位置，不拆分会话。
_英文_: conversation
_避免_: thread、进程（会话不是进程本身）

**原生会话 ID**

Claude Code 持久化的 session 标识，用于停止、空闲回收、服务重启和 compact 后的 resume。只有 `/new` 主动轮换；恢复失败不会静默替换。
_英文_: native session ID
_避免_: conversation ID、room key

**房间键**

由 tenant、app、project、聊天类型、chat ID 和私聊用户 ID 组成的路由键。不包含线程，也不包含原生 session ID。
_英文_: room key

**工作区**

为一个房间准备的目录、原生 HOME、附件目录和配置身份。可由 `seedDir`、`trustedConfigDir` 或显式 `cwd` 提供。
_英文_: workspace

**热进程**

已完成初始化、可在后续消息中复用的原生进程。空闲回收后会话历史仍保留，下一条消息按原生 session ID 恢复。
_英文_: warm process

**回收**

停止已确认空闲的原生进程以释放容量。回收不是 `/new`，不删除会话历史，也不重放未确认输入。
_英文_: reclamation

**补充**

`/btw` 或 `/ps` 把正文送入正在运行的前台原生输入流；已有排队的 compact 是边界。补充不是旁路问答，不能新建会话或代替审批回答。
_英文_: supplement

**审批**

原生权限请求或固定外部写操作的管理者确认。绑定请求、会话、操作、参数和操作者，一次性生效；停止、超时或重启后失效。
_英文_: approval

**追问**

Claude Code `AskUserQuestion` 交互。卡片按操作者保存选择；客户端缓存失效时使用 `/answer <请求ID> <回答>` 恢复。
_英文_: question

**固定操作**

配置中声明的受控外部执行器及其资源、argv 范围。`readOnly: true` 是管理员对可信执行器的声明，不是自动安全证明。
_英文_: project action
_同义_: 项目操作（如[可选项目操作](project-actions.md)的标题）

**交付**

把最终回复、卡片更新和必要附件完整送达并确认的过程。本轮成功且全部交付后才标记 Done。
_英文_: delivery
_避免_: send（单次发送不等于完整交付）

**回复卡**

一轮回复使用的同一张飞书卡片，包含思考、答案和状态栏。工具轨迹默认不展示，审批和追问仍可交互。
_英文_: reply card

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

**维护者**

配置中获准管理共享会话、执行管理命令和批准审批的用户。与普通获准成员不同。
_英文_: maintainer

**模型配置**

管理员定义的一组原生模型、轻量模型和 effort 选项。会话保存当前与待应用选择，在安全轮次边界续接同一原生 ID；不包含端点、凭据、目录或权限覆盖。
_英文_: model profile

**会话名称**

当前聊天内唯一、可修改的会话标签。名称与唯一短 ID 只查找本房间已有映射，不替代原生 ID 或导入其他聊天历史。
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
