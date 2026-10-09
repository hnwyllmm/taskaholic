# 受控的平台回写

## 职责边界

任务源只采集事件。Manager 从已持久化的 PR、原工单关联、Run 生命周期和 Agent 结构化结果生成 `publication`，独立后台执行器负责外部写入。`Publisher` 接口和 `Config.PublicationPublishers` 支持替换平台插件；没有任意 URL、命令或 HTTP 方法执行入口。

AntMultica 根任务的业务 Agent 首个 Run 真正收到 `run.started` 后，Manager 会排入一次幂等状态动作，将仍处于 `backlog`、`todo` 或 `blocked` 的原工单推进为 `in_progress`。仅仅创建、分派或进入 Runtime 队列不会修改外部状态。执行器固定使用已绑定的工作区、工单和成员身份，先读后写，以 `--no-start` 避免启动 Multica Agent，随后再次读取确认；已处于 `in_progress` / `in_review` 时不重复写，终态工单绝不回退。升级后也会依据持久化的 `started_at_ms` 补偿仍在处理的旧任务。

Manager 自己造成的 AntMultica 状态变化会在下一轮任务源读取时只推进游标，不形成新的任务消息；标题、描述、指派人或迭代属性等业务内容变化仍会产生更新事件。因此状态回写不会让开发 Agent 意外重开一轮或重新设计。

## PR 固定评审评论

- 身份为规范化 PR URL + 评审角色；跨 commit 和评审子任务保持同一评论 ID。
- 评论头部显示评审类别、配置角色、实际成员，以及角色、成员和评审任务的稳定 ID。即使多个 Reviewer 共用同一 GitHub 账号也能明确区分；配置名称按纯文本转义，不能注入链接、提及或额外 Markdown 结构。
- 只用普通 issue/PR comment 的创建和更新接口，不调用 reviews 的 Approve / Request changes。可以使用 PR 作者身份，但不满足 GitHub required approvals。
- 内容含明确的完整 SHA、角色、结论和 findings。新版本、待完成的新一轮或暂停不能显示当前通过。旧报告没有结构化结论时不猜测通过。CI 和回归 pipeline 不写成 reviewer verdict 的组成部分。
- reviewer 返回 `review_decision`: `passed`, `changes_requested`, `blocked`；非 reviewer 为空。QA reviewer 可以在 `passed` 或 `changes_requested` 报告中同时提交 `test_requests`。角色应把要发布的完整 findings 放在 `message`，不要只让人打开本地文件。
- 只有针对 findings 的外部讨论、开发者回复或同一 commit 的代码变更才续接原 reviewer Session。CI/pipeline 状态不重开 reviewer；它们由原开发任务作为独立交付门禁处理。不同 commit 仍按已有 Router 策略生成新评审子任务，但评论身份不变。
- reviewer 评论成功回写后，明确 `passed` 即可作为该角色的评审证据；原任务仍必须独立满足 GitHub CI、QA 要求的 pipeline、人类验收和 PR 合并门禁。

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
