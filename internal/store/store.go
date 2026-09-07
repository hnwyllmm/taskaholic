package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"work-assistant/internal/backup"
	"work-assistant/internal/id"
	"work-assistant/internal/model"
)

type Store struct {
	db      *sql.DB
	writeMu sync.Mutex
}

const SchemaVersion = 14

// OpenProtected is the production entrypoint. Open remains available for
// explicit first-time test fixtures and offline tools.
func OpenProtected(path string) (*Store, error) {
	directory, err := backup.Directory(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := backup.CheckBeforeOpen(ctx, path, directory, SchemaVersion, "task", "run", "event_log"); err != nil {
		return nil, err
	}
	s, err := Open(path)
	if err != nil {
		return nil, err
	}
	if err := backup.Register(path, directory); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// A single connection per process makes PRAGMA settings deterministic.
	// SQLite WAL and busy_timeout serialize the control process and supervisor;
	// transactions are intentionally short.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	for _, statement := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
		"PRAGMA wal_autocheckpoint=1000",
	} {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			return nil, fmt.Errorf("configure sqlite (%s): %w", statement, err)
		}
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := backup.SecureSQLiteFiles(path); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) MarkAllRuntimesOffline(ctx context.Context) error {
	now := time.Now().UTC().UnixMilli()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		UPDATE runtime SET state = 'OFFLINE', disconnected_at_ms = ? WHERE state = 'ONLINE'`, now)
	return err
}

// Backup writes a transactionally consistent SQLite snapshot. VACUUM INTO also
// folds the WAL into the destination, so the resulting single file is portable.
func (s *Store) Backup(ctx context.Context, destination string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return backup.SQLiteSnapshot(ctx, s.db, destination)
}

func (s *Store) CreateTask(ctx context.Context, idempotencyKey, title, goal string, requirements ...model.TaskRequirements) (model.Task, bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Task{}, false, err
	}
	defer tx.Rollback()
	task, replayed, err := createTaskTx(ctx, tx, idempotencyKey, title, goal, requirements...)
	if err != nil {
		return model.Task{}, false, err
	}
	return task, replayed, tx.Commit()
}

func createTaskTx(ctx context.Context, tx *sql.Tx, idempotencyKey, title, goal string, requirements ...model.TaskRequirements) (model.Task, bool, error) {
	title = strings.TrimSpace(title)
	goal = strings.TrimSpace(goal)
	if title == "" || goal == "" {
		return model.Task{}, false, errors.New("title and goal are required")
	}

	var needs model.TaskRequirements
	if len(requirements) > 0 {
		needs = requirements[0]
	}
	if err := validateRequirementsTx(ctx, tx, needs); err != nil {
		return model.Task{}, false, err
	}
	needsJSON, _ := json.Marshal(needs)

	if idempotencyKey != "" {
		var existingID string
		err := tx.QueryRowContext(ctx,
			"SELECT resource_id FROM idempotency_key WHERE scope = 'task.create' AND key = ?",
			idempotencyKey,
		).Scan(&existingID)
		if err == nil {
			task, err := getTaskTx(ctx, tx, existingID)
			return task, true, err
		}
		if err != sql.ErrNoRows {
			return model.Task{}, false, err
		}
	}

	now := time.Now().UTC().UnixMilli()
	taskID := id.New("task")
	revisionID := id.New("rev")
	task := model.Task{
		Requirements:      needs,
		ID:                taskID,
		Title:             title,
		Goal:              goal,
		State:             model.TaskStateNew,
		Version:           1,
		CurrentRevisionID: revisionID,
		CreatedAtMS:       now,
		UpdatedAtMS:       now,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO task(task_id, title, goal, state, version, current_revision_id, created_at_ms, updated_at_ms, requirements_json)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		task.ID, task.Title, task.Goal, task.State, task.Version, task.CurrentRevisionID, now, now, needsJSON,
	); err != nil {
		return model.Task{}, false, fmt.Errorf("insert task: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO task_revision(revision_id, task_id, revision_number, goal, created_at_ms)
		VALUES(?, ?, 1, ?, ?)`, revisionID, taskID, goal, now); err != nil {
		return model.Task{}, false, fmt.Errorf("insert task revision: %w", err)
	}
	if _, err := appendEventTx(ctx, tx, "task", taskID, "TaskCreated", "", taskID, map[string]any{
		"title": title, "goal": goal, "revision_id": revisionID, "requirements": needs,
	}); err != nil {
		return model.Task{}, false, err
	}
	if idempotencyKey != "" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO idempotency_key(scope, key, resource_id, created_at_ms)
			VALUES('task.create', ?, ?, ?)`, idempotencyKey, taskID, now); err != nil {
			return model.Task{}, false, err
		}
	}
	return task, false, nil
}

func (s *Store) ListTasks(ctx context.Context, limit int) ([]model.Task, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT task_id, title, goal, state, version, current_revision_id,
		       COALESCE(assigned_agent_id, ''), created_at_ms, updated_at_ms, requirements_json
		FROM task ORDER BY created_at_ms DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []model.Task
	for rows.Next() {
		var task model.Task
		if err := scanTask(rows, &task); err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

func (s *Store) GetTask(ctx context.Context, taskID string) (model.Task, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT task_id, title, goal, state, version, current_revision_id,
		       COALESCE(assigned_agent_id, ''), created_at_ms, updated_at_ms, requirements_json
		FROM task WHERE task_id = ?`, taskID)
	var task model.Task
	if err := scanTask(row, &task); err != nil {
		return model.Task{}, err
	}
	return task, nil
}

func (s *Store) GetTaskDetail(ctx context.Context, taskID string, eventLimits ...int) (model.TaskDetail, error) {
	task, err := s.GetTask(ctx, taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	var summary *model.TaskSummary
	latestSummary, err := s.GetLatestTaskSummary(ctx, taskID)
	if err == nil {
		summary = &latestSummary
	} else if err != sql.ErrNoRows {
		return model.TaskDetail{}, err
	}
	runs, err := s.listRunsForTask(ctx, taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	var session *model.Session
	activeSession, err := s.GetTaskSession(ctx, taskID)
	if err == nil {
		session = &activeSession
	} else if err != sql.ErrNoRows {
		return model.TaskDetail{}, err
	}
	edges, err := s.ListTaskEdges(ctx, taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	directives, err := s.ListDirectivesForTask(ctx, taskID)
	if err != nil {
		return model.TaskDetail{}, err
	}
	eventOrder := " ORDER BY global_seq"
	eventArgs := []any{taskID, taskID, taskID}
	limited := len(eventLimits) > 0 && eventLimits[0] > 0
	if limited {
		limit := eventLimits[0]
		if limit > 200 {
			limit = 200
		}
		eventOrder = " ORDER BY global_seq DESC LIMIT ?"
		eventArgs = append(eventArgs, limit)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT global_seq, event_id, aggregate_type, aggregate_id, aggregate_seq,
		       event_type, occurred_at_ms, COALESCE(causation_id, ''),
		       COALESCE(correlation_id, ''), payload_json
		FROM event_log
		WHERE aggregate_id = ?
		   OR aggregate_id IN (SELECT run_id FROM run WHERE task_id = ?)
		   OR aggregate_id IN (SELECT session_id FROM task_session WHERE task_id = ?)
		`+eventOrder, eventArgs...)
	if err != nil {
		return model.TaskDetail{}, err
	}
	defer rows.Close()
	var events []model.Event
	for rows.Next() {
		var event model.Event
		if err := rows.Scan(&event.GlobalSeq, &event.ID, &event.AggregateType, &event.AggregateID,
			&event.AggregateSeq, &event.Type, &event.OccurredAtMS, &event.CausationID,
			&event.CorrelationID, &event.Payload); err != nil {
			return model.TaskDetail{}, err
		}
		events = append(events, event)
	}
	if runs == nil {
		runs = []model.Run{}
	}
	if limited {
		for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
			events[i], events[j] = events[j], events[i]
		}
	}
	if edges == nil {
		edges = []model.TaskEdge{}
	}
	if directives == nil {
		directives = []model.Directive{}
	}
	if events == nil {
		events = []model.Event{}
	}
	return model.TaskDetail{
		Task: task, Summary: summary, Session: session, Runs: runs, Edges: edges, Directives: directives, Events: events,
	}, rows.Err()
}

type CreateRunRequest struct {
	ExpectedTaskVersion  int64 // Scheduler snapshot guard; zero for legacy callers.
	RequireNativeSession bool
	SystemBinding        *model.SystemBinding
	RoutingDecisionID    string // Internal router run guard.
	AssignmentDecisionID string // AI decision used for a business assignment.
	Managed              bool   // Only the durable work scheduler may start managed tasks.
	ReadOnly             bool
	RoleDraftID          string
	HomeChatID           string
	Instructions         string
	OutputSchema         json.RawMessage
	TaskID               string
	SessionID            string
	RuntimeID            string
	AgentID              string
	AdapterID            string
	ModelID              string
	ReasoningEffort      *string // nil inherits; an explicit empty value uses the native default.
	Command              []string
	WorkingDir           string
	IdempotencyKey       string
}

func (s *Store) CreateRun(ctx context.Context, request CreateRunRequest) (model.Run, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Run{}, err
	}
	defer tx.Rollback()
	run, err := createRunTx(ctx, tx, request)
	if err != nil {
		return model.Run{}, err
	}
	return run, tx.Commit()
}

func createRunTx(ctx context.Context, tx *sql.Tx, request CreateRunRequest) (model.Run, error) {
	if err := checkMaintenanceTx(ctx, tx); err != nil {
		return model.Run{}, err
	}
	if request.TaskID == "" {
		return model.Run{}, errors.New("task_id is required")
	}
	if err := checkSystemBindingTx(ctx, tx, request); err != nil {
		return model.Run{}, err
	}
	var routingID string
	if err := tx.QueryRowContext(ctx, `SELECT decision_id FROM routing_decision WHERE internal_task_id=?`, request.TaskID).Scan(&routingID); err == nil && request.RoutingDecisionID != routingID {
		return model.Run{}, fmt.Errorf("%w: router runs must use the scheduler", model.ErrConflict)
	} else if err != nil && err != sql.ErrNoRows {
		return model.Run{}, err
	}
	managed, err := isManagedTx(ctx, tx, request.TaskID)
	if err != nil {
		return model.Run{}, err
	}
	if managed && !request.Managed {
		return model.Run{}, fmt.Errorf("%w: managed tasks must use task messages and the scheduler", model.ErrConflict)
	}
	if request.AdapterID == "" {
		request.AdapterID = "exec-agent"
	}
	var draftID string
	if err := tx.QueryRowContext(ctx, `SELECT draft_id FROM role_draft WHERE task_id = ?`, request.TaskID).Scan(&draftID); err == nil && request.RoleDraftID != draftID {
		return model.Run{}, fmt.Errorf("%w: use the role draft messages endpoint for this task", model.ErrConflict)
	} else if err != nil && err != sql.ErrNoRows {
		return model.Run{}, err
	}
	var chatID string
	if err := tx.QueryRowContext(ctx, `SELECT chat_id FROM home_chat WHERE task_id=?`, request.TaskID).Scan(&chatID); err == nil && request.HomeChatID != chatID {
		return model.Run{}, fmt.Errorf("%w: use the home chat messages endpoint", model.ErrConflict)
	} else if err != nil && err != sql.ErrNoRows {
		return model.Run{}, err
	}
	commandJSON, err := json.Marshal(request.Command)
	if err != nil {
		return model.Run{}, err
	}

	var currentRevision, taskTitle, taskGoal string
	if err := tx.QueryRowContext(ctx, `
		SELECT current_revision_id, title, goal FROM task WHERE task_id = ?`, request.TaskID).
		Scan(&currentRevision, &taskTitle, &taskGoal); err != nil {
		return model.Run{}, err
	}
	if request.IdempotencyKey != "" {
		var existingRunID string
		scope := "task.run:" + request.TaskID
		err := tx.QueryRowContext(ctx, `SELECT resource_id FROM idempotency_key WHERE scope = ? AND key = ?`,
			scope, request.IdempotencyKey).Scan(&existingRunID)
		if err == nil {
			return scanRun(tx.QueryRowContext(ctx, runSelect+" WHERE run_id = ?", existingRunID))
		}
		if err != sql.ErrNoRows {
			return model.Run{}, err
		}
	}
	now := time.Now().UTC().UnixMilli()
	session, created, bound, err := ensureTaskSessionTx(ctx, tx, request, now)
	if err != nil {
		return model.Run{}, err
	}
	var activeRuns int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM run WHERE session_id = ? AND state IN (?, ?)`,
		session.ID, model.RunStateQueued, model.RunStateRunning).Scan(&activeRuns); err != nil {
		return model.Run{}, err
	}
	if activeRuns > 0 {
		return model.Run{}, fmt.Errorf("%w: session already has an active run", model.ErrConflict)
	}
	role, err := validateAgentAssignmentTx(ctx, tx, session, request.TaskID)
	if err != nil {
		return model.Run{}, err
	}
	execution, err := resolveExecutionTx(ctx, tx, session, request.ReasoningEffort)
	if err != nil {
		return model.Run{}, err
	}
	if created {
		session.Metadata, _ = json.Marshal(map[string]any{"role_snapshot": role, "system_binding": request.SystemBinding, "execution_defaults": execution})
		if _, err := tx.ExecContext(ctx, `UPDATE session SET metadata_json = ? WHERE session_id = ?`, session.Metadata, session.ID); err != nil {
			return model.Run{}, err
		}
	}
	if !created {
		var metadata struct {
			Role *model.Role `json:"role_snapshot"`
		}
		if err := json.Unmarshal(session.Metadata, &metadata); err != nil {
			return model.Run{}, err
		}
		role = metadata.Role
	}
	roleJSON, _ := json.Marshal(role)
	if created {
		if _, err := appendEventTx(ctx, tx, "session", session.ID, "SessionCreated", "", request.TaskID, map[string]any{
			"task_id": request.TaskID, "agent_id": session.AgentID, "adapter_id": session.AdapterID,
			"model_id": session.ModelID, "runtime_id": session.RuntimeID, "memory_owner": session.MemoryOwner,
		}); err != nil {
			return model.Run{}, err
		}
	}
	if bound {
		if _, err := appendEventTx(ctx, tx, "task", request.TaskID, "TaskSessionBound", "", request.TaskID, map[string]any{
			"session_id": session.ID, "agent_id": session.AgentID, "adapter_id": session.AdapterID,
			"model_id": session.ModelID, "runtime_id": session.RuntimeID,
		}); err != nil {
			return model.Run{}, err
		}
	}
	runID := id.New("run")
	run := model.Run{
		ExecutionSettings: execution,
		Role:              role,
		ID:                runID, TaskID: request.TaskID, SessionID: session.ID, RuntimeID: session.RuntimeID,
		AgentID: session.AgentID, AdapterID: session.AdapterID, ModelID: session.ModelID, State: model.RunStateQueued,
		Command: request.Command, WorkingDir: request.WorkingDir, CreatedAtMS: now,
	}
	executionJSON, _ := json.Marshal(execution)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO run(run_id, task_id, task_revision_id, runtime_id, agent_id, state,
		                command_json, working_dir, lease_epoch, created_at_ms, session_id, adapter_id, model_id, role_snapshot_json, execution_json)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?)`,
		runID, request.TaskID, currentRevision, session.RuntimeID, session.AgentID, run.State,
		commandJSON, request.WorkingDir, now, session.ID, session.AdapterID, session.ModelID, roleJSON, executionJSON,
	); err != nil {
		return model.Run{}, fmt.Errorf("insert run: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE session SET last_run_id = ?, updated_at_ms = ? WHERE session_id = ?`,
		runID, now, session.ID); err != nil {
		return model.Run{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE task SET state = ?, assigned_agent_id = ?, version = version + 1, updated_at_ms = ?
		WHERE task_id = ?`, model.TaskStateInProgress, session.AgentID, now, request.TaskID); err != nil {
		return model.Run{}, err
	}
	messageID := id.New("msg")
	spec := model.RunSpec{
		ExecutionSettings:    execution,
		RequireNativeSession: request.RequireNativeSession,
		ReadOnly:             request.ReadOnly,
		Instructions:         request.Instructions, OutputSchema: request.OutputSchema,
		RunID: runID, TaskID: request.TaskID, TaskTitle: taskTitle, TaskGoal: taskGoal,
		SessionID: session.ID, AgentID: session.AgentID, AdapterID: session.AdapterID,
		ModelID: session.ModelID, AgentSessionRef: session.AgentSessionRef, Command: request.Command,
		WorkingDir: request.WorkingDir, LeaseEpoch: 1, LeaseUntil: now + int64((5*time.Minute)/time.Millisecond),
		MessageID: messageID,
	}
	if role != nil {
		spec.Instructions = role.ExecutionInstructions() + "\n\n" + spec.Instructions
	}
	params, _ := json.Marshal(spec)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO outbox_message(message_id, destination_id, method, params_json, status,
		                           attempts, next_attempt_at_ms, created_at_ms)
		VALUES(?, ?, 'run.start', ?, 'PENDING', 0, ?, ?)`,
		messageID, session.RuntimeID, params, now, now); err != nil {
		return model.Run{}, fmt.Errorf("insert outbox: %w", err)
	}
	if _, err := appendEventTx(ctx, tx, "run", runID, "RunQueued", messageID, request.TaskID, map[string]any{
		"task_id": request.TaskID, "session_id": session.ID, "runtime_id": session.RuntimeID,
		"agent_id": session.AgentID, "adapter_id": session.AdapterID, "model_id": session.ModelID,
		"command": request.Command, "role_snapshot": role, "execution": execution,
	}); err != nil {
		return model.Run{}, err
	}
	if request.IdempotencyKey != "" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO idempotency_key(scope, key, resource_id, created_at_ms) VALUES(?, ?, ?, ?)`,
			"task.run:"+request.TaskID, request.IdempotencyKey, runID, now); err != nil {
			return model.Run{}, err
		}
	}
	return run, nil
}

func ensureTaskSessionTx(ctx context.Context, tx *sql.Tx, request CreateRunRequest, now int64) (model.Session, bool, bool, error) {
	active, err := getActiveSessionTx(ctx, tx, request.TaskID)
	if err != nil && err != sql.ErrNoRows {
		return model.Session{}, false, false, err
	}
	if err == nil {
		if request.SessionID != "" && request.SessionID != active.ID {
			return model.Session{}, false, false, errors.New("task is already bound to a different session")
		}
		if err := validateSessionRequest(active, request); err != nil {
			return model.Session{}, false, false, err
		}
		return active, false, false, nil
	}

	var session model.Session
	created := false
	if request.SessionID != "" {
		session, err = getSessionTx(ctx, tx, request.SessionID)
		if err != nil {
			return model.Session{}, false, false, err
		}
		if err := validateSessionRequest(session, request); err != nil {
			return model.Session{}, false, false, err
		}
	} else {
		if request.RuntimeID == "" {
			return model.Session{}, false, false, errors.New("runtime_id is required for a new session")
		}
		agentID := request.AgentID
		if agentID == "" {
			agentID = id.New("agent")
		}
		session = model.Session{
			ID: id.New("session"), AgentID: agentID, AdapterID: request.AdapterID, ModelID: request.ModelID,
			RuntimeID: request.RuntimeID, State: "ACTIVE", MemoryOwner: "agent",
			Metadata: json.RawMessage(`{}`), CreatedAtMS: now, UpdatedAtMS: now,
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO session(session_id, agent_id, adapter_id, model_id, runtime_id, state, agent_session_ref,
			                    memory_owner, metadata_json, created_at_ms, updated_at_ms)
			VALUES(?, ?, ?, ?, ?, 'ACTIVE', NULL, 'agent', '{}', ?, ?)`,
			session.ID, session.AgentID, session.AdapterID, session.ModelID, session.RuntimeID, now, now); err != nil {
			return model.Session{}, false, false, err
		}
		created = true
	}
	if session.State != "ACTIVE" {
		return model.Session{}, false, false, errors.New("session is not active")
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO task_session(task_id, session_id, bound_at_ms) VALUES(?, ?, ?)`,
		request.TaskID, session.ID, now); err != nil {
		return model.Session{}, false, false, err
	}
	return session, created, true, nil
}

func validateSessionRequest(session model.Session, request CreateRunRequest) error {
	if request.RuntimeID != "" && request.RuntimeID != session.RuntimeID {
		return errors.New("runtime_id conflicts with the task session")
	}
	if request.AgentID != "" && request.AgentID != session.AgentID {
		return errors.New("agent_id conflicts with the task session")
	}
	if request.AdapterID != "" && request.AdapterID != session.AdapterID {
		return errors.New("adapter_id conflicts with the task session")
	}
	if request.ModelID != "" && request.ModelID != session.ModelID {
		return errors.New("model_id conflicts with the task session")
	}
	return nil
}

func (s *Store) GetRun(ctx context.Context, runID string) (model.Run, error) {
	row := s.db.QueryRowContext(ctx, runSelect+" WHERE run_id = ?", runID)
	return scanRun(row)
}

func (s *Store) GetTaskSession(ctx context.Context, taskID string) (model.Session, error) {
	row := s.db.QueryRowContext(ctx, sessionSelect+`
		JOIN task_session ts ON ts.session_id = s.session_id
		WHERE ts.task_id = ? AND ts.unbound_at_ms IS NULL`, taskID)
	return scanSession(row)
}

func (s *Store) GetSession(ctx context.Context, sessionID string) (model.Session, error) {
	return scanSession(s.db.QueryRowContext(ctx, sessionSelect+" WHERE s.session_id = ?", sessionID))
}

func (s *Store) ListSessions(ctx context.Context, limit int) ([]model.Session, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, sessionSelect+" ORDER BY s.updated_at_ms DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []model.Session
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

func (s *Store) GetSessionDetail(ctx context.Context, sessionID string) (model.SessionDetail, error) {
	session, err := s.GetSession(ctx, sessionID)
	if err != nil {
		return model.SessionDetail{}, err
	}
	taskRows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT t.task_id, t.title, t.goal, t.state, t.version, t.current_revision_id,
		       COALESCE(t.assigned_agent_id, ''), t.created_at_ms, t.updated_at_ms, t.requirements_json
		FROM task t JOIN task_session ts ON ts.task_id = t.task_id
		WHERE ts.session_id = ? ORDER BY t.created_at_ms`, sessionID)
	if err != nil {
		return model.SessionDetail{}, err
	}
	var tasks []model.Task
	for taskRows.Next() {
		var task model.Task
		if err := scanTask(taskRows, &task); err != nil {
			taskRows.Close()
			return model.SessionDetail{}, err
		}
		tasks = append(tasks, task)
	}
	if err := taskRows.Close(); err != nil {
		return model.SessionDetail{}, err
	}
	runRows, err := s.db.QueryContext(ctx, runSelect+" WHERE session_id = ? ORDER BY created_at_ms", sessionID)
	if err != nil {
		return model.SessionDetail{}, err
	}
	defer runRows.Close()
	var runs []model.Run
	for runRows.Next() {
		run, err := scanRun(runRows)
		if err != nil {
			return model.SessionDetail{}, err
		}
		runs = append(runs, run)
	}
	if tasks == nil {
		tasks = []model.Task{}
	}
	if runs == nil {
		runs = []model.Run{}
	}
	return model.SessionDetail{Session: session, Tasks: tasks, Runs: runs}, runRows.Err()
}

func (s *Store) InterruptRun(ctx context.Context, runID string) (string, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var runtimeID, taskID, state string
	if err := tx.QueryRowContext(ctx, "SELECT runtime_id, task_id, state FROM run WHERE run_id = ?", runID).Scan(&runtimeID, &taskID, &state); err != nil {
		return "", err
	}
	if state == model.RunStateCompleted || state == model.RunStateFailed || state == model.RunStateInterrupted {
		return "", fmt.Errorf("run is already terminal: %s", state)
	}
	now := time.Now().UTC().UnixMilli()
	commandID := id.New("cmd")
	params, _ := json.Marshal(map[string]any{"run_id": runID, "task_id": taskID, "command_id": commandID})
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO command(command_id, run_id, command_type, status, created_at_ms)
		VALUES(?, ?, 'INTERRUPT', 'QUEUED', ?)`, commandID, runID, now); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO outbox_message(message_id, destination_id, method, params_json, status,
		                           attempts, next_attempt_at_ms, created_at_ms)
		VALUES(?, ?, 'run.interrupt', ?, 'PENDING', 0, ?, ?)`, commandID, runtimeID, params, now, now); err != nil {
		return "", err
	}
	if _, err := appendEventTx(ctx, tx, "run", runID, "RunInterruptRequested", commandID, taskID, map[string]any{
		"command_id": commandID,
	}); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return commandID, nil
}

func (s *Store) RegisterRuntime(ctx context.Context, hello model.RuntimeHello) error {
	capabilities, _ := json.Marshal(hello.Capabilities)
	now := time.Now().UTC().UnixMilli()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO runtime(runtime_id, epoch, state, hostname, os, arch, capabilities_json,
		                    connected_at_ms, last_seen_at_ms, disconnected_at_ms)
		VALUES(?, ?, 'ONLINE', ?, ?, ?, ?, ?, ?, NULL)
		ON CONFLICT(runtime_id) DO UPDATE SET
			epoch = excluded.epoch, state = 'ONLINE', hostname = excluded.hostname,
			os = excluded.os, arch = excluded.arch, capabilities_json = excluded.capabilities_json,
			connected_at_ms = excluded.connected_at_ms, last_seen_at_ms = excluded.last_seen_at_ms,
			disconnected_at_ms = NULL`,
		hello.RuntimeID, hello.Epoch, hello.Hostname, hello.OS, hello.Arch, capabilities, now, now)
	return err
}

func (s *Store) HeartbeatRuntime(ctx context.Context, runtimeID, epoch string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	result, err := s.db.ExecContext(ctx, `
		UPDATE runtime SET last_seen_at_ms = ?, state = 'ONLINE'
		WHERE runtime_id = ? AND epoch = ?`, time.Now().UTC().UnixMilli(), runtimeID, epoch)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return errors.New("runtime epoch is not current")
	}
	return nil
}

func (s *Store) DisconnectRuntime(ctx context.Context, runtimeID, epoch string) error {
	now := time.Now().UTC().UnixMilli()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		UPDATE runtime SET state = 'OFFLINE', disconnected_at_ms = ?, last_seen_at_ms = ?
		WHERE runtime_id = ? AND epoch = ?`, now, now, runtimeID, epoch)
	return err
}

func (s *Store) ListRuntimes(ctx context.Context) ([]model.Runtime, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT runtime_id, epoch, state, hostname, os, arch, capabilities_json,
		       last_seen_at_ms, connected_at_ms, disconnected_at_ms
		FROM runtime ORDER BY runtime_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runtimes []model.Runtime
	for rows.Next() {
		var runtime model.Runtime
		if err := rows.Scan(&runtime.ID, &runtime.Epoch, &runtime.State, &runtime.Hostname,
			&runtime.OS, &runtime.Arch, &runtime.Capabilities, &runtime.LastSeenAtMS,
			&runtime.ConnectedAtMS, &runtime.DisconnectedAt); err != nil {
			return nil, err
		}
		runtimes = append(runtimes, runtime)
	}
	return runtimes, rows.Err()
}

func (s *Store) ApplyRuntimeEvent(ctx context.Context, event model.RuntimeEvent) (bool, error) {
	return s.ApplyRuntimeEventFrom(ctx, event.Epoch, event)
}

// A durable event keeps its original epoch/sequence across restarts. Authenticate
// its delivery using the CURRENT connection epoch, not the event's old identity.
// This permits outbox replay without letting a fenced connection write new data.
func (s *Store) ApplyRuntimeEventFrom(ctx context.Context, connectionEpoch string, event model.RuntimeEvent) (bool, error) {
	if event.RuntimeID == "" || event.Epoch == "" || event.RuntimeSeq <= 0 || event.RunID == "" || event.Type == "" {
		return false, errors.New("invalid runtime event")
	}
	if event.Type == "run.progress" {
		event.Message, _, _ = model.ObservationText(event.Message, 16000)
		event.Error, _, _ = model.ObservationText(event.Error, 2000)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO runtime_event_dedupe(runtime_id, epoch, runtime_seq, received_at_ms)
		VALUES(?, ?, ?, ?)`, event.RuntimeID, event.Epoch, event.RuntimeSeq, time.Now().UTC().UnixMilli())
	if err != nil {
		return false, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return true, nil
	}
	var taskID, state, assignedRuntimeID, sessionID string
	if err := tx.QueryRowContext(ctx, `
		SELECT task_id, state, runtime_id, COALESCE(session_id, '') FROM run WHERE run_id = ?`,
		event.RunID).Scan(&taskID, &state, &assignedRuntimeID, &sessionID); err != nil {
		return false, err
	}
	if assignedRuntimeID != event.RuntimeID {
		return false, errors.New("runtime is not assigned to this run")
	}
	if event.TaskID != "" && event.TaskID != taskID {
		return false, errors.New("task_id does not match the run")
	}
	if event.SessionID != "" && event.SessionID != sessionID {
		return false, errors.New("session_id does not match the run")
	}
	var currentEpoch string
	if err := tx.QueryRowContext(ctx, "SELECT epoch FROM runtime WHERE runtime_id = ?", event.RuntimeID).Scan(&currentEpoch); err != nil {
		return false, err
	}
	if currentEpoch != connectionEpoch {
		return false, errors.New("runtime epoch is not current")
	}
	now := event.OccurredAt
	if now == 0 {
		now = time.Now().UTC().UnixMilli()
	}
	aggregateType := "run"
	aggregateID := event.RunID
	terminal := event.Type == "run.completed" || event.Type == "run.failed" || event.Type == "run.interrupted"
	managed, err := isManagedTx(ctx, tx, taskID)
	if err != nil {
		return false, err
	}
	if terminal && (state == model.RunStateCompleted || state == model.RunStateFailed || state == model.RunStateInterrupted) {
		return false, tx.Commit()
	}
	if terminal {
		if len(event.Output) > 128*1024 {
			event.Output = ""
			event.Type = "run.failed"
			event.Error = "run output exceeds 128 KiB"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE run SET output = ? WHERE run_id = ?`, event.Output, event.RunID); err != nil {
			return false, err
		}
		if err := applyRoleDraftResultTx(ctx, tx, event, now); err != nil {
			return false, err
		}
		if err := applyHomeResultTx(ctx, tx, event, now); err != nil {
			return false, err
		}
		if err := applyRoutingResultTx(ctx, tx, event); err != nil {
			return false, err
		}
	}
	switch event.Type {
	case "session.bound":
		if sessionID == "" || event.SessionID != sessionID || event.AgentSessionRef == "" {
			return false, errors.New("session.bound requires session_id and agent_session_ref")
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE session SET agent_session_ref = ?, updated_at_ms = ? WHERE session_id = ?`,
			event.AgentSessionRef, now, sessionID)
		aggregateType, aggregateID = "session", sessionID
	case "run.started":
		if state == model.RunStateQueued {
			_, err = tx.ExecContext(ctx, `UPDATE run SET state = ?, started_at_ms = ? WHERE run_id = ?`, model.RunStateRunning, now, event.RunID)
		}
	case "run.progress":
		// Progress is event-only; it does not change the Run state.
	case "run.configured":
		err = applyExecutionTx(ctx, tx, event)
	case "run.completed":
		_, err = tx.ExecContext(ctx, `UPDATE run SET state = ?, finished_at_ms = ?, exit_code = COALESCE(?, 0), error = NULL WHERE run_id = ?`,
			model.RunStateCompleted, now, event.ExitCode, event.RunID)
		if err == nil {
			var unfinishedChildren int
			err = tx.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM task_edge e JOIN task child ON child.task_id = e.to_task_id
				WHERE e.from_task_id = ? AND e.edge_type = ? AND child.state != ?`,
				taskID, model.TaskEdgeDecomposedInto, model.TaskStateCompleted).Scan(&unfinishedChildren)
			if err == nil {
				nextTaskState := model.TaskStateCompleted
				if managed {
					nextTaskState = model.TaskStateReview
				}
				if unfinishedChildren > 0 {
					nextTaskState = model.TaskStateWaiting
				}
				_, err = tx.ExecContext(ctx, `UPDATE task SET state = ?, version = version + 1, updated_at_ms = ? WHERE task_id = ?`, nextTaskState, now, taskID)
			}
		}
		if err == nil {
			var resultingState string
			if err = tx.QueryRowContext(ctx, `SELECT state FROM task WHERE task_id = ?`, taskID).Scan(&resultingState); err == nil && resultingState == model.TaskStateCompleted {
				err = updateParentStatesTx(ctx, tx, taskID, now)
			}
		}
	case "run.failed":
		_, err = tx.ExecContext(ctx, `UPDATE run SET state = ?, finished_at_ms = ?, exit_code = ?, error = ? WHERE run_id = ?`,
			model.RunStateFailed, now, event.ExitCode, event.Error, event.RunID)
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE task SET state = ?, version = version + 1, updated_at_ms = ? WHERE task_id = ?`, model.TaskStateBlocked, now, taskID)
		}
		if err == nil && !managed {
			err = updateParentStatesTx(ctx, tx, taskID, now)
		}
	case "run.interrupted":
		_, err = tx.ExecContext(ctx, `UPDATE run SET state = ?, finished_at_ms = ?, error = ? WHERE run_id = ?`,
			model.RunStateInterrupted, now, event.Error, event.RunID)
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE task SET state = ?, version = version + 1, updated_at_ms = ? WHERE task_id = ?`, model.TaskStateAssigned, now, taskID)
		}
	case "directive.applied", "directive.rejected":
		if event.DirectiveID == "" {
			return false, errors.New("directive result requires directive_id")
		}
		directiveState := model.DirectiveStateApplied
		if event.Type == "directive.rejected" {
			directiveState = model.DirectiveStateRejected
		}
		result, updateErr := tx.ExecContext(ctx, `
			UPDATE directive SET state = ?, error = NULLIF(?, ''), applied_at_ms = ?
			WHERE directive_id = ? AND run_id = ?`, directiveState, event.Error, now,
			event.DirectiveID, event.RunID)
		if updateErr != nil {
			err = updateErr
		} else if affected, _ := result.RowsAffected(); affected == 0 {
			return false, errors.New("directive does not belong to the run")
		}
	default:
		return false, fmt.Errorf("unsupported runtime event type: %s", event.Type)
	}
	if err != nil {
		return false, err
	}
	if terminal && managed {
		if err := applyWorkResultTx(ctx, tx, event, now); err != nil {
			return false, err
		}
	}
	if event.Type == "run.progress" && event.Activity != nil {
		if err := recordActivityTx(ctx, tx, event, time.Now().UnixMilli()); err != nil {
			return false, err
		}
	} else if _, err := appendEventTx(ctx, tx, aggregateType, aggregateID, runtimeEventName(event.Type), event.CausationID, taskID, event); err != nil {
		return false, err
	}
	if terminal {
		if err := finishActivitiesTx(ctx, tx, event); err != nil {
			return false, err
		}
	}
	if event.Type == "run.completed" && !managed {
		var taskState string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM task WHERE task_id=?`, taskID).Scan(&taskState); err != nil {
			return false, err
		}
		if taskState == model.TaskStateCompleted {
			eligible, eligibilityErr := taskSummaryEligibleTx(ctx, tx, taskID)
			if eligibilityErr != nil {
				return false, eligibilityErr
			}
			if eligible {
				if _, err := createTaskSummaryTx(ctx, tx, taskID, "run", event.RunID, time.Now().UTC().UnixMilli()); err != nil {
					return false, err
				}
			}
		}
	}
	if sessionID != "" {
		_, _ = tx.ExecContext(ctx, `UPDATE session SET updated_at_ms = ? WHERE session_id = ?`, now, sessionID)
	}
	_, _ = tx.ExecContext(ctx, `UPDATE runtime SET last_seen_at_ms = ? WHERE runtime_id = ?`, time.Now().UTC().UnixMilli(), event.RuntimeID)
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return false, nil
}

func (s *Store) ClaimOutbox(ctx context.Context, destinationID, owner string) (*model.OutboxMessage, error) {
	now := time.Now().UTC().UnixMilli()
	leaseUntil := now + int64((30*time.Second)/time.Millisecond)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var message model.OutboxMessage
	err = tx.QueryRowContext(ctx, `
		SELECT message_id, destination_id, method, params_json, attempts,
		       COALESCE(lease_expires_at_ms, 0), next_attempt_at_ms
		FROM outbox_message
		WHERE destination_id = ?
		  AND next_attempt_at_ms <= ?
		  AND (status IN ('PENDING', 'RETRY') OR (status = 'IN_FLIGHT' AND lease_expires_at_ms < ?))
		ORDER BY created_at_ms LIMIT 1`, destinationID, now, now).Scan(
		&message.ID, &message.DestinationID, &message.Method, &message.Params,
		&message.Attempts, &message.LeaseExpiresMS, &message.NextAttemptAtMS,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE outbox_message SET status = 'IN_FLIGHT', attempts = attempts + 1,
		       lease_owner = ?, lease_expires_at_ms = ? WHERE message_id = ?`,
		owner, leaseUntil, message.ID); err != nil {
		return nil, err
	}
	message.Attempts++
	message.LeaseExpiresMS = leaseUntil
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &message, nil
}

func (s *Store) MarkOutboxDelivered(ctx context.Context, messageID string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		UPDATE outbox_message SET status = 'DELIVERED', delivered_at_ms = ?,
		       lease_owner = NULL, lease_expires_at_ms = NULL WHERE message_id = ?`,
		time.Now().UTC().UnixMilli(), messageID)
	return err
}

func (s *Store) RetryOutbox(ctx context.Context, messageID, reason string, attempts int) error {
	delay := time.Second << min(attempts, 6)
	if delay > time.Minute {
		delay = time.Minute
	}
	next := time.Now().UTC().Add(delay).UnixMilli()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		UPDATE outbox_message SET status = 'RETRY', next_attempt_at_ms = ?, last_error = ?,
		       lease_owner = NULL, lease_expires_at_ms = NULL WHERE message_id = ?`, next, reason, messageID)
	return err
}

func (s *Store) EventsAfter(ctx context.Context, after int64, limit int, taskIDs ...string) ([]model.Event, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	filter := ""
	args := []any{after}
	if len(taskIDs) > 0 && taskIDs[0] != "" {
		filter = " AND correlation_id=?"
		args = append(args, taskIDs[0])
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `
		SELECT global_seq, event_id, aggregate_type, aggregate_id, aggregate_seq,
		       event_type, occurred_at_ms, COALESCE(causation_id, ''),
		       COALESCE(correlation_id, ''), payload_json
		FROM event_log WHERE global_seq > ?`+filter+` ORDER BY global_seq LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []model.Event
	for rows.Next() {
		var event model.Event
		if err := rows.Scan(&event.GlobalSeq, &event.ID, &event.AggregateType, &event.AggregateID,
			&event.AggregateSeq, &event.Type, &event.OccurredAtMS, &event.CausationID,
			&event.CorrelationID, &event.Payload); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) listRunsForTask(ctx context.Context, taskID string) ([]model.Run, error) {
	rows, err := s.db.QueryContext(ctx, runSelect+" WHERE task_id = ? ORDER BY created_at_ms", taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []model.Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

const runSelect = `
	SELECT run_id, task_id, COALESCE(session_id, ''), runtime_id, agent_id, adapter_id,
	       COALESCE(model_id, ''), state, command_json,
	       COALESCE(working_dir, ''), created_at_ms, started_at_ms, finished_at_ms,
	       exit_code, COALESCE(error, ''), output, role_snapshot_json, execution_json FROM run`

const sessionSelect = `
	SELECT s.session_id, s.agent_id, s.adapter_id, COALESCE(s.model_id, ''), s.runtime_id, s.state,
	       COALESCE(s.agent_session_ref, ''), s.memory_owner, s.metadata_json,
	       COALESCE(s.last_run_id, ''), s.created_at_ms, s.updated_at_ms
	FROM session s`

type rowScanner interface {
	Scan(...any) error
}

func scanTask(row rowScanner, task *model.Task, extra ...any) error {
	var needs []byte
	columns := []any{&task.ID, &task.Title, &task.Goal, &task.State, &task.Version,
		&task.CurrentRevisionID, &task.AssignedAgentID, &task.CreatedAtMS, &task.UpdatedAtMS, &needs}
	if err := row.Scan(append(columns, extra...)...); err != nil {
		return err
	}
	return json.Unmarshal(needs, &task.Requirements)
}

func scanRun(row rowScanner) (model.Run, error) {
	var run model.Run
	var commandJSON, roleJSON, executionJSON []byte
	var started, finished sql.NullInt64
	var exitCode sql.NullInt64
	err := row.Scan(&run.ID, &run.TaskID, &run.SessionID, &run.RuntimeID, &run.AgentID, &run.AdapterID,
		&run.ModelID, &run.State,
		&commandJSON, &run.WorkingDir, &run.CreatedAtMS, &started, &finished, &exitCode, &run.Error, &run.Output, &roleJSON, &executionJSON)
	if err != nil {
		return model.Run{}, err
	}
	if err := json.Unmarshal(commandJSON, &run.Command); err != nil {
		return model.Run{}, err
	}
	if err := json.Unmarshal(roleJSON, &run.Role); err != nil {
		return model.Run{}, err
	}
	if err := json.Unmarshal(executionJSON, &run.ExecutionSettings); err != nil {
		return model.Run{}, err
	}
	if started.Valid {
		run.StartedAtMS = &started.Int64
	}
	if finished.Valid {
		run.FinishedAtMS = &finished.Int64
	}
	if exitCode.Valid {
		code := int(exitCode.Int64)
		run.ExitCode = &code
	}
	return run, nil
}

func scanSession(row rowScanner) (model.Session, error) {
	var session model.Session
	var metadata string
	if err := row.Scan(&session.ID, &session.AgentID, &session.AdapterID, &session.ModelID, &session.RuntimeID,
		&session.State, &session.AgentSessionRef, &session.MemoryOwner, &metadata,
		&session.LastRunID, &session.CreatedAtMS, &session.UpdatedAtMS); err != nil {
		return model.Session{}, err
	}
	session.Metadata = json.RawMessage(metadata)
	return session, nil
}

func getActiveSessionTx(ctx context.Context, tx *sql.Tx, taskID string) (model.Session, error) {
	return scanSession(tx.QueryRowContext(ctx, sessionSelect+`
		JOIN task_session ts ON ts.session_id = s.session_id
		WHERE ts.task_id = ? AND ts.unbound_at_ms IS NULL`, taskID))
}

func getSessionTx(ctx context.Context, tx *sql.Tx, sessionID string) (model.Session, error) {
	return scanSession(tx.QueryRowContext(ctx, sessionSelect+" WHERE s.session_id = ?", sessionID))
}

func getTaskTx(ctx context.Context, tx *sql.Tx, taskID string) (model.Task, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT task_id, title, goal, state, version, current_revision_id,
		       COALESCE(assigned_agent_id, ''), created_at_ms, updated_at_ms, requirements_json
		FROM task WHERE task_id = ?`, taskID)
	var task model.Task
	err := scanTask(row, &task)
	return task, err
}

func appendEventTx(ctx context.Context, tx *sql.Tx, aggregateType, aggregateID, eventType, causationID, correlationID string, payload any) (model.Event, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return model.Event{}, err
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(aggregate_seq), 0) + 1 FROM event_log
		WHERE aggregate_type = ? AND aggregate_id = ?`, aggregateType, aggregateID).Scan(&sequence); err != nil {
		return model.Event{}, err
	}
	now := time.Now().UTC().UnixMilli()
	eventID := id.New("evt")
	result, err := tx.ExecContext(ctx, `
		INSERT INTO event_log(event_id, aggregate_type, aggregate_id, aggregate_seq,
		                      event_type, schema_version, occurred_at_ms, recorded_at_ms,
		                      causation_id, correlation_id, payload_json)
		VALUES(?, ?, ?, ?, ?, 1, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?)`,
		eventID, aggregateType, aggregateID, sequence, eventType, now, now,
		causationID, correlationID, encoded)
	if err != nil {
		return model.Event{}, err
	}
	globalSeq, _ := result.LastInsertId()
	return model.Event{GlobalSeq: globalSeq, ID: eventID, AggregateType: aggregateType,
		AggregateID: aggregateID, AggregateSeq: sequence, Type: eventType,
		OccurredAtMS: now, CausationID: causationID, CorrelationID: correlationID, Payload: encoded}, nil
}

func runtimeEventName(name string) string {
	parts := strings.Split(name, ".")
	var result strings.Builder
	for _, part := range parts {
		if part == "" {
			continue
		}
		result.WriteString(strings.ToUpper(part[:1]))
		result.WriteString(part[1:])
	}
	return result.String()
}

func migrate(db *sql.DB) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS schema_version(version INTEGER PRIMARY KEY, applied_at_ms INTEGER NOT NULL)`,
		`INSERT OR IGNORE INTO schema_version(version, applied_at_ms) VALUES(1, unixepoch('subsec') * 1000)`,
		`CREATE TABLE IF NOT EXISTS event_log(
			global_seq INTEGER PRIMARY KEY AUTOINCREMENT,
			event_id TEXT NOT NULL UNIQUE,
			aggregate_type TEXT NOT NULL,
			aggregate_id TEXT NOT NULL,
			aggregate_seq INTEGER NOT NULL,
			event_type TEXT NOT NULL,
			schema_version INTEGER NOT NULL,
			occurred_at_ms INTEGER NOT NULL,
			recorded_at_ms INTEGER NOT NULL,
			producer_actor_id TEXT,
			causation_id TEXT,
			correlation_id TEXT,
			payload_json TEXT NOT NULL,
			UNIQUE(aggregate_type, aggregate_id, aggregate_seq)
		)`,
		`CREATE INDEX IF NOT EXISTS event_log_correlation_idx ON event_log(correlation_id, global_seq)`,
		`CREATE TABLE IF NOT EXISTS task(
			task_id TEXT PRIMARY KEY,
			title TEXT NOT NULL,
			goal TEXT NOT NULL,
			state TEXT NOT NULL,
			version INTEGER NOT NULL,
			current_revision_id TEXT NOT NULL,
			assigned_agent_id TEXT,
			created_at_ms INTEGER NOT NULL,
			updated_at_ms INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS task_revision(
			revision_id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL REFERENCES task(task_id),
			revision_number INTEGER NOT NULL,
			goal TEXT NOT NULL,
			created_at_ms INTEGER NOT NULL,
			UNIQUE(task_id, revision_number)
		)`,
		`CREATE TABLE IF NOT EXISTS run(
			run_id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL REFERENCES task(task_id),
			task_revision_id TEXT NOT NULL REFERENCES task_revision(revision_id),
			runtime_id TEXT NOT NULL,
			agent_id TEXT NOT NULL,
			state TEXT NOT NULL,
			command_json TEXT NOT NULL,
			working_dir TEXT,
			lease_epoch INTEGER NOT NULL,
			created_at_ms INTEGER NOT NULL,
			started_at_ms INTEGER,
			finished_at_ms INTEGER,
			exit_code INTEGER,
			error TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS run_task_idx ON run(task_id, created_at_ms)`,
		`CREATE TABLE IF NOT EXISTS command(
			command_id TEXT PRIMARY KEY,
			run_id TEXT NOT NULL REFERENCES run(run_id),
			command_type TEXT NOT NULL,
			status TEXT NOT NULL,
			created_at_ms INTEGER NOT NULL,
			applied_at_ms INTEGER
		)`,
		`CREATE TABLE IF NOT EXISTS runtime(
			runtime_id TEXT PRIMARY KEY,
			epoch TEXT NOT NULL,
			state TEXT NOT NULL,
			hostname TEXT NOT NULL,
			os TEXT NOT NULL,
			arch TEXT NOT NULL,
			capabilities_json TEXT NOT NULL,
			connected_at_ms INTEGER NOT NULL,
			last_seen_at_ms INTEGER NOT NULL,
			disconnected_at_ms INTEGER
		)`,
		`CREATE TABLE IF NOT EXISTS outbox_message(
			message_id TEXT PRIMARY KEY,
			destination_id TEXT NOT NULL,
			method TEXT NOT NULL,
			params_json TEXT NOT NULL,
			status TEXT NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			next_attempt_at_ms INTEGER NOT NULL,
			lease_owner TEXT,
			lease_expires_at_ms INTEGER,
			created_at_ms INTEGER NOT NULL,
			delivered_at_ms INTEGER,
			last_error TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS outbox_due_idx ON outbox_message(destination_id, status, next_attempt_at_ms)`,
		`CREATE TABLE IF NOT EXISTS runtime_event_dedupe(
			runtime_id TEXT NOT NULL,
			epoch TEXT NOT NULL,
			runtime_seq INTEGER NOT NULL,
			received_at_ms INTEGER NOT NULL,
			PRIMARY KEY(runtime_id, epoch, runtime_seq)
		)`,
		`CREATE TABLE IF NOT EXISTS idempotency_key(
			scope TEXT NOT NULL,
			key TEXT NOT NULL,
			resource_id TEXT NOT NULL,
			created_at_ms INTEGER NOT NULL,
			PRIMARY KEY(scope, key)
		)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("migrate sqlite: %w", err)
		}
	}
	if err := migrateV2(db); err != nil {
		return err
	}
	if err := migrateV3(db); err != nil {
		return err
	}
	if err := migrateV4(db); err != nil {
		return err
	}
	if err := migrateV5(db); err != nil {
		return err
	}
	if err := migrateV6(db); err != nil {
		return err
	}
	if err := migrateV7(db); err != nil {
		return err
	}
	if err := migrateV8(db); err != nil {
		return err
	}
	if err := migrateV9(db); err != nil {
		return err
	}
	if err := migrateV10(db); err != nil {
		return err
	}
	if err := migrateV11(db); err != nil {
		return err
	}
	if err := migrateV12(db); err != nil {
		return err
	}
	if err := migrateV13(db); err != nil {
		return err
	}
	return migrateV14(db)
}

func migrateV2(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version >= 2 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	statements := []string{
		`CREATE TABLE session(
			session_id TEXT PRIMARY KEY,
			agent_id TEXT NOT NULL,
			adapter_id TEXT NOT NULL,
			runtime_id TEXT NOT NULL,
			state TEXT NOT NULL,
			agent_session_ref TEXT,
			memory_owner TEXT NOT NULL,
			metadata_json TEXT NOT NULL,
			last_run_id TEXT,
			created_at_ms INTEGER NOT NULL,
			updated_at_ms INTEGER NOT NULL
		)`,
		`CREATE INDEX session_runtime_idx ON session(runtime_id, state)`,
		`CREATE TABLE task_session(
			task_id TEXT NOT NULL REFERENCES task(task_id),
			session_id TEXT NOT NULL REFERENCES session(session_id),
			bound_at_ms INTEGER NOT NULL,
			unbound_at_ms INTEGER,
			PRIMARY KEY(task_id, session_id, bound_at_ms)
		)`,
		`CREATE UNIQUE INDEX task_session_active_idx ON task_session(task_id) WHERE unbound_at_ms IS NULL`,
		`ALTER TABLE run ADD COLUMN session_id TEXT REFERENCES session(session_id)`,
		`ALTER TABLE run ADD COLUMN adapter_id TEXT NOT NULL DEFAULT 'exec-agent'`,
		`UPDATE run SET adapter_id = agent_id WHERE adapter_id = 'exec-agent'`,
		`CREATE INDEX run_session_idx ON run(session_id, created_at_ms)`,
		`CREATE TABLE task_edge(
			edge_id TEXT PRIMARY KEY,
			from_task_id TEXT NOT NULL REFERENCES task(task_id),
			to_task_id TEXT NOT NULL REFERENCES task(task_id),
			edge_type TEXT NOT NULL,
			created_at_ms INTEGER NOT NULL,
			created_by_run_id TEXT REFERENCES run(run_id),
			UNIQUE(from_task_id, to_task_id, edge_type)
		)`,
		`CREATE INDEX task_edge_from_idx ON task_edge(from_task_id, edge_type)`,
		`CREATE INDEX task_edge_to_idx ON task_edge(to_task_id, edge_type)`,
		`CREATE TABLE directive(
			directive_id TEXT PRIMARY KEY,
			run_id TEXT NOT NULL REFERENCES run(run_id),
			task_id TEXT NOT NULL REFERENCES task(task_id),
			session_id TEXT REFERENCES session(session_id),
			kind TEXT NOT NULL,
			message TEXT,
			state TEXT NOT NULL,
			error TEXT,
			created_at_ms INTEGER NOT NULL,
			applied_at_ms INTEGER
		)`,
		`CREATE INDEX directive_run_idx ON directive(run_id, created_at_ms)`,
		`INSERT INTO schema_version(version, applied_at_ms) VALUES(2, unixepoch('subsec') * 1000)`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("migrate sqlite v2 (%s): %w", statement, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit sqlite v2 migration: %w", err)
	}
	return nil
}

func migrateV3(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version >= 3 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	statements := []string{
		`ALTER TABLE session ADD COLUMN model_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE run ADD COLUMN model_id TEXT NOT NULL DEFAULT ''`,
		`INSERT INTO schema_version(version, applied_at_ms) VALUES(3, unixepoch('subsec') * 1000)`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("migrate sqlite v3 (%s): %w", statement, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit sqlite v3 migration: %w", err)
	}
	return nil
}
