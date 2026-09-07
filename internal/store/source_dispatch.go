package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
	return nil
}

// External input never unpauses a human hold, replaces Session affinity, or
// reopens a completed task. Completion still requires explicit acceptance.
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
