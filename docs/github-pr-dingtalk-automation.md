# GitHub PR 与钉钉自动化

本仓库的 PR 通知分为两个部分：

| 场景 | GitHub 入口 | 处理方式 |
| --- | --- | --- |
| PR 创建、重新打开、从草稿转为可评审 | `pull_request_target` | `.github/workflows/pr-to-dingtalk.yml` |
| PR review thread 被解决 | 已接入群聊的 GitHub 通知机器人或 GitHub App | 通知发送到钉钉群后，由本地 DWS 事件路由器处理 |

GitHub Actions 没有 `pull_request_review_thread.resolved` 对应的 workflow 触发事件，所以第二条链路不能只靠 Actions 完成。本地 DWS 路由器不开放 HTTP webhook，只消费钉钉群事件和指定用户的单聊事件。

## PR 通知 workflow

在仓库 Actions secrets 中添加：

- `DINGTALK_PR_WEBHOOK`：钉钉自定义机器人的完整 webhook URL。
- `DINGTALK_PR_SECRET`：钉钉机器人安全设置中以 `SEC` 开头的加签密钥。

Workflow 每次发送时用当前毫秒时间戳和 `DINGTALK_PR_SECRET` 计算 HMAC-SHA256 签名，再把 `timestamp` 和 Base64 编码后的 `sign` 添加到 webhook URL。不要把带时效的 `timestamp` 或 `sign` 直接保存进 GitHub Secret。

Workflow 不 checkout PR 代码，权限只有 `contents: read` 和 `pull-requests: read`；这避免了 `pull_request_target` 在可读 secret 环境下执行贡献者代码。

每次通知都会检查 HTTP 状态和钉钉返回的 `errcode`。任一 Secret 未配置时 workflow 会直接失败，且不会输出 webhook URL 或加签密钥。

## Review thread resolved 通知

如果使用 GitHub App 或其他 GitHub 通知机器人生成 review thread resolved 通知，应保证消息最终发到 DWS 路由器监听的钉钉群，并在正文中包含完整 GitHub PR URL。

- 群事件必须来自配置的 GitHub 通知机器人 openDingTalkId。
- 消息中的仓库必须位于路由器的 `allowed_repositories` 白名单。
- 路由器按事件 ID 和 PR URL 防重，并使用 `dws chat message send --at-open-dingtalk-ids` 在目标群里 @Devix。

## 避免重复通知

推荐由 Actions 负责 PR 创建通知，群内 GitHub 通知机器人只负责 Actions 无法覆盖的事件。如果两边都启用了 PR 创建通知，应关闭其中一边，否则同一个 PR 会收到两条消息。

## 参考

- [Events that trigger workflows](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows)
