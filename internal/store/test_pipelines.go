package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
		attempt = previous.Attempt + 1
		if attempt > 3 {
			return fmt.Errorf("%w: 同一 SHA 已尝试三次，请人工检查原因，不再自动重试", model.ErrConflict)
		}
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
func listTestPipelineContextTx(ctx context.Context, tx *sql.Tx, taskID string) ([]byte, error) {
	rows, err := tx.QueryContext(ctx, testPipelineDetailSelect+` LIMIT 8`, taskID, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []json.RawMessage
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		items = append(items, raw)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}
	return json.Marshal(items)
}
func (s *Store) PendingTestActions(ctx context.Context) ([]model.TestPipeline, error) {
	return listJSONRows[model.TestPipeline](ctx, s.db, `SELECT p.data_json FROM test_pipeline p JOIN task_workflow w USING(task_id) JOIN task t USING(task_id) WHERE p.state IN ('QUEUED','SUBMITTING','UNCERTAIN') AND p.next_attempt_ms<=? AND ((w.paused=0 AND t.state!='COMPLETED') OR p.state!='QUEUED') ORDER BY p.rowid LIMIT 10`, time.Now().UnixMilli())
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
	_, err := taskEventMessageTx(ctx, tx, p.TaskID, fmt.Sprintf("回归测试未发起：%s\n申请：%s\n被测 SHA：%s\n%s\n请核实原因；新 SHA 重新申请，同 SHA 修正条件后用 retry_of=%s 显式重试。", state, p.ID, p.HeadSHA, p.Error, p.ID), "test-action-error:"+p.ID, "system")
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
	// A delayed running observation cannot overwrite a terminal result.
	if model.PipelineFinished(p.State) {
		return "RECORDED", nil
	}
	p.State = o.Status
	p.Jobs = o.Jobs
	p.Error = ""
	if err = saveTestPipelineTx(ctx, tx, p); err != nil {
		return "", err
	}
	if !model.PipelineFinished(p.State) && p.State != "manual" {
		return "RECORDED", nil
	}
	pr, err := sourceTargetTx(ctx, tx, p.PRTargetID)
	if err != nil {
		return "", err
	}
	message := fmt.Sprintf("GitLab 回归测试 pipeline #%d：%s\n%s\n被测 PR：%s\n被测 SHA：%s\n申请：%s（第 %d 次）\n", p.PipelineID, p.State, p.URL, p.PRURL, p.HeadSHA, p.ID, p.Attempt)
	for _, job := range p.Jobs {
		message += fmt.Sprintf("\n作业 #%d %s：%s / %s\n%s\n", job.ID, job.Name, job.Status, job.FailureReason, job.URL)
	}
	if p.State == "success" {
		message += "\n此精确版本的回归测试已通过，请结合评审结果继续交付；这不是人工批准。"
	} else {
		message += "\n请原开发 Agent 在原 Session 中检查失败作业和代码，区分代码缺陷、基础设施或偶发问题，修复后为新 SHA 申请测试，或说明理由后用 retry_of=" + p.ID + " 重试同 SHA。日志未自动采集，不能仅凭状态猜测根因；缺少证据时向人请求协助。不要直接跳过测试或无限重试。"
	}
	message = truncateRunes(message, 7500)
	if pr.HeadSHA != p.HeadSHA {
		_, err = insertMessageTx(ctx, tx, p.TaskID, "source", "历史版本结果（不驱动当前任务，也不作为当前版本验收证据）：\n"+message, "", "RECORDED")
		return "SUPERSEDED", err
	}
	if err = continuePRReviewsTx(ctx, tx, pr, "review-test:"+event.ID, message); err != nil {
		return "", err
	}
	return taskEventMessageTx(ctx, tx, p.TaskID, message, event.ID, "source")
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
