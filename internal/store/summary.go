package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"work-assistant/internal/id"
	"work-assistant/internal/model"
	"work-assistant/internal/workflow"
)

func migrateV8(db *sql.DB) error {
	var exists int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_version WHERE version=8`).Scan(&exists); err != nil || exists > 0 {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`CREATE TABLE task_summary(
			summary_id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL REFERENCES task(task_id),
			version INTEGER NOT NULL,
			source_type TEXT NOT NULL,
			source_id TEXT NOT NULL,
			completed_at_ms INTEGER NOT NULL,
			data_json TEXT NOT NULL,
			UNIQUE(task_id,version),
			UNIQUE(task_id,source_type,source_id)
		)`,
		`CREATE INDEX task_summary_completed_idx ON task_summary(completed_at_ms DESC)`,
	} {
		if _, err = tx.Exec(statement); err != nil {
			return fmt.Errorf("migrate v8: %w", err)
		}
	}
	if err = backfillTaskSummariesTx(context.Background(), tx); err != nil {
		return fmt.Errorf("migrate v8 summaries: %w", err)
	}
	if _, err = tx.Exec(`INSERT INTO schema_version VALUES(8,unixepoch('subsec')*1000)`); err != nil {
		return fmt.Errorf("migrate v8 version: %w", err)
	}
	return tx.Commit()
}

// Backfill only user-visible business tasks. Role-draft and homepage chat Tasks
// are execution details whose multiple turns are not independent work samples.
func backfillTaskSummariesTx(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT t.task_id,t.updated_at_ms
		FROM task t
		WHERE t.state='COMPLETED'
		  AND NOT EXISTS(SELECT 1 FROM role_draft d WHERE d.task_id=t.task_id)
		  AND NOT EXISTS(SELECT 1 FROM home_chat h WHERE h.task_id=t.task_id)
		ORDER BY t.created_at_ms`)
	if err != nil {
		return err
	}
	type completedTask struct {
		id string
		at int64
	}
	var tasks []completedTask
	for rows.Next() {
		var item completedTask
		if err = rows.Scan(&item.id, &item.at); err != nil {
			rows.Close()
			return err
		}
		tasks = append(tasks, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, task := range tasks {
		eligible, e := taskSummaryEligibleTx(ctx, tx, task.id)
		if e != nil {
			return e
		}
		if !eligible {
			continue
		}
		var sourceType, sourceID string
		if err = tx.QueryRowContext(ctx, `SELECT review_id FROM review WHERE task_id=? AND state='APPROVED' ORDER BY rowid DESC LIMIT 1`, task.id).Scan(&sourceID); err == nil {
			sourceType = "review"
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		} else if err = tx.QueryRowContext(ctx, `SELECT run_id FROM run WHERE task_id=? AND state='COMPLETED' ORDER BY created_at_ms DESC LIMIT 1`, task.id).Scan(&sourceID); err == nil {
			sourceType = "run"
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		} else {
			sourceType, sourceID = "task", task.id
		}
		if _, err = createTaskSummaryTx(ctx, tx, task.id, sourceType, sourceID, task.at); err != nil {
			return err
		}
	}
	return nil
}

func taskSummaryEligibleTx(ctx context.Context, tx *sql.Tx, taskID string) (bool, error) {
	var internal int
	err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM role_draft WHERE task_id=?)
		     + EXISTS(SELECT 1 FROM home_chat WHERE task_id=?)`, taskID, taskID).Scan(&internal)
	if err != nil || internal != 0 {
		return false, err
	}
	// v8 backfill also runs on databases that have not reached v9 yet.
	var routingTable int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='routing_decision'`).Scan(&routingTable); err != nil {
		return false, err
	}
	if routingTable > 0 {
		err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM routing_decision WHERE internal_task_id=?`, taskID).Scan(&internal)
	}
	if err != nil || internal != 0 {
		return false, err
	}
	var consultationTable int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='task_consultation'`).Scan(&consultationTable); err != nil {
		return false, err
	}
	if consultationTable > 0 {
		err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_consultation WHERE execution_task_id=?`, taskID).Scan(&internal)
	}
	return internal == 0, err
}

func createTaskSummaryTx(ctx context.Context, tx *sql.Tx, taskID, sourceType, sourceID string, completedAt int64) (model.TaskSummary, error) {
	var summary model.TaskSummary
	if taskID == "" || sourceID == "" || (sourceType != "review" && sourceType != "run" && sourceType != "task") {
		return summary, fmt.Errorf("%w: invalid task summary source", model.ErrValidation)
	}
	existing, err := readJSONRow[model.TaskSummary](tx.QueryRowContext(ctx, `SELECT data_json FROM task_summary WHERE task_id=? AND source_type=? AND source_id=?`, taskID, sourceType, sourceID))
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return summary, err
	}
	task, err := getTaskTx(ctx, tx, taskID)
	if err != nil {
		return summary, err
	}
	if task.State != model.TaskStateCompleted {
		return summary, fmt.Errorf("%w: task summary requires completed task", model.ErrConflict)
	}
	if completedAt <= 0 {
		completedAt = task.UpdatedAtMS
	}
	var version int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM task_summary WHERE task_id=?`, taskID).Scan(&version); err != nil {
		return summary, err
	}

	var sourceReview *model.Review
	runID := ""
	acceptanceComment := ""
	if sourceType == "review" {
		review, readErr := readJSONRow[model.Review](tx.QueryRowContext(ctx, `SELECT data_json FROM review WHERE review_id=? AND task_id=?`, sourceID, taskID))
		if readErr != nil {
			return summary, readErr
		}
		sourceReview = &review
		runID, acceptanceComment = review.RunID, review.Comment
	} else if sourceType == "run" {
		runID = sourceID
	} else {
		_ = tx.QueryRowContext(ctx, `SELECT run_id FROM run WHERE task_id=? AND state='COMPLETED' ORDER BY created_at_ms DESC LIMIT 1`, taskID).Scan(&runID)
	}

	var sourceRun *model.Run
	if runID != "" {
		run, readErr := scanRun(tx.QueryRowContext(ctx, runSelect+` WHERE run_id=? AND task_id=?`, runID, taskID))
		if readErr != nil && !errors.Is(readErr, sql.ErrNoRows) {
			return summary, readErr
		}
		if readErr == nil {
			sourceRun = &run
		}
	}

	resultText := ""
	learnings := []string{}
	improvements := []string{}
	if sourceRun != nil {
		if result, parseErr := workflow.Parse(sourceRun.Output); parseErr == nil {
			resultText = strings.TrimSpace(result.Message)
			if result.Summary != nil {
				resultText = strings.TrimSpace(result.Summary.Result)
				learnings = cleanSummaryItems(result.Summary.Learnings)
				improvements = cleanSummaryItems(result.Summary.Improvements)
			}
		} else {
			resultText = strings.TrimSpace(sourceRun.Output)
		}
	}
	if resultText == "" {
		if sourceType == "review" {
			resultText = "任务已完成并通过人工验收。"
		} else {
			resultText = "任务已完成。"
		}
	}
	resultText = truncateRunes(resultText, 4000)

	metrics, err := taskSummaryMetricsTx(ctx, tx, task, completedAt)
	if err != nil {
		return summary, err
	}
	accepted, err := acceptedSummaryArtifactsTx(ctx, tx, sourceReview)
	if err != nil {
		return summary, err
	}
	executor := model.TaskSummaryExecutor{}
	if sourceRun != nil {
		executor = model.TaskSummaryExecutor{
			AgentID: sourceRun.AgentID, RuntimeID: sourceRun.RuntimeID,
			AdapterID: sourceRun.AdapterID, ModelID: sourceRun.ModelID,
			ExecutionSettings: sourceRun.ExecutionSettings,
		}
		if sourceRun.Role != nil {
			executor.RoleID = sourceRun.Role.ID
			executor.RoleName = sourceRun.Role.Name
			executor.RoleVersion = sourceRun.Role.Version
		}
	}
	now := time.Now().UTC().UnixMilli()
	summary = model.TaskSummary{
		ID: id.New("summary"), TaskID: taskID, Version: version,
		SourceType: sourceType, SourceID: sourceID, Title: task.Title, Goal: task.Goal,
		Result: resultText, Learnings: learnings, Improvements: improvements,
		Signals: efficiencySignals(metrics), Executor: executor, AcceptedArtifacts: accepted,
		Metrics: metrics, AcceptanceComment: acceptanceComment,
		CompletedAtMS: completedAt, CreatedAtMS: now,
	}
	raw, err := json.Marshal(summary)
	if err != nil {
		return summary, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO task_summary(summary_id,task_id,version,source_type,source_id,completed_at_ms,data_json) VALUES(?,?,?,?,?,?,?)`, summary.ID, taskID, version, sourceType, sourceID, completedAt, raw); err != nil {
		return summary, err
	}
	_, err = appendEventTx(ctx, tx, "task", taskID, "TaskSummaryCreated", sourceID, taskID, map[string]any{
		"summary_id": summary.ID, "version": summary.Version, "source_type": sourceType,
		"source_id": sourceID, "completed_at_ms": completedAt, "metrics": metrics,
	})
	return summary, err
}

func taskSummaryMetricsTx(ctx context.Context, tx *sql.Tx, task model.Task, completedAt int64) (model.TaskSummaryMetrics, error) {
	metrics := model.TaskSummaryMetrics{CycleTimeMS: nonNegative(completedAt - task.CreatedAtMS)}
	rows, err := tx.QueryContext(ctx, runSelect+` WHERE task_id=? ORDER BY created_at_ms`, task.ID)
	if err != nil {
		return metrics, err
	}
	var runs []model.Run
	for rows.Next() {
		run, scanErr := scanRun(rows)
		if scanErr != nil {
			rows.Close()
			return metrics, scanErr
		}
		runs = append(runs, run)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return metrics, err
	}
	if err = rows.Close(); err != nil {
		return metrics, err
	}
	for index, run := range runs {
		metrics.RunCount++
		switch run.State {
		case model.RunStateCompleted:
			metrics.CompletedRunCount++
		case model.RunStateFailed:
			metrics.FailedRunCount++
		case model.RunStateInterrupted:
			metrics.InterruptedRunCount++
		}
		startedAt, finishedAt, timingErr := recordedRunTimingTx(ctx, tx, run)
		if timingErr != nil {
			return metrics, timingErr
		}
		if startedAt != nil && finishedAt != nil && *finishedAt >= *startedAt {
			metrics.ActiveRunTimeMS += *finishedAt - *startedAt
		} else if run.State == model.RunStateCompleted || run.State == model.RunStateFailed || run.State == model.RunStateInterrupted {
			metrics.RunWithoutTimingCount++
		}
		if startedAt != nil && *startedAt >= run.CreatedAtMS {
			metrics.RuntimeQueueWaitMS += *startedAt - run.CreatedAtMS
		}
		var inputAt sql.NullInt64
		if err = tx.QueryRowContext(ctx, `SELECT MIN(created_at_ms) FROM task_message WHERE run_id=?`, run.ID).Scan(&inputAt); err != nil {
			return metrics, err
		}
		if inputAt.Valid && run.CreatedAtMS >= inputAt.Int64 {
			metrics.QueueWaitMS += run.CreatedAtMS - inputAt.Int64
		} else if index == 0 && run.CreatedAtMS >= task.CreatedAtMS {
			metrics.QueueWaitMS += run.CreatedAtMS - task.CreatedAtMS
		}
	}

	reviewRows, err := tx.QueryContext(ctx, `SELECT data_json FROM review WHERE task_id=? ORDER BY rowid`, task.ID)
	if err != nil {
		return metrics, err
	}
	for reviewRows.Next() {
		review, readErr := readJSONRow[model.Review](reviewRows)
		if readErr != nil {
			reviewRows.Close()
			return metrics, readErr
		}
		metrics.ReviewRoundCount++
		switch review.State {
		case "CHANGES_REQUESTED":
			metrics.ChangesRequestedCount++
		case "SUPERSEDED":
			metrics.SupersededReviewCount++
		}
		requestedAt, decidedAt, timingErr := recordedReviewTimingTx(ctx, tx, review)
		if timingErr != nil {
			reviewRows.Close()
			return metrics, timingErr
		}
		if requestedAt != nil && decidedAt != nil && *decidedAt >= *requestedAt {
			metrics.HumanReviewWaitMS += *decidedAt - *requestedAt
		}
	}
	if err = reviewRows.Err(); err != nil {
		reviewRows.Close()
		return metrics, err
	}
	if err = reviewRows.Close(); err != nil {
		return metrics, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT CASE WHEN COUNT(*)>0 THEN COUNT(*)-1 ELSE 0 END FROM task_message WHERE task_id=? AND speaker='user'`, task.ID).Scan(&metrics.GuidanceMessageCount); err != nil {
		return metrics, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM directive WHERE task_id=? AND kind=?`, task.ID, model.DirectiveKindInterrupt).Scan(&metrics.InterruptCount); err != nil {
		return metrics, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT name),COUNT(*) FROM artifact WHERE task_id=?`, task.ID).Scan(&metrics.DeliverableCount, &metrics.ArtifactVersionCount); err != nil {
		return metrics, err
	}
	if err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN child.state='COMPLETED' THEN 1 ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN child.state='BLOCKED' THEN 1 ELSE 0 END),0)
		FROM task_edge edge JOIN task child ON child.task_id=edge.to_task_id
		WHERE edge.from_task_id=? AND edge.edge_type=?`, task.ID, model.TaskEdgeDecomposedInto).
		Scan(&metrics.SubtaskCount, &metrics.CompletedSubtaskCount, &metrics.BlockedSubtaskCount); err != nil {
		return metrics, err
	}
	return metrics, nil
}

// Cross-machine event timestamps may have clock skew. Efficiency durations use
// control-plane receipt times from the append-only event log and only fall back
// to Runtime timestamps for records created before those events were retained.
func recordedRunTimingTx(ctx context.Context, tx *sql.Tx, run model.Run) (*int64, *int64, error) {
	var started, finished sql.NullInt64
	err := tx.QueryRowContext(ctx, `
		SELECT MIN(CASE WHEN event_type='RunStarted' THEN recorded_at_ms END),
		       MAX(CASE WHEN event_type IN ('RunCompleted','RunFailed','RunInterrupted') THEN recorded_at_ms END)
		FROM event_log WHERE aggregate_type='run' AND aggregate_id=?`, run.ID).Scan(&started, &finished)
	if err != nil {
		return nil, nil, err
	}
	var startedAt, finishedAt *int64
	if started.Valid {
		value := started.Int64
		startedAt = &value
	} else {
		startedAt = run.StartedAtMS
	}
	if finished.Valid {
		value := finished.Int64
		finishedAt = &value
	} else {
		finishedAt = run.FinishedAtMS
	}
	return startedAt, finishedAt, nil
}

func recordedReviewTimingTx(ctx context.Context, tx *sql.Tx, review model.Review) (*int64, *int64, error) {
	var requested, decided sql.NullInt64
	err := tx.QueryRowContext(ctx, `
		SELECT MIN(CASE WHEN event_type='ReviewRequested' AND json_extract(payload_json,'$.review_id')=? THEN recorded_at_ms END),
		       MIN(CASE WHEN event_type='ReviewDecided' AND json_extract(payload_json,'$.review_id')=? THEN recorded_at_ms END)
		FROM event_log WHERE aggregate_type='task' AND aggregate_id=?`, review.ID, review.ID, review.TaskID).Scan(&requested, &decided)
	if err != nil {
		return nil, nil, err
	}
	var requestedAt, decidedAt *int64
	if requested.Valid {
		value := requested.Int64
		requestedAt = &value
	} else if review.CreatedAtMS > 0 {
		value := review.CreatedAtMS
		requestedAt = &value
	}
	if decided.Valid {
		value := decided.Int64
		decidedAt = &value
	} else if review.DecidedAtMS > 0 {
		value := review.DecidedAtMS
		decidedAt = &value
	}
	return requestedAt, decidedAt, nil
}

func acceptedSummaryArtifactsTx(ctx context.Context, tx *sql.Tx, review *model.Review) ([]model.TaskSummaryArtifact, error) {
	items := []model.TaskSummaryArtifact{}
	if review == nil {
		return items, nil
	}
	for _, artifactID := range review.ArtifactIDs {
		artifact, err := readJSONRow[model.Artifact](tx.QueryRowContext(ctx, `SELECT data_json FROM artifact WHERE artifact_id=? AND task_id=?`, artifactID, review.TaskID))
		if err != nil {
			return items, err
		}
		items = append(items, model.TaskSummaryArtifact{ArtifactID: artifact.ID, Name: artifact.Name, Version: artifact.Version, SHA256: artifact.SHA256})
	}
	return items, nil
}

func efficiencySignals(metrics model.TaskSummaryMetrics) []model.TaskEfficiencySignal {
	signals := []model.TaskEfficiencySignal{}
	if metrics.ChangesRequestedCount > 0 {
		signals = append(signals, model.TaskEfficiencySignal{Code: "review_rework", Evidence: fmt.Sprintf("人工验收打回 %d 次", metrics.ChangesRequestedCount), Suggestion: "把打回意见沉淀到该角色的提交前自检清单。"})
	}
	if metrics.FailedRunCount > 0 {
		signals = append(signals, model.TaskEfficiencySignal{Code: "run_failure", Evidence: fmt.Sprintf("失败运行 %d 次", metrics.FailedRunCount), Suggestion: "补充运行前环境与依赖检查，复用本次失败原因。"})
	}
	if metrics.InterruptCount > 0 || metrics.InterruptedRunCount > 0 {
		signals = append(signals, model.TaskEfficiencySignal{Code: "execution_interruption", Evidence: fmt.Sprintf("中断指令 %d 次，中断运行 %d 次", metrics.InterruptCount, metrics.InterruptedRunCount), Suggestion: "在开始执行前集中确认关键约束，降低中途改向成本。"})
	}
	if metrics.QueueWaitMS > 60_000 && metrics.QueueWaitMS > metrics.ActiveRunTimeMS {
		signals = append(signals, model.TaskEfficiencySignal{Code: "queue_bottleneck", Evidence: "排队等待长于 Agent 实际运行时间", Suggestion: "检查 Agent 在线率、容量和路由匹配是否成为瓶颈。"})
	}
	if metrics.HumanReviewWaitMS > 60_000 && metrics.HumanReviewWaitMS > metrics.ActiveRunTimeMS {
		signals = append(signals, model.TaskEfficiencySignal{Code: "review_bottleneck", Evidence: "人工验收等待长于 Agent 实际运行时间", Suggestion: "考虑集中审批提醒或缩小每轮验收范围。"})
	}
	if metrics.BlockedSubtaskCount > 0 {
		signals = append(signals, model.TaskEfficiencySignal{Code: "blocked_subtask", Evidence: fmt.Sprintf("有 %d 个子任务受阻", metrics.BlockedSubtaskCount), Suggestion: "复盘拆分边界、依赖和子任务输入是否完整。"})
	}
	if metrics.RunWithoutTimingCount > 0 {
		signals = append(signals, model.TaskEfficiencySignal{Code: "incomplete_timing", Evidence: fmt.Sprintf("有 %d 次运行缺少完整起止时间", metrics.RunWithoutTimingCount), Suggestion: "让 Runtime 稳定上报 run.started，以提高效率分析可信度。"})
	}
	return signals
}

func cleanSummaryItems(items []string) []string {
	clean := []string{}
	seen := map[string]bool{}
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item != "" && !seen[item] {
			seen[item] = true
			clean = append(clean, truncateRunes(item, 1000))
		}
	}
	return clean
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

func nonNegative(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func (s *Store) GetLatestTaskSummary(ctx context.Context, taskID string) (model.TaskSummary, error) {
	return readJSONRow[model.TaskSummary](s.db.QueryRowContext(ctx, `SELECT data_json FROM task_summary WHERE task_id=? ORDER BY version DESC LIMIT 1`, taskID))
}

func (s *Store) ListTaskSummaries(ctx context.Context, limit int) ([]model.TaskSummary, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT current_summary.data_json
		FROM task_summary current_summary
		WHERE current_summary.version=(SELECT MAX(history.version) FROM task_summary history WHERE history.task_id=current_summary.task_id)
		ORDER BY current_summary.completed_at_ms DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []model.TaskSummary{}
	for rows.Next() {
		item, readErr := readJSONRow[model.TaskSummary](rows)
		if readErr != nil {
			return nil, readErr
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
