package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"work-assistant/internal/model"
	"work-assistant/internal/workflow"
)

// RetryWork resumes a failed, blocked, interrupted, or explicitly paused
// execution without turning recovery into new requirements.
func (s *Store) RetryWork(ctx context.Context, taskID string, expected int64) (model.TaskMessage, error) {
	var message model.TaskMessage
	if maintenance, err := s.Maintenance(ctx); err != nil {
		return message, err
	} else if maintenance != "" {
		return message, fmt.Errorf("%w: maintenance active", model.ErrConflict)
	}
	err := s.sourceWrite(ctx, func(tx *sql.Tx) error {
		task, err := getTaskTx(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if task.Version != expected || (task.State != model.TaskStateBlocked && task.State != model.TaskStatePaused) {
			return fmt.Errorf("%w: 请刷新页面，仅失败受阻或已中断的任务可以继续", model.ErrConflict)
		}
		var runID, state, output string
		if err = tx.QueryRowContext(ctx, `SELECT run_id,state,output FROM run WHERE task_id=? ORDER BY created_at_ms DESC, rowid DESC LIMIT 1`, taskID).Scan(&runID, &state, &output); err != nil && err != sql.ErrNoRows {
			return err
		}
		var paused bool
		var schedulerError string
		if err = tx.QueryRowContext(ctx, `SELECT paused,scheduler_error FROM task_workflow WHERE task_id=?`, taskID).Scan(&paused, &schedulerError); err != nil {
			return err
		}
		var rejectedOutput bool
		if task.State == model.TaskStateBlocked && state == model.RunStateCompleted && paused {
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_message WHERE task_id=? AND run_id=? AND speaker='system' AND content LIKE 'Agent 返回的业务结果格式不正确，未创建验收单。%')`, taskID, runID).Scan(&rejectedOutput); err != nil {
				return err
			}
		}
		// scheduler_error is an operational hint and may be cleared by later
		// reviewer/source events. The per-run rejection receipt is durable, so a
		// completed output remains retryable after those unrelated updates.
		invalidOutput := task.State == model.TaskStateBlocked && state == model.RunStateCompleted && paused && (schedulerError != "" || rejectedOutput)
		completedBlock := task.State == model.TaskStateBlocked && state == model.RunStateCompleted && paused
		failed := task.State == model.TaskStateBlocked && state == "FAILED"
		interrupted := task.State == model.TaskStatePaused && state == "INTERRUPTED"
		explicitPause := task.State == model.TaskStatePaused && paused
		if !failed && !interrupted && !completedBlock && !explicitPause {
			return fmt.Errorf("%w: 只能重试失败/受阻执行，或恢复已暂停的执行", model.ErrConflict)
		}
		var gated bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM run WHERE task_id=? AND state IN ('QUEUED','RUNNING')) OR EXISTS(SELECT 1 FROM review WHERE task_id=? AND state='PENDING') OR EXISTS(SELECT 1 FROM review_turn WHERE task_id=? AND state IN ('QUEUED','RUNNING'))`, taskID, taskID, taskID).Scan(&gated); err != nil {
			return err
		}
		if gated {
			return fmt.Errorf("%w: 任务仍在执行或等待评审，不能重试", model.ErrConflict)
		}
		if invalidOutput {
			var materialized int
			if err = tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM task_message WHERE task_id=? AND run_id=? AND speaker='assistant')+(SELECT COUNT(*) FROM artifact WHERE task_id=? AND run_id=?)`, taskID, runID, taskID, runID).Scan(&materialized); err != nil {
				return err
			}
			if _, parseErr := workflow.Parse(output); parseErr == nil && materialized == 0 {
				if _, err = tx.ExecContext(ctx, `UPDATE task_workflow SET paused=0,scheduler_error='',retry_at_ms=0 WHERE task_id=?`, taskID); err != nil {
					return err
				}
				replay := model.RuntimeEvent{RunID: runID, TaskID: taskID, Type: "run.completed", Output: output, OccurredAt: time.Now().UTC().UnixMilli()}
				if err = applyWorkResultTx(ctx, tx, replay, replay.OccurredAt); err != nil {
					return err
				}
				message, err = insertMessageTx(ctx, tx, taskID, "system", "系统已按当前结果协议重新处理上次完成的输出；沿用原 Agent 结论，没有重新执行评审。", runID, "RECORDED")
				if err != nil {
					return err
				}
				_, err = appendEventTx(ctx, tx, "task", taskID, "WorkResultReprocessed", runID, taskID, map[string]any{"run_id": runID, "message_id": message.ID})
				return err
			}
		}
		d, err := developmentTx(ctx, tx, taskID)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil {
			if d.Phase != "PLANNING" && d.Phase != "IMPLEMENTING" {
				return fmt.Errorf("%w: 请先完成当前方案评审", model.ErrConflict)
			}
			if d.Phase == "IMPLEMENTING" {
				r, e := readJSONRow[model.Review](tx.QueryRowContext(ctx, `SELECT data_json FROM review WHERE review_id=? AND task_id=?`, d.ApprovedReviewID, taskID))
				if e != nil {
					return e
				}
				if r.State != "PLAN_APPROVED" || r.PlanHash != d.PlanHash || r.RunID != d.PlanRunID {
					return fmt.Errorf("%w: approved plan changed", model.ErrConflict)
				}
			}
		}
		// Keep existing pending messages, approvals, permissions and session affinity.
		content := "用户请求重试上次失败的执行（例如额度已重置）。"
		kind := "retry_failed"
		if explicitPause && !interrupted {
			content = "用户恢复了此前明确暂停的任务。请先重新核对暂停前的状态及等待条件。"
			kind = "resume_manual_pause"
		} else if interrupted {
			content = "用户请求继续上次已中断的执行。"
			kind = "resume_interrupted"
		} else if invalidOutput {
			content = "系统未能接受上轮已完成的结果：" + schedulerError + "\n上轮消息和产物已保留，不会重放或删除。请沿用已有分析和结论，只纠正被拒绝的结果字段或下一步，仅重新提交符合当前协议的业务结果，不要重做已完成工作。"
			kind = "correct_invalid_output"
		} else if completedBlock {
			content = "用户请求重新评估上轮受阻结果并继续。先核对已有事实、外部操作回执和当前等待条件；能自主解决就直接处理，只有确实需要人的产品决策时才返回 needs_input。"
			kind = "resume_completed_block"
		}
		content += "沿用原 Agent、原 Session、当前阶段和已批准方案继续；这不是新需求或重新设计指令。先检查已有工作树和执行回执，不盲目重放外部操作。"
		message, err = insertMessageTx(ctx, tx, taskID, "system", content, "", "PENDING")
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE task_workflow SET paused=0,scheduler_error='',retry_at_ms=0 WHERE task_id=?`, taskID); err != nil {
			return err
		}
		if err = setWorkStateTx(ctx, tx, taskID, model.TaskStateQueued); err != nil {
			return err
		}
		_, err = appendEventTx(ctx, tx, "task", taskID, "WorkRetryRequested", "", taskID, map[string]any{"failed_run_id": runID, "run_id": runID, "message_id": message.ID, "kind": kind})
		return err
	})
	return message, err
}
