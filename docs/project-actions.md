# 项目操作

优先复用已有可信 CLI、MCP 和项目 skills。网关的固定操作适用于需要明确资源范围和维护者审批的共享系统，不是正常编码工具的替代品。

CC 原生运行，可以直接使用已安装的工具、网络和项目依赖；不再要求把测试放进离线镜像。默认 `acceptEdits` 自动批准本地文件操作，常用测试命令在项目 `allowedTools` 中按原生规则预授权。规则是否可信由项目维护者决定，不默认批准任意 shell。

## 固定业务操作

`examples/project-actions.json` 是可选 actions 数组。修改执行器绝对路径、资源 ID 和环境变量引用，再放入 `projects[].actions`；没有配置时不注册空的操作目录。示例共七项：

| 操作 | 范围 | 审批 |
|---|---|---|
| jenkins-status / jenkins-build | 固定 job 查询／构建 | 查询自动，构建逐次 |
| gitlab-publish | 快照补丁 → 新分支＋MR，不合并 | 快照及参数逐次批准 |
| feishu-doc-read / feishu-doc-append | 固定文档读取／追加 | 读取自动，追加逐次 |
| feishu-table-list / feishu-table-update | 固定表及允许字段 | 读取自动，更新逐次 |

`vars` 是固定非敏感配置；`env` 指向服务进程环境变量；`parameters` 约束 agent 输入；`readOnly` 是管理员对执行器的声明，不能误标写操作。

`workspaceFile` 在审批前把文件保存为快照并计算 SHA256，审批后执行器读取该快照。GitLab 示例在独立的清洁 checkout 应用补丁，禁用 hooks／全局 Git 配置，不在可修改项目中运行携带业务凭据的脚本。

Linux 示例使用 `/usr/bin/node`，Windows 改为 `node.exe` 和脚本实际绝对路径。不要把可变输入拼进 `sh -c`／PowerShell。已发生但结果不明的远端动作不会自动重试，先核对远端状态。

## 原生 Monitor

配置了业务操作后，原生 Bash／Monitor 也可以使用同一入口：

```sh
node "$FEISHU_ACTION_CLIENT" jenkins-status '{}'
```

权限与 MCP `project_action` 相同；写操作仍等维护者批准。这里没有新增 cron 或监控调度器。进程停止后入口失效，恢复会话不会自动重启旧 Monitor。

## 技能、源码和服务身份

`seedDir` 用于首次复制适合项目组共享的干净源码，生成独立 Git baseline，之后不覆盖修改。管理员可将审核后的 `.claude` 内容放在 `trustedConfigDir`，首次准备时复制到该聊天工作区；更新原有工作区配置须由管理员明确处理。不加载个人全局登录态。

业务密钥不会默认注入 CC，但与 gateway 相同系统账户运行的任意代码可能访问该账户的其他数据。因此这是可信项目组服务，不宣称通过几个环境变量和目录完成强隔离。高风险服务身份仍应由外部 API scope、受保护分支和公司权限限制。

真实 Jenkins／GitLab／飞书实例的认证和资源权限须联调；配方默认未启用。Jenkins 示例使用 API token 和 REVISION 参数，表格列表只返回第一页与分页信息，不声称已经读取全部记录。

接口参考：[Jenkins](https://www.jenkins.io/doc/book/using/remote-access-api/) · [GitLab MR](https://docs.gitlab.com/api/merge_requests/) · [飞书文档子块](https://open.feishu.cn/document/server-docs/docs/docs/docx-v1/document-block-children/create)
