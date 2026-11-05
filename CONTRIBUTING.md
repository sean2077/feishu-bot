# 开发与贡献

需要 Go 1.25+ 和 Git；真实原生协议测试还需要 Claude Code 2.1.316。安装依赖后运行检查：

```sh
go mod download
go test -race ./...
go vet ./...
go build -trimpath -o ./bin/feishu-bot ./cmd/feishu-bot
```

依赖通过 `go.mod`／`go.sum` 固定。保持无 CGO 的生产构建，SQLite 使用 `modernc.org/sqlite`；竞态检测需要平台的 C 工具链。变更依赖后运行 `go mod tidy` 并检查差异。真实 CLI 测试使用 `CLAUDE_NATIVE_PATH` 指向固定版本的原生二进制，未设置时明确跳过。

## 设计边界

飞书是消息入口，Claude Code 是执行运行时。不同使用场景优先通过资源、权限、技能和固定操作配置实现，不增加第二套意图分类、agent loop、压缩器或定时调度器。

会话按需启动、跨轮复用；进程回收不删除会话历史。后台工作、审批、未确认输入和未知状态不能被当作空闲。不要把共享系统账户下的文件夹描述成安全沙箱。

## 回归要求

变更消息或运行时逻辑时，覆盖忙时输入归属、群与私聊路由、引用和附件、审批回调、停止与恢复、容量回收及结果未知时的处理。涉及平台差异时分别验证 Linux、macOS 和 Windows；架构相关修改还须验证 amd64／arm64。macOS 的网关可通过 `run` 运行，内置 `daemon` 管理仅支持 Linux／Windows。

原生运行时测试使用真实 CC 与本地模拟模型，不需要真实服务密钥。CI 仅在安装固定测试二进制时使用 npm；生产网关及每个会话均不加载 Node.js 或 TypeScript SDK。模拟测试、平台 CI 和实际飞书／模型服务验收必须分别记录。

## 模块

`internal/core` 是跨模块的消息和运行时接口；`config` 负责严格配置；`store` 负责兼容 SQLite；`workspace` 负责目录身份和文件快照；`native` 负责 Claude 双向协议；`process` 为会话、固定操作和诊断提供统一的进程生命周期；`feishu` 负责平台 HTTP、长连接和 CardKit；`gateway` 负责会话、授权、审批和持久交付；`cli` 只负责启动、诊断和关闭。

不要在数据库事务内再次通过 Store 调用数据库；当前使用单连接来保持事务顺序。网络操作不应持有入站锁或 SQLite 事务。增加异步操作时同时处理取消、迟到返回、授权变化及结果未知的情形。

## 提交与交付

现有历史混用 `feat:`、`fix:`、`test:`、`docs:`、`refactor:` 等前缀和句子式标题；仓库没有提交信息检查钩子。发布分析按 Conventional Commit 前缀推断版本，未分类提交需要人工确认。一次提交只包含本次任务的改动；不要顺手加入 `bin/`、本地配置、状态库、日志或凭据，`.gitignore` 只覆盖未跟踪文件。

完成边界按请求区分：本地提交、推送分支、PR、合并和发布互不授权。CI 在 push `main` 和 PR 上运行；合并前核实实际 CI 结果，不用工作流存在代替通过。

## 版本与发布

`CHANGELOG.md` 是版本记录权威，`Unreleased` 汇总尚未打标签的变更。标签使用 `vMAJOR.MINOR.PATCH`，当前已有 `v0.3.0`。Go 模块没有单独的包版本文件；当前发布以 `CHANGELOG.md` 和标签为准。

独立的 [release 工作流](.github/workflows/release.yml) 只监听稳定版本 tag；在精确的 tag 提交上完成五个平台的原生测试、构建、便携包与安装包冒烟，完整附件上传并校验后才公开 GitHub Release。普通 push／PR 验证不会发布版本。版本映射、发布步骤、重跑规则和完成证据以[版本发布](docs/releasing.md)为准。

公开范围、可见性和发布材料按[公开发布检查](docs/publication.md)由维护者确认。区分本地测试、平台 CI 与真实飞书／模型验收；新增发布工作流或合并 PR 不等于已发布新版本。

## 可公开的内容

示例使用占位 ID、保留的示例域名和环境变量引用。文档说明功能及限制，不记录个人需求访谈、使用轨迹、组织配置、内部地址或本地路径。测试数据也应是虚构数据。

提交前检查 diff、文档链接、示例配置及新增文件；PR 描述、评论、日志和附件同样属于发布内容。不要删除许可证归属来替代隐私审查。完整范围见[公开发布检查](docs/publication.md)。
