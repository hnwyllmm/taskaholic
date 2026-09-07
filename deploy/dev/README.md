# dev 部署

源码/Git 仓库：`/data/wangyunlai.wyl/workspace/work-assistant`。这是与 Mac 原实例独立的部署，不复制 Mac 的 Task、聊天、SQLite、工作目录或 Agent 凭据。

## 运行与访问

- systemd 用户服务：`work-assistant.service`；用户已启用 linger，不依赖 SSH 会话存活。进程链是 `systemd --user → assistant-supervisor → assistant-local → Cursor / Codex CLI`。systemd 维护守护进程，守护进程维护应用并负责升级安装/健康检查/回滚；重启应用不会结束升级的父进程。
- 监听：`0.0.0.0:17343`，可通过 dev 的网络地址访问，例如 `http://6.12.233.133:17343/`（需所在网络能路由到 dev）。按用户要求显式开启 `--allow-remote --no-api-auth`，浏览器/控制 API 免登录；Runtime 仍要求至少 32 字符的 Token，内部连接仍使用 `127.0.0.1`。其它实例及默认启动设置不变。
- 主成员：`Cursor 工作助手`，Adapter 为 `cursor-agent`，模型为 Cursor 的 `auto`。首页助理、角色设计和 AI 路由已明确绑定此成员，之后可在成员管理的系统岗位中调整。
- 同机额外启用 `codex-agent`，使用 `/home/wangyunlai.wyl/.n/bin/codex`。为保持历史绑定，Runtime ID 仍为 `dev-cursor`，这只是稳定机器标识，不表示只支持 Cursor。主 Adapter 和原系统岗位保持不变；增加 Adapter 不复制、迁移原 Session。
- Codex 普通任务强制 `read-only` 和 `approval_policy=never`，覆盖 CLI 全局默认沙箱，不关闭沙箱。读取公开 JSONL 活动，用原生 thread ID 恢复验收对话；取消会终止对应进程组。模型下拉通过临时 stdio App Server 的只读 `model/list` 获取，不新建任务，不读取聊天记录，不暴露新网络端口。
- 普通工作与验收仍只读。Cursor 使用原生 Ask 模式和开启的沙箱；每个隔离 Session 目录有自己的 Cursor 权限文件，禁止 Write、Shell 和 MCP 调用。不会修改全局 Cursor 配置、使用 `--force/--yolo` 或自动批准外部 MCP。当前主要支持已有材料的文档、分析、只读原生文件工具和结构化文本交付。
- Cursor 保留自己的原生会话；重试/验收用 `--resume` 指定原 chat ID，返回不同 ID 时失败且保留原绑定。结果须是完整 JSON 对象，具体业务再按固定协议验证，不把提示词当作权限校验。
- Linux 候选验证使用 bubblewrap，启动时先验证命名空间是否可用，再启用升级。系统工具、固定 Go/Node 编译器和依赖缓存只读，候选工作目录可写；宿主数据、凭据和网络不挂载。测试可启动隔离的本地 HTTP 服务。缺少 bubblewrap 或内核限制不满足时拒绝启动升级守护进程，不回退裸执行。

直接访问无需填写 Token，页面显示“免登录模式”。能连接此端口的设备均可读取数据、创建/操作任务及访问备份；应用不会自动判断请求是否来自局域网/VPN。当前是明文 HTTP，网络访问范围由用户的局域网、VPN 和防火墙控制；本次未修改防火墙或 NAT。不要将免登录端口直接暴露到不受信任的网络。

已有 SSH 隧道仍可使用（17345 不影响 Mac 本机已有的 17343），但隧道本身不会关闭 dev 的直接访问入口：

```bash
ssh -N -L 127.0.0.1:17345:127.0.0.1:17343 dev
```

打开 `http://127.0.0.1:17345/` 也无需 Token。原 API Token 保留在 dev 的 `~/.config/work-assistant/dev.env`（0600），目前被 `--no-api-auth` 忽略，并未删除。需要恢复网页认证时，从 systemd 单元移除 `--no-api-auth`，执行 `daemon-reload` 并重启服务、刷新网页即可。Runtime Token 始终保留并生效，不能填写到网页。Cursor 登录仍单独由 `ssh dev` 后的 `agent login` 管理，与网页免登录无关。

```bash
systemctl --user status work-assistant
systemctl --user restart work-assistant
journalctl --user -u work-assistant -n 80 --no-pager
node deploy/dev/api.mjs /system
node deploy/dev/api.mjs /admin/backups POST '{}'
```

## 数据与备份

控制库、Runtime spool 和隔离工作目录位于 `data/dev/`，Git 忽略它们。自动恢复点位于 `/data/wangyunlai.wyl/work-assistant-backups/dev/e688631bf5cc20df2aa521fd/`，启动和每 5 分钟生成，包含两个数据库并检查完整性及校验和。新角色、成员和会话映射在原控制库中，复用此备份。恢复点仍在同一台机器/磁盘，不等于异机保护；Cursor / Codex 原生 Session 文件仍不在这两个数据库的备份范围，不删除或迁移其认证/会话目录。

## SeekDB 开发与评审成员

提供六个独立角色：SeekDB 开发者、seekdb-bindings 开发者、通用代码仓库开发者，以及 SeekDB / seekdb-bindings 共用的架构 Reviewer、QA / 测试 Reviewer、通用 Reviewer。职责模板位于 `seekdb-roles.mjs`；每个角色对应一位带 `· Codex` 后缀的 dev 成员，并发上限 1。模型显式沿用本次 dev 的 `gpt-5.6-sol` 配置，后续可在成员管理中分别修改，已有 Session 不切换模型。

三个评审角色分别使用 architecture.review、qa.review/test.review、code.review 等能力标签，也可在创建任务时直接指定成员或角色。不预置固定 Task Graph，不自动替人批准合并。通用 Reviewer 必须检查仓库的 code-review skill 并完整读取其要求；dev 的 SeekDB 当前有 `.agents/skills/code-review/SKILL.md`，seekdb-bindings 本次未发现该 skill，后续任务仍需按实际仓库版本重新查找。

创建工具默认只预览，明确 `--apply` 才写入。通过现有 API 生成可追溯草案、发布角色、创建成员；有同名但不同配置时拒绝覆盖，重复运行保留记录。应用前后创建并验证恢复点，不修改系统岗位：

```bash
cd /data/wangyunlai.wyl/workspace/work-assistant
node deploy/dev/seed-seekdb-roles.mjs --members --model gpt-5.6-sol
node deploy/dev/seed-seekdb-roles.mjs --apply --members --model gpt-5.6-sol
```

这些仍是只读工作成员，可以分析代码、交付文档/评审报告和文本补丁；角色或团队资料中的路径不是仓库写权限。Git worktree 写入、PR 创建和合并尚未接入，不能把开发角色上线等同于已实现自动代码交付闭环。

## 通过聊天升级

1. 首页明确提出“给这个系统增加/修改……”，确认助理生成的升级需求。
2. 升级守护进程读取记录，Cursor 查看隔离副本并返回有界 JSON 编辑提案。可信代码检查路径/保护区/唯一文本匹配后写入副本。普通任务和 Cursor 的只读权限不变。
3. 独立沙箱执行全量 Go 测试、vet、JavaScript 语法与单元测试、完整二进制构建。结果和改动存入升级记录，首页显示候选版本。
4. 点击“确认安装并重启”。系统暂停新增运行、等待既有运行结束，保留原源码和二进制以及两个数据库快照，然后自动切换并重启应用。
5. 新版本健康检查失败则恢复旧代码/程序并重启，不用旧数据库覆盖新记录。父进程或机器在安装中断时，会先恢复持久化的 `INSTALLING` 记录，再对外提供服务。

“成员管理 → 系统岗位 → 升级构建”可选择本机 Cursor/Codex 成员。dev 使用已有的 Cursor 工作助手，沿用成员配置的模型和角色，不创建替代成员或改变已有工作 Session。构建失败只记录错误，不触碰线上源码。升级构建器、守护进程、数据库迁移、依赖和启动/认证策略属于保护区，必须人工发布新基线。

每次安装的备份和候选位于 `data/dev/upgrades/<id>/`，与日常恢复点并存。不自动推送 Git，不删除历史候选或原生 Session。若日志报告“升级恢复受阻”，服务会保持维护锁并停止启动，避免对外运行未验证的混合版本。此时应先检查该升级记录、候选和 `backup/`，修复/恢复已审查的源码与二进制后再启动，不删除 SQLite 或通过清空维护锁绕过恢复。

## 安装和维护

需要 Linux amd64、Go 1.27.1、Node.js、bubblewrap 和已登录的 Cursor Agent / Codex CLI。此机 Cursor 版本为 `2026.09.02-c22c1a3`，Codex 为 `0.151.0`，bubblewrap 为 0.4.0（可在 `NoNewPrivileges=yes` 下使用）。Codex 沿用 dev 已有 ChatGPT 登录，不复制认证文件。systemd 单元固定 Go 1.27.1 的绝对路径以及 `GOMODCACHE=/home/wangyunlai.wyl/local/go/lib/pkg/mod`，验证器不继承 shell 的旧 `GOROOT`，也不依赖 SSH 的 GOPATH。守护进程在实际后台环境中先检查沙箱、编译器及离线依赖图。依赖应提前从 Go 模块代理取得并按 go.sum/校验服务校验，候选构建禁止联网下载。

```bash
cd /data/wangyunlai.wyl/workspace/work-assistant
GOTOOLCHAIN=go1.27.1 GOPROXY=https://goproxy.cn go test ./...
GOTOOLCHAIN=go1.27.1 GOPROXY=https://goproxy.cn go vet ./...
GOTOOLCHAIN=go1.27.1 GOPROXY=https://goproxy.cn go build -trimpath -o ./bin/ ./cmd/...
node --test internal/server/ui/*.test.cjs
```

第一次部署时用 `node deploy/dev/prepare.mjs` 生成私有环境文件（不覆盖已有值），将 `deploy/dev/work-assistant.service` 安装到 `~/.config/systemd/user/work-assistant.service`，再 `systemctl --user daemon-reload` 和 `systemctl --user enable --now work-assistant`。

后续更新前先创建并验证恢复点、确认无活动 Run、保留旧程序，再停服务、构建/切换并重启验证。不要在已有进程运行时直接覆盖其可执行文件，也不要用旧版程序打开新 schema。尚未给 Git 配置外部远程，不自动向 GitHub 发布源码。

dev 仓库的 `docs/design/` 另外归档了当前项目已有的抽象架构、详细设计和用例分析文档。实际验收记录见 [VERIFICATION.md](VERIFICATION.md)，历史登录限制与后续实际模型验证按日期区分。

Cursor 接口依据：[CLI 参数](https://cursor.com/docs/cli/reference/parameters)、[公开事件格式](https://cursor.com/docs/cli/reference/output-format)、[权限](https://cursor.com/docs/cli/reference/permissions)。本应用不承诺 Cursor 未上报的中间输出或完整 OS/秘密读取隔离。
