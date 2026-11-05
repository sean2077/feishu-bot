# 首次接入：逐层确认

## 1. 本地准备

运行 `./bin/feishu-bot init` 生成配置，不覆盖已有文件。填写应用、允许的群／作者或私聊、维护者及模型入口；凭据只设置在运行主机的环境变量中。随后运行：

```sh
./bin/feishu-bot doctor
```

默认不访问飞书或模型。`ready` 继续表示本次请求的检查全部通过，不代表服务已完成真实验收。报告的 `verification` 分别列出 `passed`、`failed`、`blocked`、`not_checked`；不要把 `not_checked` 当作通过。

本地检查通过后安装常驻服务：

```sh
./bin/feishu-bot daemon install --config ~/.feishu-bot/config.json
./bin/feishu-bot daemon status
./bin/feishu-bot daemon logs -n 50
```

`daemon install` 会执行同一组离线检查、复制二进制、生成或校验 `service.env`，注册当前用户的登录启动服务并默认启动；不需要编写 launcher 脚本。没有凭据时可用 `--env-file <KEY=VALUE 文件>`。`daemon status --json` 适合脚本；`daemon logs -f` 跟随日志。需要只安装不启动时加 `--no-start`。

## 2. 明确选择联网检查

```sh
./bin/feishu-bot doctor --probe-feishu
./bin/feishu-bot doctor --probe
```

`--probe-feishu` 使用当前配置的应用凭据交换临时访问令牌，然后查询机器人身份。只调用飞书认证与机器人信息接口，不连接事件流、不发送消息、不修改权限，也不调用模型。响应最多 64 KiB，共享 15 秒请求期限；拒绝跳转，日志不显示令牌、机器人名称、身份 ID 或原始错误正文。认证失败与机器人身份查询失败分别报告。

`--probe` 仍是独立的基础模型请求，可能计费。两个开关可以一起使用，也可以分别排查。接口返回成功不代表发布范围、消息事件、卡片回调、文件权限、流式工具或多模态已可用。

探针使用 Go HTTP 客户端，不证明正式长连接或代理链路可用；网络失败需结合运行主机的 DNS、代理与 CA 设置诊断。不能据此修改权限或关闭证书检查。

自定义配置用 `./bin/feishu-bot doctor --config <配置路径> --probe-feishu`；不要在命令参数或聊天中填写密钥。

## 3. 独立测试聊天

应用发布、事件与权限配置见[部署说明](deployment.md)。在获准的测试群和私聊中分别确认：消息能够进入、正确 @ 才触发（默认；显式开启 `mentionOptional` 的测试群除外）、审批／追问卡片可以操作、文件确实返回。再验证多人补充、停止和重启恢复。具体步骤见[验收清单](validation.md)。

这些行为必须通过实际操作确认，doctor 不会把身份接口成功写成完整验收完成。连接日志的降级状态与真实客户端验证仍是独立工作。

## 参考

借鉴 [cc-connect 的飞书接入流程](https://github.com/chenhg5/cc-connect/blob/main/docs/feishu.md) 的阶段划分，不复制其凭据写回或权限管理能力。

接口依据：[自建应用 token](https://open.feishu.cn/document/server-docs/authentication-management/access-token/tenant_access_token_internal)、[机器人信息](https://open.feishu.cn/document/client-docs/bot-v3/obtain-bot-info)。本项目诊断不会把可执行文件的版本检查当作认证检查。
