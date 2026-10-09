package store

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	"work-assistant/internal/model"
)

var persistedVMOperationID = regexp.MustCompile(`^[a-f0-9]{32}$`)

// resumeLateVMOperationTx reconnects a blocked approved implementation to a
// result that arrived after its Agent turn ended. It only queues evidence for
// the original Agent/session; it neither changes the approved plan nor repeats
// the external operation.
func resumeLateVMOperationTx(ctx context.Context, tx *sql.Tx, event model.RuntimeEvent, taskID, runState string, managed bool) error {
	if !managed || (runState != model.RunStateCompleted && runState != model.RunStateFailed && runState != model.RunStateInterrupted) {
		return nil
	}
	operationID, _ := event.Attributes["operation_id"].(string)
	available, _ := event.Attributes["result_available"].(bool)
	exitCode, validExit := integerAttribute(event.Attributes["exit_code"])
	if !persistedVMOperationID.MatchString(operationID) || !available || !validExit {
		return nil
	}
	var taskState string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM task WHERE task_id=?`, taskID).Scan(&taskState); err != nil {
		return err
	}
	if taskState != model.TaskStateBlocked {
		return nil
	}
	var latestRunID, output string
	if err := tx.QueryRowContext(ctx, `SELECT run_id,output FROM run WHERE task_id=? ORDER BY created_at_ms DESC,rowid DESC LIMIT 1`, taskID).Scan(&latestRunID, &output); err != nil {
		return err
	}
	// A stale operation from an older turn must not revive a task which later
	// blocked for a different reason. Completed Agents must also have named the
	// operation they were waiting for in their structured result.
	if latestRunID != event.RunID || (runState == model.RunStateCompleted && !strings.Contains(output, operationID)) {
		return nil
	}
	d, err := developmentTx(ctx, tx, taskID)
	if err == sql.ErrNoRows || d.Phase != "IMPLEMENTING" || d.ApprovedReviewID == "" {
		return nil
	}
	if err != nil {
		return err
	}
	var runPlanVersion int64
	var runPhase string
	if err = tx.QueryRowContext(ctx, `SELECT plan_version,phase FROM development_run WHERE run_id=? AND task_id=?`, event.RunID, taskID).Scan(&runPlanVersion, &runPhase); err == sql.ErrNoRows {
		return nil
	} else if err != nil {
		return err
	}
	if runPhase != "IMPLEMENTING" || runPlanVersion != d.Version {
		return nil
	}
	review, err := readJSONRow[model.Review](tx.QueryRowContext(ctx, `SELECT data_json FROM review WHERE review_id=? AND task_id=?`, d.ApprovedReviewID, taskID))
	if err != nil {
		return err
	}
	if review.State != "PLAN_APPROVED" || review.PlanHash != d.PlanHash || review.RunID != d.PlanRunID {
		return nil
	}
	var busy bool
	if err = tx.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM run WHERE task_id=? AND state IN ('QUEUED','RUNNING')) OR
		EXISTS(SELECT 1 FROM task_message WHERE task_id=? AND delivery='PENDING') OR
		EXISTS(SELECT 1 FROM review_turn WHERE task_id=? AND state IN ('QUEUED','RUNNING')) OR
		EXISTS(SELECT 1 FROM environment_job WHERE parent_task_id=? AND state IN ('QUEUED','RUNNING')) OR
		EXISTS(SELECT 1 FROM publication WHERE task_id=? AND state IN ('QUEUED','SUBMITTING','UNCERTAIN'))`,
		taskID, taskID, taskID, taskID, taskID).Scan(&busy); err != nil {
		return err
	}
	if busy {
		return nil
	}
	content := fmt.Sprintf("系统收到上一轮延迟完成的 Windows VM 操作回执：%s，exit=%d。结果已经持久化。请先使用本轮 Windows 客户端的 `result %s` 读取原 stdout/stderr；该命令只读回执，不会重跑操作。沿用原 Agent、原 Session 和已批准方案，根据真实回执继续诊断；不要盲目重放。", operationID, exitCode, operationID)
	message, err := insertMessageTx(ctx, tx, taskID, "system", content, "", "PENDING")
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE task_workflow SET paused=0,scheduler_error='',retry_at_ms=0 WHERE task_id=?`, taskID); err != nil {
		return err
	}
	if err = setWorkStateTx(ctx, tx, taskID, model.TaskStateQueued); err != nil {
		return err
	}
	_, err = appendEventTx(ctx, tx, "task", taskID, "VMOperationContinuationQueued", event.RunID, taskID, map[string]any{
		"run_id": event.RunID, "operation_id": operationID, "exit_code": exitCode, "message_id": message.ID,
	})
	return err
}

func integerAttribute(value any) (int, bool) {
	switch n := value.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n != float64(int(n)) {
			return 0, false
		}
		return int(n), true
	default:
		return 0, false
	}
}
