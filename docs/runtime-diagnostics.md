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

这些结构化记录不打印原始错误正文、堆栈、请求头、消息、文件路径或完整嵌套对象。采集仅遍历固定元数据字段，限制深度与条目数，不求值对象 getter。SDK 的 debug／trace／info 仍静默；没有增加日志后台任务、连接重试或消息重放。

分类只解释 SDK 已发出的事件和识别到的元数据，不代表已经确定根因。SDK 内部未发出日志的降级路径仍不可见，不能把本功能称为完整的降级追踪。聊天管理读取和原生执行的其他日志保持原有行为，本页不宣称所有日志来源都已完成审计。

## 验证范围

纯函数测试检查分类、敏感内容省略、未知字段、循环对象和日志写入失败。SDK 边界测试使用锁定的真实 Channel SDK，但替换其 HTTP 请求：令 public `connect()` 在机器人身份查询阶段失败，验证分类保留且未启动 WebSocket，也不增加重试。另一项测试通过重复订阅实际 SDK 事件触发警告，检查生产 logger 接线。

这些不是飞书线上认证或故障演练，也不验证手机／桌面端、消息原始格式或附件交付。首次接入步骤见[首次接入](first-connection.md)，会话与补发操作见[使用场景](usage.md#状态与恢复)。
