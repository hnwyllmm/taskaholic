package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"work-assistant/internal/model"
)

func migrateV11(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 11 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`CREATE TABLE run_activity(run_id TEXT NOT NULL REFERENCES run(run_id),action_id TEXT NOT NULL,task_id TEXT NOT NULL REFERENCES task(task_id),first_seq INTEGER NOT NULL,last_seq INTEGER NOT NULL,data_json TEXT NOT NULL,PRIMARY KEY(run_id,action_id))`,
		`CREATE INDEX run_activity_task_idx ON run_activity(task_id,first_seq DESC)`,
		`CREATE INDEX event_log_correlation_seq_idx ON event_log(correlation_id,global_seq)`,
		`INSERT INTO schema_version VALUES(11,unixepoch('subsec')*1000)`,
	} {
		if _, err = tx.Exec(q); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Projection and immutable observation event commit together. It cannot touch
// task/review/session ownership or interpret output as a work result.
func recordActivityTx(ctx context.Context, tx *sql.Tx, event model.RuntimeEvent, now int64) error {
	a := model.CleanAction(*event.Activity)
	if a.ID == "" || len(a.ID) > 256 || len(a.Kind) > 64 {
		return fmt.Errorf("%w: invalid activity identity", model.ErrValidation)
	}
	run, err := scanRun(tx.QueryRowContext(ctx, runSelect+` WHERE run_id=?`, event.RunID))
	if err != nil {
		return err
	}
	previous, err := readJSONRow[model.Activity](tx.QueryRowContext(ctx, `SELECT data_json FROM run_activity WHERE run_id=? AND action_id=?`, run.ID, a.ID))
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	// Preserve runtime observation times on durable outbox replay. An obviously
	// invalid/future clock must not produce negative durations in the UI.
	if event.OccurredAt > 0 && event.OccurredAt <= now {
		now = event.OccurredAt
	}
	started, firstSeq := now, int64(0)
	if err == nil {
		started, firstSeq = previous.StartedAtMS, previous.FirstSeq
		// Delayed updates must not resurrect a completed action.
		if !model.ActionActive(previous.State) && model.ActionActive(a.State) {
			return nil
		}
		if now < previous.UpdatedAtMS {
			now = previous.UpdatedAtMS
		}
		if a.Command == "" {
			a.Command = previous.Command
		}
		if a.Title == "" {
			a.Title = previous.Title
		}
	}
	if run.State != model.RunStateQueued && run.State != model.RunStateRunning && model.ActionActive(a.State) {
		a.State = "UNKNOWN"
		a.Error = "运行已结束，未收到此行动的结束事件。"
	}
	observed := model.Activity{Action: a, TaskID: run.TaskID, RunID: run.ID, SessionID: run.SessionID, AgentID: run.AgentID, RuntimeID: run.RuntimeID, ModelID: run.ModelID, FirstSeq: firstSeq, StartedAtMS: started, UpdatedAtMS: now}
	if !model.ActionActive(a.State) {
		observed.FinishedAtMS = now
		if previous.FinishedAtMS > 0 {
			observed.FinishedAtMS = previous.FinishedAtMS
		}
	}
	e, err := appendEventTx(ctx, tx, "run", run.ID, "RunActivity", event.CausationID, run.TaskID, observed)
	if err != nil {
		return err
	}
	observed.LastSeq = e.GlobalSeq
	if observed.FirstSeq == 0 {
		observed.FirstSeq = e.GlobalSeq
	}
	raw, err := json.Marshal(observed)
	if err != nil {
		return err
	}
	// Fill the just-allocated sequence before committing this new event. No
	// previously committed history is changed; stream and snapshot agree exactly.
	if _, err = tx.ExecContext(ctx, `UPDATE event_log SET payload_json=? WHERE global_seq=?`, raw, e.GlobalSeq); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO run_activity VALUES(?,?,?,?,?,?) ON CONFLICT(run_id,action_id) DO UPDATE SET last_seq=excluded.last_seq,data_json=excluded.data_json`, run.ID, a.ID, run.TaskID, observed.FirstSeq, e.GlobalSeq, raw)
	return err
}

func finishActivitiesTx(ctx context.Context, tx *sql.Tx, event model.RuntimeEvent) error {
	rows, err := tx.QueryContext(ctx, `SELECT data_json FROM run_activity WHERE run_id=? AND json_extract(data_json,'$.state') IN ('RUNNING','PENDING')`, event.RunID)
	if err != nil {
		return err
	}
	var active []model.Activity
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		var a model.Activity
		if err = json.Unmarshal(raw, &a); err != nil {
			rows.Close()
			return err
		}
		active = append(active, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, a := range active {
		a.State = "UNKNOWN"
		a.Error = "运行已结束，未收到此行动的结束事件；不代表行动成功。"
		if event.Type == "run.interrupted" {
			a.State = "INTERRUPTED"
			a.Error = "所属运行已中断。"
		}
		if event.Type == "run.failed" {
			a.Error = "所属运行失败，行动最终结果未知。"
		}
		if err = recordActivityTx(ctx, tx, model.RuntimeEvent{RunID: event.RunID, Activity: &a.Action, CausationID: event.CausationID, OccurredAt: event.OccurredAt}, time.Now().UnixMilli()); err != nil {
			return err
		}
	}
	return nil
}

// The snapshot and cursor share a read transaction. A stream starting after
// Cursor cannot miss an event committed concurrently with snapshot loading.
func (s *Store) ListActivities(ctx context.Context, taskID string, before int64, limit int) (model.ActivityPage, error) {
	page := model.ActivityPage{Items: []model.Activity{}}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	if _, err = getTaskTx(ctx, tx, taskID); err != nil {
		return page, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(global_seq),0) FROM event_log`).Scan(&page.Cursor); err != nil {
		return page, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT data_json FROM run_activity WHERE task_id=? AND (?=0 OR first_seq<?) ORDER BY first_seq DESC LIMIT ?`, taskID, before, before, limit+1)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return page, err
		}
		var item model.Activity
		if err = json.Unmarshal(raw, &item); err != nil {
			rows.Close()
			return page, err
		}
		page.Items = append(page.Items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return page, err
	}
	page.HasMore = len(page.Items) > limit
	if page.HasMore {
		page.Items = page.Items[:limit]
	}
	if len(page.Items) > 0 {
		page.NextBefore = page.Items[len(page.Items)-1].FirstSeq
	}
	return page, tx.Commit()
}
