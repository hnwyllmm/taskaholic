# 系统岗位 Agent 可替换：验证记录

验证时间：2026-09-06，Asia/Shanghai。

## 已交付

- 成员管理的系统岗位配置：`home_chat`、`role_builder`、`task_router`、`upgrade_builder`。
- 首页/角色草案按指定成员创建真实 Run，包含成员身份、模型和角色快照。已有 Session 保持原归属。
- 首页显式接手：运行中拒绝切换；闲置时创建新 Session，保留旧记录，交接最近可见对话，作废未确认旧建议。
- AI Router：持久化决策与只读结构化 Run；仅选择合格候选；派发时再次校验版本、能力、成员状态和容量。规则模式和人工指定成员仍可使用。
- 本机升级构建保存成员/角色/模型快照，占用成员容量；不改变隔离副本、两次确认、安装或回滚权限。
- SQLite schema v9；岗位修改有乐观锁和事件记录，配置与路由决策随既有备份保存。

## 自动化验证

通过 `go test ./...`、`go test -race ./...`、`go vet ./...` 和全部 UI JavaScript 的 `node --check`，五个程序均重新构建。

新增测试覆盖：配置合法性/CAS/停用成员/本机限制、备份重开保持配置；不同成员模型实际进入首页和角色 Run；已有聊天保持 Session；显式接手保留原生引用和可见历史；路由重复轮询去重、无效输出、候选外选择、任务版本过期、并发抢占、内部任务排除效率总结；API 认证/同源保护、完整调度决策闭环、人工指定跳过 AI；升级成员快照、容量预留以及实际传入构建适配器的模型。

模型输出通过确定性的测试事件/适配器验证，没有为了验收向真实模型发送消息或创建生产任务。其它模型的账户权限、远程机器和未接入 Adapter 未作现场端到端验证。

## 页面验证

使用应用内浏览器检查成员管理、岗位选择、选中成员的机器/Adapter/模型/容量、保存按钮的脏状态、默认配置恢复以及首页旧聊天记录和“使用当前配置接手”入口。修正首次进入岗位区未自动加载的问题后，重新构建、重启并复核自动加载。

仅改变后撤销了未保存的下拉选择，没有改变用户的生产岗位策略；默认依然是聊天/角色/升级 `auto`，Router `rules`。持久化写入与切换的验证使用独立测试数据库。

## 数据与发布

备份根目录：

`/Users/wangyunlai.wyl/Library/Application Support/WorkAssistant/backups/0f6b30d4e0caf5a92f65c7b3`

- 更新前校验恢复点：`snapshot-20260906T012818.893614000Z-3374614363`。
- 旧五个程序：`release-before-system-agents.kJDdK0/`。
- 更新后校验恢复点：`snapshot-20260906T013343.960732000Z-3602520456`；再次执行 `assistantctl backup-verify` 成功。
- 最终服务实例：`supervisor_1788658368764_9e7914f5cd5f3906807c`，`http://127.0.0.1:17343/health/ready` 返回 ready。

以更新前的 SQLite 快照为基线，对 23 张原有业务/历史表执行双向 `EXCEPT`，均为 0 行差异；不把运行机器在线状态、schema version、事件日志和传输 outbox 的运行期变化视为业务修改。10 个任务、2 项托管工作、4 个角色、2 个成员、1 个首页对话、7 个 Session、12 个 Run、3 份任务总结均保留。`integrity_check` 为 `ok`，`foreign_key_check` 无输出。备份中的 Runtime SQLite SHA-256 与更新前一致。

最终程序 SHA-256：

| 程序 | SHA-256 |
|---|---|
| assistant-local | 89d83f68d33aff598066779575118ae422a4a9b53122cc1e8b3cbe7491832b97 |
| assistant-runtime | fe3f8c0a9a97a041bc3d26c7a25e55947e0f0bab072c1ccdd9413c64f27ede3b |
| assistant-supervisor | 7a0dfc584f139680975c229c9cb6608812dbb121ff3b0dc0295aea8a99521212 |
| assistantctl | 766b46a618b4bd4ff9ac1f3fc58bd822bf8cb9f925b18742accce1fa1def1c42 |
| assistantd | 274dc7be623ff2ed038a94eed64c687351f2ef576a037e53f8bd5ea512a60139 |

旧版本程序不能直接打开 v9 数据库；如需回退，必须停机并按数据保护流程核对、恢复到独立目录，不能覆盖线上数据库或直接回放未对账的消息。
