# 本地闭环验收 — 2026-09-05

本次实现的是文档 / 分析 / 只读评审的可用纵向闭环，不是完整 Multica → 多 Agent → PR → 合并产品。

## 自动化检查

- `go test ./...`、`go test -race ./...`、`go vet ./...` 通过。
- 两份浏览器脚本通过 `node --check`；启动脚本通过 `zsh -n`。
- 新测试覆盖：事务化任务提交、离线队列、容量恢复、消息重放、重复调度、消息与完成竞态、打断续接、暂停与完成竞态、非法产物、旧验收拒绝、角色/模型快照、备份重开、跨任务文件访问限制、跨域写入和尾随 JSON 拒绝。

## 真实执行（不是 mock）

通过内嵌 Browser 的页面操作创建“验收示例：Work Assistant 使用说明”，选择日常工作助手，其模型为 `gpt-5.6-sol`。

1. 创建后系统自动分派并生成 `quickstart.md` v1，任务进入 `WAITING_REVIEW`，没有自动关闭。
2. 从验收表单要求增加 macOS 使用提示和准确的备份范围。
3. 同一 Agent、同一控制 Session、同一 Codex 原生 Session 完成第二轮，生成 v2；v1 保留。
4. 检查 v2 内容后，通过页面批准这个**测试样例**，任务才变成 `COMPLETED`。
5. 程序重启后，记录、文件版本和验收结果均可查看；无重复执行。

控制 Session：`session_1788607841817_de7a63a051328e04a9f7`。

原生 Session：`codex:01a07155-cfe6-7fb3-99d0-1a6cf8735e85`。

任务：`task_1788607841670_5a4871642ad7fb3ce9df`。

两个 Run：`run_1788607841820_d92a33eca4fcfcfcac39`、`run_1788607893818_7e141e3ea050bde4d1a6`。

下载 v2 到 `data/role-studio/quickstart-verified.md`，实测 SHA-256 与产物记录一致：

`688ea2eaaa52a9dbb2ba4428a1f473898981d996c57cb82b778497a1142371dc`

## 页面与数据

- 已发布的内置角色从页面更新到 v2，原测试 Session 的角色快照仍为 v1。
- 页面 Agent 编辑保存通过；未修改既有用户角色或其它 Agent 的配置。
- 任务页面桌面布局检查通过；390 px 浏览器尺寸下内容宽与可见区同为 375 px，无横向溢出；测试后恢复原尺寸。
- 浏览器无 error / warn 日志。
- `workbench-verified-20260905.sqlite` 完整性检查 `ok`；含 2 个不可变产物版本、1 次要求修改与 1 次通过验收。
- 升级前 v4 数据另有 `pre-workbench-v5-20260905.sqlite` 备份，完整性检查 `ok`。

## 尚未覆盖 / 边界

没有验证真实仓库写入、PR、多 Agent 自主委派、Multica/GitHub 接入、局域网实机访问、OS 登录自启或跨机器原生记忆恢复。SQLite 备份不含原生 Session 文件和外部仓库。

工作台中的队列、人工门和产物存储是新增实现；旧式手动 Task/Run API 的行为为兼容而保留，不自动执行旧任务。详细操作及恢复边界见 README。

Codex 的 JSONL、结构化输出和恢复会话使用 [OpenAI 官方非交互模式文档](https://learn.chatgpt.com/docs/non-interactive-mode)核对；页面操作按 Browser 技能进行真实验收。
