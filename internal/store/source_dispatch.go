package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"work-assistant/internal/model"
	"work-assistant/internal/router"
)

// Manager entrypoint: accepted events remain deliverable even after the source
// stops collecting. Polling configuration cannot select or pause an executor.
func (s *Store) ProcessSourceEvents(ctx context.Context, planners ...router.ReviewPlanner) error {
	var planner router.ReviewPlanner = router.CapabilityReviews{}
	if len(planners) > 0 && planners[0] != nil {
		planner = planners[0]
	}
	maintenance, err := s.Maintenance(ctx)
	if err != nil || maintenance != "" {
		return err
	}
	events, err := listJSONRows[model.SourceEvent](ctx, s.db, `SELECT data_json FROM source_event WHERE state='PENDING' AND next_attempt_ms<=? ORDER BY rowid LIMIT 100`, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	for _, e := range events {
		err = s.sourceWrite(ctx, func(tx *sql.Tx) error {
			current, err := readJSONRow[model.SourceEvent](tx.QueryRowContext(ctx, `SELECT data_json FROM source_event WHERE event_id=?`, e.ID))
			if err != nil || current.State != "PENDING" {
				return err
			}
			t, err := sourceTargetTx(ctx, tx, e.TargetID)
			if err != nil {
				return err
			}
			current.State = "APPLIED"
			switch e.Kind {
			case "gitlab.pipeline":
				current.State, err = applyTestPipelineEventTx(ctx, tx, t, e)
				if err != nil {
					return err
				}
			case "antmultica.issue":
				var taskID string
				err = tx.QueryRowContext(ctx, `SELECT task_id FROM source_entity WHERE entity=?`, e.Entity).Scan(&taskID)
				if err == sql.ErrNoRows {
					task, createErr := createWorkTx(ctx, tx, CreateWorkRequest{Title: e.Title, Goal: e.Message, Key: e.ID, Source: "antmultica"})
					if createErr != nil {
						return createErr
					}
					taskID = task.ID
					_, err = tx.ExecContext(ctx, `INSERT INTO source_entity VALUES(?,?,?)`, e.SourceID, e.Entity, taskID)
				} else if err == nil {
					current.State, err = sourceMessageTx(ctx, tx, taskID, e.Message, e.ID)
				}
				if err != nil {
					return err
				}
				current.TaskID = taskID
			case "github.head":
				if t.HeadSHA != e.HeadSHA {
					current.State = "SUPERSEDED"
					break
				}
				if err = createSourceReviewsTx(ctx, tx, planner, t, e); err != nil {
					return err
				}
			case "github.comment", "github.ci_failed", "github.review_result":
				if e.HeadSHA != "" && e.HeadSHA != t.HeadSHA {
					current.State = "SUPERSEDED"
					break
				}
				current.State, err = sourceMessageTx(ctx, tx, t.TaskID, e.Message, e.ID)
				if err != nil {
					return err
				}
				if e.Kind == "github.comment" || e.Kind == "github.review_result" {
					if err = continuePRReviewsTx(ctx, tx, t, "review-discussion:"+e.ID, e.Message); err != nil {
						return err
					}
				}
			case "github.ci_succeeded":
				if e.HeadSHA != t.HeadSHA {
					current.State = "SUPERSEDED"
					break
				}
				// CI is a delivery gate owned by the original development task. Wake
				// that task's existing Agent/Session exactly once; reviewer verdicts
				// are independent and are never reopened by CI.
				current.State, err = sourceMessageTx(ctx, tx, t.TaskID, e.Message, e.ID)
				if err != nil {
					return err
				}
			case "github.closed", "github.merged":
				_, err = insertMessageTx(ctx, tx, t.TaskID, "source", e.Message, "", "RECORDED")
				if err != nil {
					return err
				}
				current.State = "RECORDED"
			default:
				return fmt.Errorf("%w: 未知任务源事件 %s", model.ErrValidation, e.Kind)
			}
			current.Error = ""
			raw, _ := json.Marshal(current)
			if _, err = tx.ExecContext(ctx, `UPDATE source_event SET state=?,data_json=? WHERE event_id=?`, current.State, raw, e.ID); err != nil {
				return err
			}
			_, err = appendEventTx(ctx, tx, "task", current.TaskID, "SourceEventApplied", e.ID, current.TaskID, current)
			return err
		})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Preserve the event for retry, without preventing independent events.
			_ = s.sourceWrite(ctx, func(tx *sql.Tx) error {
				current, x := readJSONRow[model.SourceEvent](tx.QueryRowContext(ctx, `SELECT data_json FROM source_event WHERE event_id=?`, e.ID))
				if x != nil || current.State != "PENDING" {
					return x
				}
				current.Error = truncateRunes(err.Error(), 500)
				raw, _ := json.Marshal(current)
				_, x = tx.ExecContext(ctx, `UPDATE source_event SET data_json=?,next_attempt_ms=? WHERE event_id=?`, raw, time.Now().Add(30*time.Second).UnixMilli(), e.ID)
				return x
			})
		}
	}
	// Terminal PR events produced by older versions were intentionally recorded
	// without changing task state. Reconcile from durable source history on every
	// pass so a restart or deployment also closes work whose delivery PRs were
	// already merged.
	return s.reconcileMergedPRTasks(ctx)
}

// reconcileMergedPRTasks completes a work item only when every GitHub delivery
// target registered to that task has a matching merged event for its current
// head. A closed-but-unmerged PR remains visible for explicit disposition and
// is never counted as a successful delivery.
func (s *Store) reconcileMergedPRTasks(ctx context.Context) error {
	return s.sourceWrite(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT DISTINCT target.task_id
			FROM source_target target JOIN task_source source USING(source_id)
			JOIN task ON task.task_id=target.task_id
			WHERE target.task_id IS NOT NULL AND task.state!='COMPLETED'
			  AND json_extract(source.data_json,'$.kind')='github'
			  AND COALESCE(json_extract(target.data_json,'$.test_request_id'),'')=''`)
		if err != nil {
			return err
		}
		var taskIDs []string
		for rows.Next() {
			var taskID string
			if err = rows.Scan(&taskID); err != nil {
				rows.Close()
				return err
			}
			taskIDs = append(taskIDs, taskID)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err = rows.Close(); err != nil {
			return err
		}
		for _, taskID := range taskIDs {
			if _, err = completeAllMergedPRsTx(ctx, tx, taskID); err != nil {
				return err
			}
		}
		return nil
	})
}

func completeAllMergedPRsTx(ctx context.Context, tx *sql.Tx, taskID string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT target.target_id,target.entity,COALESCE(json_extract(target.data_json,'$.head_sha'),'')
		FROM source_target target JOIN task_source source USING(source_id)
		WHERE target.task_id=? AND json_extract(source.data_json,'$.kind')='github'
		  AND COALESCE(json_extract(target.data_json,'$.test_request_id'),'')=''
		ORDER BY target.rowid`, taskID)
	if err != nil {
		return false, err
	}
	type deliveryTarget struct{ id, entity, head string }
	var targets []deliveryTarget
	for rows.Next() {
		var target deliveryTarget
		if err = rows.Scan(&target.id, &target.entity, &target.head); err != nil {
			rows.Close()
			return false, err
		}
		targets = append(targets, target)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	if err = rows.Close(); err != nil {
		return false, err
	}
	if len(targets) == 0 {
		return false, nil
	}
	for _, target := range targets {
		if target.head == "" {
			return false, nil
		}
		var merged bool
		err = tx.QueryRowContext(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM source_event event
				WHERE event.target_id=? AND event.state IN ('PENDING','APPLIED','RECORDED')
				  AND json_extract(event.data_json,'$.kind')='github.merged'
				  AND json_extract(event.data_json,'$.head_sha')=?
			)`, target.id, target.head).Scan(&merged)
		if err != nil {
			return false, err
		}
		if !merged {
			return false, nil
		}
	}

	task, err := getTaskTx(ctx, tx, taskID)
	if err != nil {
		return false, err
	}
	if task.State == model.TaskStateCompleted {
		return false, nil
	}
	if err = supersedeSourceReviewsAfterMergeTx(ctx, tx, taskID); err != nil {
		return false, err
	}

	// If the Agent is still running, stop that obsolete turn first. The next
	// reconciliation pass completes the task after the terminal Run event, so
	// summary timing remains accurate and a late result cannot reopen the task.
	activeRows, err := tx.QueryContext(ctx, `SELECT run_id FROM run WHERE task_id=? AND state IN ('QUEUED','RUNNING')`, taskID)
	if err != nil {
		return false, err
	}
	var activeRuns []string
	for activeRows.Next() {
		var runID string
		if err = activeRows.Scan(&runID); err != nil {
			activeRows.Close()
			return false, err
		}
		activeRuns = append(activeRuns, runID)
	}
	if err = activeRows.Err(); err != nil {
		activeRows.Close()
		return false, err
	}
	if err = activeRows.Close(); err != nil {
		return false, err
	}
	if len(activeRuns) > 0 {
		for _, runID := range activeRuns {
			var pending bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM directive WHERE run_id=? AND kind=? AND state=?)`, runID, model.DirectiveKindInterrupt, model.DirectiveStateQueued).Scan(&pending); err != nil {
				return false, err
			}
			if !pending {
				if err = interruptWorkTx(ctx, tx, taskID, runID); err != nil {
					return false, err
				}
			}
		}
		if err = invalidatePermissionsTx(ctx, tx, taskID); err != nil {
			return false, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE task_message SET delivery='CANCELLED' WHERE task_id=? AND delivery='PENDING'`, taskID); err != nil {
			return false, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE task_workflow SET paused=1,scheduler_error='',retry_at_ms=0 WHERE task_id=?`, taskID); err != nil {
			return false, err
		}
		if task.State != model.TaskStatePaused {
			if _, err = insertMessageTx(ctx, tx, taskID, "system", "所有关联交付 PR 均已合并。系统正在停止已失效的执行轮次，随后将自动完成任务；无需人工回复。", "", "RECORDED"); err != nil {
				return false, err
			}
			if _, err = appendEventTx(ctx, tx, "task", taskID, "TaskCompletionPendingMergedPRs", "", taskID, map[string]any{"run_ids": activeRuns}); err != nil {
				return false, err
			}
			if err = setWorkStateTx(ctx, tx, taskID, model.TaskStatePaused); err != nil {
				return false, err
			}
		}
		return false, nil
	}

	if err = invalidatePermissionsTx(ctx, tx, taskID); err != nil {
		return false, err
	}
	if err = pauseEnvironmentChildrenTx(ctx, tx, taskID); err != nil {
		return false, err
	}
	if err = supersedeReviewsTx(ctx, tx, taskID); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE task_message SET delivery='CANCELLED' WHERE task_id=? AND delivery='PENDING'`, taskID); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE task_workflow SET paused=0,scheduler_error='',retry_at_ms=0 WHERE task_id=?`, taskID); err != nil {
		return false, err
	}

	links := make([]string, 0, len(targets))
	for _, target := range targets {
		links = append(links, target.entity)
	}
	completedAt := time.Now().UnixMilli()
	if _, err = insertMessageTx(ctx, tx, taskID, "system", "所有关联交付 PR 均已合并，过期的待回复、授权和验收请求已作废，任务已自动完成。\n"+strings.Join(links, "\n"), "", "RECORDED"); err != nil {
		return false, err
	}
	if err = setWorkStateTx(ctx, tx, taskID, model.TaskStateCompleted); err != nil {
		return false, err
	}
	if _, err = appendEventTx(ctx, tx, "task", taskID, "TaskAutoCompletedByMergedPRs", "", taskID, map[string]any{"pull_requests": links}); err != nil {
		return false, err
	}

	var summaryRunID string
	err = tx.QueryRowContext(ctx, `
		SELECT run_id FROM run
		WHERE task_id=? AND state='COMPLETED' AND json_valid(output)
		  AND json_extract(output,'$.outcome') IN ('review','complete')
		ORDER BY created_at_ms DESC LIMIT 1`, taskID).Scan(&summaryRunID)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	if summaryRunID != "" {
		if _, err = createTaskSummaryTx(ctx, tx, taskID, "run", summaryRunID, completedAt); err != nil {
			return false, err
		}
	} else if _, err = createTaskSummaryTx(ctx, tx, taskID, "task", taskID, completedAt); err != nil {
		return false, err
	}
	if err = updateParentStatesTx(ctx, tx, taskID, completedAt); err != nil {
		return false, err
	}
	return true, nil
}

// Once the code is merged, unfinished reviewer work can no longer affect that
// delivery. Stop those child Runs and retain them as superseded history instead
// of leaving no-op reviewer tasks in the queue after their parent completes.
func supersedeSourceReviewsAfterMergeTx(ctx context.Context, tx *sql.Tx, parentTaskID string) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT review.task_id
		FROM source_review review JOIN source_target target USING(target_id)
		WHERE target.task_id=? AND review.state!='SUPERSEDED'`, parentTaskID)
	if err != nil {
		return err
	}
	var taskIDs []string
	for rows.Next() {
		var taskID string
		if err = rows.Scan(&taskID); err != nil {
			rows.Close()
			return err
		}
		taskIDs = append(taskIDs, taskID)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, taskID := range taskIDs {
		if _, err = tx.ExecContext(ctx, `UPDATE source_review SET state='SUPERSEDED',feedback_sent=1 WHERE task_id=?`, taskID); err != nil {
			return err
		}
		task, taskErr := getTaskTx(ctx, tx, taskID)
		if taskErr != nil {
			return taskErr
		}
		if task.State == model.TaskStateCompleted {
			continue
		}
		if err = invalidatePermissionsTx(ctx, tx, taskID); err != nil {
			return err
		}
		activeRows, queryErr := tx.QueryContext(ctx, `SELECT run_id FROM run WHERE task_id=? AND state IN ('QUEUED','RUNNING')`, taskID)
		if queryErr != nil {
			return queryErr
		}
		var runIDs []string
		for activeRows.Next() {
			var runID string
			if err = activeRows.Scan(&runID); err != nil {
				activeRows.Close()
				return err
			}
			runIDs = append(runIDs, runID)
		}
		if err = activeRows.Err(); err != nil {
			activeRows.Close()
			return err
		}
		if err = activeRows.Close(); err != nil {
			return err
		}
		for _, runID := range runIDs {
			var pending bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM directive WHERE run_id=? AND kind=? AND state=?)`, runID, model.DirectiveKindInterrupt, model.DirectiveStateQueued).Scan(&pending); err != nil {
				return err
			}
			if !pending {
				if err = interruptWorkTx(ctx, tx, taskID, runID); err != nil {
					return err
				}
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE task_message SET delivery='CANCELLED' WHERE task_id=? AND delivery='PENDING'`, taskID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE task_workflow SET paused=1,scheduler_error='',retry_at_ms=0 WHERE task_id=?`, taskID); err != nil {
			return err
		}
		if err = setWorkStateTx(ctx, tx, taskID, model.TaskStatePaused); err != nil {
			return err
		}
		if _, err = appendEventTx(ctx, tx, "task", taskID, "SourceReviewSupersededByMergedPR", "", taskID, map[string]any{"parent_task_id": parentTaskID, "run_ids": runIDs}); err != nil {
			return err
		}
	}
	return nil
}

// External input never unpauses a human hold, replaces Session affinity, or
// reopens a completed task. The sole terminal exception is reconciled above:
// every registered delivery PR was independently observed as merged.
func sourceMessageTx(ctx context.Context, tx *sql.Tx, taskID, content, key string) (string, error) {
	return taskEventMessageTx(ctx, tx, taskID, content, key, "source")
}

func taskEventMessageTx(ctx context.Context, tx *sql.Tx, taskID, content, key, speaker string) (string, error) {
	task, err := getTaskTx(ctx, tx, taskID)
	if err != nil {
		return "", err
	}
	w, err := scanWork(tx.QueryRowContext(ctx, workSelect+` WHERE task_id=?`, taskID))
	if err != nil {
		return "", err
	}
	if task.State == model.TaskStateCompleted {
		_, err = insertMessageTx(ctx, tx, taskID, speaker, content, "", "RECORDED")
		return "RECORDED", err
	}
	_, err = messageWorkFromTx(ctx, tx, taskID, content, key, false, speaker)
	if err != nil {
		return "", err
	}
	if w.Paused {
		if _, err = tx.ExecContext(ctx, `UPDATE task_workflow SET paused=1 WHERE task_id=?`, taskID); err != nil {
			return "", err
		}
		if err = setWorkStateTx(ctx, tx, taskID, task.State); err != nil {
			return "", err
		}
	}
	return "APPLIED", nil
}
