# 文档索引

本页说明文档状态、所有权和新文档放置位置。面向使用者的入口仍是 [README](../README.md)；本页面向修改代码或文档的 Agent 和维护者。

## 当前文档

| 文档 | 内容 | 状态 |
|---|---|---|
| [README](../README.md) | 安装、配置、命令和使用边界 | 当前 |
| [架构](architecture.md) | Go 服务与 Claude Code 的模块边界、会话和并发模型 | 当前契约 |
| [对话行为](conversation-contract.md) | 授权、会话、输入、审批、compact 和交付行为 | 当前契约 |
| [部署与配置](deployment.md) | 飞书应用、配置、daemon、升级和恢复 | 当前 |
| [首次连接](first-connection.md) | 首次接入检查 | 当前 |
| [使用场景](usage.md) | 使用场景、追问卡片、状态和用量 | 当前 |
| [原始消息](raw-messages.md) | 入站消息格式支持边界 | 当前 |
| [图片批次](image-batches.md) | 图片合批配置和恢复规则 | 当前 |
| [补充语义](supplements.md) | `/btw`、`/ps` 的补充语义和竞态 | 当前 |
| [固定操作](project-actions.md) | 固定操作和示例执行器 | 当前 |
| [运行诊断](runtime-diagnostics.md) | 运行诊断 | 当前 |
| [运行基线](runtime-baseline.md) | 性能与内存比较口径 | 当前 |
| [验证与验收](validation.md) | 测试、平台 CI 和真实验收边界 | 当前 |
| [版本发布](releasing.md) | tag 触发、版本与产物、发布验证和失败重跑 | 当前 |
| [公开发布检查](publication.md) | 公开发布前检查 | 当前 |
| [Go 迁移](go-migration.md) | TypeScript 到 Go 的迁移、部署调整和回滚 | 当前迁移说明 |
| [术语](terminology.md) | 项目术语的唯一定义位置 | 当前 |
| [权限与信任范围](../SECURITY.md) | 权限、信任范围和凭据 | 当前 |
| [开发与贡献](../CONTRIBUTING.md) | 开发、回归、提交和发布 | 当前 |
| [版本记录](../CHANGELOG.md) | 版本记录；`Unreleased` 是尚未打标签的变更 | 权威记录 |

## 历史与证据

- [迭代状态](iteration-status.md) 和 [PR #5–#47 记录](iteration-status-through-47.md) 保留 TypeScript 版本的迭代证据，不描述当前 Go 行为，也不计入 Go 测试结果。
- [benchmarks/](benchmarks/) 保存迁移基线样本和补丁；它们是比较材料，不是当前性能结论。
- Git 历史和 PR／CI 记录是历史证据。文档中的“已修复”“已验证”仍须按[验证与验收](validation.md)区分模拟测试、平台 CI 和真实飞书／模型验收。

## 放置新文档

- 面向使用者的安装、配置和操作说明放在 `docs/`，并从 README 或本索引链接。
- 当前行为契约放在[架构](architecture.md)或[对话行为](conversation-contract.md)；不要另建同义契约。
- 一次性问题记录、迁移证据和验收日志留在 PR 描述或历史迭代文档中，不写成当前契约。
- 术语定义放在[术语](terminology.md)；不要在多个页面重复定义同一概念。
- 文档使用中文；代码标识符、协议字段和命令保持原样。示例使用虚构 ID 和保留域名。
