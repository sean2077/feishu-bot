# 可选项目操作

优先复用已有可信 CLI、MCP 和原生技能。固定操作适用于需要明确资源范围和审批的外部系统，不是正常文件处理或编码工具的替代品。

未配置 `actions` 时不注册空操作目录，也不启动相关本地接口。所有示例默认未启用，不是本项目的安装依赖。

## 示例与配置

[project-actions.json](../examples/project-actions.json) 是可合入 `projects[].actions` 的配置数组。替换执行器绝对路径、虚构资源 ID 和环境变量引用后再使用。

| 操作 | 范围 | 审批 |
|---|---|---|
| jenkins-status / jenkins-build | 固定 job 查询或构建 | 查询自动，构建逐次 |
| gitlab-publish | 文件快照应用到新分支并创建 MR，不合并 | 快照与参数逐次批准 |
| feishu-doc-read / feishu-doc-append | 固定文档读取或追加 | 读取自动，追加逐次 |
| feishu-table-list / feishu-table-update | 固定表和允许字段 | 读取自动，更新逐次 |

`vars` 是固定非敏感设置，`env` 引用服务进程环境变量，`parameters` 约束 agent 提供的字符串。`readOnly` 是管理员对可信执行器的声明，不能把写操作标为只读以绕过审批。

`workspaceFile` 在审批前保存文件快照和 SHA256，批准后读取快照。GitLab 示例在独立的干净 checkout 中应用补丁，禁用 hooks 和全局 Git 配置，不在可修改项目中运行携带业务凭据的脚本。

Linux 示例使用 `/usr/bin/node`，Windows 改成实际 `node.exe` 与脚本路径。不要把输入拼入 `sh -c`、PowerShell 或动态脚本；固定 argv 并不自动证明执行器的业务逻辑安全。

## 原生后台工具

配置业务操作后，原生 Bash／Monitor 可使用相同入口：

```sh
node "$FEISHU_ACTION_CLIENT" jenkins-status '{}'
```

权限与 MCP `project_action` 相同，写操作仍等待维护者批准。这里不新增 cron 或监控调度器。进程停止后接口失效，恢复会话不重启旧 Monitor。

## 资料、技能与服务身份

`seedDir` 提供获准使用的初始资料或源码，首次复制并建立 Git baseline，后续不覆盖修改。审核后的 `.claude` 内容可放在 `trustedConfigDir`；首次准备时复制到会话工作区，已有工作区的更新需显式处理。

业务密钥不默认注入 CC，但同一系统账户下的任意代码可能访问该账户其他数据。通过外部 API scope、受保护资源和专用服务身份控制权限，不将环境白名单描述成强隔离。

各服务的认证、资源权限和错误语义需联调。Jenkins 示例使用 API token 与 REVISION 参数；表格示例仅读取第一页及分页信息，不宣称返回全部记录。结果未知的写操作不自动重试，应先核对远端状态。

接口参考：[Jenkins](https://www.jenkins.io/doc/book/using/remote-access-api/) · [GitLab MR](https://docs.gitlab.com/api/merge_requests/) · [飞书文档子块](https://open.feishu.cn/document/server-docs/docs/docs/docx-v1/document-block-children/create)
