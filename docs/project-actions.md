# 可选项目操作

[固定操作](terminology.md)是配置中声明的受控外部执行器。优先复用已有可信 CLI、MCP 和原生技能；固定操作适用于需要明确资源范围和审批的外部系统，不是正常文件处理或编码工具的替代品。

未配置 `actions` 时不注册空操作目录；原生中继仍会启动，用于只读的请求上下文查询，但不接受任何 action 调用。所有示例默认未启用，不是本项目的安装依赖。

`projects[].preContext` 可以复用其中一个 `readOnly: true` 的操作，在新一轮人类输入前执行检索并注入带出处的背景材料。它沿用相同的可信 executable、固定 argv、参数 pattern／maxLength、env／vars 和审计；参数必须作为独立 argv 放在字面量 `--` 之后。该操作仍会通过 `project_action` 暴露给模型，配置和边界见[预检索上下文](deployment.md#预检索上下文)。

## 示例与配置

[project-actions.json](../examples/project-actions.json) 是可合入 `projects[].actions` 的配置数组。替换执行器绝对路径、虚构资源 ID 和环境变量引用后再使用。

| 操作 | 范围 | 审批 |
|---|---|---|
| jenkins-status / jenkins-build | 固定 job 查询或构建 | 查询自动，构建逐次 |
| gitlab-publish | 文件快照应用到新分支并创建 MR，不合并 | 快照与参数逐次批准 |
| feishu-doc-read / feishu-doc-append | 固定文档读取或追加 | 读取自动，追加逐次 |
| feishu-table-list / feishu-table-update | 固定表和允许字段 | 读取自动，更新逐次 |

`vars` 是固定非敏感设置，`env` 引用服务进程环境变量，`parameters` 约束 agent 提供的字符串。固定操作只继承少量基础环境变量：`PATH`、`SystemRoot`、`WINDIR`、`COMSPEC`、`PATHEXT`、`LANG`、`LC_ALL`、`TZ`、`TEMP`、`TMP`，以及 Windows 上常用的 `LOCALAPPDATA`、`APPDATA`、`USERPROFILE`、`SystemDrive`、`ProgramData`、`ALLUSERSPROFILE`；其他服务环境变量不会自动继承，`vars` 显式配置的同名值覆盖这些继承值。`readOnly: true` 的操作自动执行，其余逐次由维护者审批；`readOnly` 是管理员对可信执行器的声明，不是自动安全证明，不能把写操作标为只读以绕过审批。`actions[].env` 引用的源变量名同时是项目环境变量的保留名，不能通过 `projects[].env` 或 `envPassthrough` 注入原生进程；固定操作的凭据只在操作子进程中提供。

`actions[].approval` 可选，默认 `"card"`；非只读操作逐次弹审批卡。设为 `"none"` 时改为直接执行，但仅当本轮请求上下文 active 且 `senders` 全部是该项目 `maintainers`；否则返回 `{"ok":false,"error":"需要管理员发起"}`，不回退到审批卡。`senders` 只包含请求到达时仍在活动的 deliveries；缓存背景、引用内容以及跨轮次后台进程的延迟调用不参与判定。直接执行仍注入 `FEISHU_REQUEST_*`，不设置审批人环境变量；`action_runs.approval` 记为 `none`。污染轮次（读过其他群）仍是直接执行，但 `action_started` 审计记 `tainted=true`。`approval: "none"` 不使用 `approvers`，也不适用自批规则。`readOnly: true` 与 `approval: "none"` 同时配置是多余的，`doctor` 给出 warning。管理员应只对可回滚、范围受限的操作使用该模式。

`actions[].approval: "open"` 也只对非 `readOnly` 操作生效：不检查 maintainer，只要求请求上下文 active 且 `senders` 非空；调用时没有活动请求（inactive）才返回 `{"ok":false,"error":"需要活跃的用户请求"}`，不回退到审批卡。后台进程若在后续活动轮次发起调用，会归属该轮次。执行时同样注入 `FEISHU_REQUEST_*`，不设置审批人环境变量，`action_runs.approval` 记为 `open`；污染轮次照常直接执行并记 `tainted=true`。权限由动作自身判定（例如 wrapper 读取 roles）；`doctor` 会给出 info 提示，`readOnly: true` 与 `approval: "open"` 同时配置时给出冗余 warning。

`open` 只证明调用时有活动 senders，不证明是哪一个 sender 发起；机器人 sender 已被网关拒绝，但多个人类 sender 仍不能只按 anchor 判定。动作必须按 `FEISHU_REQUEST_SENDERS` 全集判定，要求每个 sender 都在自身允许名单内，未列入者自然拒绝；不能只信 `FEISHU_REQUEST_SENDER_OPEN_ID`。跨轮次后台进程的延迟调用会按调用时的活动请求归属；动作不能把请求上下文当作调用来源证明，应使用自身可信的权限系统。

`none` 和 `open` 都拒绝含机器人 sender 的轮次：本轮 `senders` 中只要有一个 `SenderIsBot`（包括 `allowOtherBots` 群里的其他机器人），返回 `{"ok":false,"error":"机器人消息不能触发写操作"}`，`action_direct_rejected` 审计记 `bot_sender=true`。`card` 不受影响，仍由真人点击审批卡。

`actions[].timeoutMs` 默认 60 秒；`readOnly: true` 的 action 上限 10 分钟，非只读 action 上限 30 分钟。超过 10 分钟的 action 由 `doctor` 给 info。超时、取消或输出超限时，网关先发中断信号（Unix `SIGTERM`，Windows 尽力发送 `Ctrl+Break`），最多等待 5 秒让 wrapper 执行 finally（释放租约等），然后强制结束整个进程树；结果按失败处理，不自动重放。

`actions[].approvers` 可选，用 open_id 列表指定该操作的审批人；配置后只认这个列表，不再使用项目 maintainers，也不会隐式加入 maintainers 或 owners；空列表按未配置处理，沿用 maintainers。approvers 不要求是 maintainers，但群内审批卡还要求点击者是该群授权作者，否则无法审批，只能等待超时；`doctor` 无法验证群成员关系。私聊中需要审批时，卡片会转发到 approvers 的私聊，而不是 maintainers。未配置 approvers 时仍使用项目 maintainers。原生工具 ask 始终使用 maintainers，不使用 action 的 approvers。自批规则和 owner 例外见[审批资格与自批](deployment.md#审批资格与自批)。`approval: "none"` 或 `"open"` 的 action 不经过这条审批卡路径。

`workspaceFile` 在审批前保存文件快照和 SHA256，批准后读取快照。GitLab 示例在独立的干净 checkout 中应用补丁，禁用 hooks 和全局 Git 配置，不在可修改项目中运行携带业务凭据的脚本。

示例使用编译后的 `project-operations` 二进制，Windows 配置对应 `.exe` 的绝对路径。不要把输入拼入 `sh -c`、PowerShell 或动态脚本；固定 argv 并不自动证明执行器的业务逻辑安全。

## 原生后台工具

配置业务操作后，原生 Bash／Monitor 可使用相同入口：

```sh
"$FEISHU_ACTION_CLIENT" action jenkins-status '{}'
```

权限与 MCP `project_action` 相同：写操作默认仍等待维护者批准；`approval: "none"` 或 `"open"` 的 action 按直接执行规则判定。这只是调用通道，不新增 cron 或监控调度器。原生进程停止后接口失效，恢复会话不重启旧 Monitor。

## 请求上下文

请求上下文是网关在请求到达时从当前 slot 的 deliveries 和 anchor 得到的发起信息，不是模型自己报告的内容。它用于固定操作子进程环境、审计和原生 Bash 的只读查询。

固定操作子进程会获得以下变量；空值不设置：

| 变量 | 内容 |
|---|---|
| `FEISHU_REQUEST_CHAT_ID` | anchor 消息所在的 chat ID |
| `FEISHU_REQUEST_CHAT_TYPE` | `group` 或 `p2p` |
| `FEISHU_REQUEST_THREAD_ID` | 话题 ID；非话题输入没有该变量 |
| `FEISHU_REQUEST_MESSAGE_ID` | anchor 消息 ID |
| `FEISHU_REQUEST_SENDER_OPEN_ID` | anchor 发送者 open_id |
| `FEISHU_REQUEST_SENDER_NAME` | 飞书事件中的发送者显示名；可能没有 |
| `FEISHU_REQUEST_SENDERS` | 当前 deliveries 去重后的发送者 open_id，逗号分隔 |

没有进行中的 delivery 时（例如轮次已结束、后台任务、定时任务）视为无上下文：不设置这些变量，审计为空，审批卡不显示发起人。网关不会退回到最近一条消息，也不猜测发起人。上下文在 action 请求到达时取得，审批等待期间 deliveries 变化不会改变本次记录。

非只读操作经审批卡批准后，子进程另获得以下审计变量（只读操作和 `approval: "none"`／`"open"` 的直接执行不设置）：

| 变量 | 内容 |
|---|---|
| `FEISHU_ACTION_REQUEST_ID` | 最终批准该操作的审批请求 ID，与 `requests.id` 一致 |
| `FEISHU_ACTION_APPROVER_OPEN_ID` | 最终点击批准的审批人 open_id；系统拒绝/超时不会执行操作 |
| `FEISHU_ACTION_APPROVED_AT` | 最终批准时间的 RFC3339 UTC |
| `FEISHU_ACTION_TAINT_CONFIRMER_OPEN_ID` | 污染轮次中先完成私聊确认的 open_id；非污染轮次没有该变量 |

这些变量由网关在清理后的环境上设置，不能由 `actions[].env`／`vars` 或项目环境伪造。

`actions[].env` 和 `actions[].vars` 的目标键不能以 `FEISHU_REQUEST_` 或 `FEISHU_ACTION_` 开头；项目环境变量和 settings 文件也保留这两个前缀。

审计与状态：

- `action_runs.requester` 保存 anchor 发送者 open_id，`action_runs.request_context` 保存上下文 JSON；两列可空，旧库升级时自动追加。`action_runs.request` 和 `action_runs.approver` 保存最终审批请求 ID 和审批人 open_id，`taint_request` 保存污染轮次的私聊确认请求 ID；均可空。`approval` 保存 `card`、`none`、`open` 或 `readonly`，区分审批卡、两种直接执行和只读自动执行。保留期沿用 `action_runs` 的 90 天。
- `action_started` 审计事件包含 `requester` 和 `messageId`，不写显示名；同时记录 `approval`，`approval: "none"`／`"open"` 的直接执行还记录 `tainted`。`action_direct_rejected` 记录 `approval`、`reason` 和可得的 `requester`／`messageId`；执行后的完整 sender 集合在 `action_runs.request_context`，被拒绝的调用不写 `action_runs`。
- 非只读操作的审批卡摘要增加一行「发起人：<转义后的显示名>（`open_id`）」；没有显示名时只显示 open_id。显示名来自事件，经过控制字符和空白清理以及 markdown 转义，不额外调用通讯录 API。

原生进程始终启动一个只绑定 `127.0.0.1` 的中继；即使项目没有配置 `actions`，也可以通过 `FEISHU_ACTION_CLIENT`、`FEISHU_ACTION_URL` 和 `FEISHU_ACTION_TOKEN` 查询只读上下文：

```sh
"$FEISHU_ACTION_CLIENT" context
```

输出示例（虚构 ID）：

```json
{
  "active": true,
  "chatId": "oc_example_group",
  "chatType": "group",
  "threadId": "om_example_thread",
  "messageId": "om_example_message",
  "senderOpenId": "ou_example_member",
  "senderName": "Example Member",
  "senders": ["ou_example_member"],
  "messageIds": ["om_example_message"]
}
```

没有进行中的输入时输出 `{"active":false}`。中继使用每个原生进程独立的 token，只返回本 worker 所在 slot 的数据；没有 action 的项目对 action 调用返回错误。Bash 的子进程会继承这些变量，下游工具可以自行调用客户端，不需要模型拼接命令。

上下文反映查询时刻该原生进程正在处理的输入。后台任务应在启动时读取并保存；轮次结束后再查询会得到 inactive，下一轮开始后查询会得到下一轮的发起人。只提供 chat、消息和发送者 ID，不构造飞书消息链接，由下游自行拼接。

## 资料、技能与服务身份

`seedDir` 提供获准使用的初始资料或源码，首次复制并建立 Git baseline，后续不覆盖修改。审核后的 `.claude` 内容可放在 `trustedConfigDir`；首次准备时复制到会话工作区，已有工作区的更新需显式处理。

业务密钥不默认注入 CC，但同一系统账户下的任意代码可能访问该账户其他数据。通过外部 API scope、受保护资源和专用服务身份控制权限，不将环境白名单描述成强隔离。

各服务的认证、资源权限和错误语义需联调。Jenkins 示例使用 API token 与 REVISION 参数；表格示例仅读取第一页及分页信息，不宣称返回全部记录。结果未知的写操作不自动重试，应先核对远端状态；审批绑定的冻结参数与快照在停止、超时或重启后失效。

接口参考：[Jenkins](https://www.jenkins.io/doc/book/using/remote-access-api/) · [GitLab MR](https://docs.gitlab.com/api/merge_requests/) · [飞书文档子块](https://open.feishu.cn/document/server-docs/docs/docs/docx-v1/document-block-children/create)

## 构建 Go 示例执行器

```sh
go build -trimpath -o ./bin/project-operations ./examples/actions/project-operations
```

将生成的受信任二进制部署到配置的绝对路径；Windows 使用 `.exe`。`examples/project-actions.json` 的默认路径是示例，部署时按实际位置修改。执行器不随聊天工作区复制，也不接受模型提供的 URL、仓库、基础分支或 HTTP 方法。网关与示例均不需要 Node.js。
