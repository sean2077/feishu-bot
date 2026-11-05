# 版本发布

本页定义 Go 网关的发布入口、产物和完成边界。使用者的下载、校验、安装与平台限制见[部署与配置](deployment.md#发布包安装)；验证证据口径见[验证与验收](validation.md)。

## 触发与版本来源

独立的 [Release 工作流](../.github/workflows/release.yml) 仅由推送稳定版本 tag 触发。入口过滤后严格校验 `vMAJOR.MINOR.PATCH`，每段为没有多余前导零的非负整数，例如 `v1.2.3`。本流程不接受 beta／rc 等预发布标签。

普通分支 push、PR、定时事件和手动工作流按钮均不触发发布；也不监听 `release.published`。现有 [普通 CI](../.github/workflows/ci.yml) 继续检查代码、平台兼容性和发布工具，但不会创建 Release 或上传公开附件。新工作流需要先进入待发布提交，已存在的旧 tag 不会被自动补发。

版本身份按以下映射保持一致：

| 位置 | 规则 |
|---|---|
| Git tag | 完整的 `vX.Y.Z`，固定到一个准确提交 |
| `CHANGELOG.md` | 保留现有 `## X.Y.Z — YYYY-MM-DD` 标题格式，必须唯一且有非空正文 |
| 二进制 | 从 tag 去掉前缀 `v`，经 `-ldflags -X .../internal/cli.Version=X.Y.Z` 注入 |
| 冒烟检查 | 解压后的 `--version` 必须返回相同版本 |
| 构建信息与发布清单 | 记录完整 tag、完整 commit SHA、目标系统和架构 |

`Unreleased` 仍汇总未发布变更，不会自动转成发布说明；发布时需准备对应版本章节。缺失、重复、空正文或版本不匹配会终止流程，不回退到 GitHub 自动生成的说明。仓库无需新增一个与 tag 重复维护的版本文件，源码中的开发版本仅用于普通开发构建。

## 发布产物

每次发布必须包含下列五个便携压缩包和三个安装包：

| 目标 | 便携版 | 安装版 |
|---|---|---|
| Linux amd64 | `feishu-bot-vX.Y.Z-linux-amd64.tar.gz` | `feishu-bot-vX.Y.Z-linux-amd64.deb` |
| Linux arm64 | `feishu-bot-vX.Y.Z-linux-arm64.tar.gz` | `feishu-bot-vX.Y.Z-linux-arm64.deb` |
| Windows amd64 | `feishu-bot-vX.Y.Z-windows-amd64.zip` | `feishu-bot-vX.Y.Z-windows-amd64.msi` |
| macOS Intel | `feishu-bot-vX.Y.Z-darwin-amd64.tar.gz` | 不提供 |
| macOS Apple Silicon | `feishu-bot-vX.Y.Z-darwin-arm64.tar.gz` | 不提供 |

便携包内只有一个同名根目录 `feishu-bot-vX.Y.Z-OS-ARCH/`，包含：

- `feishu-bot` 或 `feishu-bot.exe`；
- `config.example.json` 和 `INSTALL.md`；
- `LICENSE` 和 `THIRD_PARTY_NOTICES.md`；
- `BUILDINFO.json`，记录 `tag`、`version`、`commit`、`os`、`arch`、提交时间 `source_date_epoch` 以及二进制校验和 `binary_sha256`。

安装包只复制同样的程序和文档，不注册后台服务：

- Windows MSI 按当前用户安装到 `%LOCALAPPDATA%\feishu-bot`，不需要管理员权限；卸载通过 Windows“设置 > 应用”。当前 MSI 没有代码签名，SmartScreen 可能提示。
- Linux deb 把二进制安装到 `/usr/bin/feishu-bot`，文档安装到 `/usr/share/doc/feishu-bot/`；使用 `dpkg -i` 和 `dpkg -r` 安装、卸载。
- 常驻服务仍由 `feishu-bot daemon install` 单独注册；安装器不创建 systemd unit、任务计划或启动项。
- 安装包使用 `source_date_epoch` 固定文件时间戳；工作流在相同输入下重复构建并逐字节比较，避免同一 tag 的重跑产生不同附件。

Release 另外附带 `SHA256SUMS.txt` 和 `release-manifest.json`，用于核对完整文件清单、校验和及同一次发布的身份。生产二进制使用 `CGO_ENABLED=0`；发布包不包含部署配置、凭据、状态库、原生 Claude Code 或 Git。

macOS 包提供原生网关和前台 `run`，内置 `daemon` 管理仍限 Linux／Windows。当前发布不包含 Apple Developer ID 签名和公证，也不生成 DMG、PKG 或自动安装服务；首次运行说明见[部署文档](deployment.md#发布包安装)。

## 检查、构建与公开

1. **绑定发布身份。** 检出 tag 对应的准确提交，验证 tag、commit 和 CHANGELOG，生成发布元数据与说明。
2. **在各目标上检查。** 五个平台使用匹配架构的原生 runner，执行 `go test -race`、`go vet` 及固定 Claude CLI 配合本地确定性模型的协议回归。检查 CLI 文件确实存在、平台和版本匹配；缺失不能作为 skip 放行。竞态检测保留平台 C 工具链，`CGO_ENABLED=0` 仅用于生产构建。
3. **验证最终包。** 注入版本，构建、打包后解压；实际执行其中二进制的 `--version`、`--help`、临时目录 `init` 等检查。安装包还要重复构建并逐字节比较，然后在原生平台实际安装、运行并卸载。编译成功不代表另一个架构或安装包已经实际运行过。
4. **汇总完整产物。** 所有目标成功后才进入唯一发布 job，核对五个便携包、三个安装包、构建身份和 SHA-256。构建 job 只有只读权限，发布 job 获得 `contents: write`，发布令牌只传给发布步骤。
5. **先草稿，后公开。** 创建该 tag 的 draft Release，上传附件，重新下载逐一验证，再确认远端 tag 仍指向原提交，最后公开。发布说明使用已校验的 CHANGELOG 内容。

同一 tag 的工作流串行处理，不取消正在进行的发布。构建或验证失败时不会公开缺少附件的版本；上传阶段失败可以留下草稿，以便按下节规则继续。

构建、打包和离线校验由 [.github/scripts/release.py](../.github/scripts/release.py) 提供；GitHub 发布状态由 [.github/scripts/publish_release.py](../.github/scripts/publish_release.py) 管理。它们属于项目发布实现，不修改 `.agents/tools/` 中的 scaffold runtime。

## 维护者操作

1. 通过正常变更流程准备选定版本的 CHANGELOG、相关迁移说明和发布材料，完成检查并合入要发布的提交。公开范围沿用[公开发布检查](publication.md)。
2. 在该准确提交上创建新的注释 tag，显式推送这一个 tag。下面的 `vX.Y.Z` 是占位符，需替换成已选定版本；不要移动或重建已有 tag。

   ```sh
   release_tag="vX.Y.Z"
   git tag -a "$release_tag" -m "Release $release_tag"
   git push origin "$release_tag"
   ```

3. 查看该 tag 的 Release 工作流。完成后核对公开 Release 页面、tag／commit、五个便携压缩包、三个安装包及两个清单文件，记录实际的工作流与 Release URL。

推送 tag 是启动发布；完成边界是工作流成功、Release 已公开且完整附件验证通过。PR、合并、tag 已存在或 Actions 里有临时 artifact，都不能单独作为发布成功的证据。本地离线测试也不能替代 GitHub 上传及公开步骤的真实执行。

## 失败与重跑

重跑前先查看失败步骤，保留原 tag 和原提交。建议重跑失败 jobs，以便沿用已经通过的平台压缩包；完整重跑也必须产出相同的发布身份和内容。

- **尚未创建草稿：** 修复环境或暂时性外部问题后，可以重新运行原工作流。需要修改源码、发布说明或产物时，应使用新的提交和新版本 tag。
- **草稿已存在：** 只有 tag、完整 commit SHA 与发布清单一致，才补上传缺失文件。已有附件必须内容一致；未知附件、相同名字的不同内容或 tag 已移动均使流程失败。GitHub 上传失败可能留下 `state=starter`、大小为零的占位附件；仅当名称属于本次预期文件集合时，脚本才会删除该失败占位并重传。已上传或非空附件不会自动删除、覆盖。
- **版本已公开：** 相同版本的重跑只读核对已有页面和附件。内容一致则结束；有差异则报告失败，保留公开附件，修正通过新版本交付。

确认每个平台的原生检查、便携包与安装包冒烟已经完成，且发布 job 的下载核验成功后，才能把一次中断的发布视为恢复完成。真实飞书、客户端及模型服务的验收仍按[验证清单](validation.md)单独记录。
