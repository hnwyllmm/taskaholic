package runtimehost

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"work-assistant/internal/backup"
	"work-assistant/internal/model"
)

type spoolMessage struct {
	RuntimeSeq int64
	MessageID  string
	Method     string
	Params     json.RawMessage
}

type recoveredRun struct {
	RunID  string
	TaskID string
}

type Spool struct {
	db      *sql.DB
	writeMu sync.Mutex
}

func OpenSpoolProtected(path string) (*Spool, error) {
	directory, err := backup.Directory(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := backup.CheckBeforeOpen(ctx, path, directory, 0, "local_run", "inbound_message", "outbound_message"); err != nil {
		return nil, err
	}
	s, err := OpenSpool(path)
	if err != nil {
		return nil, err
	}
	if err := backup.Register(path, directory); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Spool) Backup(ctx context.Context, destination string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return backup.SQLiteSnapshot(ctx, s.db, destination)
}

func OpenSpool(path string) (*Spool, error) {
	if path == "" {
		return nil, errors.New("spool path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create spool directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open runtime spool: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			return nil, fmt.Errorf("configure runtime spool: %w", err)
		}
	}
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS inbound_message(
			message_id TEXT PRIMARY KEY,
			method TEXT NOT NULL,
			params_json TEXT NOT NULL,
			status TEXT NOT NULL,
			result_json TEXT,
			received_at_ms INTEGER NOT NULL,
			updated_at_ms INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS outbound_message(
			runtime_seq INTEGER PRIMARY KEY AUTOINCREMENT,
			message_id TEXT UNIQUE,
			method TEXT NOT NULL,
			params_json TEXT,
			status TEXT NOT NULL,
			created_at_ms INTEGER NOT NULL,
			delivered_at_ms INTEGER,
			last_error TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS outbound_pending_idx ON outbound_message(status, runtime_seq)`,
		`CREATE TABLE IF NOT EXISTS local_run(
			run_id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL,
			spec_json TEXT NOT NULL,
			working_dir TEXT NOT NULL,
			state TEXT NOT NULL,
			updated_at_ms INTEGER NOT NULL
		)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrate runtime spool: %w", err)
		}
	}
	if err := backup.SecureSQLiteFiles(path); err != nil {
		db.Close()
		return nil, err
	}
	return &Spool{db: db}, nil
}

func (s *Spool) Close() error { return s.db.Close() }

// AcceptInbound durably records a control-plane command before the runtime
// acknowledges it. A repeated JSON-RPC id is accepted but never re-executed.
func (s *Spool) AcceptInbound(ctx context.Context, messageID, method string, params json.RawMessage) (bool, error) {
	if messageID == "" {
		return false, errors.New("durable command requires a message id")
	}
	now := time.Now().UTC().UnixMilli()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	result, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO inbound_message(message_id, method, params_json, status, received_at_ms, updated_at_ms)
		VALUES(?, ?, ?, 'ACCEPTED', ?, ?)`, messageID, method, params, now, now)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 0, err
}

func (s *Spool) AcceptRunStart(ctx context.Context, messageID string, params json.RawMessage, spec model.RunSpec, workingDir string) (bool, error) {
	if messageID == "" {
		return false, errors.New("durable command requires a message id")
	}
	encodedSpec, err := json.Marshal(spec)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC().UnixMilli()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO inbound_message(message_id, method, params_json, status, received_at_ms, updated_at_ms)
		VALUES(?, 'run.start', ?, 'ACCEPTED', ?, ?)`, messageID, params, now, now)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 0 {
		return true, nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO local_run(run_id, task_id, spec_json, working_dir, state, updated_at_ms)
		VALUES(?, ?, ?, ?, 'ACCEPTED', ?)`, spec.RunID, spec.TaskID, encodedSpec, workingDir, now); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return false, nil
}

func (s *Spool) SetInboundStatus(ctx context.Context, messageID, status string, result any) error {
	var encoded []byte
	var err error
	if result != nil {
		encoded, err = json.Marshal(result)
		if err != nil {
			return err
		}
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = s.db.ExecContext(ctx, `
		UPDATE inbound_message SET status = ?, result_json = ?, updated_at_ms = ? WHERE message_id = ?`,
		status, encoded, time.Now().UTC().UnixMilli(), messageID)
	return err
}

func (s *Spool) SetRunState(ctx context.Context, runID, state string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE local_run SET state = ?, updated_at_ms = ? WHERE run_id = ?`,
		state, time.Now().UTC().UnixMilli(), runID)
	return err
}

// LoadRunSpec returns the immutable accepted spec for a local run. VM result
// retrieval uses it to ensure one task/session cannot read another one's
// persisted operation output.
func (s *Spool) LoadRunSpec(ctx context.Context, runID string) (model.RunSpec, bool, error) {
	var spec model.RunSpec
	var encoded []byte
	err := s.db.QueryRowContext(ctx, `SELECT spec_json FROM local_run WHERE run_id = ?`, runID).Scan(&encoded)
	if err == sql.ErrNoRows {
		return spec, false, nil
	}
	if err != nil {
		return spec, false, err
	}
	if err = json.Unmarshal(encoded, &spec); err != nil {
		return spec, false, fmt.Errorf("decode local run %s: %w", runID, err)
	}
	return spec, true, nil
}

// EnqueueEvent allocates the runtime-local sequence and writes the complete
// event in one transaction, so reconnects can safely resume transmission.
func (s *Spool) EnqueueEvent(ctx context.Context, base model.RuntimeEvent) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	sequence, err := enqueueEventTx(ctx, tx, base)
	if err != nil {
		return 0, err
	}
	return sequence, tx.Commit()
}

func enqueueEventTx(ctx context.Context, tx *sql.Tx, base model.RuntimeEvent) (int64, error) {
	if base.Type == "run.progress" {
		base.Message, _, _ = model.ObservationText(base.Message, 16000)
		base.Error, _, _ = model.ObservationText(base.Error, 2000)
		if base.Activity != nil {
			clean := model.CleanAction(*base.Activity)
			base.Activity = &clean
		}
	}
	now := time.Now().UTC().UnixMilli()
	result, err := tx.ExecContext(ctx, `
		INSERT INTO outbound_message(method, status, created_at_ms) VALUES('run.event', 'PENDING', ?)`, now)
	if err != nil {
		return 0, err
	}
	sequence, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	base.RuntimeSeq = sequence
	if base.OccurredAt == 0 {
		base.OccurredAt = now
	}
	encoded, err := json.Marshal(base)
	if err != nil {
		return 0, err
	}
	messageID := fmt.Sprintf("%s:%s:%d", base.RuntimeID, base.Epoch, sequence)
	if _, err := tx.ExecContext(ctx, `
		UPDATE outbound_message SET message_id = ?, params_json = ? WHERE runtime_seq = ?`,
		messageID, encoded, sequence); err != nil {
		return 0, err
	}
	return sequence, nil
}

// CompleteRun makes the result/outbox and local completion one durable unit.
// A disk-full error cannot leave a completed run with its output discarded.
func (s *Spool) CompleteRun(ctx context.Context, messageID, state string, terminal model.RuntimeEvent) error {
	if (state != "COMPLETED" || terminal.Type != "run.completed") && (state != "FAILED" || terminal.Type != "run.failed") && (state != "INTERRUPTED" || terminal.Type != "run.interrupted") {
		return errors.New("invalid terminal state/event")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current, taskID string
	if err := tx.QueryRowContext(ctx, "SELECT state, task_id FROM local_run WHERE run_id = ?", terminal.RunID).Scan(&current, &taskID); err != nil {
		return err
	}
	if taskID != terminal.TaskID {
		return errors.New("terminal task does not match run")
	}
	if current == state {
		return nil
	}
	if current != "ACCEPTED" && current != "RUNNING" {
		return fmt.Errorf("run is already terminal: %s", current)
	}
	if _, err := enqueueEventTx(ctx, tx, terminal); err != nil {
		return err
	}
	now := time.Now().UTC().UnixMilli()
	if _, err := tx.ExecContext(ctx, "UPDATE local_run SET state = ?, updated_at_ms = ? WHERE run_id = ?", state, now, terminal.RunID); err != nil {
		return err
	}
	result, err := json.Marshal(map[string]any{"exit_code": terminal.ExitCode})
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE inbound_message SET status = ?, result_json = ?, updated_at_ms = ? WHERE message_id = ?", state, result, now, messageID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Spool) NextOutbound(ctx context.Context) (*spoolMessage, error) {
	var message spoolMessage
	err := s.db.QueryRowContext(ctx, `
		SELECT runtime_seq, message_id, method, params_json
		FROM outbound_message WHERE status = 'PENDING' ORDER BY runtime_seq LIMIT 1`).Scan(
		&message.RuntimeSeq, &message.MessageID, &message.Method, &message.Params)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &message, nil
}

func (s *Spool) MarkOutboundDelivered(ctx context.Context, sequence int64) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		UPDATE outbound_message SET status = 'DELIVERED', delivered_at_ms = ?, last_error = NULL
		WHERE runtime_seq = ?`, time.Now().UTC().UnixMilli(), sequence)
	return err
}

func (s *Spool) RecordOutboundError(ctx context.Context, sequence int64, reason string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE outbound_message SET last_error = ? WHERE runtime_seq = ?`, reason, sequence)
	return err
}

func (s *Spool) RecoverActiveRuns(ctx context.Context, runtimeID, epoch string) ([]recoveredRun, error) {
	if runtimeID == "" || epoch == "" {
		return nil, errors.New("runtime identity is required for recovery")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT run_id, task_id FROM local_run WHERE state IN ('ACCEPTED', 'RUNNING')`)
	if err != nil {
		return nil, err
	}
	var runs []recoveredRun
	for rows.Next() {
		var run recoveredRun
		if err := rows.Scan(&run.RunID, &run.TaskID); err != nil {
			rows.Close()
			return nil, err
		}
		runs = append(runs, run)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Recovery notification and LOST state must commit together. Otherwise a
	// crash between them permanently strands the central run as still running.
	for _, run := range runs {
		if _, err := enqueueEventTx(ctx, tx, model.RuntimeEvent{RuntimeID: runtimeID, Epoch: epoch, RunID: run.RunID, TaskID: run.TaskID, Type: "run.failed", Error: "runtime restarted before the run reached a terminal checkpoint"}); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE local_run SET state = 'LOST', updated_at_ms = ? WHERE state IN ('ACCEPTED', 'RUNNING')`,
		time.Now().UTC().UnixMilli()); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return runs, nil
}
