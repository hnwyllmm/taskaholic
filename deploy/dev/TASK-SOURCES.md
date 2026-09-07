# 任务源（阶段一）

入口：`/sources`。人工录入仍在首页和工作列表。任务源页只配置采集范围、迭代、轮询间隔、忽略账号和启停状态；查看轮询失败、下次尝试时间、事件与对应任务。不在任务源配置执行角色、评审角色、成员、团队资料或调度暂停。

GitLab 回归测试支持见 [QA-PIPELINES.md](QA-PIPELINES.md)：QA 申请测试由专用动作执行器处理，GitLab 源仅采集已登记 pipeline 状态、产生原任务更新，不能发起或重试测试。

职责边界：`Provider / Poller → 持久化事件 → Manager → Router → 一个或多个待分派任务 → Agent`。已有归属的任务更新直接进入原任务消息队列，调度继续原 Agent / Session，不做重新分派。

## 当前接入

AntMultica 使用 dev 上已有的 `multica` CLI 登录态，只调用 `property list` 和 `issue list`，不读取或复制 Token。

- 工作区：`https://antmultica.alipay.com/seekdb`，UUID `44029359-53fc-4a6a-bd6f-aae91c2bd754`。
- 指派人：宁封，UUID `51410f21-5e32-490e-875d-eb3f28fb4095`；只匹配 member 类型的精确 ID。
- 迭代字段：`迭代`，当前值 `1.5.0`。每次读取属性定义，把名称解析为选项 ID；不存在或有歧义时停止导入，不放宽筛选。
- 未完成工单导入为普通、未预设角色/成员的工作任务，由既有 Router 根据任务内容与成员能力选择执行者。系统岗位使用 AI 路由时会生成持久化 AI 路由决策；规则模式继续使用原有能力约束与负载选择。人工暂停和指定成员是任务操作，不是采集配置。
- 修改迭代不删除旧任务。工单身份与任务映射持久化；标题、描述、状态等相关内容改变时更新原任务。已完成任务仅记录新事件，不自动重开。
- 不自动关闭、改派或评论 AntMultica 工单，不自动下载附件；原文中的链接作为材料保留。

GitHub 使用 dev 上 `gh` 的现有身份，只执行 REST GET；只跟踪明确登记的 PR，不扫描账号下所有仓库。

- 默认 5 秒一次；单控制端串行发起请求，实际周期也受请求耗时、目标数量与退避影响。
- 保留 ETag、分页缓存，支持 304；遵守 X-Poll-Interval、Retry-After 和主限流恢复时间。限流门槛持久化到同类轮询目标，重启不立即冲击接口。这些行为遵循 [GitHub REST 最佳实践](https://docs.github.com/en/rest/using-the-rest-api/best-practices-for-using-the-rest-api)。
- 读取普通评论、代码评论、提交的 review、check runs 与传统 commit statuses。仅当前 head 的失败检查生成更新；同一检查重复失败状态不重复生成，重跑后的新失败可生成事件。
- 首次登记前的旧评论不回放；编辑或新增评论可触发。旧 commit 的 inline 反馈、APPROVED/PENDING/DISMISSED review 和带 `<!-- work-assistant:... -->` 标记的自动回复被过滤。
- 同账号可能既代表 Agent 又代表用户，因此不笼统屏蔽 PR 作者。若以后接入自动评论发布，发布方必须附带上述标记；未带标记的第三方自动回复不能可靠区分。
- 对其余评论采取“产生待判断反馈”的策略，由原 Agent 判断是否需要修改或回复，而不是把每一句评论都当作强制改动。
- PR 关闭/合并后停止该目标轮询，记录状态；没有替代人工验收或自动合并。

## PR 登记与同 Session 续接

人工在任务源页点击“登记 PR”，选择已开始执行的原任务。尚未建立 Session 的任务不能登记，防止后续失去归属。PR 全局只能属于一个原任务，不能被另一任务/源接管。

Agent 的结构化交付可包含：

```json
{
  "outcome": "review",
  "message": "交付说明",
  "artifacts": [],
  "summary": {"result": "实际结果", "learnings": [], "improvements": []},
  "pull_requests": [
    {"url": "https://github.com/oceanbase/seekdb/pull/123", "source_id": "github"}
  ]
}
```

没有 PR 时返回 `pull_requests: []`；只有一个启用的 GitHub 源时，source_id 可为空。旧交付结果仍可解析。登记失败时保留交付，并显示需要检查的原因。自动评审子任务禁止登记 PR，避免递归邀请。

登记仅声明关联，不授予代码推送、评论发布或合并权限。当前正常工作 Agent 仍只读执行；本功能不意味着“自动写代码、提交 PR”的权限链路也已完成。

外部消息进入原任务的 PENDING 输入，当前 Run 结束后再创建下一轮 Run。调度沿用原 Task → Session → Agent/Runtime/Adapter/Model 绑定。源事件不会自动打断工作，不解除人工暂停，不换人、不换 Session。原成员离线时留在队列等待。

## 版本评审

每个新观测到的 head SHA 产生评审请求。Manager 调用 `router.ReviewPlanner`，将 Router 计划、独立子任务、父子关系和事件处理状态原子提交；计划含角色和理由，存入 `PRReviewRouted` 审计事件。每个子任务仍走现有规则 / AI 成员路由，排除原作者，不由任务源选人。

默认 `CapabilityReviews` 按成员声明的 `<repository>.review` 能力匹配仓库专属评审；同类角色有多个成员时只创建一项待分派任务。没有仓库专属成员时才使用通用 `code.review / architecture.review / qa.review` 等评审能力；不会将其它仓库专属成员混入。当前 SeekDB 与 seekdb-bindings 的架构、QA/测试、通用评审成员均符合匹配规则，职责和 skill 要求不变。该规划是可替换的规则策略，具体成员选择仍遵循系统岗位配置，不声称规划本身由 AI 完成。

暂时繁忙或离线不减少本轮评审职责，任务排队等待。没有独立候选成员时保留 PENDING 事件并显示 Router 错误，延迟重试，不把“无人评审”算作通过。默认每批最多 8 个角色；超限明确报错，不静默截断。评审子任务继承原任务冻结的团队资料。

评审输入包含固定 SHA、PR 地址和评审要求，不内嵌 diff；GitHub 轮询器不再读取 PR files 接口。reviewer 自行读取固定版本的完整变更、上下文、讨论及 CI，并核对 PR head，不能把更新后的版本算作旧版本。读取不足时必须明确 blocked。交付后内部评审任务自动完成并生成总结；所有评审完成后把结论和报告摘录送回原 Agent。完整报告保留在各评审任务；摘录不是完整原文。

## 任务来源链接

任务详情的 `work.references` 以结构化形式提供 PR / AntMultica 工单链接，页面在标题下显示可点击引用及固定评审 commit。AntMultica 使用 `https://antmultica.alipay.com/{workspace}/issues/{issue_id}`，不是工作区首页；该路由已从 2026-09-07 部署的前端路由构造器核实，issue_id 使用 CLI 返回的稳定 ID。新工单事件在 URL 字段及任务输入中保存详情链接；已有工单从持久化事件的工作区 URL 和 source_entity 中的工单 ID 还原链接，不依赖当前可变的任务源配置。评审子任务也能看到原需求工单链接。

已有评审任务使用 `work.review_brief` 展示精简目标和 PR 编号/角色名称，首条自动生成的 diff 输入在页面及后续新 Run 中换为链接式评审要求。历史 task.goal、消息、Run、审计和完成总结保持原样；原 Agent / 原生 Session 不变，不声称清除了 Agent 已接收的历史上下文。真正的评审 findings、人工消息与交付物不会被过滤。任务来源引用也加入后续 Agent 执行输入。

本改动不新增数据库表或迁移版本；引用依赖的实体、事件、PR 关联及固定 SHA 已由 SQLite 备份覆盖。只显示经对应来源验证的 HTTPS 链接，不把任意任务文本中的 URL 转成可点击引用。

新 head 暂停未完成的旧评审、标记旧批次 SUPERSEDED，保留原始结果。迟到的旧 head CI/评审汇总不会驱动当前版本。人工验收检查未完成评审、尚未汇总结果以及待处理 PR 事件，不能绕过这些门槛。

**采样边界：** 一次推送多个 commit 时，评审最新 head 的完整变更，不对每个中间 commit 重复消耗三轮评审。两次轮询之间已经被 force-push 丢弃的瞬时版本无法保证捕获；需要严格逐 push/逐 commit 历史时，应增加 webhook 接入。REST 代码评论也不等同于完整的 GraphQL unresolved-thread 状态跟踪。

## 持久化与进程

初版 schema 12 → 13 增加下列表和索引。职责修正 schema 13 → 14 只移除任务源中的旧执行配置，并提升配置版本防止旧表单覆盖；完整旧配置写入 `SourceExecutionSettingsRetired` 审计事件。历史任务要求、Run、Session、成员、对话、评审记录及游标不改写。

| 表 | 作用 |
|---|---|
| task_source | 插件类型、可修改配置、版本 CAS |
| source_target | 每个工作区/PR 的独立游标、原任务关联、启停、错误与下次轮询 |
| source_event | 稳定 ID 去重的 inbox、处理状态、失败重试时间、原始规范化事件 |
| source_entity | 外部工单与原任务的稳定映射 |
| source_review | Manager 管理的版本评审任务与汇总状态；保留原表名以兼容历史数据 |

完整的一次外部快照、所有事件和游标在同一个 SQLite 事务提交；分页失败不推进游标。事件处理和创建/更新工作也在同一事务。进程崩溃后重读游标和 inbox，不依靠内存队列。事件处理失败保留记录并延迟重试，避免单个错误阻塞后续事件。

轮询由既有 assistant-local / assistantd 控制进程维护，复用 systemd → supervisor 生命周期。`Engine.Tick` 只采集和保存，不调用 Router，不创建任务、不执行 fan-out/fan-in。Manager 的工作循环独立消费事件和汇总评审，不受一次远程轮询延迟阻塞。停用任务源只停止采集；已接收事件继续处理，内部评审结果直接由 Manager 回传，即使任务源停用也不会卡住。升级维护期间停止拉取和派发源事件；关闭时取消请求并等待源循环结束。无需增加独立 cron、消息队列或数据库服务。

所有新增表随控制 SQLite 的既有校验备份保存。备份仍不包含 gh/multica 凭据、外部仓库、原生 CLI Session 文件；同主机备份也不等于异地备份。

## 扩展与接口

`internal/tasksource.Provider` 是只读生产者接口；`Engine.Providers` 是插件注册表。后续 MCP/API/webhook 适配器应输出同一种 SourceEvent，复用持久化、归属、调度和汇总，不直接调用 Agent。配置验证与 UI 按插件 schema 扩展；本阶段不允许网页配置任意 shell 命令、Token 或任意认证 URL。

Router 插件独立：`server.Config.ReviewPlanner` 替换评审分工策略，`AgentSelector` 替换成员选择策略。同步 ReviewPlanner 必须纯计算、耗时有界，不在数据库事务中执行网络请求或模型推理；需要异步 AI 规划时应仿照已有 routing_decision / Run outbox 持久化决策后再应用。Manager 会复核计划的去重、成员有效性和作者排除约束。

- `GET /api/v1/sources`：配置、目标、最近 200 条事件、最近 500 条评审。
- `PUT /api/v1/sources/{id}`：`{"source":{...},"expected_version":N}`，首次创建 N=0。
- `POST /api/v1/work/tasks/{id}/pull-requests`：`{"url":"...","source_id":"github"}`。
- `PUT /api/v1/source-targets/{id}`：`{"enabled":false}`，仅停止轮询，不撤回已持久化事件。

CLI 路径可通过控制进程启动环境的 `WORK_ASSISTANT_GH_BINARY`、`WORK_ASSISTANT_MULTICA_BINARY` 替换，不对网页开放。子进程只继承认证/网络/基础运行所需白名单环境，不继承控制 Token 等系统秘密。GitHub 初始请求与分页严格限制到登记仓库和 api.github.com。分页允许 `/repos/owner/repo/`，以及由首次 PR 响应的 base.repo.id 和 full_name 共同确认的 `/repositories/id/`；不信任 Link 中任意仓库 ID，不把 fork 的 head 仓库当作目标仓库。

只读连通性验证：

```sh
go run ./deploy/dev/source-probe --kind antmultica
go run ./deploy/dev/source-probe --kind github --pr https://github.com/owner/repo/pull/123
```

probe 不打开生产数据库、不登记 PR、不创建任务；只打印规范化事件数量、标题和第二次轮询的去重结果。
