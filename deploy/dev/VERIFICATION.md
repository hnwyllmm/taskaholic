# dev 部署验收记录

日期：2026-09-06（Asia/Shanghai）。本次为独立新部署，没有迁移或改动 Mac 实例的数据库、运行程序、原生会话或凭据。

## 已验证

- 源码/Git：`/data/wangyunlai.wyl/workspace/work-assistant`，`main` 分支；程序来自初始提交 `85e34ae0056d41e9a0824d4ea9c7f3028e7842d0`。Git 无外部 remote。后续文档提交不改变此程序的构建来源。
- dev Linux amd64 原生 Go 1.27.1 编译五个入口；完整 `go test -race ./...`、`go vet ./...` 通过。最后增加的应用 Token 环境隔离也在 dev 重跑 Agent/bootstrap race 测试通过。
- Mac 完整 race 回归通过，Cursor 和 bootstrap 最终增量测试通过。前端 SSE 和适配器选择的 5 个 Node 测试通过；新资源 HTTP 路由在 Go 测试和部署服务上验证。
- Cursor 协议夹具覆盖公开消息/工具行动、正确原生 Session ID、续轮与角色/Schema 保留、错误/缺失结果拒绝、中断整个进程组、每工作区只读策略、常见密钥脱敏和不传递 `ASSISTANT_*` 环境变量。这些是自动化协议测试，不等于真实 Cursor 端到端成功。
- 用户级 `work-assistant.service` 已 enabled/active/running，`Linger=yes`，`Restart=on-failure`；主进程由 systemd 维护，脱离 SSH 会话。执行过正常重启，原成员身份和三个系统岗位绑定均保留，没有重复创建成员。
- 在线 Runtime 为 `dev-cursor`，仅注册 `cursor-agent`；主成员为 `Cursor 工作助手`（`agent_1788680851852_1fccbccd22469176cb6b`），模型 `auto`，并发 1。首页聊天、角色设计、任务路由均显式绑定该成员，版本 2。
- 首页、工作列表、成员管理、团队资料、系统状态和两个新增/相关 JS 资源返回 200，并带 CSP。无 API Token 的控制 API 返回 401，无 Runtime Token 的 `/runtime/ws` 返回 401。
- 私有环境文件及两个数据库权限 0600；数据目录、备份根目录权限 0700。Git 不包含数据、二进制、日志、环境文件或原生 Session。没有向公共网络开放服务。
- 初始工作列表为空、活动 Run 为 0；没有为了部署引入模拟用户任务。
- 启动备份与手动备份成功；重启后的再次启动备份成功，完整性检查和 SHA-256 验证通过。

手动恢复点：

`/data/wangyunlai.wyl/work-assistant-backups/dev/e688631bf5cc20df2aa521fd/snapshot-20260906T074842.325280330Z-2278555008`

| 文件 | 字节数 | SHA-256 |
|---|---:|---|
| control.sqlite | 401408 | d413361afa5e09afe8741433b0d1d3fd1f668f87da9276d966cf7582efc497c3 |
| runtime.sqlite | 36864 | 8468996924aeec9da020742cc26967f3fcbd863c2a104fa6c88774f1e139517d |

已用部署程序的 `assistantctl backup-verify` 再次离线验证此恢复点，未覆盖或恢复到正在运行的数据库。

主程序 `bin/assistant-local` 的 SHA-256：`cb0b60a49898ea720e31f5b192409a87c1e9456b84e96dd8f8fcb1f346be0ad4`。Go 构建信息记录上述源码提交且 `vcs.modified=false`。

## 尚未通过：Cursor 账户认证

dev 已有 Cursor CLI `2026.09.02-c22c1a3`。服务端网络可达，但保存的登录不能成功执行模型调用。`agent status --format json` 虽显示本地存在登录 Token，同时报告无法取得账户详情；实际 `agent models` 和只读最小请求返回 `Authentication required`。

因此不能把 Runtime 在线或页面可访问称为 AI 已可用。需用户在 dev 上执行 `agent login` 完成自己的账户授权，再验证真实首页聊天、结构化任务交付、公开行动事件和验收时原 Session 续接。没有复制 Mac 凭据、输出 Token 或偷偷切回 Codex。

## 部署边界

Cursor 仍使用 Ask/只读模式和启用的沙箱，每个 Session 的策略拒绝写文件、Shell 和 MCP；这不是全面的秘密读取隔离。Linux 当前没有原升级器要求的验证沙箱，自动源码升级关闭，不能用裸执行替代。自动重启不等于自动源码升级。

备份在同一台 dev 上，不是异机/异盘副本；不包含 Cursor 自己管理的原生 Session 文件。访问走 SSH 隧道，页面 Token 仅由私有配置提供，不进入 URL 或 Git。
