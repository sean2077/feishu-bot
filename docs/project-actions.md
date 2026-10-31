# 项目操作接入

优先使用你已有的可信 CLI／MCP；这里提供可直接配置的宿主执行器与示例，而不是另一套 agent 框架。示例默认不启用，管理员按项目选择后拷贝到该项目的 `actions`。普通成员不能从聊天里添加或修改执行器。

## 配置结构

`examples/project-actions.json` 是 actions 数组，不是完整配置。修改可执行文件、脚本绝对路径、固定资源 ID 和服务环境变量，再放入 `projects[].actions`。Linux 示例使用 `/usr/bin/node`；Windows 改为实际 `node.exe` 及脚本绝对路径。

- `vars`：非敏感的固定配置，如 Jenkins job、GitLab 项目和文档 ID。
- `env`：宿主环境变量引用。`"GITLAB_TOKEN": "BOT_GITLAB_TOKEN"` 表示仅执行器启动时读宿主的 `BOT_GITLAB_TOKEN`，不会把 token 交给 worker。
- `parameters`：模型能提供的字符串参数。允许值／长度／格式在运行前验证。
- `readOnly`：管理员对执行器的保证，默认 false。true 自动执行，false 必须批准每一次。
- `workspaceFile`：审批前将该房间工作区文件快照到宿主私有目录，摘要绑定 SHA256，执行器读取不可由 worker 修改的副本。

每个操作固定可执行文件和 argv，不用 shell 拼接。执行器不得在持有凭据时运行不受信任项目的 hooks、构建命令、安装脚本或动态程序。

## 已提供的配方

| 操作 | 作用与范围 | 交互 |
|---|---|---|
| `jenkins-status` | 固定 job 的 lastBuild 状态 | 只读 |
| `jenkins-build` | 固定 job 的 REVISION 参数构建 | 每次审批 |
| `gitlab-publish` | 清洁宿主 checkout 应用批准的 patch，推独立分支并建 MR | 文件快照＋审批，不合并 |
| `feishu-doc-read` | 固定项目文档正文 | 只读 |
| `feishu-doc-append` | 固定文档根块追加文字 | 每次审批 |
| `feishu-table-list` | 固定表第一页，最多 50 条；返回分页信息而非声称全部 | 只读 |
| `feishu-table-update` | 固定表、指定记录、配置允许的字段 | 每次审批 |

GitLab 配方只从配置的同一 HTTPS origin 取仓库和调用 API，禁用全局 Git 配置、hooks 和凭据交互。在 worker 中先生成相对于 source baseline 的 diff，交付 patch。远端已经变化导致无法 apply 时明确失败，不强行覆盖。`git push` 成功而建 MR 失败时，先核对远端生成的 `feishu-bot/<uuid>` 分支再重试，不把一次超时当作什么都没发生。

Jenkins 配方使用用户 API token 认证，不支持密码／cookie 登录；目标 job 需要声明 REVISION 参数。公司网关、认证插件或文件夹策略不同，应替换成现有可信 CLI。飞书服务身份必须对目标文档／多维表格有实际权限；应用 scope 本身不能代替资源授权。

## 原生 Monitor

MCP `project_action` 和原生 Bash／Monitor 的操作入口使用同一个宿主授权检查。worker 环境中只包含作用域限定的临时通道，不包含真实业务 token：

```sh
node "$FEISHU_ACTION_CLIENT" jenkins-status '{}'
```

CC 可以将上述命令用于自己的 Monitor。网关不添加轮询业务调度器；只转发原生后台事件和已批准的操作。写操作不应伪装成只读监控。worker 关闭后临时通道失效，旧 Monitor 不自动重启。

## 原生技能与项目依赖

把审核过的 `.claude` 内容放在 `trustedConfigDir`。生产容器将其只读挂载到 `/workspace/.claude`，不复制宿主的个人登录态。会话的原生记忆留在独立 HOME。普通 source seed 中可包含项目说明，但指令不是权限边界。

容器默认禁网；构建工具和依赖预装到受控镜像或提供离线缓存。不要通过给 worker 全部生产凭据解决依赖下载问题。内网机器和部署服务只经管理员新增的固定操作开放，首版没有任意 SSH／shell 入口。

## 核查依据与验收边界

接口参考官方文档：
- [Jenkins Remote Access API](https://www.jenkins.io/doc/book/using/remote-access-api/)
- [GitLab Merge requests API](https://docs.gitlab.com/api/merge_requests/)
- [飞书新增文档子块](https://open.feishu.cn/document/server-docs/docs/docs/docx-v1/document-block-children/create)

这些配方不是对你公司实例的实机验收。先用隔离测试资源检查服务身份、目标限制、维护者审批和失败后的远端状态，再启用生产操作。
