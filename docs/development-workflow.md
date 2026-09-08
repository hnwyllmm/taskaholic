# 方案评审与受控开发（schema 17）

## 生命周期

新根任务第一次分派给具有 `code.implement` 能力的成员后，Manager 自动启用开发流程。普通写文档成员、内部 PR reviewer、角色生成器和升级任务不转换。历史任务保留旧语义；显式 `POST /api/v1/work/tasks/{id}/development/restart`，body `{"expected_version":当前任务版本}`，可重新进入方案阶段。

1. `PLANNING`：开发 Agent 在只读沙箱分析真实代码；提交完整方案文件与 `plan_scope.repository/base_branch`。
2. `AGENT_REVIEW`：Router 选择独立设计/架构成员，优先仓库专属能力。通过普通任务路由分派。没有合适 reviewer 时保留待评审状态及可见原因，不能跳过。
3. reviewer 以 `changes_requested/blocked` 反馈原开发者；开发者修订完整方案，再交回同一 reviewer Task / Session。每个 Session 的原生记忆仍由对应 adapter 管理。
4. reviewer 明确 `passed` 后进入 `HUMAN_REVIEW`。审批单引用开发者的方案 Run、完整文件版本、方案 hash、仓库和目标分支。人与开发者沿用原 Session 只读沟通。
5. 只有明确 `PLAN_APPROVED` 才进入 `IMPLEMENTING`。旧的 `APPROVED` 最终验收接口不能代替方案批准。点击“批准方案，开始开发”只授权该方案的实现与 PR 发布，不关闭任务。
6. 开发在 Session 下独立 checkout 中修改、验证。由 runtime 受控 commit/push/创建 PR，登记后复用已有 PR 轮询、自动 reviewer、测试、反馈修复与最终人工验收流程。不自动合并。

重大方案变更：Agent 返回 `replan`，或用户通过普通任务消息明确调整方向，撤销原批准并重新评审。方案审批聊天不自动修改成果、不自动批准。运行中到达新输入会阻止旧结果触发方案审批；版本过期的 reviewer 结论只保留历史。最多自动互审 8 轮，之后显式显示分歧并等待方向。

## 持久化与恢复

原工单回写：方案形成、Agent 互审及等待人工确认期间不发布方案进展；人工批准后，将用于外部回写的最终方案摘要作为单次里程碑发布到 AntMultica / GitHub Issue。中间方案与评审报告仅保存在内部任务中。后续实现、PR、测试及阻塞说明继续同步，不随状态变动重复发布方案。此规则仅为新批准启用最终方案发布，不回填历史审批、不改写或删除已有评论与发布记录，也不迁移历史数据。

- `development` 保存当前阶段、版本、方案 Run/hash、reviewer Task/Run、审批单与仓库范围。
- `development_run` 将每次 Run 固定到当时的方案版本/阶段，防止迟到结果推进新方案。
- 原有 `artifact`、`review`、`event_log` 和 Session 映射继续保留历史。schema 16→17 只增加两张表，不修改现有任务或启动 Agent。
- 重走流程要求任务及子任务空闲、没有已登记 PR、任务未完成。保留原任务 ID、来源链接、Agent、Session、历史产物与记录；旧待验收单失效。不把 AntMultica 原文、原始工单或已有记录删除重建。
- 数据库在线备份包含新表、审批与报告。原生 Codex Session、隔离源码和 `runtime work root/development/` 的 Git 元数据/发布回执仍在本地文件系统，**不在现有双 SQLite 备份范围内**；需另做文件备份/异机保护。不要清理这些目录。

## 执行权限和适配器

本版隔离开发支持 Codex。Cursor 仍保留只读 ask 模式；不自动换 Agent、模型或 Session，遇到不支持的 runtime 会排队并显示原因。runtime 通过 `approved_development` 能力协商，旧版本 runtime 不会收到可写任务。

Manager 签发内部 `ExecutionGrant`，包含审批单、方案 hash、GitHub 仓库及 base branch。它经已有带认证的 JSON-RPC runtime 通道传递，不能由任务源或模型直接提交。原角色的历史只读部署说明由本轮明确授权取代，角色职责和其它边界不变。

- runtime 使用主机配置的基准仓库，通过 SSH fetch 明确批准的 upstream 分支并固定 commit，为每个 Session 创建独立任务分支和 worktree。保留基准仓库的工作目录、分支和已有修改，不 checkout/reset/clean，也不改其 remote 配置。
- 主机 `work root/repositories.json` 配置仓库到 `directory`、`upstream_ssh` 的映射。路径与 SSH 入口由主机配置，不由模型指定。缺少配置或基准仓库时报错，不再为每个任务克隆整个 upstream。
- `gh api` 核实当前身份及同名 fork 的 parent 和 push 权限；基准仓库 origin 必须是该 fork 的 SSH 地址，可使用主机 SSH alias。新 worktree 只向核实后的 origin 推送任务分支，使用 `gh pr create --repo <upstream> --head <fork-owner>:<task-branch>` 发起 PR，不向 upstream 推送。
- Git 元数据仍在基准仓库的 `.git/worktrees/` 中，位于 Agent 可写 Session 之外。重试复用原 worktree，不覆盖未知残留目录；旧版已登记的独立 clone 保留兼容，不迁移历史数据。
- Codex `workspace-write`，不启用 full-access；Git 元数据和发布回执在可写 Session 目录之外。没有额外为 Agent 开放网络或外部系统写权限。依赖/远程 Windows 验证若不可用，应报告阻塞或未验证，不能虚报。
- runtime 使用已登录 `gh` 的现有 fork，核对 fork 的 parent 后，只推送 `work-assistant/<task_id>`，不强推、不建 fork、不合并。
- PR POST 前 fsync 记录尝试。响应不确定时按任务 marker 与 branch 查找已有 PR；未找到也不自动重复 POST。手动编辑/关闭的 PR 不自动重建。
- 角色/外部文本不能指定任意本地目录、凭据、Git URL 或发布目标。明显的 `.env` 和私钥文件阻止自动提交；这不是完整 secret scanner，不能代替安全 review。
- 这是个人受信任机器上的工作区隔离，不宣称是不同 Unix 用户/容器级隔离。

## 扩展边界与当前限制

源仍只负责采集事件。方案选择位于 `router.PlanReviewer`，状态机在 Store/Manager，原生运行在 adapter，受控 Git/PR 操作在 runtimehost，不塞进 AntMultica/GitHub 轮询器。

本版每个方案一个 GitHub 仓库及 base branch；跨仓库应拆成独立任务。修改仓库范围后不会覆盖已有 checkout，而会阻塞要求配置新隔离空间。尚无通用仓库配置 UI、Cursor 可写执行、自动 fork、PR 不确定状态解除 UI。现有任务重走通过带版本检查的 API 操作，页面仅展示阶段与审批。

## 验证要求

运行环境失败后的恢复使用 `POST /api/v1/work/tasks/{id}/development/retry`，body 为当前 `expected_version`。仅允许最新 Run 失败且审批仍有效、任务空闲的受阻开发任务；保留原 Session、方案 hash、审批和历史失败记录。普通任务消息仍表示调整方向，不能用普通聊天绕过审批。Git 错误仅返回已知脱敏错误类别和失败步骤，不回显可能含凭据的完整 stderr。

自动测试覆盖多轮互审、双方 Session 连续性、旧版本拒绝、普通聊天不能批准、无 reviewer 不跳过、显式人工批准后才有写授权、文档不能作为开发完成、备份恢复、PR POST 不确定结果不重复、foreign fork/符号链接拒绝。

真实 SEEK-533 测试必须在方案互审通过后停在人工确认点。未经用户查看并批准具体方案，不能为完成测试而代替用户点击批准；因此这一轮不能宣称已完成真实需求开发/PR/合并全流程。
