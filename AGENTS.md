# feishu-bot

- Feishu + Claude Code. Preserve native streaming input, compaction and background work; do not add a second planner or business scheduler.
- Linux, macOS and Windows use the same native service; built-in daemon management is Linux/Windows only. Start sessions on demand and reuse warm processes; reclaim only confirmed idle processes without discarding history.
- Group triggers require an allowed chat, allowed author and explicit bot mention. A single `groups[]` binding with `chatId: "*"` covers otherwise unlisted groups; explicit bindings win. `members` adds authors, `allowAllMembers` admits any user in that chat, and project maintainers are always allowed. A group may opt out of only the mention requirement with `groups[].mentionOptional`; chat and author authorization still apply. DMs require an allowed user. Reject silently before context, downloads or reactions.
- Background pre-injection defaults to zero. History, quotes, pagination and attachments must enforce current chat and author scope; never turn history access into a cross-chat tool.
- Preserve the native session across process reclamation, restart, stop and compact. Only explicit new-session operations rotate it. Never silently replace a failed resume.
- Compact is a native command at a turn boundary, not a summarization prompt. No gateway automatic compaction. Preserve approvals while a control command is waiting.
- Keep reasoning and answers in one reply card, omit tool traces, bind input origins, and mark Done only after successful final delivery. Signatures and redacted thinking are not display content.
- Preserve ownership on sessions, callbacks, attachments and outputs. Do not replay uncertain external writes or evict active background work and approvals.
- Use native permission rules and scoped maintainer approvals. Same-account directories are not a security sandbox.
- Keep documentation, examples, prompts, PR descriptions and comments independent of any individual's environment or private discussions. Use fictional identifiers and reserved example domains; never copy deployment credentials, internal endpoints or personal paths.
- Document existing capabilities and limitations. Support different usage scenarios through configuration, not a general-purpose platform framework.
- The production service is Go. Keep native Claude Code as a direct child; do not reintroduce per-session Node workers or a second agent loop. Preserve the existing SQLite schema and conversation namespace during migrations.
- Run relevant tests, `go test -race ./...`, `go vet ./...` and the native CLI fixtures when available. Separate simulated-provider evidence, platform CI and live-service acceptance; report only what actually ran.

## 开始工作

以下命令默认在仓库根目录执行。仓库没有 Makefile、Justfile 或 Taskfile；构建和检查直接使用 Go 命令。

- 构建：`go build -trimpath -o ./bin/feishu-bot ./cmd/feishu-bot`；Windows 输出 `./bin/feishu-bot.exe`。生产构建使用 `CGO_ENABLED=0`。
- 检查：`go test -race ./...`、`go vet ./...`。竞态检测需要平台的 C 工具链；CI 保留 Ubuntu／Windows 的 Go 1.25／1.27，并覆盖 Linux arm64 与 macOS 两种架构。
- 原生协议回归：设置 `CLAUDE_NATIVE_PATH` 指向 Claude Code 2.1.316 后运行 `go test -race -count=1 -timeout 180s ./internal/native ./internal/integration ./cmd/benchmark-native`；未设置时相关用例明确跳过。
- 使用与部署命令：`./bin/feishu-bot init|doctor|run|daemon ...`；完整路径、配置和限制见[部署与配置](docs/deployment.md)。

## 文档、规格与术语

- 文档索引、所有权和历史状态见[docs/README.md](docs/README.md)；新文档按该页的放置规则归属。
- [架构](docs/architecture.md)和[对话行为](docs/conversation-contract.md)是当前行为契约；`docs/iteration-status*.md` 是 TypeScript 迁移前的历史证据，不是当前实现。
- 代码、配置、SQLite schema 和测试是已实现行为的最终证据；文档中的“已修复”“已验证”仍须按[验证与验收](docs/validation.md)区分模拟测试、平台 CI 和真实飞书／模型验收。
- 项目术语统一定义在[术语](docs/terminology.md)；新增持久概念时更新该页，不在多处重复定义。

## 交付与发布

- 现有历史混用 `feat:`、`fix:`、`test:`、`docs:`、`refactor:` 等前缀和句子式标题；仓库没有提交信息检查钩子。发布分析工具按前缀推断版本，未分类提交需人工确认。
- 完成边界按请求区分：本地提交、推送、PR、合并和发布互不授权。CI 在 push `main` 和 PR 上运行；只报告实际运行的结果。
- [CHANGELOG](CHANGELOG.md)是版本记录权威；推送严格稳定标签 `vMAJOR.MINOR.PATCH` 触发五个平台的构建、验证和 GitHub Release 发布。版本映射、产物、失败重跑与完成边界见[版本发布](docs/releasing.md)。公开与可见性按[公开发布检查](docs/publication.md)由维护者确认。

## 环境与协作

- 需要 Go 1.25+ 和 Git；真实原生协议测试还需要 Claude Code 2.1.316。生产二进制不依赖 Node.js、npm 或 CGO 动态库。
- Windows 使用 Git for Windows；Linux、macOS 和 Windows 共用同一原生服务。macOS 使用前台 `run` 或外部服务管理，内置 `daemon` 暂不支持 macOS。
- Agent harness 位于 `.agents/`，`.claude/` 和 `.codex/` 是投影。Hooks 需要 Python 3.8+ 和真实符号链接；Windows 需启用 Git `core.symlinks`，否则 `agent-scaffold verify` 会失败。

<!-- agent-scaffold:start (managed; edit outside) profile=default domains=docs,tools,testing,specs,terminology,git,release,environment -->
## Agent Harness

`.agents/` is the harness source; `.claude/` and `.codex/` hold generated projections. `CLAUDE.md` links to this contract.

### Session and task context

Honor the user's session entry; prefer task-local implementation/review. Resolve the task checkout and revision; use its `AGENTS.md` chain, terminology, skills, and tool working directories. Pass peers its absolute path and review revision. A shell `cd` does not reload host instructions or permissions; resolve access or guidance conflicts explicitly.

### Worktree-per-change (hard rule)

Never edit the primary worktree, including docs. Reuse an assigned linked worktree; otherwise, if the scaffold owns creation, run `bash .agents/tools/worktree.sh new <name>`.

Keep one lifecycle owner. `done --dir <absolute-wt>` merges, ff-only pushes, and removes the worktree; it requires explicit authorization and scaffold ownership, runs outside that worktree, and is not a PR/MR handoff. Leave externally managed worktrees to their owner instead of merging or cleaning them up. Bypass the trunk guard only with explicit user approval.

### Authority documents (hard rules)

`AGENTS.md` is the canonical repository-level Agent contract; read the applicable nested chain before acting. Keep it lean and current; route detail to project docs and nest only for real local differences. Repair durable guidance drift in the same change; follow higher-priority instructions and surface material disagreement instead of guessing. Interpret document status and freshness alongside repository evidence and user intent.

### Project terminology (hard rule)

Every Agent, project skill, and subagent uses the declared glossary, else root `CONTEXT-MAP.md`, then `CONTEXT.md`; read only relevant contexts before using project terms. A term and each language equivalent are equally valid names for one concept — use whichever is clearest and do not force one language. Reserve avoided names for history or compatibility. Resolve durable term changes with evidence and owner intent; update the glossary in the same change. Adopt an existing glossary; add definitions as durable concepts are resolved.

### Convention guides

Before the matching work, read its generic guide; project documentation and nested contracts win where they are more specific.

- Documentation: `.agents/conventions/docs.md`
- Commands and tools: `.agents/conventions/tools.md`
- Tests: `.agents/conventions/testing.md`
- Specifications and design records: `.agents/conventions/specs.md`
- Glossary changes: `.agents/conventions/terminology.md`
- Commits and delivery: `.agents/conventions/git.md`
- Version bumps, tags, and publication: `.agents/tools/release/README.md`
- Setup and shared environments: `.agents/conventions/environment.md`

### Sources and projections

- Edit skills in `.agents/skills/`, then run `bash .agents/relink-skills.sh`; commit source and symlinks.
- Edit subagents in `.agents/subagents/`, then run `python .agents/tools/generate-subagents.py`; commit source and projections.
- Do not hand-edit host projections or scaffold runtime (`.agents/tools/**`, `.agents/conventions/**`, `.agents/relink-skills.sh`, `.agents/symlink-manager.py`); use `agent-scaffold upgrade`, then `agent-scaffold verify`.
- **Third-party skills** follow project-owned placement and installation policy; preserve unrelated entries.

For Codex, confirm project trust, agent discovery, and `/hooks` approval; re-review changed definitions. Restore symlink/hardlink targets with Git, not Claude checkpoints.
<!-- agent-scaffold:end -->
