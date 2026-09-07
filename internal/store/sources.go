package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"work-assistant/internal/id"
	"work-assistant/internal/model"
)

func migrateV13(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version >= 13 {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`CREATE TABLE task_source(source_id TEXT PRIMARY KEY, data_json TEXT NOT NULL)`,
		`CREATE TABLE source_target(target_id TEXT PRIMARY KEY, source_id TEXT NOT NULL REFERENCES task_source(source_id), entity TEXT NOT NULL, task_id TEXT REFERENCES task(task_id), enabled INTEGER NOT NULL, next_poll_ms INTEGER NOT NULL, cursor_json TEXT NOT NULL DEFAULT '{}', data_json TEXT NOT NULL, UNIQUE(source_id,entity))`,
		`CREATE INDEX source_target_due ON source_target(enabled,next_poll_ms)`,
		`CREATE UNIQUE INDEX source_target_pr_owner ON source_target(entity) WHERE task_id IS NOT NULL`,
		`CREATE TABLE source_event(event_id TEXT PRIMARY KEY, source_id TEXT NOT NULL REFERENCES task_source(source_id), target_id TEXT NOT NULL REFERENCES source_target(target_id), state TEXT NOT NULL, data_json TEXT NOT NULL, next_attempt_ms INTEGER NOT NULL DEFAULT 0)`,
		`CREATE INDEX source_event_pending ON source_event(state,next_attempt_ms)`,
		`CREATE TABLE source_entity(source_id TEXT NOT NULL REFERENCES task_source(source_id), entity TEXT NOT NULL, task_id TEXT NOT NULL REFERENCES task(task_id), PRIMARY KEY(source_id,entity))`,
		`CREATE UNIQUE INDEX source_entity_identity ON source_entity(entity)`,
		`CREATE TABLE source_review(target_id TEXT NOT NULL REFERENCES source_target(target_id), head_sha TEXT NOT NULL, role_id TEXT NOT NULL REFERENCES role(role_id), task_id TEXT NOT NULL UNIQUE REFERENCES task(task_id), state TEXT NOT NULL, feedback_sent INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(target_id,head_sha,role_id))`,
		`INSERT INTO schema_version VALUES(13,unixepoch('subsec')*1000)`,
	} {
		if _, err = tx.Exec(q); err != nil {
			return fmt.Errorf("migrate v13: %w", err)
		}
	}
	return tx.Commit()
}

func (s *Store) sourceWrite(ctx context.Context, fn func(*sql.Tx) error) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListTaskSources(ctx context.Context) ([]model.TaskSource, error) {
	return listJSONRows[model.TaskSource](ctx, s.db, `SELECT data_json FROM task_source ORDER BY source_id`)
}
func (s *Store) GetTaskSource(ctx context.Context, sourceID string) (model.TaskSource, error) {
	return readJSONRow[model.TaskSource](s.db.QueryRowContext(ctx, `SELECT data_json FROM task_source WHERE source_id=?`, sourceID))
}

func (s *Store) SaveTaskSource(ctx context.Context, source model.TaskSource, expected int64) (model.TaskSource, error) {
	if err := model.ValidateTaskSource(source); err != nil {
		return source, err
	}
	err := s.sourceWrite(ctx, func(tx *sql.Tx) error {
		old, err := readJSONRow[model.TaskSource](tx.QueryRowContext(ctx, `SELECT data_json FROM task_source WHERE source_id=?`, source.ID))
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if expected != old.Version {
			return fmt.Errorf("%w: 任务源配置已更新，请刷新", model.ErrConflict)
		}
		if old.Version > 0 && old.Kind != source.Kind {
			return fmt.Errorf("%w: 不能修改任务源类型", model.ErrValidation)
		}
		if source.Kind == "antmultica" && old.Version > 0 && (old.Config.WorkspaceID != source.Config.WorkspaceID || old.Config.AssigneeID != source.Config.AssigneeID) {
			return fmt.Errorf("%w: 工作区或指派人改变时请创建新任务源；迭代可直接修改", model.ErrValidation)
		}
		source.Version = old.Version + 1
		raw, _ := json.Marshal(source)
		if _, err = tx.ExecContext(ctx, `INSERT INTO task_source VALUES(?,?) ON CONFLICT(source_id) DO UPDATE SET data_json=excluded.data_json`, source.ID, raw); err != nil {
			return err
		}
		if old.Version == 0 && source.Kind == "antmultica" {
			target := model.SourceTarget{ID: id.New("source_target"), SourceID: source.ID, Entity: source.Config.WorkspaceID, Enabled: true, CreatedAtMS: time.Now().UnixMilli(), Cursor: json.RawMessage("{}")}
			if err = insertSourceTargetTx(ctx, tx, target); err != nil {
				return err
			}
		}
		_, err = appendEventTx(ctx, tx, "source", source.ID, "TaskSourceConfigured", "", source.ID, source)
		return err
	})
	return source, err
}

func insertSourceTargetTx(ctx context.Context, tx *sql.Tx, target model.SourceTarget) error {
	raw, _ := json.Marshal(target)
	_, err := tx.ExecContext(ctx, `INSERT INTO source_target VALUES(?,?,?,NULLIF(?,''),?,?,?,?)`, target.ID, target.SourceID, target.Entity, target.TaskID, target.Enabled, target.NextPollMS, string(target.Cursor), raw)
	return err
}
func scanSourceTarget(row rowScanner) (model.SourceTarget, error) {
	var target model.SourceTarget
	var raw, cursor []byte
	err := row.Scan(&raw, &cursor)
	if err == nil {
		err = json.Unmarshal(raw, &target)
		target.Cursor = cursor
	}
	return target, err
}
func sourceTargetTx(ctx context.Context, tx *sql.Tx, targetID string) (model.SourceTarget, error) {
	return scanSourceTarget(tx.QueryRowContext(ctx, `SELECT data_json,cursor_json FROM source_target WHERE target_id=?`, targetID))
}
func saveSourceTargetTx(ctx context.Context, tx *sql.Tx, t model.SourceTarget) error {
	raw, _ := json.Marshal(t)
	_, err := tx.ExecContext(ctx, `UPDATE source_target SET enabled=?,next_poll_ms=?,cursor_json=?,data_json=? WHERE target_id=?`, t.Enabled, t.NextPollMS, string(t.Cursor), raw, t.ID)
	return err
}
func (s *Store) ListSourceTargets(ctx context.Context) ([]model.SourceTarget, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data_json,cursor_json FROM source_target ORDER BY next_poll_ms,target_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	targets := []model.SourceTarget{}
	for rows.Next() {
		t, err := scanSourceTarget(rows)
		if err != nil {
			return nil, err
		}
		targets = append(targets, t)
	}
	return targets, rows.Err()
}

func (s *Store) RegisterPR(ctx context.Context, taskID, sourceID, url string) (model.SourceTarget, error) {
	var result model.SourceTarget
	err := s.sourceWrite(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = registerPRTx(ctx, tx, taskID, sourceID, url)
		return err
	})
	return result, err
}
func registerPRTx(ctx context.Context, tx *sql.Tx, taskID, sourceID, url string) (model.SourceTarget, error) {
	_, _, _, canonical, err := model.ParseGitHubPR(url)
	if err != nil {
		return model.SourceTarget{}, err
	}
	if sourceID == "" {
		// An agent may omit source_id only when the destination is unambiguous.
		rows, e := tx.QueryContext(ctx, `SELECT source_id FROM task_source WHERE json_extract(data_json,'$.kind')='github' AND json_extract(data_json,'$.enabled')=1`)
		if e != nil {
			return model.SourceTarget{}, e
		}
		var ids []string
		for rows.Next() {
			var v string
			if e = rows.Scan(&v); e != nil {
				rows.Close()
				return model.SourceTarget{}, e
			}
			ids = append(ids, v)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return model.SourceTarget{}, e
		}
		if len(ids) != 1 {
			return model.SourceTarget{}, fmt.Errorf("%w: 请指定已启用的 GitHub 任务源", model.ErrValidation)
		}
		sourceID = ids[0]
	}
	source, err := readJSONRow[model.TaskSource](tx.QueryRowContext(ctx, `SELECT data_json FROM task_source WHERE source_id=?`, sourceID))
	if err != nil {
		return model.SourceTarget{}, err
	}
	if source.Kind != "github" {
		return model.SourceTarget{}, fmt.Errorf("%w: PR 只能登记到 GitHub 源", model.ErrValidation)
	}
	existing, err := scanSourceTarget(tx.QueryRowContext(ctx, `SELECT data_json,cursor_json FROM source_target WHERE entity=? AND task_id IS NOT NULL`, canonical))
	if err == nil {
		if existing.TaskID != taskID {
			return existing, fmt.Errorf("%w: 此 PR 已属于另一任务，不能接管其 Session", model.ErrConflict)
		}
		if existing.SourceID != sourceID {
			return existing, fmt.Errorf("%w: 此 PR 已由另一任务源跟踪，请修改原任务源配置", model.ErrConflict)
		}
		return existing, nil
	}
	if err != sql.ErrNoRows {
		return existing, err
	}
	task, err := getTaskTx(ctx, tx, taskID)
	if err != nil {
		return existing, err
	}
	if task.State == model.TaskStateCompleted {
		return existing, fmt.Errorf("%w: 已完成任务不能新登记 PR", model.ErrConflict)
	}
	if managed, err := isManagedTx(ctx, tx, taskID); err != nil || !managed {
		return existing, fmt.Errorf("%w: PR 必须关联工作任务", model.ErrValidation)
	}
	if _, err := getActiveSessionTx(ctx, tx, taskID); err != nil {
		return existing, fmt.Errorf("%w: 请先把任务分派并执行一次，再登记 PR，确保能够回到原成员和 Session", model.ErrConflict)
	}
	var reviewTask bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM source_review WHERE task_id=?)`, taskID).Scan(&reviewTask); err != nil {
		return existing, err
	}
	if reviewTask {
		return existing, fmt.Errorf("%w: 自动评审子任务不能登记 PR，请关联原开发任务", model.ErrConflict)
	}
	// Record the owner Task, never a caller-supplied agent/session identity.
	t := model.SourceTarget{ID: id.New("source_target"), SourceID: sourceID, Entity: canonical, TaskID: taskID, Enabled: true, CreatedAtMS: time.Now().UnixMilli(), Cursor: json.RawMessage("{}")}
	if err = insertSourceTargetTx(ctx, tx, t); err != nil {
		return t, err
	}
	_, err = appendEventTx(ctx, tx, "task", taskID, "PRWatchRegistered", t.ID, taskID, t)
	return t, err
}

func (s *Store) SetSourceTargetEnabled(ctx context.Context, targetID string, enabled bool) error {
	return s.sourceWrite(ctx, func(tx *sql.Tx) error {
		t, err := sourceTargetTx(ctx, tx, targetID)
		if err != nil {
			return err
		}
		t.Enabled = enabled
		// Preserve backoff and cursor even on manual re-enable.
		if err = saveSourceTargetTx(ctx, tx, t); err != nil {
			return err
		}
		_, err = appendEventTx(ctx, tx, "source", t.SourceID, "SourceTargetEnabled", "", t.SourceID, map[string]any{"target_id": targetID, "enabled": enabled})
		return err
	})
}

func sourceEventID(sourceID, targetID, key string) string {
	return fmt.Sprintf("source_event_%x", sha256.Sum256([]byte(sourceID+"\x00"+targetID+"\x00"+key)))
}
func insertSourceEventTx(ctx context.Context, tx *sql.Tx, e model.SourceEvent) error {
	e.ID = sourceEventID(e.SourceID, e.TargetID, e.Key)
	e.State = "PENDING"
	if e.CreatedAtMS == 0 {
		e.CreatedAtMS = time.Now().UnixMilli()
	}
	raw, _ := json.Marshal(e)
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO source_event(event_id,source_id,target_id,state,data_json) VALUES(?,?,?,?,?)`, e.ID, e.SourceID, e.TargetID, e.State, raw)
	return err
}

// The complete validated provider snapshot and its inbox events commit together.
// Configuration races discard the stale poll without advancing its cursor.
func (s *Store) CommitSourcePoll(ctx context.Context, source model.TaskSource, target model.SourceTarget, cursor json.RawMessage, head string, events []model.SourceEvent, nextMS int64, closed bool) error {
	return s.sourceWrite(ctx, func(tx *sql.Tx) error {
		current, err := readJSONRow[model.TaskSource](tx.QueryRowContext(ctx, `SELECT data_json FROM task_source WHERE source_id=?`, source.ID))
		if err != nil {
			return err
		}
		t, err := sourceTargetTx(ctx, tx, target.ID)
		if err != nil {
			return err
		}
		if !current.Enabled || current.Version != source.Version || !t.Enabled || string(t.Cursor) != string(target.Cursor) {
			return fmt.Errorf("%w: 轮询期间配置或游标已改变", model.ErrConflict)
		}
		if !json.Valid(cursor) || len(cursor) > 16*1024*1024 || len(events) > 2000 {
			return fmt.Errorf("%w: 任务源快照过大或无效", model.ErrValidation)
		}
		for _, e := range events {
			if e.Key == "" || len(e.Key) > 500 || len(e.Message) > 32000 || (e.HeadSHA != "" && !model.CommitSHA.MatchString(e.HeadSHA)) {
				return fmt.Errorf("%w: 无效的任务源事件", model.ErrValidation)
			}
			e.SourceID, e.TargetID = source.ID, target.ID
			if target.TaskID != "" {
				e.TaskID = target.TaskID
			}
			if err = insertSourceEventTx(ctx, tx, e); err != nil {
				return err
			}
		}
		t.Cursor, t.HeadSHA, t.NextPollMS, t.LastSuccessMS, t.Error, t.Failures = cursor, head, nextMS, time.Now().UnixMilli(), "", 0
		if closed {
			t.Enabled = false
		}
		return saveSourceTargetTx(ctx, tx, t)
	})
}

func (s *Store) FailSourcePoll(ctx context.Context, targetID string, cause string, retryAt int64) error {
	return s.sourceWrite(ctx, func(tx *sql.Tx) error {
		t, err := sourceTargetTx(ctx, tx, targetID)
		if err != nil {
			return err
		}
		t.Error, t.Failures = truncateRunes(cause, 500), t.Failures+1
		if retryAt > t.NextPollMS {
			t.NextPollMS = retryAt
		}
		return saveSourceTargetTx(ctx, tx, t)
	})
}
func (s *Store) ListSourceEvents(ctx context.Context) ([]model.SourceEvent, error) {
	return listJSONRows[model.SourceEvent](ctx, s.db, `SELECT data_json FROM source_event ORDER BY rowid DESC LIMIT 200`)
}
func (s *Store) ListSourceReviews(ctx context.Context) ([]model.SourceReview, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT target_id,head_sha,role_id,task_id,state,feedback_sent FROM source_review ORDER BY rowid DESC LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []model.SourceReview{}
	for rows.Next() {
		var r model.SourceReview
		if err = rows.Scan(&r.TargetID, &r.HeadSHA, &r.RoleID, &r.TaskID, &r.State, &r.FeedbackSent); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// Inbox retries do not depend on the external service being online. Pausing a
// source stops polling AND delivery; the durable inbox is retained.
