package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"work-assistant/internal/id"
	"work-assistant/internal/model"
	"work-assistant/internal/workflow"
)

func migrateV15(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version >= 15 {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`CREATE TABLE test_pipeline(request_id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES task(task_id), pr_target_id TEXT NOT NULL REFERENCES source_target(target_id), head_sha TEXT NOT NULL, attempt INTEGER NOT NULL, state TEXT NOT NULL, next_attempt_ms INTEGER NOT NULL, data_json TEXT NOT NULL, UNIQUE(pr_target_id,head_sha,attempt))`,
		`CREATE INDEX test_pipeline_due ON test_pipeline(state,next_attempt_ms)`,
		`CREATE UNIQUE INDEX test_pipeline_identity ON test_pipeline(json_extract(data_json,'$.pipeline_id')) WHERE json_extract(data_json,'$.pipeline_id')>0`,
		`INSERT INTO schema_version VALUES(15,unixepoch('subsec')*1000)`,
	} {
		if _, err = tx.Exec(q); err != nil {
			return fmt.Errorf("migrate v15: %w", err)
		}
	}
	return tx.Commit()
}

func testPipelineTx(ctx context.Context, tx *sql.Tx, requestID string) (model.TestPipeline, error) {
	return readJSONRow[model.TestPipeline](tx.QueryRowContext(ctx, `SELECT data_json FROM test_pipeline WHERE request_id=?`, requestID))
}
func saveTestPipelineTx(ctx context.Context, tx *sql.Tx, p model.TestPipeline) error {
	p.UpdatedAtMS = time.Now().UnixMilli()
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE test_pipeline SET state=?,next_attempt_ms=?,data_json=? WHERE request_id=?`, p.State, p.NextAttemptMS, raw, p.ID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE task SET version=version+1,updated_at_ms=? WHERE task_id=?`, p.UpdatedAtMS, p.TaskID)
	return err
}

func testPipelineRetryCountTx(ctx context.Context, tx *sql.Tx, targetID, headSHA string) (int, error) {
	var retries int
	err := tx.QueryRowContext(ctx, `
		SELECT MAX(COALESCE(attempt,1))-1+
		       COALESCE(SUM(COALESCE(json_extract(data_json,'$.quick_retries'),0)),0)
		FROM test_pipeline WHERE pr_target_id=? AND head_sha=?`, targetID, headSHA).Scan(&retries)
	return retries, err
}

func pipelineFailureFingerprint(jobs []model.PipelineJob) string {
	parts := make([]string, 0, len(jobs))
	for _, job := range jobs {
		parts = append(parts, strings.Join([]string{job.Name, job.Status, job.FailureReason}, "\x00"))
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return fmt.Sprintf("%x", sum[:8])
}

func pipelineLogTail(value string, limit int) string {
	runes := []rune(value)
	if len(runes) > limit {
		runes = runes[len(runes)-limit:]
	}
	return string(runes)
}

func recordPipelineFailure(p *model.TestPipeline) {
	if len(p.Jobs) == 0 {
		return
	}
	if n := len(p.FailureHistory); n > 0 {
		if p.FailureHistory[n-1].Retry == p.QuickRetries || samePipelineFailureExecution(*p, p.Jobs) {
			return
		}
	}
	names := make([]string, 0, len(p.Jobs))
	jobIDs := make([]int64, 0, len(p.Jobs))
	jobs := make([]model.PipelineFailureJob, 0, len(p.Jobs))
	for _, job := range p.Jobs {
		names = append(names, job.Name)
		jobIDs = append(jobIDs, job.ID)
		jobs = append(jobs, model.PipelineFailureJob{
			ID: job.ID, Name: job.Name, Status: job.Status,
			FailureReason: job.FailureReason, LogExcerpt: pipelineLogTail(job.LogExcerpt, 1200),
		})
	}
	sort.Strings(names)
	sort.Slice(jobIDs, func(i, j int) bool { return jobIDs[i] < jobIDs[j] })
	p.FailureHistory = append(p.FailureHistory, model.PipelineFailure{
		Retry: p.QuickRetries, Fingerprint: pipelineFailureFingerprint(p.Jobs),
		JobIDs: jobIDs, JobNames: names, Jobs: jobs,
		FailureSummary: model.EffectivePipelineFailureSummary(p.FailureSummary, p.Jobs),
		ObservedAtMS:   time.Now().UnixMilli(),
	})
}

func samePipelineFailureExecution(p model.TestPipeline, jobs []model.PipelineJob) bool {
	if len(p.FailureHistory) == 0 {
		return false
	}
	previous := p.FailureHistory[len(p.FailureHistory)-1]
	if len(previous.JobIDs) != len(jobs) {
		return false
	}
	ids := make([]int64, 0, len(jobs))
	for _, job := range jobs {
		ids = append(ids, job.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for i := range ids {
		if ids[i] != previous.JobIDs[i] {
			return false
		}
	}
	return true
}

func smallNonMySQLRetryablePipelineFailure(p model.TestPipeline) bool {
	summary := model.EffectivePipelineFailureSummary(p.FailureSummary, p.Jobs)
	// A retry request for non-mysqltest failures is constrained to an exact,
	// fully enumerated set of no more than three jobs. The Agent, not this
	// predicate, decides whether those failures are actually retryable.
	if !summary.AllowsQuickRetry() || len(p.Jobs) != summary.TotalFailures || len(p.Jobs) > model.PipelineQuickRetryMaxJobs {
		return false
	}
	for _, job := range p.Jobs {
		if job.ID <= 0 || (job.Status != "failed" && job.Status != "canceled") {
			return false
		}
	}
	return true
}

// Only an actual current QA run (or the original owner continuing an existing
// test requirement) can request this action. No agent/session IDs come from JSON.
func requestTestPipelineTx(ctx context.Context, tx *sql.Tx, taskID, runID string, r workflow.TestRequest) error {
	_, _, _, canonical, err := model.ParseGitHubPR(r.PRURL)
	if err != nil {
		return err
	}
	target, err := scanSourceTarget(tx.QueryRowContext(ctx, `SELECT data_json,cursor_json FROM source_target WHERE entity=? AND task_id IS NOT NULL`, canonical))
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: PR 尚未登记到原任务", model.ErrValidation)
	}
	if err != nil {
		return err
	}
	if target.HeadSHA != r.HeadSHA {
		return fmt.Errorf("%w: PR 最新版本已变化或尚未采集，请核对完整 SHA", model.ErrConflict)
	}
	parent, err := getTaskTx(ctx, tx, target.TaskID)
	if err != nil {
		return err
	}
	if parent.State == model.TaskStateCompleted {
		return fmt.Errorf("%w: 已完成任务不能发起新测试", model.ErrConflict)
	}
	var role model.Role
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT role_snapshot_json FROM run WHERE run_id=? AND task_id=?`, runID, taskID).Scan(&raw); err != nil {
		return err
	}
	if err = json.Unmarshal(raw, &role); err != nil {
		return err
	}
	var qa bool
	for _, c := range role.Capabilities {
		if c == "qa.review" || c == "test.review" {
			qa = true
		}
	}
	var reviewer bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM source_review WHERE task_id=? AND target_id=? AND head_sha=? AND state!='SUPERSEDED')`, taskID, target.ID, r.HeadSHA).Scan(&reviewer); err != nil {
		return err
	}
	var prior bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM test_pipeline WHERE pr_target_id=?)`, target.ID).Scan(&prior); err != nil {
		return err
	}
	if !(qa && reviewer) && !(taskID == target.TaskID && prior) {
		return fmt.Errorf("%w: 首次测试必须由此 PR 的当前 QA reviewer 申请，复测由原任务 Agent 或 QA 申请", model.ErrValidation)
	}
	previous, err := readJSONRow[model.TestPipeline](tx.QueryRowContext(ctx, `SELECT data_json FROM test_pipeline WHERE pr_target_id=? AND head_sha=? ORDER BY attempt DESC LIMIT 1`, target.ID, r.HeadSHA))
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	attempt := 1
	if previous.ID != "" {
		// Repeated reports must not turn into repeated external writes.
		if r.RetryOf == "" {
			return nil
		}
		if previous.RetryOf == r.RetryOf {
			return nil
		} // retry request already accepted
		if previous.ID != r.RetryOf || !model.PipelineRetryable(previous.State) {
			return fmt.Errorf("%w: 仅能重试此 SHA 的最近一次失败测试；提交状态不明时先等待对账", model.ErrConflict)
		}
		summary := model.EffectivePipelineFailureSummary(previous.FailureSummary, previous.Jobs)
		message := fmt.Sprintf("未接受同一 SHA 的新完整 Pipeline 申请。失败统计：mysqltest %d 个，非 mysqltest %d 个（完整采集：%t）。Manager 不会自行决定重试；原开发 Agent 必须先基于失败日志判断关联性并提交结论。非 mysqltest 失败不超过 %d 个时，Agent 可以请求在原 Pipeline 内重试失败作业；mysqltest 则必须逐作业统计失败 case 后再由 Agent 决定修复、重试或保留说明。有关时由原开发 Agent 修复并提交新 SHA 后再测；无关或无法判断时记录原因并等待人工决定。", summary.MySQLTestFailures, summary.NonMySQLTestFailures, summary.CollectionComplete, model.PipelineQuickRetryMaxJobs)
		if _, err = insertMessageTx(ctx, tx, target.TaskID, "system", message, runID, "RECORDED"); err != nil {
			return err
		}
		_, err = appendEventTx(ctx, tx, "task", target.TaskID, "TestPipelineFullRetryRejected", runID, target.TaskID, map[string]any{"request_id": previous.ID, "head_sha": r.HeadSHA, "failure_summary": summary})
		if err != nil {
			return err
		}
		// The rejection is a durable, idempotent task event rather than a
		// transaction error: callers must not lose the explanation merely
		// because they submitted an obsolete retry request.
		return nil
	} else if r.RetryOf != "" {
		return fmt.Errorf("%w: 新 SHA 应创建新测试而不是重试旧版本", model.ErrValidation)
	}
	var sourceKind string
	err = tx.QueryRowContext(ctx, `SELECT json_extract(data_json,'$.kind') FROM task_source WHERE source_id=?`, model.SeekDBTestSource).Scan(&sourceKind)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if sourceKind != "" && sourceKind != "gitlab" {
		return fmt.Errorf("%w: GitLab 测试源标识已被其它类型占用", model.ErrConflict)
	}
	p := model.TestPipeline{ID: id.New("test_request"), TaskID: target.TaskID, RequestedByTaskID: taskID, RunID: runID, PRTargetID: target.ID, PRURL: canonical, HeadSHA: r.HeadSHA, Kind: r.Kind, Reason: r.Reason, RetryOf: r.RetryOf, Attempt: attempt, State: "QUEUED", CreatedAtMS: time.Now().UnixMilli(), UpdatedAtMS: time.Now().UnixMilli()}
	encoded, _ := json.Marshal(p)
	if _, err = tx.ExecContext(ctx, `INSERT INTO test_pipeline VALUES(?,?,?,?,?,?,?,?)`, p.ID, p.TaskID, p.PRTargetID, p.HeadSHA, p.Attempt, p.State, 0, encoded); err != nil {
		return err
	}
	// Collection defaults are not execution or reviewer settings. An existing
	// source's disabled state / interval is deliberately preserved.
	source := model.TaskSource{ID: model.SeekDBTestSource, Kind: "gitlab", Name: "GitLab 回归测试", Enabled: true, Version: 1, IntervalSeconds: 15}
	encoded, _ = json.Marshal(source)
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO task_source VALUES(?,?)`, source.ID, encoded); err != nil {
		return err
	}
	if _, err = insertMessageTx(ctx, tx, p.TaskID, "system", fmt.Sprintf("已登记 QA 回归测试申请 %s（第 %d 次）。\nPR：%s\n被测 SHA：%s\n原因：%s\n等待执行器发起 GitLab obqa/seekdb_test / master-pipeline；JOBS=all，RUN_PROFILE=1。流水线编号将在发起后记录。", p.ID, p.Attempt, p.PRURL, p.HeadSHA, p.Reason), runID, "RECORDED"); err != nil {
		return err
	}
	if _, err = appendEventTx(ctx, tx, "task", p.TaskID, "TestPipelineRequested", runID, p.TaskID, p); err != nil {
		return err
	}
	return saveTestPipelineTx(ctx, tx, p)
}

func (s *Store) GetTestPipeline(ctx context.Context, requestID string) (model.TestPipeline, error) {
	return readJSONRow[model.TestPipeline](s.db.QueryRowContext(ctx, `SELECT data_json FROM test_pipeline WHERE request_id=?`, requestID))
}

const testPipelineDetailSelect = `SELECT json_set(p.data_json,'$.current',json(CASE WHEN p.head_sha=json_extract(t.data_json,'$.head_sha') THEN 'true' ELSE 'false' END),'$.poll_error',COALESCE(json_extract(w.data_json,'$.error'),''),'$.next_poll_ms',CASE WHEN w.enabled=1 THEN w.next_poll_ms ELSE 0 END,'$.last_polled_ms',COALESCE(json_extract(w.data_json,'$.last_success_ms'),0)) FROM test_pipeline p JOIN source_target t ON t.target_id=p.pr_target_id LEFT JOIN source_target w ON w.target_id=json_extract(p.data_json,'$.poll_target_id') WHERE p.task_id=? OR p.pr_target_id IN (SELECT target_id FROM source_review WHERE task_id=?) ORDER BY p.rowid DESC`

func (s *Store) ListTestPipelines(ctx context.Context, taskID string) ([]model.TestPipeline, error) {
	// A reviewer sees the same immutable test history as its owning task.
	return listJSONRows[model.TestPipeline](ctx, s.db, testPipelineDetailSelect+` LIMIT 100`, taskID, taskID)
}

// currentTestPipelinePendingTx is deliberately narrower than the whole
// delivery gate. It lets a task show "waiting for tests" while a trusted
// pipeline is still active, without masking an unrelated Agent blocker behind
// an arbitrary GitHub CI status.
func currentTestPipelinePendingTx(ctx context.Context, tx *sql.Tx, taskID string) (bool, error) {
	var pending bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM test_pipeline p
		JOIN source_target target ON target.target_id=p.pr_target_id
		WHERE p.task_id=?
		  AND p.head_sha=json_extract(target.data_json,'$.head_sha')
		  AND lower(p.state) NOT IN ('success','failed','error','canceled','cancelled','skipped','superseded')
	)`, taskID).Scan(&pending)
	return pending, err
}

func setTaskWaitingForCurrentTestPipelineTx(ctx context.Context, tx *sql.Tx, taskID string) error {
	pending, err := currentTestPipelinePendingTx(ctx, tx, taskID)
	if err != nil || !pending {
		return err
	}
	var state string
	var paused bool
	if err = tx.QueryRowContext(ctx, `SELECT t.state,w.paused FROM task t JOIN task_workflow w USING(task_id) WHERE t.task_id=?`, taskID).Scan(&state, &paused); err != nil {
		return err
	}
	if paused || state == model.TaskStateCompleted || state == model.TaskStatePaused || state == model.TaskStateInput {
		return nil
	}
	return setWorkStateTx(ctx, tx, taskID, model.TaskStateWaitingTests)
}

func pipelineFailureAnalysisRefreshRequired(p model.TestPipeline) bool {
	summary := model.EffectivePipelineFailureSummary(p.FailureSummary, p.Jobs)
	// Use the persisted summary, not the inferred fallback, for the migration
	// check. A legacy record can contain a small displayed job subset that
	// looks complete when summarized locally, while GitLab still has additional
	// failures and mysqltest traces to collect.
	return !model.PipelineFailureEvidenceReady(p.Jobs) || !model.PipelineFailureSummaryKnown(p.FailureSummary) || (summary.MySQLTestFailures > 0 && !summary.MySQLTestDetailsKnown)
}

func mysqlTestLogsAvailable(p model.TestPipeline) bool {
	summary := model.EffectivePipelineFailureSummary(p.FailureSummary, p.Jobs)
	if summary.MySQLTestFailures == 0 {
		return true
	}
	if !summary.CollectionComplete || !summary.MySQLTestDetailsKnown || !summary.MySQLTestDetailsComplete {
		return false
	}
	count := 0
	for _, job := range p.Jobs {
		if !model.IsMySQLTestPipelineJob(job.Name) {
			continue
		}
		if !job.LogCollected || job.LogCollectError != "" || strings.TrimSpace(job.LogExcerpt) == "" {
			return false
		}
		count++
	}
	return count == summary.MySQLTestFailures
}

// pipelineFailureAssessmentNeedsMySQLRefresh identifies a conclusion written
// before the Manager started supplying every mysqltest job trace. It is only
// used after the new evidence is complete, so reopening the original Session
// gives the Agent a strictly better fact set rather than a duplicate prompt.
func pipelineFailureAssessmentNeedsMySQLRefresh(p model.TestPipeline) bool {
	summary := model.EffectivePipelineFailureSummary(p.FailureSummary, p.Jobs)
	if summary.MySQLTestFailures == 0 || !mysqlTestLogsAvailable(p) {
		return false
	}
	assessment := p.FailureAssessment
	if assessment == nil || len(assessment.MySQLTestJobs) != summary.MySQLTestFailures {
		return true
	}
	expected := make(map[int64]string, summary.MySQLTestFailures)
	for _, job := range p.Jobs {
		if model.IsMySQLTestPipelineJob(job.Name) {
			expected[job.ID] = job.Name
		}
	}
	if len(expected) != summary.MySQLTestFailures {
		return true
	}
	seen := make(map[int64]bool, len(assessment.MySQLTestJobs))
	for _, job := range assessment.MySQLTestJobs {
		if seen[job.JobID] || expected[job.JobID] != job.JobName {
			return true
		}
		seen[job.JobID] = true
	}
	return len(seen) != len(expected)
}

func validateMySQLTestFailureAnalysis(p model.TestPipeline, assessment workflow.PipelineFailureAssessment) error {
	summary := model.EffectivePipelineFailureSummary(p.FailureSummary, p.Jobs)
	if assessment.MySQLTestCaseCount < 0 {
		return fmt.Errorf("%w: mysqltest 失败 case 数不能为负", model.ErrValidation)
	}
	if summary.MySQLTestFailures == 0 {
		if assessment.MySQLTestCaseCount != 0 || len(assessment.MySQLTestJobs) != 0 {
			return fmt.Errorf("%w: 当前 Pipeline 没有 mysqltest 失败，不能填写 mysqltest case 汇总", model.ErrValidation)
		}
		return nil
	}
	if !mysqlTestLogsAvailable(p) {
		return fmt.Errorf("%w: mysqltest 失败作业日志尚未完整取得，不能提交关联性或重试结论", model.ErrValidation)
	}
	expected := map[int64]model.PipelineJob{}
	for _, job := range p.Jobs {
		if !model.IsMySQLTestPipelineJob(job.Name) {
			continue
		}
		if !job.LogCollected || job.LogCollectError != "" || strings.TrimSpace(job.LogExcerpt) == "" {
			return fmt.Errorf("%w: mysqltest 作业 #%d 日志尚未可供分析", model.ErrValidation, job.ID)
		}
		expected[job.ID] = job
	}
	if len(expected) != summary.MySQLTestFailures || len(assessment.MySQLTestJobs) != len(expected) {
		return fmt.Errorf("%w: 必须逐个分析 %d 个 mysqltest 失败作业", model.ErrValidation, summary.MySQLTestFailures)
	}
	seen := map[int64]bool{}
	cases := 0
	for _, item := range assessment.MySQLTestJobs {
		job, ok := expected[item.JobID]
		if !ok || seen[item.JobID] || item.JobName != job.Name || item.FailedCaseCount < 0 || item.FailedCaseCount > 100000 {
			return fmt.Errorf("%w: mysqltest 作业分析与已采集日志不一致", model.ErrValidation)
		}
		seen[item.JobID] = true
		cases += item.FailedCaseCount
	}
	if cases != assessment.MySQLTestCaseCount {
		return fmt.Errorf("%w: mysqltest 总失败 case 数与逐作业汇总不一致", model.ErrValidation)
	}
	return nil
}

// recordPipelineFailureAssessmentTx accepts a conclusion only from the owner
// of the original task, for the current failed pipeline revision. The Agent
// decides whether a retry is justified; the Manager validates the evidence and
// executes only an in-place retry of the same trusted Pipeline.
func recordPipelineFailureAssessmentTx(ctx context.Context, tx *sql.Tx, taskID, runID string, assessment workflow.PipelineFailureAssessment) error {
	p, err := testPipelineTx(ctx, tx, assessment.RequestID)
	if err != nil {
		return err
	}
	if p.TaskID != taskID || p.State != "failed" {
		return fmt.Errorf("%w: 只能为当前原任务的已失败回归测试记录结论", model.ErrConflict)
	}
	target, err := sourceTargetTx(ctx, tx, p.PRTargetID)
	if err != nil {
		return err
	}
	if target.HeadSHA != p.HeadSHA {
		return fmt.Errorf("%w: 测试版本已不是 PR 当前版本", model.ErrConflict)
	}
	if assessment.Relation != "related" && assessment.Relation != "unrelated" && assessment.Relation != "inconclusive" || assessment.Decision != "repair" && assessment.Decision != "retry" && assessment.Decision != "hold" {
		return fmt.Errorf("%w: 无效的失败关联性结论", model.ErrValidation)
	}
	reason, evidence := strings.TrimSpace(assessment.Reason), strings.TrimSpace(assessment.Evidence)
	if reason == "" || evidence == "" || len(reason) > 4000 || len(evidence) > 6000 {
		return fmt.Errorf("%w: 失败关联性结论需要受限的原因和证据", model.ErrValidation)
	}
	if err = validateMySQLTestFailureAnalysis(p, assessment); err != nil {
		return err
	}
	if prior := p.FailureAssessment; prior != nil && prior.Relation == assessment.Relation && prior.Decision == assessment.Decision && prior.Reason == reason && prior.Evidence == evidence && prior.MySQLTestCaseCount == assessment.MySQLTestCaseCount && prior.RunID == runID {
		return nil
	}
	stored := &model.PipelineFailureAssessment{Relation: assessment.Relation, Decision: assessment.Decision, Reason: reason, Evidence: evidence, MySQLTestCaseCount: assessment.MySQLTestCaseCount, MySQLTestJobs: append([]model.MySQLTestJobAssessment{}, assessment.MySQLTestJobs...), RunID: runID, RecordedAtMS: time.Now().UnixMilli()}
	p.FailureAssessment = stored
	for i := len(p.FailureHistory) - 1; i >= 0; i-- {
		if p.FailureHistory[i].Retry == p.QuickRetries {
			copy := *stored
			copy.MySQLTestJobs = append([]model.MySQLTestJobAssessment{}, stored.MySQLTestJobs...)
			p.FailureHistory[i].Assessment = &copy
			break
		}
	}
	if assessment.Decision == "retry" {
		summary := model.EffectivePipelineFailureSummary(p.FailureSummary, p.Jobs)
		if !summary.CollectionComplete || p.QuickRetries >= model.PipelineQuickRetryLimit {
			return fmt.Errorf("%w: 当前 Pipeline 不具备可用的原地重试条件", model.ErrConflict)
		}
		if summary.MySQLTestFailures == 0 && !smallNonMySQLRetryablePipelineFailure(p) {
			return fmt.Errorf("%w: 非 mysqltest 重试只允许 1 到 %d 个完整失败作业", model.ErrValidation, model.PipelineQuickRetryMaxJobs)
		}
		p.State = "RETRY_QUEUED"
		p.AnalysisRequired = false
		p.Error = ""
		p.NextAttemptMS = 0
	}
	if err = saveTestPipelineTx(ctx, tx, p); err != nil {
		return err
	}
	if _, err = appendEventTx(ctx, tx, "task", taskID, "TestPipelineFailureAssessed", runID, taskID, map[string]any{"request_id": p.ID, "pipeline_id": p.PipelineID, "assessment": p.FailureAssessment}); err != nil {
		return err
	}
	if assessment.Decision != "retry" {
		return nil
	}
	if _, err = insertMessageTx(ctx, tx, taskID, "system", fmt.Sprintf("开发 Agent 已完成 Pipeline #%d 的失败 case 汇总，并请求在原 Pipeline 内重试失败作业（%d/%d）。Manager 不会创建新的完整 Pipeline。", p.PipelineID, p.QuickRetries+1, model.PipelineQuickRetryLimit), runID, "RECORDED"); err != nil {
		return err
	}
	_, err = appendEventTx(ctx, tx, "task", taskID, "TestPipelineAgentRetryQueued", runID, taskID, map[string]any{"request_id": p.ID, "pipeline_id": p.PipelineID, "failure_summary": p.FailureSummary, "assessment": p.FailureAssessment, "retry": p.QuickRetries + 1, "limit": model.PipelineQuickRetryLimit})
	return err
}
func listTestPipelineContextTx(ctx context.Context, tx *sql.Tx, taskID string) ([]byte, error) {
	rows, err := tx.QueryContext(ctx, testPipelineDetailSelect+` LIMIT 8`, taskID, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []model.TestPipeline
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var item model.TestPipeline
		if err = json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		// Historical revisions remain useful as status evidence, but their job
		// logs would repeatedly consume Agent context after the PR has moved on.
		if !item.Current {
			for i := range item.Jobs {
				item.Jobs[i].LogExcerpt = ""
				item.Jobs[i].LogTruncated = false
			}
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}
	return json.Marshal(items)
}

// PendingTestEvidence is independent of task state: an Agent may have returned
// needs_input after seeing a failed pipeline, while the trusted Manager can
// still obtain the missing evidence and resume the same Session automatically.
func (s *Store) PendingTestEvidence(ctx context.Context) ([]model.TestPipeline, error) {
	items, err := listJSONRows[model.TestPipeline](ctx, s.db, `
		SELECT p.data_json
		FROM test_pipeline p
		JOIN source_target pr ON pr.target_id=p.pr_target_id
		JOIN task t ON t.task_id=p.task_id
		WHERE p.state='failed' AND p.next_attempt_ms<=? AND t.state!='COMPLETED'
		  AND p.head_sha=json_extract(pr.data_json,'$.head_sha')
		ORDER BY p.rowid DESC LIMIT 20`, time.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	result := items[:0]
	for _, item := range items {
		// Old records can already contain usable, bounded excerpts while lacking
		// either the full count or all mysqltest traces. Re-read them once so a
		// decision is based on per-job mysqltest evidence rather than a display
		// cap or an aggregate job count.
		if pipelineFailureAnalysisRefreshRequired(item) {
			result = append(result, item)
		}
	}
	return result, nil
}

func (s *Store) DeferTestEvidence(ctx context.Context, requestID, message string) error {
	return s.sourceWrite(ctx, func(tx *sql.Tx) error {
		p, err := testPipelineTx(ctx, tx, requestID)
		if err != nil || p.State != "failed" || !pipelineFailureAnalysisRefreshRequired(p) {
			return err
		}
		p.EvidenceError = truncateRunes(message, 500)
		p.NextAttemptMS = time.Now().Add(time.Minute).UnixMilli()
		return saveTestPipelineTx(ctx, tx, p)
	})
}

func (s *Store) AttachTestEvidence(ctx context.Context, requestID string, observed model.PipelineObservation) error {
	if observed.ID <= 0 || observed.Status != "failed" || observed.Ref != model.SeekDBTestRef || !model.CommitSHA.MatchString(observed.SHA) || !model.PipelineFailureEvidenceReady(observed.Jobs) {
		return model.ErrValidation
	}
	return s.sourceWrite(ctx, func(tx *sql.Tx) error {
		p, err := testPipelineTx(ctx, tx, requestID)
		if err != nil {
			return err
		}
		if p.State != "failed" || p.PipelineID != observed.ID || p.ConfigSHA != observed.SHA {
			return model.ErrConflict
		}
		evidenceReady := model.PipelineFailureEvidenceReady(p.Jobs)
		if !pipelineFailureAnalysisRefreshRequired(p) {
			return nil
		}
		p.Jobs = observed.Jobs
		p.FailureSummary = model.EffectivePipelineFailureSummary(observed.FailureSummary, observed.Jobs)
		p.EvidenceError = ""
		p.NextAttemptMS = 0
		// A record created by an earlier control binary may already have
		// delivered its failure-analysis continuation.  Refresh its trusted
		// metadata without reopening the Agent, duplicating messages, or
		// consuming another retry.  New records still follow the normal
		// evidence-to-analysis path below.
		if evidenceReady {
			// Historical failures may already have been sent to the original
			// Agent before mysqltest traces were collected one-by-one. If its
			// conclusion has no per-job case analysis, give that same Session one
			// new fact-based turn instead of leaving an obsolete human-input
			// request in place. A complete assessment is never reopened.
			refreshAgent := p.AnalysisDelivered && p.AnalysisRequired && pipelineFailureAssessmentNeedsMySQLRefresh(p)
			if err = saveTestPipelineTx(ctx, tx, p); err != nil {
				return err
			}
			if !refreshAgent {
				return nil
			}
			_, err = deliverTestPipelineResultTx(ctx, tx, p, "test-evidence-refresh:"+p.ID)
			return err
		}
		_, err = handleTestPipelineFailureTx(ctx, tx, p, "test-evidence:"+p.ID)
		return err
	})
}

func handleTestPipelineFailureTx(ctx context.Context, tx *sql.Tx, p model.TestPipeline, key string) (string, error) {
	p.FailureSummary = model.EffectivePipelineFailureSummary(p.FailureSummary, p.Jobs)
	recordPipelineFailure(&p)
	p.State = "failed"
	p.AnalysisRequired = true
	p.NextAttemptMS = 0
	if err := saveTestPipelineTx(ctx, tx, p); err != nil {
		return "", err
	}
	if p.AnalysisDelivered {
		return "RECORDED", nil
	}
	state, err := deliverTestPipelineResultTx(ctx, tx, p, key)
	if err != nil {
		return "", err
	}
	p.AnalysisDelivered = true
	if err = saveTestPipelineTx(ctx, tx, p); err != nil {
		return "", err
	}
	return state, nil
}

func (s *Store) PendingTestActions(ctx context.Context) ([]model.TestPipeline, error) {
	return listJSONRows[model.TestPipeline](ctx, s.db, `SELECT p.data_json FROM test_pipeline p JOIN task_workflow w USING(task_id) JOIN task t USING(task_id) WHERE p.state IN ('QUEUED','SUBMITTING','UNCERTAIN') AND p.next_attempt_ms<=? AND ((w.paused=0 AND t.state!='COMPLETED') OR p.state!='QUEUED') ORDER BY p.rowid LIMIT 10`, time.Now().UnixMilli())
}

func (s *Store) PendingTestRetries(ctx context.Context) ([]model.TestPipeline, error) {
	return listJSONRows[model.TestPipeline](ctx, s.db, `
		SELECT p.data_json FROM test_pipeline p
		JOIN task_workflow w USING(task_id) JOIN task t USING(task_id)
		WHERE p.state IN ('RETRY_QUEUED','RETRY_SUBMITTING','RETRY_UNCERTAIN')
		  AND p.next_attempt_ms<=?
		  AND ((w.paused=0 AND t.state!='COMPLETED') OR p.state!='RETRY_QUEUED')
		ORDER BY p.rowid LIMIT 10`, time.Now().UnixMilli())
}

func (s *Store) ClaimTestRetry(ctx context.Context, requestID string) (bool, error) {
	claimed := false
	err := s.sourceWrite(ctx, func(tx *sql.Tx) error {
		p, err := testPipelineTx(ctx, tx, requestID)
		if err != nil || p.State != "RETRY_QUEUED" {
			return err
		}
		w, err := scanWork(tx.QueryRowContext(ctx, workSelect+` WHERE task_id=?`, p.TaskID))
		if err != nil || w.Paused {
			return err
		}
		target, err := sourceTargetTx(ctx, tx, p.PRTargetID)
		if err != nil {
			return err
		}
		if target.HeadSHA != p.HeadSHA {
			p.State, p.NextAttemptMS = "SUPERSEDED", 0
			return saveTestPipelineTx(ctx, tx, p)
		}
		p.State = "RETRY_SUBMITTING"
		p.QuickRetries++
		p.RetrySubmittedAtMS = time.Now().UnixMilli()
		p.NextAttemptMS = time.Now().Add(time.Minute).UnixMilli()
		if err = saveTestPipelineTx(ctx, tx, p); err != nil {
			return err
		}
		_, err = appendEventTx(ctx, tx, "task", p.TaskID, "TestPipelineQuickRetryClaimed", p.ID, p.TaskID, map[string]any{"request_id": p.ID, "pipeline_id": p.PipelineID, "retry": p.QuickRetries})
		claimed = err == nil
		return err
	})
	return claimed, err
}

func (s *Store) DeferTestRetry(ctx context.Context, requestID, message string) error {
	return s.sourceWrite(ctx, func(tx *sql.Tx) error {
		p, err := testPipelineTx(ctx, tx, requestID)
		if err != nil {
			return err
		}
		if p.State != "RETRY_SUBMITTING" && p.State != "RETRY_UNCERTAIN" {
			return nil
		}
		first := p.State == "RETRY_SUBMITTING"
		p.State = "RETRY_UNCERTAIN"
		p.Error = truncateRunes(message, 500)
		p.NextAttemptMS = time.Now().Add(time.Minute).UnixMilli()
		if err = saveTestPipelineTx(ctx, tx, p); err != nil {
			return err
		}
		if first {
			_, err = insertMessageTx(ctx, tx, p.TaskID, "system", fmt.Sprintf("Pipeline #%d 的失败作业重试结果暂未确认。系统只会读取原 Pipeline 对账，不会重复 POST 或新建 Pipeline。", p.PipelineID), p.RunID, "RECORDED")
		}
		return err
	})
}

func (s *Store) CompleteTestRetry(ctx context.Context, requestID string, observed model.PipelineObservation) error {
	if observed.ID <= 0 || observed.Ref != model.SeekDBTestRef || !model.CommitSHA.MatchString(observed.SHA) {
		return model.ErrValidation
	}
	return s.sourceWrite(ctx, func(tx *sql.Tx) error {
		p, err := testPipelineTx(ctx, tx, requestID)
		if err != nil {
			return err
		}
		if p.State != "RETRY_SUBMITTING" && p.State != "RETRY_UNCERTAIN" {
			return nil
		}
		if observed.ID != p.PipelineID || observed.SHA != p.ConfigSHA {
			return model.ErrConflict
		}
		watch, err := sourceTargetTx(ctx, tx, p.PollTargetID)
		if err != nil {
			return err
		}
		watch.Enabled = true
		watch.NextPollMS = 0
		watch.Error = ""
		watch.Failures = 0
		watch.Cursor = json.RawMessage(`{}`)
		if err = saveSourceTargetTx(ctx, tx, watch); err != nil {
			return err
		}
		p.State = "created"
		p.Jobs = nil
		p.FailureSummary = model.PipelineFailureSummary{}
		p.FailureAssessment = nil
		p.Error = ""
		p.EvidenceError = ""
		p.NextAttemptMS = 0
		if err = saveTestPipelineTx(ctx, tx, p); err != nil {
			return err
		}
		if _, err = insertMessageTx(ctx, tx, p.TaskID, "system", fmt.Sprintf("Pipeline #%d 已开始第 %d 次失败作业快重试；继续轮询同一 Pipeline，不创建新的完整测试。", p.PipelineID, p.QuickRetries), p.RunID, "RECORDED"); err != nil {
			return err
		}
		_, err = appendEventTx(ctx, tx, "task", p.TaskID, "TestPipelineQuickRetryStarted", p.ID, p.TaskID, map[string]any{"request_id": p.ID, "pipeline_id": p.PipelineID, "retry": p.QuickRetries})
		return err
	})
}

func (s *Store) FailTestRetry(ctx context.Context, requestID, message string) error {
	return s.sourceWrite(ctx, func(tx *sql.Tx) error {
		p, err := testPipelineTx(ctx, tx, requestID)
		if err != nil {
			return err
		}
		if p.State != "RETRY_SUBMITTING" && p.State != "RETRY_UNCERTAIN" && p.State != "RETRY_QUEUED" {
			return nil
		}
		p.State = "failed"
		p.Error = truncateRunes(message, 500)
		p.AnalysisRequired = true
		p.NextAttemptMS = 0
		if err = saveTestPipelineTx(ctx, tx, p); err != nil {
			return err
		}
		_, err = handleTestPipelineFailureTx(ctx, tx, p, fmt.Sprintf("test-quick-retry-error:%s:%d", p.ID, p.QuickRetries))
		return err
	})
}

// Claim persists the uncertainty boundary BEFORE the only mutating HTTP call.
// A resumed SUBMITTING action may only reconcile; it never repeats POST.
func (s *Store) ClaimTestAction(ctx context.Context, requestID string) (bool, error) {
	claimed := false
	err := s.sourceWrite(ctx, func(tx *sql.Tx) error {
		p, err := testPipelineTx(ctx, tx, requestID)
		if err != nil || p.State != "QUEUED" {
			return err
		}
		w, err := scanWork(tx.QueryRowContext(ctx, workSelect+` WHERE task_id=?`, p.TaskID))
		if err != nil || w.Paused {
			return err
		}
		target, err := sourceTargetTx(ctx, tx, p.PRTargetID)
		if err != nil {
			return err
		}
		if target.HeadSHA != p.HeadSHA {
			return finishTestActionErrorTx(ctx, tx, p, "SUPERSEDED", "PR 已更新，未发起旧版本测试；请针对最新 SHA 重新申请")
		}
		p.State = "SUBMITTING"
		p.SubmittedAtMS = time.Now().UnixMilli()
		p.NextAttemptMS = time.Now().Add(time.Minute).UnixMilli()
		if err = saveTestPipelineTx(ctx, tx, p); err != nil {
			return err
		}
		claimed = true
		return nil
	})
	return claimed, err
}
func (s *Store) DeferTestAction(ctx context.Context, requestID, message string) error {
	return s.sourceWrite(ctx, func(tx *sql.Tx) error {
		p, err := testPipelineTx(ctx, tx, requestID)
		if err != nil {
			return err
		}
		if p.State != "SUBMITTING" && p.State != "UNCERTAIN" && p.State != "QUEUED" {
			return nil
		}
		if p.State == "SUBMITTING" {
			p.State = "UNCERTAIN"
		}
		first := p.Error == ""
		p.Error = truncateRunes(message, 500)
		p.NextAttemptMS = time.Now().Add(time.Minute).UnixMilli()
		if err = saveTestPipelineTx(ctx, tx, p); err != nil {
			return err
		}
		if first {
			_, err = insertMessageTx(ctx, tx, p.TaskID, "system", "回归测试申请 "+p.ID+"："+p.Error+"。系统会继续检查；不会盲目重复发起流水线。", p.RunID, "RECORDED")
		}
		return err
	})
}
func (s *Store) FailTestAction(ctx context.Context, requestID, state, message string) error {
	if state != "ERROR" && state != "SUPERSEDED" {
		return model.ErrValidation
	}
	return s.sourceWrite(ctx, func(tx *sql.Tx) error {
		p, err := testPipelineTx(ctx, tx, requestID)
		if err != nil {
			return err
		}
		if p.PipelineID > 0 || p.State == state {
			return nil
		}
		return finishTestActionErrorTx(ctx, tx, p, state, message)
	})
}

// Explicit human confirmation is the escape hatch for a crash before POST.
// The handler must first reconcile GitLab successfully. Never reset by timer.
func (s *Store) ConfirmTestPipelineNotCreated(ctx context.Context, taskID, requestID string) error {
	return s.sourceWrite(ctx, func(tx *sql.Tx) error {
		p, err := testPipelineTx(ctx, tx, requestID)
		if err != nil {
			return err
		}
		if p.TaskID != taskID || p.PipelineID != 0 || (p.State != "SUBMITTING" && p.State != "UNCERTAIN") || time.Now().UnixMilli()-p.SubmittedAtMS < 120000 {
			return fmt.Errorf("%w: 仅可核对提交超过两分钟且未取得编号的申请，请刷新状态", model.ErrConflict)
		}
		if _, err = appendEventTx(ctx, tx, "task", taskID, "TestPipelineNotCreatedConfirmed", requestID, taskID, map[string]any{"request_id": requestID, "confirmed_by": "user"}); err != nil {
			return err
		}
		return finishTestActionErrorTx(ctx, tx, p, "ERROR", "用户已核查 GitLab 并确认没有对应 pipeline；系统对账也未找到，允许原 Agent 分析后显式重试")
	})
}

func finishTestActionErrorTx(ctx context.Context, tx *sql.Tx, p model.TestPipeline, state, message string) error {
	p.State = state
	p.Error = truncateRunes(message, 500)
	p.NextAttemptMS = 0
	if err := saveTestPipelineTx(ctx, tx, p); err != nil {
		return err
	}
	_, err := taskEventMessageTx(ctx, tx, p.TaskID, fmt.Sprintf("回归测试未发起：%s\n申请：%s\n被测 SHA：%s\n%s\n请核实原因；确认修复后用新 SHA 重新申请。系统不会为同一 SHA 自动新建完整 Pipeline。", state, p.ID, p.HeadSHA, p.Error), "test-action-error:"+p.ID, "system")
	return err
}

func (s *Store) AttachTestPipeline(ctx context.Context, requestID string, observed model.PipelineObservation) error {
	if observed.ID <= 0 || observed.Ref != model.SeekDBTestRef || !model.CommitSHA.MatchString(observed.SHA) {
		return model.ErrValidation
	}
	return s.sourceWrite(ctx, func(tx *sql.Tx) error {
		p, err := testPipelineTx(ctx, tx, requestID)
		if err != nil {
			return err
		}
		if p.PipelineID == observed.ID {
			return nil
		}
		if p.PipelineID != 0 || (p.State != "SUBMITTING" && p.State != "UNCERTAIN") {
			return model.ErrConflict
		}
		p.PipelineID = observed.ID
		p.URL = model.PipelineURL(observed.ID)
		p.ConfigSHA = observed.SHA
		p.State = "created"
		p.Error = ""
		p.NextAttemptMS = 0
		t := model.SourceTarget{ID: id.New("source_target"), SourceID: model.SeekDBTestSource, Entity: p.URL, TaskID: p.TaskID, Enabled: true, CreatedAtMS: time.Now().UnixMilli(), Cursor: json.RawMessage(`{}`), HeadSHA: p.HeadSHA, TestRequestID: p.ID}
		if err = insertSourceTargetTx(ctx, tx, t); err != nil {
			return err
		}
		p.PollTargetID = t.ID
		if err = saveTestPipelineTx(ctx, tx, p); err != nil {
			return err
		}
		if _, err = insertMessageTx(ctx, tx, p.TaskID, "system", fmt.Sprintf("回归测试 pipeline #%d 已发起并登记轮询。\n%s\nPR：%s\n被测 SHA：%s\n测试配置：%s / %s @ %s\n申请：%s · 第 %d 次\n发起不代表通过；测试结果返回原 Agent/Session，失败后由 Agent 分析修复或申请重试。", p.PipelineID, p.URL, p.PRURL, p.HeadSHA, model.SeekDBTestProject, model.SeekDBTestRef, p.ConfigSHA, p.ID, p.Attempt), p.RunID, "RECORDED"); err != nil {
			return err
		}
		_, err = appendEventTx(ctx, tx, "task", p.TaskID, "TestPipelineStarted", p.ID, p.TaskID, p)
		return err
	})
}

func applyTestPipelineEventTx(ctx context.Context, tx *sql.Tx, target model.SourceTarget, event model.SourceEvent) (string, error) {
	p, err := testPipelineTx(ctx, tx, target.TestRequestID)
	if err != nil {
		return "", err
	}
	o := event.Pipeline
	if o == nil || o.ID != p.PipelineID || o.Ref != model.SeekDBTestRef || o.SHA != p.ConfigSHA || event.HeadSHA != p.HeadSHA {
		return "", fmt.Errorf("%w: pipeline observation identity mismatch", model.ErrValidation)
	}
	if (p.State == "RETRY_QUEUED" || p.State == "RETRY_SUBMITTING" || p.State == "RETRY_UNCERTAIN") && o.Status == "failed" {
		// A delayed observation from the just-finished polling cycle must not
		// cancel or duplicate the durable quick-retry action.
		return "RECORDED", nil
	}
	if p.State == "created" && p.QuickRetries > 0 && o.Status == "failed" && samePipelineFailureExecution(p, o.Jobs) {
		// GitLab may briefly return the old terminal snapshot after accepting a
		// retry. Do not spend another retry budget on that same job execution, and
		// undo CommitSourcePoll's terminal close so the new jobs can be observed.
		target.Enabled = true
		target.NextPollMS = time.Now().Add(3 * time.Second).UnixMilli()
		target.Cursor = json.RawMessage(`{}`)
		if err = saveSourceTargetTx(ctx, tx, target); err != nil {
			return "", err
		}
		return "RECORDED", nil
	}
	if p.State == "failed" && o.Status == "failed" && model.PipelineFailureEvidenceReady(p.Jobs) {
		// The background evidence reader may have won a race with this durable
		// source event. Its continuation was already queued exactly once.
		return "RECORDED", nil
	}
	// A delayed running observation cannot overwrite a terminal result.
	if model.PipelineFinished(p.State) {
		return "RECORDED", nil
	}
	p.State = o.Status
	p.Jobs = o.Jobs
	if o.Status == "failed" || o.Status == "canceled" {
		p.FailureSummary = model.EffectivePipelineFailureSummary(o.FailureSummary, o.Jobs)
	}
	p.Error = ""
	if err = saveTestPipelineTx(ctx, tx, p); err != nil {
		return "", err
	}
	if !model.PipelineFinished(p.State) && p.State != "manual" {
		if err = setTaskWaitingForCurrentTestPipelineTx(ctx, tx, p.TaskID); err != nil {
			return "", err
		}
		return "RECORDED", nil
	}
	if p.State == "failed" && !model.PipelineFailureEvidenceReady(p.Jobs) {
		message := testPipelineResultMessage(p) + "\n\nManager 正在使用隔离的只读凭据自动采集失败作业日志；未取得日志前不会要求工作 Agent 或用户提供 Token，也不会重跑测试。"
		_, err = insertMessageTx(ctx, tx, p.TaskID, "system", truncateRunes(message, 7500), "", "RECORDED")
		return "RECORDED", err
	}
	if p.State == "failed" {
		return handleTestPipelineFailureTx(ctx, tx, p, "review-test:"+event.ID)
	}
	return deliverTestPipelineResultTx(ctx, tx, p, "review-test:"+event.ID)
}

func testPipelineResultMessage(p model.TestPipeline) string {
	message := fmt.Sprintf("GitLab 回归测试 pipeline #%d：%s\n%s\n被测 PR：%s\n被测 SHA：%s\n申请：%s（完整 Pipeline 第 %d 次，失败作业快重试 %d 次）\n", p.PipelineID, p.State, p.URL, p.PRURL, p.HeadSHA, p.ID, p.Attempt, p.QuickRetries)
	if p.Error != "" {
		message += "\n自动重试处理：" + p.Error + "\n"
	}
	for _, job := range p.Jobs {
		message += fmt.Sprintf("\n作业 #%d %s：%s / %s\n%s\n", job.ID, job.Name, job.Status, job.FailureReason, job.URL)
		if job.LogCollected && job.LogExcerpt != "" {
			message += "Manager 已采集脱敏日志摘要；请从本轮回归测试记录的 log_excerpt 分析。\n"
		} else if job.LogCollectError != "" {
			message += "日志采集状态：" + job.LogCollectError + "\n"
		}
	}
	if p.State == "success" {
		message += "\n此精确版本的回归测试已通过，请结合评审结果继续交付；这不是人工批准。"
	} else {
		summary := model.EffectivePipelineFailureSummary(p.FailureSummary, p.Jobs)
		message += fmt.Sprintf("\n失败分类：共 %d 个；mysqltest %d 个，非 mysqltest %d 个；完整采集：%t。", summary.TotalFailures, summary.MySQLTestFailures, summary.NonMySQLTestFailures, summary.CollectionComplete)
		if summary.DetailTruncated {
			message += "非 mysqltest 作业详情可能已截取；上述失败统计仍来自完整采集。"
		}
		if summary.CollectionError != "" {
			message += "采集说明：" + summary.CollectionError + "。"
		}
		if summary.MySQLTestFailures > 0 {
			if mysqlTestLogsAvailable(p) {
				message += "每个 mysqltest 失败作业的脱敏日志均已采集到本轮上下文。"
			} else {
				message += "mysqltest 作业日志尚未完整取得；不能声称已完成逐作业 case 分析或请求重试。"
			}
		}
		message += fmt.Sprintf("Manager 只采集、保存和执行受控动作，不自行判定是否重试。请原开发 Agent 在原 Session 中把失败日志与当前 diff/调用链对照并提交 pipeline_failure_assessment（request_id=%s）：mysqltest 必须逐作业统计失败 case、填写总数和每个 job 的 case 名/数量，再判断关联性。有关时 decision=repair，直接修复并返回 outcome=blocked + recovery_request；修复提交新 SHA 后才可申请新测试。确认可重试时 decision=retry，返回 outcome=blocked 且不带 recovery_request；Manager 仅对原 Pipeline 重试失败作业。非 mysqltest 失败不超过 %d 个时可申请这种重试；mysqltest 是否重试完全由逐项 case 分析决定。无关或无法判断时 decision=hold，返回 outcome=needs_input，写明原因和证据。不要用 retry_of 新建同 SHA 完整 Pipeline，不要跳过测试、读取 GitLab Token、匿名请求 GitLab 或要求用户转贴日志。", p.ID, model.PipelineQuickRetryMaxJobs)
	}
	return truncateRunes(message, 7500)
}

func deliverTestPipelineResultTx(ctx context.Context, tx *sql.Tx, p model.TestPipeline, key string) (string, error) {
	pr, err := sourceTargetTx(ctx, tx, p.PRTargetID)
	if err != nil {
		return "", err
	}
	message := testPipelineResultMessage(p)
	if pr.HeadSHA != p.HeadSHA {
		_, err = insertMessageTx(ctx, tx, p.TaskID, "source", "历史版本结果（不驱动当前任务，也不作为当前版本验收证据）：\n"+message, "", "RECORDED")
		return "SUPERSEDED", err
	}
	// Pipeline state is a root-task delivery gate. The QA reviewer may have
	// requested it, but its completed verdict and Session are not reopened by
	// pipeline progress or results.
	return taskEventMessageTx(ctx, tx, p.TaskID, message, key, "source")
}

// Once QA has required regression, every later head needs its own latest
// successful attempt. Disabling a poller is never a way to bypass acceptance.
func guardTestPipelinesTx(ctx context.Context, tx *sql.Tx, taskID string) error {
	var blocked int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM source_target t WHERE t.task_id=? AND EXISTS(SELECT 1 FROM test_pipeline p WHERE p.pr_target_id=t.target_id) AND COALESCE((SELECT state FROM test_pipeline p WHERE p.pr_target_id=t.target_id AND p.head_sha=json_extract(t.data_json,'$.head_sha') ORDER BY attempt DESC LIMIT 1),'missing')!='success'`, taskID).Scan(&blocked)
	if err != nil {
		return err
	}
	if blocked > 0 {
		return fmt.Errorf("%w: QA 要求的最新 PR 版本回归尚未通过，请检查 pipeline 记录；旧版本通过、失败或停止轮询不能用于验收", model.ErrConflict)
	}
	return nil
}
