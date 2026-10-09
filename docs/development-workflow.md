# 方案评审与受控开发（schema 17）

## 生命周期

新根任务第一次分派给具有 `code.implement` 能力的成员后，Manager 自动启用开发流程。普通写文档成员、内部 PR reviewer、角色生成器和升级任务不转换。历史任务保留旧语义；显式 `POST /api/v1/work/tasks/{id}/development/restart`，body `{"expected_version":当前任务版本}`，可重新进入方案阶段。

1. `PLANNING`：开发 Agent 在只读沙箱分析真实代码；提交完整方案文件与 `plan_scope.repository/base_branch`。
2. `AGENT_REVIEW`：Router 选择独立设计/架构成员，优先仓库专属能力。通过普通任务路由分派。没有合适 reviewer 时保留待评审状态及可见原因，不能跳过。
3. reviewer 以 `changes_requested/blocked` 反馈原开发者；开发者修订完整方案，再交回同一 reviewer Task / Session。每个 Session 的原生记忆仍由对应 adapter 管理。
4. reviewer 明确 `passed` 后进入 `HUMAN_REVIEW`。审批单引用开发者的方案 Run、完整文件版本、方案 hash、仓库和目标分支。人与开发者沿用原 Session 只读沟通。
5. 每个新方案同时提交结构化验证策略：复用已有证据、受影响需重跑、需要补测、本次排除以及稳定候选的最终门禁。Reviewer 必须检查证据适用前提和改动影响，不能用“全部重跑”或“历史全部有效”代替分析。验证策略随方案版本持久化并在任务页展示。
6. 只有明确 `PLAN_APPROVED` 才进入 `IMPLEMENTING`。旧的 `APPROVED` 最终验收接口不能代替方案批准。点击“批准方案，开始开发”同时批准实现范围与验证策略，只授权该方案的实现与 PR 发布，不关闭任务。
7. 开发时每次修改只执行直接受影响的重跑/补测项；代码、依赖、制品、配置或关键环境身份变化会使相关旧证据失效。候选稳定后执行最终门禁并逐项记录结果，随后才可申请创建 PR。
8. 开发在 Session 下独立 checkout 中修改、验证。由 runtime 受控 commit/push/创建 PR，登记后复用已有 PR 轮询、自动 reviewer、测试、反馈修复与最终人工验收流程。不自动合并。
   实施期间不额外设置 Phase 0/Phase 1、入口评审或清单评审等 Agent 放行门槛。实现和必要验证完成后提交 `publish_request`；PR 新建及每次出现新 commit 后，再由 Router 邀请配置的 reviewer。Agent 过早返回 `outcome=review` 且未发布 PR 时，Manager 续接原 Agent/Session，不创建人工验收或 PR 前复审任务。

重大方案变更：Agent 返回 `replan`，或用户通过普通任务消息明确调整方向，撤销原批准并重新评审。方案审批聊天不自动修改成果、不自动批准。运行中到达新输入会阻止旧结果触发方案审批；版本过期的 reviewer 结论只保留历史。方案互审和同一 PR 版本的讨论没有固定轮次上限；每轮仍必须带来明确的新结论或修订，调度器以退避控制失败重试频率，而不是耗尽次数后要求人手工续跑。

## 持久化与恢复

原工单回写：方案形成、Agent 互审及等待人工确认期间不发布方案进展；人工批准后，将用于外部回写的最终方案摘要作为单次里程碑发布到 AntMultica / GitHub Issue。中间方案与评审报告仅保存在内部任务中。后续实现、PR、测试及阻塞说明继续同步，不随状态变动重复发布方案。此规则仅为新批准启用最终方案发布，不回填历史审批、不改写或删除已有评论与发布记录，也不迁移历史数据。

- `development` 保存当前阶段、版本、方案 Run/hash、reviewer Task/Run、审批单与仓库范围。
- `development_run` 将每次 Run 固定到当时的方案版本/阶段，防止迟到结果推进新方案。
- 原有 `artifact`、`review`、`event_log` 和 Session 映射继续保留历史。schema 16→17 只增加两张表，不修改现有任务或启动 Agent。
- 重走流程要求任务及子任务空闲、没有已登记 PR、任务未完成。保留原任务 ID、来源链接、Agent、Session、历史产物与记录；旧待验收单失效。不把 AntMultica 原文、原始工单或已有记录删除重建。
- 数据库在线备份包含新表、审批与报告。原生 Codex Session、隔离源码和 `runtime work root/development/` 的 Git 元数据/发布回执仍在本地文件系统，**不在现有双 SQLite 备份范围内**；需另做文件备份/异机保护。不要清理这些目录。

### 自动解阻

- 新版第一次启动会在现有 `event_log` 中保存一个启用边界；只处理边界之后新发生的阻塞，部署时不会批量唤醒历史任务。恢复队列、原因、来源 Run 和下一次执行时间均写入 SQLite，进程重启后仍可继续。
- Run 失败、Agent 返回普通 `blocked`、或结果协议错误时，Manager 将原因作为新的系统输入交回原 Agent、原 Session 和原阶段。`IMPLEMENTING` 中的代码、构建及测试问题由原开发 Agent 在已有工作树定位和修复；Manager 不代写业务修复，也不重新设计已批准方案。
- 自动恢复没有次数额度。连续失败采用持久事件计数和指数退避，最长等待 5 分钟；结果协议错误最长等待 30 秒。它限制空转频率，不把达到某个次数当作人工门槛。同一 SHA 的测试复测也没有固定三次上限，但 Agent 必须依据失败证据显式申请 `retry_of`，外部 POST 仍受幂等和不确定状态保护。
- 用户主动暂停、待人工方案/最终验收、待产品决定、待凭据、待未预授权敏感权限、运行中的环境任务、测试流水线和状态不明的外部写入都不会被自动越过。已记忆的安全授权仍由权限策略正常注入；没有授权时继续生成结构化申请。
- 环境能力或远端验证失败时，优先复用已有环境执行闭环或拆出环境子任务；环境结果返回原开发 Session。Reviewer 的执行失败则恢复原 Reviewer Session，不改派为另一名成员。

## 执行权限和适配器

本版隔离开发支持 Codex。Cursor 仍保留只读 ask 模式；不自动换 Agent、模型或 Session，遇到不支持的 runtime 会排队并显示原因。runtime 通过 `approved_development` 能力协商，旧版本 runtime 不会收到可写任务。

Manager 签发内部 `ExecutionGrant`，包含审批单、方案 hash、GitHub 仓库及 base branch。它经已有带认证的 JSON-RPC runtime 通道传递，不能由任务源或模型直接提交。原角色的历史只读部署说明由本轮明确授权取代，角色职责和其它边界不变。

- runtime 使用主机配置的基准仓库，通过 SSH fetch 明确批准的 upstream 分支并固定 commit，为每个 Session 创建独立任务分支和 worktree。保留基准仓库的工作目录、分支和已有修改，不 checkout/reset/clean，也不改其 remote 配置。
- 主机 `work root/repositories.json` 配置仓库到 `directory`、`upstream_ssh` 的映射。路径与 SSH 入口由主机配置，不由模型指定。缺少配置或基准仓库时报错，不再为每个任务克隆整个 upstream。
- `gh api` 核实当前身份及同名 fork 的 parent 和 push 权限；基准仓库 origin 必须是该 fork 的 SSH 地址，可使用主机 SSH alias。新 worktree 只向核实后的 origin 推送任务分支，使用 `gh pr create --repo <upstream> --head <fork-owner>:<task-branch>` 发起 PR，不向 upstream 推送。
- Git 元数据仍在基准仓库的 `.git/worktrees/` 中，位于 Agent 可写 Session 之外。重试复用原 worktree，不覆盖未知残留目录；旧版已登记的独立 clone 保留兼容，不迁移历史数据。
- 开发 Codex 使用 `workspace-write`，不启用 full-access；Git 元数据和发布回执在可写 Session 目录之外。实现阶段不会自动获得网络或外部系统写权限。Manager 只为两类受管只读 Codex Run 自动联网：带 `*.review` 能力的 Reviewer，以及具有经校验并持久化的 `antmultica.issue` 来源引用、需要读取原工单的 Agent。二者都位于仅 Session 目录可写的隔离 `workspace-write` 沙箱；被评审或待开发仓库仍只读，联网不授权评论、改状态、合并或发布。任务正文中的 URL 不能触发授权。依赖/远程 Windows 验证若不可用，应报告阻塞或未验证，不能虚报。
- runtime 使用已登录 `gh` 的现有 fork，核对 fork 的 parent 后，只推送 `work-assistant/<task_id>`，不建 fork、不合并。新分支使用普通 push；已由任务 marker 绑定且仍处于 open 状态的任务 PR，在核实 fork、head ref、base ref 和 GitHub 返回的精确 head SHA 后，允许使用绑定该 SHA 的 `--force-with-lease` 更新 rebase 后的历史。无条件强推、未知分支和检查后发生变化的远端分支仍会被拒绝。
- PR POST 前 fsync 记录尝试。响应不确定时按任务 marker 与 branch 查找已有 PR；未找到也不自动重复 POST。手动编辑/关闭的 PR 不自动重建。
- 角色/外部文本不能指定任意本地目录、凭据、Git URL 或发布目标。明显的 `.env` 和私钥文件阻止自动提交；这不是完整 secret scanner，不能代替安全 review。
- 这是个人受信任机器上的工作区隔离，不宣称是不同 Unix 用户/容器级隔离。

## 扩展边界与当前限制

源仍只负责采集事件。方案选择位于 `router.PlanReviewer`，状态机在 Store/Manager，原生运行在 adapter，受控 Git/PR 操作在 runtimehost，不塞进 AntMultica/GitHub 轮询器。

本版每个方案一个 GitHub 仓库及 base branch；跨仓库应拆成独立任务。修改仓库范围后不会覆盖已有 checkout，而会阻塞要求配置新隔离空间。尚无通用仓库配置 UI、Cursor 可写执行、自动 fork、PR 不确定状态解除 UI。现有任务重走通过带版本检查的 API 操作，页面仅展示阶段与审批。

## 验证要求

启用边界之后的运行失败由自动解阻器恢复；`POST /api/v1/work/tasks/{id}/development/retry`（body 为当前 `expected_version`）仍保留为运维兜底和历史任务的显式恢复入口。两条路径都只允许审批仍有效、任务空闲的受阻开发任务，并保留原 Session、方案 hash、审批和历史失败记录。普通任务消息仍表示调整方向，不能用普通聊天绕过审批。Git 错误仅返回已知脱敏错误类别和失败步骤，不回显可能含凭据的完整 stderr。

自动测试覆盖多轮互审、双方 Session 连续性、自动解阻仍保持原 Session/审批/执行授权、历史阻塞不批量唤醒、用户暂停和待定外部写入不可越过、无固定恢复或同 SHA 测试次数上限、旧版本拒绝、普通聊天不能批准、无 reviewer 不跳过、显式人工批准后才有写授权、文档不能作为开发完成、备份恢复、PR POST 不确定结果不重复、foreign fork/符号链接拒绝。

真实 SEEK-533 测试必须在方案互审通过后停在人工确认点。未经用户查看并批准具体方案，不能为完成测试而代替用户点击批准；因此这一轮不能宣称已完成真实需求开发/PR/合并全流程。
