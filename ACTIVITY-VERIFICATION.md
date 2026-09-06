# Agent 实时行动：实现与验证记录

日期：2026-09-06（Asia/Shanghai）。已部署到本机 `http://127.0.0.1:17343/`。

## 实现范围

工作详情顶部增加实时行动面板，展示命令、工具调用、文件变更、检索、计划与公开消息，包含行动状态、观察/原生耗时、输出、退出码、错误和原 Run / Session / 成员 / Runtime / 模型归属。页面提供按轮次筛选、展开明细和历史分页；更新卡片不重建整个任务页，未发送聊天草稿和验收文件选择保持不变。

当前接入 Codex CLI `exec --json`，继续原 `exec resume` 和原 Session/权限。公开事件格式按 [Codex 官方非交互执行文档](https://learn.chatgpt.com/docs/non-interactive-mode) 核对；单个命令是否有中间输出取决于 CLI 实际发出的事件，不承诺逐 token 或逐字输出。没有结束事件不能推断成功，没有进度也不能推断卡住。不采集或展示内部推理。其它 Adapter 可以上报相同的 Action 结构；未适配者保留文字日志，未实现 OpenCode 或 Codex App Server 接入。

链路：Adapter → Runtime SQLite outbox → 原 JSON-RPC/WebSocket → 控制端 SQLite（事件及最新投影同事务）→ 任务级 SSE → 页面。没有新增消息中间件、gRPC、外部存储或新的任务调度实体。控制端 schema v11 只增加 `run_activity` 与索引；事件快照和任务映射按原备份机制保存，原生 Session 文件备份范围没有扩大。

关键实现：

- `internal/agent/activity.go`：只解析公开行动字段；每次 CLI 调用使用独立 scope，避免原生 item ID 在续轮时碰撞。`codex.go` 同时保留受限文字视图，以兼容升级构建日志等现有消费者；业务最终结果不因展示脱敏被改写。
- `internal/model/activity.go`：统一状态及有界字段、常见密钥格式尽力脱敏、UTF-8 安全截断。Runtime 入队和控制端落盘边界再次清理，重复清理幂等。它不是完整 DLP，不会追溯清理旧日志或原产物。
- `internal/store/activity.go`：可信身份从 Run 获取；Runtime/Task/Session/连接 epoch 必须匹配。原 outbox 事件保持原 epoch/seq，当前认证连接可重放；重复事件不重复保存。完成行动不被迟到的活动快照覆盖，未闭合行动随 Run 结束标为 UNKNOWN/INTERRUPTED。
- `internal/server/activity.go` 与 SSE：快照和游标同一读事务；任务级过滤、cursor 校验、Bearer 鉴权、250 ms 查询、5 秒保活、断开退出和写入截止时间。前端断线退避重连，不把 Token 放在 URL 中。
- `internal/server/ui/activity.js`：流式 UTF-8/SSE 分帧，以完整输出快照更新同一行动；使用 textContent，输出中的 HTML 不执行。最多约 500 条浏览器缓存，数据库历史不删除；原机器心跳与页面流连接分开显示，离线/断线时停止外推耗时。

## 自动化测试

以下全部通过：

```text
go test ./...
go test -race ./...
go vet ./...
node --check <每一个 internal/server/ui/*.js>
node --test internal/server/ui/activity.test.cjs
go build -trimpath -o <独立临时目录>/ ./cmd/...
```

最后增加旧文本日志消费者兼容断言后，重新执行 Agent/upgrade 普通与 race 测试及全量 vet。构建输出为五个独立程序。

新增验证覆盖：

1. Codex JSONL 开始/更新/完成生命周期、同一命令输出快照替换、跨 CLI 调用 ID 隔离、原生线程引用、六类公开行动、退出码/失败状态、内部推理过滤；展示脱敏不改变业务最终输出。
2. 常见密钥格式清理、Unicode 截断和重复清理幂等；Runtime outbox 重开仍保留两阶段行动、顺序和旧 epoch，敏感展示字段不先进入 spool。
3. 行动归属、输入身份伪造拒绝、事件去重、迟到更新不复活终态、任务/验收/产物不被观察改变；同一 Session 的不同 Run 可有同名行动。
4. 最新快照与 SSE payload 完全一致，三页历史分页，旧日志限量返回有序、备份重开保留行动、原连接 fencing 和跨 Runtime 重启的可靠重放。
5. Run 完成/失败/中断都关闭未结束的行动；未接收到最终结果时不填充虚假成功。无效 Action 事务回滚，不消耗去重键。
6. HTTP 鉴权、无效游标、不存在的任务、静态文件和页面挂载；真实 HTTP 流在 Run 仍为 RUNNING 时返回事件，不等结果完成。
7. 快照之后、订阅之前提交的事件不丢失；其它任务事件不泄露；连接断开后用 Last-Event-ID 只补发后续事件；空闲 SSE 收到保活。
8. 浏览器端协议测试覆盖逐字节 UTF-8、CRLF、多行 data、保活、超长/未完成帧、重复/乱序快照、跨 Run 同名行动和不重复拼接输出。

## 浏览器演练

用显式构建标签 `activity_ui_fixture` 启动 `TestActivityBrowserFixture`，监听 `127.0.0.1:17344`。独立临时 SQLite、模拟 Runtime，预置 55 条公开历史行动，再每 3 秒上报命令状态/输出。演练不调用模型、不访问正式数据库；源码及测试显式标注模拟，测试服务已停止。

通过应用内浏览器观察到：

- 命令在运行中即出现，随后追加阶段输出并更新为完成及退出码 0，原行动 ID 不变。
- 稳定显示“实时连接”，原 Run / Session / 机器信息与交付者一致；任务仍待人工验收，没有因观察而完成。
- 点击加载更早历史可看到最早的第 00 条记录，同时新行动继续到达。
- 模拟 token 显示为 `[REDACTED]`，脚本文本按字面显示；行动区实际 script 元素数量为 0。
- 填写但未发送的验收问题，在多轮实时更新、历史分页和窄屏检查后仍逐字保留。未发送到 Agent。
- 窄屏实测 DOM 宽度 375、scrollWidth 375、行动面板宽 345，没有横向溢出；做过桌面/窄屏截图检查，临时 viewport 设置已恢复。非真实手机设备测试。
- 页面错误日志为空。浏览器控制过程中出现过一次命令分发超时，改用已支持的 DOM 检查继续取得页面与草稿证据；没有绕过控制接口或使用备用自动化浏览器。
- 正式版本通过浏览器打开原历史工作，显示实时连接、当前无运行、0 条结构化行动及原文字日志，错误日志为空。

断线续传的准确游标由 HTTP/前端协议自动化测试覆盖；未做真实网络设备断网演练。本次没有调用真实模型或创建正式测试任务，因此不把模拟事件演练写成真实模型、远程机器或其它 Adapter 的现场端到端验证。后续新启动/继续的 Codex Run 才产生结构化行动；旧任务不补造历史。

## 正式数据和部署

备份根目录：

`/Users/wangyunlai.wyl/Library/Application Support/WorkAssistant/backups/0f6b30d4e0caf5a92f65c7b3`

- 升级前手工恢复点 `snapshot-20260906T025850.597574000Z-136485267`，`assistantctl backup-verify` 成功。
- 原五个程序保留在 `release-before-activity.ryqrtI/`。
- 确认活动/排队 Run 为 0、升级任务为 0 后，精确停止原 supervisor，确认原进程及子进程退出，再替换五个程序并经原启动脚本启动。
- 新实例 `supervisor_1788663609087_586ba9fa1477d1d83492`；ready 正常，原 `local-role-studio` Runtime 在线并声明 `structured_activity: true`。
- schema v11，`integrity_check=ok`，`foreign_key_check` 无违规；`run_activity=0`、`review_turn=0`，没有演练数据混入正式库。
- 与升级前快照对 25 张业务/历史表执行双向 EXCEPT，差异全部为 0；对原 event_log 再做旧记录缺失/改写检查，结果为 0。排除 schema、机器在线状态与传输 outbox 的正常运行期变化。
- 保留 10 个 Task、12 个 Run、7 个 Session 及映射、4 个角色、2 个成员、1 个首页对话、2 项托管工作、4 个产物、4 张验收单与 3 份总结。
- 升级后手工恢复点 `snapshot-20260906T030120.935089000Z-1348050915`，校验通过。Runtime 快照 SHA-256 与升级前相同：`70f2b2cd500e4e8eba52a7e43a3ad1336d1304119c706d5050a45dea0d46a6f7`。
- 正式 `/assets/activity.js` HTTP 响应与源码 SHA-256 一致：`9913d8a95553e91ccfc3595f8c9a0a99269dd6925673feaeaadf66e4f2cbd698`。

已部署程序 SHA-256：

| 程序 | SHA-256 |
|---|---|
| assistant-local | e8b54bf0efeb91e9685e99d8789be259949a60165e169241ffb41cb8c6f05a04 |
| assistant-runtime | 4ab8b899c3f3daf77d7d33482747d4513733fcdd20d41e0c6fabdf0f3b8d219d |
| assistant-supervisor | 81ac914bd30cac29215aa65a3531dabf2a6f334fbc44105c5beeac456932eb93 |
| assistantctl | 2898a39bf655d02990223709e32a880d798e5f269076da21c3b50f0bc57360d9 |
| assistantd | bee059252bd763f180014b453369a19587a199f22ecda314a32db7ea034eacfb |

旧版本不能直接打开 v11 数据库；回退需按既有离线恢复/对账流程操作，不能覆盖当前原库。恢复点仍是本机文件，不代表已有异机副本或绝对零丢失保障。
