# 运行诊断

先用聊天中的 `/status` 区分输入处理、审批等待和输出交付，再按时间查服务标准错误流中的 JSON 日志。`/outputs` 用于核实未完成交付；不要用重新发送原任务代替恢复已保存的回复。

## 飞书 SDK 警告与错误

`feishu_warning`、`feishu_sdk_error` 和 `feishu_error` 保留原事件名，增加 `category`、`summary` 与 `nextStep`。识别到时附带已有 SDK `code`、`transportCode`、`httpStatus` 和已知的 `apiCode`，没有证据的字段不填。`code` 仅保留已知 SDK 错误分类，不输出任意提供方字符串。

| 分类 | 核查方向 |
|---|---|
| `permission_denied` | 应用凭据、发布状态、接口权限；身份检查可单独显式运行 `doctor --probe-feishu`。 |
| `rate_limited` | 请求频率与待交付记录，不重复执行原任务。 |
| `send_timeout` / `network_error` | 网络、DNS、代理；涉及发送时先核实飞书是否已经接收。 |
| `not_connected` | 连接或机器人身份查询未完成，不能仅据此断定网络故障。 |
| `format_error` / `target_revoked` | 对应消息或卡片格式、目标消息与聊天可见性。 |
| `ssrf_blocked` | 核实资源来源，不关闭地址检查来绕过拒绝。 |
| `http_error` / `unclassified` | 保留可用状态码或标明未分类，再结合本地检查和聊天状态定位。 |

这些结构化记录不打印原始错误正文、堆栈、请求头、消息、文件路径或完整嵌套对象。采集仅遍历固定元数据字段，限制深度与条目数，不求值对象 getter。SDK 的 debug／info 仅放行下文列出的固定信号，trace 仍静默；不放行原始正文，也不重放业务消息。

分类只解释 SDK 已发出的事件和识别到的元数据，不代表已经确定根因。SDK 内部未发出日志的降级路径仍不可见，不能把本功能称为完整的降级追踪。聊天管理读取和原生执行的其他日志保持原有行为，本页不宣称所有日志来源都已完成审计。

## 验证范围

纯函数测试检查分类、敏感内容省略、未知字段、循环对象和日志写入失败。SDK 边界测试使用锁定的真实 Channel SDK，但替换其 HTTP 请求：令 public `connect()` 在机器人身份查询阶段失败，验证分类保留且未启动 WebSocket，也不增加重试。另一项测试通过重复订阅实际 SDK 事件触发警告，检查生产 logger 接线。

这些不是飞书线上认证或故障演练，也不验证手机／桌面端、消息原始格式或附件交付。首次接入步骤见[首次接入](first-connection.md)，会话与补发操作见[使用场景](usage.md#状态与恢复)。
## 连接与卡片回调诊断

连接启用锁定 SDK 的原生 pong 看门狗：发出 ping 后30秒内没有收到任何入站帧则终止旧连接，由 SDK 既有重连流程建立替代连接；ping 周期仍由飞书服务端提供。单次握手等待上限为10000毫秒。这不重放业务输入，也不证明某次卡片回调缺失来自连接故障；普通消息可达时，卡片订阅、客户端投递和 SDK action 去重仍需分别核对。

Channel 0.7.1 的可选应用级 `keepalive` 保持关闭：其 HTTP 探测等待期间关闭后，迟到探测仍可能触发重新连接。原生 pong 看门狗与该独立定时器不同。本地真实 WebSocket 测试验证无 pong 时先关闭旧 socket 再重连、显式 close 后不复活，以及服务器不完成升级时握手超时；它们不是飞书线上故障复现。

日志仅保留白名单信号，例如 `ws_reconnecting`、`ws_reconnected`、`ws_pong_timeout`、`ws_handshake_timeout`、`card_duplicate_dropped`、`card_inflight_dropped`、`card_handler_missing`。`card_callback_started/completed/failed` 记录公开回调入口、耗时及是否返回提示；不记录 action value、消息正文、用户或聊天标识、连接 URL 和原始错误。没有入口日志只能说明未到应用 handler，不能单凭这一点断言平台未发送；先查看 SDK 去重／在途信号和连接状态，再核对平台及客户端。

`feishu_policy_rejected` 来自 SDK 的公开拒绝事件，只记录拒绝原因及消息、聊天、作者标识的截断哈希，不保存消息正文或资源 key，不进入 Gateway 或触发回复、回执、下载和模型。真实拒绝验收须用测试消息哈希对应入口记录，并核对零后续副作用；仅观察机器人未回复不足以证明消息已到达权限检查。
