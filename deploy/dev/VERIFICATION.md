# dev 部署验收记录

日期：2026-09-06（Asia/Shanghai）。本次为独立新部署，没有迁移或改动 Mac 实例的数据库、运行程序、原生会话或凭据。下文保留初始部署记录；用户之后要求的网络监听变更见文末补充。

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

## 补充：允许通过 dev IP 直接访问

2026-09-06，按用户要求将 dev 服务改为 `--listen 0.0.0.0:17343 --allow-remote`。未改变默认 loopback 启动方式或 Mac 实例。运行程序源码提交为 `31f0a72`，包含 `7af7297` 的远程监听校验和 HTTP 页面兼容修正。

- 非 loopback 必须显式开关并配置不同的 API / Runtime Token，各至少 32 个可打印非空白 ASCII 字符；检查在打开数据库、监听端口之前完成。原有 Token 保留且不进入命令参数。
- 内部 Runtime 使用 `http://127.0.0.1:17343`，不把 `0.0.0.0` 当作目标地址。配置在重启后生效，进程 active/running，通配监听可由 `ss -ltnp` 确认。
- 本机直连 `http://6.12.233.133:17343/` 返回 200；无 Token 的控制 API 和 Runtime WebSocket 均返回 401，没有改动防火墙或 NAT。
- 保留原 Cursor 成员和三个系统岗位绑定；原 Runtime 在线，启动备份成功。切换前确认活动 Run 为 0，没有中断正在执行的任务。
- HTTP 非安全上下文不能使用 `crypto.randomUUID`，共享页面改用 `crypto.getRandomValues` 生成 128 位幂等键。新增 Node 回归测试；本机与 dev 的启动入口/服务器测试、相关 race 检查和 Go vet 通过，6 个前端 Node 测试通过。
- 切换前恢复点：`snapshot-20260906T080131.313357017Z-3176536416`（同一备份根目录）。旧主程序和 systemd 单元保存在 `.deploy/listen-all-20260906/`，不进入 Git；SQLite schema 未改变。

当前直连是明文 HTTP，Token 鉴权不提供加密；只在可信网络使用。SSH 隧道仍可用。不将此网络验证视为 Cursor 账户或真实任务验证。

## 补充：局域网/VPN 免登录

2026-09-06，按用户明确要求，dev 服务加入 `--no-api-auth`，网页及控制 API 不再要求 Token。程序源码提交为 `e8d0e3c`。默认认证行为没有改变，私有环境文件中的原 Token 未删除或改写；去掉该开关即可恢复网页认证。

- `GET /api/v1/auth/config` 只公开 `api_token_required: false`，不返回任何凭据；页面据此隐藏 Token 连接设置并显示“免登录模式”。无需修改浏览器存储，已有保存的 Token 不被删除。
- Mac 与 dev 的启动入口/服务器 race 测试、dev Go vet、8 个 Node 测试通过。测试覆盖显式免登录、缺少 Runtime Token 时仍拒绝远程启动、默认认证不变、匿名同源写入及跨站写入拒绝。
- 实机无 Token 访问 `/api/v1/system` 及首页数据、工作列表、角色、成员、机器、团队资料、备份等读取接口均成功；`/runtime/ws` 无 Token 仍为 401，携带其它站点 Origin 的写入请求仍为 403。
- 服务重启 active/running，原 Cursor 成员、三个系统岗位绑定和在线 Runtime 均保留，活动 Run 为 0；启动备份成功且无备份错误。本次未进行模型调用，未创建真实测试任务。
- 切换前已验证恢复点：`snapshot-20260906T083044.504856966Z-1480775297`。旧主程序和单元保存在 `.deploy/no-api-auth-20260906/`，SQLite schema 未改变。
- 浏览器自动页面演练两次超时，未作为成功证据；本次以实际 HTTP 访问与页面逻辑自动测试验证免登录行为。

免登录模式不自动限制 LAN/VPN 来源：所有能连接 17343 端口的设备均可访问数据、操作任务和备份。网络范围仍由用户的局域网、VPN、防火墙控制，本次未扩大防火墙规则或删除 Runtime 认证。Cursor 自身的账户登录仍独立，不因网页免登录而跳过。

## 补充：后台守护进程与 Cursor 自升级

2026-09-06 17:39 起（Asia/Shanghai），正式程序对应源码提交 `9a1713d`，包含 `11fc2c5` 的升级实现及后台离线依赖检查修正。此节替代早期记录中“Linux 自动升级关闭”和“Cursor 登录尚未验证”的限制；历史记录保留。

- 正式进程链：systemd 用户管理器 `34389` → `assistant-supervisor` `177316` → `assistant-local` `177459`。PID 仅是本次验收快照。服务 enabled/active/running，`Linger=yes`、`Restart=on-failure`、`NoNewPrivileges=yes`、`KillMode=mixed`，最终检查 `NRestarts=0`，不依赖 SSH 会话。
- 继续监听 `0.0.0.0:17343`，Mac 直连首页返回 200，`/health/ready` 返回带 supervisor instance 的 ready。`/api/v1/upgrades` 返回 `enabled:true`，系统状态显示 `Linux bubblewrap`；浏览器免登录保持不变，未认证 Runtime 仍返回 401。
- 升级构建绑定原 Cursor 成员 `agent_1788680851852_1fccbccd22469176cb6b`，版本 2。首页聊天、角色设计和路由的原绑定及版本未改；模型仍为 `auto`，没有创建新的正式成员或测试工单。
- 单元固定 Go 1.27.1 绝对路径和 `GOMODCACHE=/home/wangyunlai.wyl/local/go/lib/pkg/mod`。启动先检查沙箱、编译器和实际源码/测试的离线依赖，避免错误使用 systemd 默认空缓存；不会因不可用而降级到裸执行。
- 曾因启动检查过度要求未使用的上游测试依赖而启动失败，部署保护脚本自动恢复了上一版后台程序及单元，没有恢复或覆盖数据库。已用实际源码依赖检查修复，并先在独立 systemd 测试服务中验证成功后发布；测试服务已停止。

验证范围：

- 最终 dev 源码全量 `go test -race ./...`、`go vet ./...` 和 9 个 Node 前端测试通过；Mac 全量 race 回归及后续增量测试通过。
- 真实 Cursor 在临时项目中生成结构化补丁，候选达到 READY，并记录原生 Cursor Session；在从后台服务获取的环境配置下也成功。没有使用 `--force`，普通 Cursor 的 Write/Shell/MCP 禁止规则不变。
- 整个应用的源码候选在 bubblewrap + `NoNewPrivileges` 下完成启动预检、全部 Go 测试/vet、前端语法和单元测试、五个程序构建，达到 READY。没有把候选测试当作真实生产功能变更来安装。
- 隔离测试验证无法读写宿主私有测试文件、不能写升级目录之外、不能连接宿主 loopback 服务；隔离命名空间中的本地 HTTP 测试仍可正常执行。
- 真实进程夹具验证安装成功、健康检查失败自动回滚、子进程异常退出后拉起、安装完成但未记成功时恢复，以及候选被破坏时保持 INSTALLING/维护锁并拒绝启动混合版本。
- 升级备份包含 control.sqlite 和 runtime.sqlite；测试确认代码回滚不会删除备份之后写入的数据库记录。最终切换前活动 Run 为 0。

数据保留核对：`task=2`（包括系统内部任务）、`run=7`（全部完成）、`session=1`、`task_session=1`、`home_chat=1`、`agent_profile=1`；Runtime 的 local_run=7、inbound_message=7、outbound_message=35。用 `node deploy/dev/inspect.mjs` 比较切换前后，六个业务表的完整记录 SHA-256 全部一致，两个数据库 quick_check=ok、外键错误=0，原生 Session 引用没有替换或公开。

恢复点均位于 `/data/wangyunlai.wyl/work-assistant-backups/dev/e688631bf5cc20df2aa521fd/`：

- 切换前：`snapshot-20260906T092337.449910849Z-1708334599`，已离线验证。
- 最终切换后：`snapshot-20260906T093946.217008982Z-4234813536`，已再次通过 `assistantctl backup-verify`。

| 最终恢复点文件 | 字节数 | SHA-256 |
|---|---:|---|
| control.sqlite | 577536 | 48909dab353a8a7391bc0aaf65f116dc290a0ce044467322aefdbe59ce391af8 |
| runtime.sqlite | 192512 | d1ea19245ebd0c67b704f4440f0c02d1f73c0efdd0f80a574a9388c700422d2d |

最终安装二进制 SHA-256：

- assistant-local：`1664f687c632a8f0fd7179160fb4f19dc7f0b749e7990f62ce0c645de447df6c`
- assistant-supervisor：`248eb4db7c3730801c170d68e56aaee7a7c511180d527fe79a8f25c647c4b793`

旧程序和单元保留在 `.deploy/supervisor-20260906/previous/` 及 `before-cache-pin/`，不进入 Git。未修改 Mac 正式实例、认证文件或 SQLite schema，未向 GitHub 推送。数据库迁移、依赖、升级器和启动/认证策略仍是人工发布保护区；备份仍为同机副本，不包含 Cursor 自己管理的原生 Session 文件。

## 补充：成员模型下拉与手动填写

2026-09-06 20:50（Asia/Shanghai），源码 `8b34820` 已在 dev 上部署。添加/修改成员及角色草案的手动运行环境复用模型控件，支持“下拉选择 / 手动填写”、默认模型和未收录的自定义 ID。系统岗位补充说明：它是现有成员承担系统内置功能的绑定，并不是另一类成员；未改名称、布局位置或实际分工。

- 可选 `agent.ModelProvider` → Runtime `models.list` → HTTP 模型列表接口。Cursor 在 dev 上真实返回 24 个模型选项（包含 `auto`）；列表来自 CLI，不创建 Run 或原生 Session。其它 Adapter 暂无动态提供器时明确降级为已有配置和手填，不伪造在线型号。
- Mac 和 dev 全量 `go test -race ./...`、`go vet ./...` 通过，14 个 Node 测试通过。覆盖模型解析、命令参数/凭据过滤/超时/输出限制、错误不回显、查询并发缓存与失败恢复、HTTP 鉴权/离线/旧 Runtime 以及实际 WebSocket RPC、两种填写方式、旧型号保留和跨机器异步响应竞争。
- 部署前后 task=6、run=12、session=4、task_session=4、agent_profile=2、home_chat=1；这六个业务表记录摘要完全一致。四个系统岗位绑定未改，两个 SQLite quick_check=ok、外键错误=0。无数据库迁移、无测试成员或测试工单写入正式数据。
- 部署恢复目录 `.deploy/member-models-20260906/` 保留旧源码、旧程序、前后只读审计及模型查询结果。未修改服务单元、监听范围、登录策略、认证文件或 Mac 正式实例；未推送 GitHub。
- 同一备份根目录下，切换前 `snapshot-20260906T125012.483368281Z-3660684957`、切换后 `snapshot-20260906T125017.893177520Z-701259224` 均通过 `assistantctl backup-verify`。
- 页面自动浏览连接两次超时，未声称通过视觉验收；交互逻辑由 DOM 单元测试验证，正式链路由真实 HTTP/Runtime 查询验证。

## 补充：任务创建后的指定成员与自动分派

2026-09-06 21:17（Asia/Shanghai），源码 `98576f7` 已发布到 dev。创建弹窗默认“先创建，稍后分派”，保留直接自动分派或指定成员执行的选项。任务详情新增“执行安排”，尚未建立执行 Session 的任务可指定成员或改为自动分派；待分派任务出现在等待队列和首页提醒中。已指定但仍等待的成员显示在工作列表，不与实际 Session 归属混淆。

- 后端新增带任务版本校验的 assignment API；首轮调度校验同一版本，角色/能力/排除规则仍生效。旧 AI 路由决策失效并记录中断，晚到结果不能覆盖人工选择。已有 Run / Session 拒绝直接换人，历史和原生上下文不被重置。
- 无数据库迁移。待分派任务使用 NEW 和既有 paused 标记阻止调度；补充要求、数据库重开、备份恢复以及旧版调度条件都不会自动解除保持。明确分派后才进入 QUEUED。
- Mac 全量回归及最终存储/服务增量 race、Go vet 通过；dev 最终源码全量 `go test -race ./...`、Go vet、五个程序构建通过。19 个 Node 测试通过，包括完整任务页脚本的先保存后手动/自动分派、直接创建执行、候选约束、版本冲突和保留未提交选择。HTTP 测试覆盖鉴权、首页待分派提醒、实际调度到指定成员及自动选择空闲成员；未在正式库创建测试任务。
- 切换前后 task=6（含内部任务）、run=12、session=4、task_session=4、agent_profile=2、home_chat=1；六个业务表的完整记录摘要相同，四个系统岗位绑定未改变，两库完整性和外键检查正常。
- 同一备份根目录下，切换前 `snapshot-20260906T131725.019827289Z-1417530032`、切换后 `snapshot-20260906T131730.583718223Z-2658269091` 均已验证。旧代码、程序及前后审计保留在 `.deploy/task-assignment-20260906/`。没有覆盖数据库、修改登录策略或 Mac 正式实例，未向 GitHub 推送。
- Mac 直连验证任务页面、新脚本和样式均返回 200，新增 API 拒绝缺少版本的请求；原 Cursor 模型查询仍返回 24 个选项，Runtime 连接与守护升级正常。Browser 页面连接仍超时，视觉验收尚未完成；不把脚本模拟测试当作浏览器渲染验收。

## 补充：Codex 并存与 SeekDB 开发/评审团队

2026-09-07 11:05（Asia/Shanghai），源码 `07b57ae` 已部署。`work-assistant.service` 同时启用 Cursor 和 Codex，Runtime ID 保持 `dev-cursor`；原两个成员和四个系统岗位不变。服务 active/running、`NRestarts=0`，本次监督进程 PID 为 `3047785`。HTTP 监听和认证设置不变，升级守护进程仍启用。

- 新增六个角色和各自一位 `· Codex` 成员：SeekDB 开发者、seekdb-bindings 开发者、通用代码仓库开发者、SeekDB / seekdb-bindings 架构 Reviewer、QA / 测试 Reviewer、通用 Reviewer。每位新成员 `model_id=gpt-5.6-sol`、`max_concurrent=1`、ACTIVE，沿用 dev 当前模型配置；原 Cursor 成员仍为 `auto`。运行态共 8 位可用成员。
- 架构评审涵盖扩展性、产品路径、跨仓库契约与兼容性；QA 涵盖方案/代码可测试性、场景覆盖、真实测试结果和最终制品验收；通用评审涵盖代码质量、性能、过度防御和低价值单测，并要求先查找、完整阅读和遵守仓库 code-review skill。实查 SeekDB 的 `.agents/skills/code-review/SKILL.md` 存在，seekdb-bindings 本次未发现；未开展实际仓库代码评审或修改这两个仓库。
- 模型发现复用可选 ModelProvider 和现有 RPC。dev 正式 HTTP/Runtime 查询返回 Cursor 24 个、Codex 7 个选项；Codex 从已安装 `0.151.0` CLI 的 stdio App Server 读取可见列表，不硬编码型号，不创建原生线程。成员的旧值、手工 ID 和默认模型仍可使用。
- Codex 执行固定普通任务的只读沙箱及不自动批准，过滤 `ASSISTANT_*` 凭据，取消时终止相应进程组。`--extra-adapters` 由 supervisor 传给子进程；重启不变更已绑定的角色、模型、成员和原生 Session。未增加数据库表、服务端口或仓库写权限。

验证：

- Mac / dev 全量 Go race 回归及 vet 通过，dev 五个程序构建通过，23 个 Node 测试通过。覆盖双 Adapter 保留主成员、无效/缺失 CLI 拒绝启动、子进程参数继承、模型发现的初始化/分页/重复游标/超时/边界与错误脱敏、进程组取消、角色增量发布与重复执行/冲突保护。
- `ASSISTANT_TEST_CODEX=1 go test -v ./internal/agent -run TestInstalledCodexCatalogAndNativeSession -count=1` 实际通过（65.94 秒）：真实结构化结果，第二轮只通过原 thread 恢复前轮随机标记。未在正式数据库创建测试任务。
- 独立 API 实例 `/data/wangyunlai.wyl/tmp/wa-codex-smoke-FeX8eN` 验证：正常任务产生 ready.txt 和待人工验收记录；验收聊天在原成员/原模型/原 Session 上回答前轮标记；交付文件没有变化，持久化活动记录 2 条。最终脚本退出 0，测试服务已停止。先前一次夹具误将已完成状态判为 SUCCEEDED 而非 COMPLETED，修正夹具后重新跑通；不是正式服务的业务失败。
- Mac 直连成员页面和新资源 HTTP 200。Browser 连接超时，未完成视觉验收；不把 API、源码或 DOM 模拟测试当作实际浏览器截图验收。

数据保护：

- 安装前/后六个主要业务表完整摘要一致。新增角色/成员后，进一步将安装前恢复点中的 47 条原有业务记录逐行与当前库比较，全部一致；会话、运行、聊天、旧角色、旧成员、工单和系统岗位未被重写。
- 最终 `task=12`（新增 6 个角色设计的内部任务，不是测试工单）、`run=12`、`session=4`、`task_session=4`、`agent_profile=8`、`role=8`、`role_draft=9`、`home_chat=1`。原有两个 FAILED 升级记录保留，未重试或扩大升级范围。
- 同一备份根 `/data/wangyunlai.wyl/work-assistant-backups/dev/e688631bf5cc20df2aa521fd/` 下，安装前 `snapshot-20260907T030454.369621890Z-1264462633`、安装后 `snapshot-20260907T030459.636096947Z-3198512547`、配置前 `snapshot-20260907T030529.892057019Z-3320448210`、配置后 `snapshot-20260907T030530.189281931Z-326917921` 均通过 `assistantctl backup-verify`。
- 旧源码、五个程序、原 systemd 单元与部署前后审计位于 `.deploy/codex-roles-20260907/previous/` 及同级 JSON 文件。角色模板已入 Git，实际角色、成员和任务映射在备份的 SQLite 内。备份仍是同机副本，不包含 CLI 自己管理的原生 Session 文件、凭据或外部仓库；没有向 GitHub 推送。

Codex 连接修复：

- 首次真实验证失败于旧代理链路：dev `.codex/.env` 中同时有旧 Mac 地址和失效的 `127.0.0.1:13678` 转发，返回 CONNECT 503。按 `restore-dev-codex-proxy` 流程重新发现地址，恢复为 `dev → 30.249.224.87:18080 → Mac 127.0.0.1:13659 → OpenAI`，没有把 SSH 当作代理传输。
- Mac LaunchAgent `com.wangyunlai.codex-dev-openai-proxy` 为 loaded/running，本次 PID `81519`；plist 为 `/Users/wangyunlai.wyl/.codex/restore-dev-codex-proxy/launch-agent/com.wangyunlai.codex-dev-openai-proxy.plist`，日志为同目录上级 `gateway.log`。仅绑定该 Mac 地址，要求已有代理认证及 dev `6.12.233.133/32` 来源限制，保持 CONNECT 443 白名单。
- 白名单为 api.openai.com、chatgpt.com、*.chatgpt.com、auth.openai.com、*.oaiusercontent.com，并依据日志中实际拒绝项补入精确的 developers.openai.com；未增加全域通配。实际模型返回 OK，之后会话和业务链路验证通过；非 OpenAI 的 example.com CONNECT 返回 403。
- dev 环境文件以 0600 原子更新，原文件保留为 `/home/wangyunlai.wyl/.codex/.env.bak-20260907-105827`。没有复制登录凭据、打印代理密码或修改 `.bashrc`；未重启无关的 Codex App Server/用户任务。Work Assistant 的 Codex CLI 每轮新进程读取当前环境。
- Codex 网络仍依赖这台 Mac 的现有代理在线；Mac 网络地址变化、退出登录或重启后需重新检查/恢复该链路。这不是 dev 完全独立的出网方案。Mac 正式 Work Assistant 实例、数据库和成员未改。

协议依据：[Codex 非交互执行与恢复](https://learn.chatgpt.com/docs/non-interactive-mode)、[App Server 模型发现](https://learn.chatgpt.com/docs/app-server)。实际命令同时以 dev 已安装版本的 help 和真实调用校验。

## 补充：统一 SeekDB / seekdb-bindings 开发者

2026-09-07 11:15（Asia/Shanghai），按用户纠正，两个仓库由同一位开发者负责，不再分别设置开发岗位。此节修正上节的六角色配置，历史记录保留。

- 保留原 SeekDB 角色和成员 ID，名称更新为“SeekDB / seekdb-bindings 开发者”和对应的 `· Codex` 成员；合并内核、绑定 API、跨仓库契约、构建打包与制品验证职责。成员仍为 `codex-agent`、`gpt-5.6-sol`、并发 1，权限边界不变。
- 误建的独立 bindings 成员停用，角色和成员均标记“已合并”，保留 ID、原配置内容和版本历史。变更前确认这两个成员均无任务执行或 Session 引用，也没有任务指定重复角色；未删除或迁移历史记录。
- 角色模板和种子测试改为五个工作角色。其它四个工作角色、两个原有成员及四个系统岗位绑定不变。通过带版本校验的配置 API 完成变更，无数据库迁移、无程序替换、无需服务重启。
- Mac 与 dev 的三个种子测试通过；变更后核对 task、run、session、task_session、home_chat 的完整记录摘要均与变更前一致。审计记录和一次性修正脚本保留于 `.deploy/developer-merge-20260907/`，不进入 Git。
- 同一备份根目录下，变更前 `snapshot-20260907T031506.802161250Z-3724103075`、变更后 `snapshot-20260907T031506.985999178Z-2660715458` 均通过 `assistantctl backup-verify`。保留历史配置后仍有 8 条成员记录，其中 7 位启用、1 位停用，不将历史记录数误报为当前团队配置数。

## 补充：Runtime 更名 dev 与双 Agent 类型下拉框

2026-09-07 11:55（Asia/Shanghai），源码 `35c4bed` 已部署。Runtime 统一为 `dev`，仍在同一机器、同一数据目录和工作区，同时注册 `cursor-agent`、`codex-agent`；服务单元及种子工具默认 Runtime 同步更新。成员配置、系统岗位、认证和权限边界不变。

- 原 Adapter 控件是填有 `codex-agent` 的 input/datalist，浏览器会按当前文本过滤候选，造成只支持 Codex 的错觉。现改为“Agent 类型”原生下拉框，按 Runtime 实际能力展示 Cursor Agent、Codex CLI 和后续插件；新成员切换类型时重置待填模型并读取对应列表，已建立的草案 Session 仍锁定原 Adapter 和模型。
- Runtime 更名仅修改控制库中的机器标识及 8 条成员、4 条 Session、12 条 Run 的引用，成员版本递增以拒绝旧表单覆盖。补齐新名称对应的初始化幂等键，使启动器仍使用原草案和原成员。原生 Session 引用、成员/任务 ID、工作目录、任务消息和角色历史保留；Runtime spool 与已送达传输消息不改，旧事件保留原名称并追加更名审计事件。
- 正式发布过程暴露并修复了两项遗漏：正常停服不会立即清除持久化 ONLINE 标记，现先核实 systemd 进程及监听均停止后同步；初始化幂等键包含 Runtime 名称，初次更名遗漏该键导致重复初始化和短时启动失败。补齐映射后启动恢复，未恢复或覆盖任何数据库。
- 对重复初始化产生的一份未执行配置，严格按改名前快照差集验证其角色、草案、角色版本、内部任务、任务版本共 5 条记录，保存在 `.deploy/runtime-dev-20260907/before-bootstrap-repair.v8` 后清理；原事件全部保留并追加恢复记录。原有任务、会话、角色和成员未删除。用户期间在页面上修改的开发者名称、职责和并发配置作为最新基线保留，没有用种子模板覆盖。

验证与恢复：

- Mac / dev 最终 41 项 Node 检查通过，涵盖双类型下拉、模型切换、Session 锁定、事务中途失败回滚、未排空/在线/命名冲突拒绝和初始化键保护。dev 全量 Go race / vet 通过；增加真实 SQLite + 实际离线改名模块 + bootstrap 再启动回归后，相关 Go race / vet 再次通过，确认不会创建第二个默认成员或草案。
- 最终真实 HTTP/Runtime 模型查询：Cursor 24 个、Codex 7 个选项，均 ready。Mac 直连成员页返回新的两个 select 控件。Browser 自动连接超时，未声称完成真实浏览器视觉验收。
- 修复阶段逐表检查改名前所有原有业务记录；最终 UI 发布再次对最新业务基线逐表检查，除正常 Runtime 心跳外无变化。最终 task=12、run=12、session=4、task_session=4、agent_profile=8、home_chat=1；两库完整性和外键检查正常。运行服务仍由后台 supervisor 维护，不依赖 SSH 会话。
- 同一备份根目录中，更名前恢复点 `snapshot-20260907T034508.709759416Z-4122543421`、最终页面发布前 `snapshot-20260907T035501.175961887Z-2232169319`、发布后 `snapshot-20260907T035507.046740320Z-1445967243` 均通过 `assistantctl backup-verify`。演练、失败尝试、源码/程序备份和逐表快照留在 `.deploy/runtime-dev-20260907/`，不进入 Git；没有向 GitHub 推送。
