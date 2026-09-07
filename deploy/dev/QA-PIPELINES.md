# QA 回归流水线

QA reviewer 根据 PR 的影响范围与改动规模决定是否需要回归，提交 `test_requests`。专用执行器负责发起测试，GitLab 任务源仅 GET 采集状态并产生事件，Manager 将终态反馈送回原开发任务，Router/调度沿用原 Agent / 原生 Session。

## 已批准的测试动作

- PR：当前只支持 `https://github.com/oceanbase/seekdb/pull/N`，必须已登记到原任务。首次申请只能来自该 PR 当前版本的 QA/测试评审子任务。
- 测试项目：`https://gitlab.oceanbase-dev.com/obqa/seekdb_test`，分支 `master-pipeline`。
- 变量：`SEEKDB_SOURCE=<最新 PR 完整 40 位 SHA>`、`JOBS=all`、`RUN_PROFILE=1`。使用仓库当前默认的全部稳定场景与运行档位，不接受 Agent 传入任意 ref、下载 URL、变量或项目。
- 流水线配置本身的 SHA 单独记为 `config_sha`，不能把它混淆成被测 SeekDB SHA。2026-09-07 核实的分支配置为 `4bd4ca2567a23a652ed692f8383927d1fb81e1c9`：构建脚本从 GitHub oceanbase/seekdb 按完整 SHA fetch/checkout，父流水线以 `strategy: depend` 等待子流水线。
- **不支持将 seekdb-bindings SHA 传入这条 engine 流水线。** bindings 独立改动仍需对应构建/安装/兼容性验证，不能冒用 engine 回归结果。

```json
{
  "kind": "seekdb_regression",
  "pr_url": "https://github.com/oceanbase/seekdb/pull/123",
  "head_sha": "完整40位commitSHA",
  "reason": "核心持久化路径和恢复行为变化，需要全量稳定场景回归",
  "retry_of": ""
}
```

这是外层交付 JSON 的 `test_requests` 数组元素，每轮最多一项；无申请时 `[]`。旧版本交付 JSON 仍可解析。普通工作仍是只读沙箱：此专用动作只授权启动批准的测试，不扩大代码推送、生产访问或其它外部写权限。

## 记录、重试和验收

SQLite schema 15 新增 `test_pipeline`，保存原任务/申请任务/Run、PR、被测 SHA、触发原因、尝试次数、pipeline 编号/链接、配置 SHA、执行状态和重试前项；旧表业务记录不改写。申请在 QA 子任务自动完成之前落库。编号取得后，在同一事务中保存记录、原任务消息与 `source_target`，自动登记轮询。任务页显示紧凑测试表，评审子任务可查看同一原任务的测试历史。

状态：`QUEUED → SUBMITTING → created/running/... → success/failed/canceled/skipped`。提交响应不明进入 `UNCERTAIN`，重启后仅通过 `WORK_ASSISTANT_REQUEST_ID` pipeline 变量对账，不重发 POST。未找到时保留申请并定期检查，不假定“超时=未创建”。HTTP 明确拒绝为 `ERROR`。这种策略优先避免重复测试，不声称跨 SQLite/HTTP 已实现分布式 exactly-once。长期无法对账需要人工核实 GitLab 和记录，不能盲目重新提交。

轮询默认 15 秒，任务源页可改；实际周期受网络耗时和目标数量影响，失败退避。采集失败不当作测试失败或通过。状态变化产生去重的 `gitlab.pipeline` 事件，终态停止该目标轮询。`manual` 请求人工关注但继续观察；不能算通过。失败会附父/子流水线的作业 ID、名称、原因和链接；最多 30 个失败作业、10 个同项目流水线、两层子流水线，不取任意跨项目结果。明细不可用时报告缺口，不延迟失败状态通知；不自动持久化原始 CI trace，避免复制日志中的敏感信息。

失败反馈回到原任务 PENDING 消息，原 Agent 下一轮分析代码缺陷、环境或偶发问题；当前 Run 不被轮询器中断，人工暂停不被解除。旧 SHA 的迟到结果只记录为历史，不驱动当前任务。Agent 分析后显式申请重试：同 SHA 的 `retry_of` 填最近失败记录的 request_id；重试创建新的 pipeline，完整保留之前编号，最多三次尝试。修改为新 SHA 时重新申请，`retry_of` 留空；旧测试不会作为新版本通过的证据。不自动取消已经运行的旧版本测试。

一旦 QA 要求回归，人工批准前检查当前 PR head 最新一次尝试为 success，仍需通过既有评审/子任务/消息门槛。停用任务源不能绕过验收。已完成任务不因迟到事件重新打开。

提交超过两分钟仍不明确时，任务页展开详情可“已核实未创建，允许重试”。必须先人工核查并确认；服务端再次对账，若找到旧 pipeline 就只恢复登记，不重新发起；对账失败不允许重置；未找到且人工确认才记录审计并交回原 Agent。错误确认仍可能造成重复测试，此操作不能替代核查。对应接口为 `POST /api/v1/work/tasks/{task_id}/test-pipelines/{request_id}/resolve`，请求 `{"confirm_not_created":true}`。

## 部署与验证

`enable-qa-pipelines.mjs` 默认仅预览；`--apply` 给当前 QA 角色追加专项规则（CAS，保留其它用户修改），创建缺失的只读 GitLab 源，保留已有配置。复用 dev 的 GitLab 登录凭据，保存到 `~/.config/work-assistant/gitlab.token`（0600），不写进仓库、任务、网页或 SQLite 备份。可通过 `WORK_ASSISTANT_GITLAB_TOKEN_FILE` 指定控制端文件。普通 Agent 子进程不继承 GitLab Token 环境变量；同用户本地文件访问不是强 OS 安全隔离。

```sh
node deploy/dev/enable-qa-pipelines.mjs
node deploy/dev/enable-qa-pipelines.mjs --apply
go run ./deploy/dev/pipeline-probe
```

probe 只验证认证、项目与分支，不发起流水线，不修改生产任务。新增表随既有在线 SQLite 校验备份保存；凭据和原生 CLI Session 仍需主机级保护。上线前应备份并在备份副本演练迁移，不能用旧库覆盖新记录。

插件边界：`gitlabci.Executor` / `Reader` 分别注入 `server.Config.TestPipelineExecutor` / `TestPipelineReader`；`taskaction.PipelineActions.Executors` 注册不同受控测试动作；`tasksource.Provider` 只负责事件采集。新增测试套件需同步增加严格参数验证、授权、UI 和测试，不能仅靠网页放开任意带凭据 HTTP 请求。

接口依据：[GitLab Pipelines API](https://docs.gitlab.com/api/pipelines/)、[Jobs / 子流水线 API](https://docs.gitlab.com/api/jobs/)。本部署使用经现有实例支持的 `bridges` 路由。
