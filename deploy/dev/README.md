# dev 部署

源码/Git 仓库：`/data/wangyunlai.wyl/workspace/work-assistant`。这是与 Mac 原实例独立的部署，不复制 Mac 的 Task、聊天、SQLite、工作目录或 Agent 凭据。

## 运行与访问

- systemd 用户服务：`work-assistant.service`；用户已启用 linger，服务启用后不依赖 SSH 会话存活，进程异常退出自动重启。
- 监听：`0.0.0.0:17343`，可通过 dev 的网络地址访问，例如 `http://6.12.233.133:17343/`（需所在网络能路由到 dev）。显式开启 `--allow-remote`，启动时要求独立的 API / Runtime Token，各至少 32 个字符；内部 Runtime 仍连 `127.0.0.1`。默认启动地址保持 loopback，未更改其它实例。
- 主成员：`Cursor 工作助手`，Adapter 为 `cursor-agent`，模型为 Cursor 的 `auto`。首页助理、角色设计和 AI 路由已明确绑定此成员，之后可在成员管理的系统岗位中调整。
- 普通工作与验收仍只读。Cursor 使用原生 Ask 模式和开启的沙箱；每个隔离 Session 目录有自己的 Cursor 权限文件，禁止 Write、Shell 和 MCP 调用。不会修改全局 Cursor 配置、使用 `--force/--yolo` 或自动批准外部 MCP。当前主要支持已有材料的文档、分析、只读原生文件工具和结构化文本交付。
- Cursor 保留自己的原生会话；重试/验收用 `--resume` 指定原 chat ID，返回不同 ID 时失败且保留原绑定。结果须是完整 JSON 对象，具体业务再按固定协议验证，不把提示词当作权限校验。
- Linux 没有当前自动升级器要求的 macOS 验证沙箱，因此本部署关闭自动升级，不以裸执行代替。常驻与自动重启由 systemd 负责，升级源码/程序目前需人工发布。

直接访问时，在连接设置填写 dev 的 `ASSISTANT_API_TOKEN`。当前是明文 HTTP，Token 鉴权不提供传输加密，只在可信网络使用。没有修改防火墙、NAT 或配置 HTTPS；不可信网络请使用现有 SSH 隧道（17345 不影响 Mac 本机已有的 17343）：

```bash
ssh -N -L 127.0.0.1:17345:127.0.0.1:17343 dev
```

打开 `http://127.0.0.1:17345/`，连接设置填写 dev 的 `ASSISTANT_API_TOKEN`。Token 位于 dev 的 `~/.config/work-assistant/dev.env`，权限 0600；不要放进 URL、提交 Git 或粘贴到公共聊天。Cursor 登录单独由 `ssh dev` 后的 `agent login` 管理，服务复用该账户已有登录，不传输 Mac 凭据。

```bash
systemctl --user status work-assistant
systemctl --user restart work-assistant
journalctl --user -u work-assistant -n 80 --no-pager
node deploy/dev/api.mjs /system
node deploy/dev/api.mjs /admin/backups POST '{}'
```

## 数据与备份

控制库、Runtime spool 和隔离工作目录位于 `data/dev/`，Git 忽略它们。自动恢复点位于 `/data/wangyunlai.wyl/work-assistant-backups/dev/e688631bf5cc20df2aa521fd/`，启动和每 5 分钟生成，包含两个数据库并检查完整性及校验和。恢复点仍在同一台机器/磁盘，不等于异机保护；Cursor 原生 Session 文件仍不在这两个数据库的备份范围，不删除或迁移其认证/会话目录。

## 安装和维护

需要 Linux amd64、Go 1.27.1、Node.js 和已登录 Cursor Agent。此机验证的 Cursor 版本为 `2026.09.02-c22c1a3`。Go 工具链已可通过 Go 的 toolchain 机制选择；依赖从 Go 模块代理取得并按 go.sum/校验服务校验。

```bash
cd /data/wangyunlai.wyl/workspace/work-assistant
GOTOOLCHAIN=go1.27.1 GOPROXY=https://goproxy.cn go test ./...
GOTOOLCHAIN=go1.27.1 GOPROXY=https://goproxy.cn go vet ./...
GOTOOLCHAIN=go1.27.1 GOPROXY=https://goproxy.cn go build -trimpath -o ./bin/ ./cmd/...
node --test internal/server/ui/activity.test.cjs internal/server/ui/adapters.test.cjs
```

第一次部署时用 `node deploy/dev/prepare.mjs` 生成私有环境文件（不覆盖已有值），将 `deploy/dev/work-assistant.service` 安装到 `~/.config/systemd/user/work-assistant.service`，再 `systemctl --user daemon-reload` 和 `systemctl --user enable --now work-assistant`。

后续更新前先创建并验证恢复点、确认无活动 Run、保留旧程序，再停服务、构建/切换并重启验证。不要在已有进程运行时直接覆盖其可执行文件，也不要用旧版程序打开新 schema。尚未给 Git 配置外部远程，不自动向 GitHub 发布源码。

dev 仓库的 `docs/design/` 另外归档了当前项目已有的抽象架构、详细设计和用例分析文档。实际验收记录见 [VERIFICATION.md](VERIFICATION.md)，其中区分已验证的服务/适配协议与尚待登录验证的真实模型调用。

Cursor 接口依据：[CLI 参数](https://cursor.com/docs/cli/reference/parameters)、[公开事件格式](https://cursor.com/docs/cli/reference/output-format)、[权限](https://cursor.com/docs/cli/reference/permissions)。本应用不承诺 Cursor 未上报的中间输出或完整 OS/秘密读取隔离。
