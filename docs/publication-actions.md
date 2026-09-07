# 受控的平台回写

## 职责边界

任务源只采集事件。Manager 从已持久化的 PR、原工单关联和 Agent 结构化结果生成 `publication`，独立后台执行器负责外部写入。`Publisher` 接口和 `Config.PublicationPublishers` 支持替换平台插件；没有任意 URL、命令或 HTTP 方法执行入口。

## PR 固定评审评论

- 身份为规范化 PR URL + 评审角色；跨 commit 和评审子任务保持同一评论 ID。
- 只用普通 issue/PR comment 的创建和更新接口，不调用 reviews 的 Approve / Request changes。可以使用 PR 作者身份，但不满足 GitHub required approvals。
- 内容含明确的完整 SHA、角色、结论、findings 和 pipeline 链接。新版本、待完成的新一轮、暂停、必需测试未成功都不能显示当前通过。旧报告没有结构化结论时不猜测通过。
- reviewer 返回 `review_decision`: `passed`, `changes_requested`, `waiting_tests`, `blocked`；非 reviewer 为空。角色应把要发布的完整 findings 放在 `message`，不要只让人打开本地文件。
- 外部讨论、pipeline 终态沿用原 reviewer Session 继续；开发者提交对未解决问题的回复也会触发复审。每个版本最多自动 8 轮，再有讨论进入需要处理状态。人工暂停不自动恢复。不同 commit 仍按已有 Router 策略生成新评审子任务，但评论身份不变。
- 评论送达和当前版本必需测试成功后，才允许把新的明确 `passed` 报告作为原任务验收依据。原任务的人类验收与 PR 合并授权不改变。

## 原工单进展

Agent 使用 `task_update` 提交用于外部发布的精简分析：kind（bug/feature/other）、analysis、approach、reason、validation、blocked_reason。BUG 登记 PR 时须有问题分析、方案和修复理由；无法修复必须说明原因。

AntMultica 工单来自既有 source_entity 关联；GitHub Issue 可通过 `POST /api/v1/work/tasks/{task_id}/origin-issue` 显式绑定，body 为 `{"source_id":"github","url":"https://github.com/owner/repo/issues/123"}`。此接口只是绑定/回写，不宣称已经支持自动发现 GitHub Issues。

回写采用每个业务里程碑一条评论，包括工作助手状态、最近提交的分析、PR 与 pipeline 记录。不覆盖工单描述、不修改平台状态字段，不因提交 PR 自动关闭工单。没有 task_update 的历史自由文本不自动批量发布；现有历史记录和 Session 不迁移、不篡改。

## 持久化与恢复

SQLite schema 16 新增 `publication` 和只追加的 `publication_history`，纳入现有在线备份。记录目标、期望版本、已同步版本、评论 ID/URL、已写正文、内容摘要、状态及失败信息。提交前持久化 SUBMITTING；进程崩溃或写超时按 UNCERTAIN 对账。核对唯一标记、精确目标及评论作者，已存在则复用；固定评论被人工改写后不强制覆盖。

新评论创建结果不明时只查询、不盲目重发。工作详情的“平台回写”表格显示错误；人工在原页面确认确未发布后，可以请求再次完整核对并允许重试（`.../publications/{key}/resolve`，`confirm_not_created:true`）。已保存评论 ID 的记录不能用此操作另起一条评论。

评论携带 `<!-- work-assistant:... -->` 标记；GitHub 采集器忽略系统回写，内部评审结果通过 Manager 消息送回原任务，不依赖再次轮询自己的评论。AntMultica 采集器不读取评论，回写也不改其采集字段。

默认使用控制主机上 gh/multica 的既有登录，正文走 stdin，错误日志不保存命令 stderr 或凭据。Agent 子进程过滤控制凭据环境变量；这不等同于不同 OS 账号的文件隔离。

设置 `WORK_ASSISTANT_PUBLICATIONS_DISABLED=1` 可在维护/升级验证期间关闭回写后台循环；已持久化的队列和历史保留。维护锁同样阻止自动执行。关闭采集源不撤销已经接受的任务或待回写结果。
