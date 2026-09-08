package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"work-assistant/internal/id"
	"work-assistant/internal/model"
	"work-assistant/internal/workflow"
)

func migrateV5(db *sql.DB) error {
	var exists int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_version WHERE version = 5`).Scan(&exists); err != nil || exists > 0 {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`CREATE TABLE project(project_id TEXT PRIMARY KEY, data_json TEXT NOT NULL)`,
		`CREATE TABLE task_workflow(task_id TEXT PRIMARY KEY REFERENCES task(task_id), agent_id TEXT NOT NULL DEFAULT '', project_json TEXT NOT NULL, paused INTEGER NOT NULL DEFAULT 0, scheduler_error TEXT NOT NULL DEFAULT '', retry_at_ms INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE task_message(seq INTEGER PRIMARY KEY AUTOINCREMENT, message_id TEXT NOT NULL UNIQUE, task_id TEXT NOT NULL REFERENCES task(task_id), speaker TEXT NOT NULL, content TEXT NOT NULL, run_id TEXT NOT NULL DEFAULT '', delivery TEXT NOT NULL, created_at_ms INTEGER NOT NULL)`,
		`CREATE INDEX task_message_pending_idx ON task_message(task_id, delivery, seq)`,
		`CREATE TABLE artifact(artifact_id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES task(task_id), run_id TEXT NOT NULL REFERENCES run(run_id), name TEXT NOT NULL, version INTEGER NOT NULL, data_json TEXT NOT NULL, UNIQUE(task_id,name,version))`,
		`CREATE TABLE review(review_id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES task(task_id), run_id TEXT NOT NULL UNIQUE REFERENCES run(run_id), state TEXT NOT NULL, data_json TEXT NOT NULL)`,
		`CREATE TABLE role_revision(role_id TEXT NOT NULL REFERENCES role(role_id), version INTEGER NOT NULL, data_json TEXT NOT NULL, PRIMARY KEY(role_id,version))`,
		`INSERT INTO role_revision SELECT role_id, json_extract(data_json,'$.version'), data_json FROM role`,
		`INSERT INTO schema_version(version,applied_at_ms) VALUES(5,unixepoch('subsec')*1000)`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("migrate v5: %w", err)
		}
	}
	return tx.Commit()
}

func (s *Store) CreateProject(ctx context.Context, name, content string) (model.Project, error) {
	p := model.Project{ID: id.New("project"), Name: strings.TrimSpace(name), Context: content, Version: 1}
	if p.Name == "" || len(p.Name) > 200 || len(content) > 64000 {
		return p, fmt.Errorf("%w: project name required (max 200 bytes), context max 64 KB", model.ErrValidation)
	}
	data, _ := json.Marshal(p)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO project VALUES(?,?)`, p.ID, data)
	return p, err
}
func (s *Store) ListProjects(ctx context.Context) ([]model.Project, error) {
	return listJSONRows[model.Project](ctx, s.db, `SELECT data_json FROM project ORDER BY rowid DESC`)
}

type CreateWorkRequest struct {
	Source          string                 `json:"-"`
	DeferAssignment bool                   `json:"defer_assignment"`
	Title           string                 `json:"title"`
	Goal            string                 `json:"goal"`
	Requirements    model.TaskRequirements `json:"requirements"`
	AgentID         string                 `json:"agent_id"`
	ProjectID       string                 `json:"project_id"`
	Key             string                 `json:"idempotency_key"`
}

// Creation of the task, source event, first message and scheduling intent is
// atomic. Legacy explicitly-driven tasks and role-builder tasks are untouched.
func (s *Store) CreateWork(ctx context.Context, req CreateWorkRequest) (model.Task, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Task{}, err
	}
	defer tx.Rollback()
	task, err := createWorkTx(ctx, tx, req)
	if err != nil {
		return task, err
	}
	return task, tx.Commit()
}

func createWorkTx(ctx context.Context, tx *sql.Tx, req CreateWorkRequest) (model.Task, error) {
	if req.DeferAssignment && req.AgentID != "" {
		return model.Task{}, fmt.Errorf("%w: choose either deferred assignment or a specific member", model.ErrValidation)
	}
	if len(req.Title) > 400 || len(req.Goal) > 32000 || strings.TrimSpace(req.Title) == "" || strings.TrimSpace(req.Goal) == "" {
		return model.Task{}, fmt.Errorf("%w: title and goal required, maximum 400/32000 bytes", model.ErrValidation)
	}
	key := ""
	if req.Key != "" {
		key = "work:" + req.Key
	}
	task, replay, err := createTaskTx(ctx, tx, key, req.Title, req.Goal, req.Requirements)
	if err != nil {
		return task, err
	}
	if replay {
		return task, nil
	}
	var project model.Project
	if req.ProjectID != "" {
		project, err = readJSONRow[model.Project](tx.QueryRowContext(ctx, `SELECT data_json FROM project WHERE project_id=?`, req.ProjectID))
		if err != nil {
			return task, err
		}
	}
	if req.AgentID != "" {
		if _, err = validateWorkMemberTx(ctx, tx, task, req.AgentID); err != nil {
			return task, err
		}
	}
	data, _ := json.Marshal(project)
	// Also set the existing scheduling hold. Older binaries must not start a
	// deferred task if the application is rolled back after it was saved.
	if _, err = tx.ExecContext(ctx, `INSERT INTO task_workflow(task_id,agent_id,project_json,paused) VALUES(?,?,?,?)`, task.ID, req.AgentID, data, req.DeferAssignment); err != nil {
		return task, err
	}
	if _, err = insertMessageTx(ctx, tx, task.ID, "user", req.Goal, "", "PENDING"); err != nil {
		return task, err
	}
	source, eventType := req.Source, "ExternalTaskSubmitted"
	if source == "" {
		source, eventType = "manual", "ManualTaskSubmitted"
	}
	if _, err = appendEventTx(ctx, tx, "task", task.ID, eventType, "", task.ID, map[string]any{"source": source, "project": project, "preferred_agent_id": req.AgentID, "defer_assignment": req.DeferAssignment}); err != nil {
		return task, err
	}
	initialState := model.TaskStateQueued
	if req.DeferAssignment {
		initialState = model.TaskStateNew
	}
	if err = setWorkStateTx(ctx, tx, task.ID, initialState); err != nil {
		return task, err
	}
	task, err = getTaskTx(ctx, tx, task.ID)
	if err != nil {
		return task, err
	}
	return task, nil
}

func setWorkStateTx(ctx context.Context, tx *sql.Tx, taskID, state string) error {
	_, err := tx.ExecContext(ctx, `UPDATE task SET state=?,version=version+1,updated_at_ms=? WHERE task_id=?`, state, time.Now().UnixMilli(), taskID)
	return err
}

const workSelect = `SELECT task_id,agent_id,project_json,paused,scheduler_error,(SELECT version FROM task WHERE task.task_id=task_workflow.task_id) FROM task_workflow`

func scanWork(row rowScanner) (model.WorkConfig, error) {
	var w model.WorkConfig
	var raw []byte
	if err := row.Scan(&w.TaskID, &w.AgentID, &raw, &w.Paused, &w.SchedulerError, &w.TaskVersion); err != nil {
		return w, err
	}
	err := json.Unmarshal(raw, &w.Project)
	return w, err
}
func (s *Store) GetWorkConfig(ctx context.Context, taskID string) (model.WorkConfig, error) {
	return scanWork(s.db.QueryRowContext(ctx, workSelect+` WHERE task_id=?`, taskID))
}

func insertMessageTx(ctx context.Context, tx *sql.Tx, taskID, speaker, content, runID, delivery string) (model.TaskMessage, error) {
	m := model.TaskMessage{ID: id.New("message"), TaskID: taskID, Speaker: speaker, Content: content, RunID: runID, Delivery: delivery, CreatedAtMS: time.Now().UnixMilli()}
	r, err := tx.ExecContext(ctx, `INSERT INTO task_message(message_id,task_id,speaker,content,run_id,delivery,created_at_ms) VALUES(?,?,?,?,?,?,?)`, m.ID, taskID, speaker, content, runID, delivery, m.CreatedAtMS)
	if err != nil {
		return m, err
	}
	m.Seq, err = r.LastInsertId()
	if err != nil {
		return m, err
	}
	_, err = appendEventTx(ctx, tx, "task", taskID, "TaskMessageCreated", m.ID, taskID, m)
	return m, err
}

const messageSelect = `SELECT seq,message_id,task_id,speaker,content,run_id,delivery,created_at_ms FROM task_message`

func scanMessage(row rowScanner) (model.TaskMessage, error) {
	var m model.TaskMessage
	err := row.Scan(&m.Seq, &m.ID, &m.TaskID, &m.Speaker, &m.Content, &m.RunID, &m.Delivery, &m.CreatedAtMS)
	return m, err
}

// Messages remain PENDING during a running turn. The next Run consumes them in
// the same transaction as its durable outbox; there is no late-directive race.
func (s *Store) MessageWork(ctx context.Context, taskID, content, key string, interrupt bool) (model.TaskMessage, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.TaskMessage{}, err
	}
	defer tx.Rollback()
	m, err := messageWorkTx(ctx, tx, taskID, content, key, interrupt)
	if err != nil {
		return m, err
	}
	return m, tx.Commit()
}

func messageWorkTx(ctx context.Context, tx *sql.Tx, taskID, content, key string, interrupt bool) (model.TaskMessage, error) {
	return messageWorkFromTx(ctx, tx, taskID, content, key, interrupt, "user")
}

func messageWorkFromTx(ctx context.Context, tx *sql.Tx, taskID, content, key string, interrupt bool, speaker string) (model.TaskMessage, error) {
	content = strings.TrimSpace(content)
	if content == "" || len(content) > 32000 {
		return model.TaskMessage{}, fmt.Errorf("%w: message required, max 32 KB", model.ErrValidation)
	}
	var err error
	if key != "" {
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT resource_id FROM idempotency_key WHERE scope=? AND key=?`, "work.message:"+taskID, key).Scan(&existing)
		if err == nil {
			return scanMessage(tx.QueryRowContext(ctx, messageSelect+` WHERE message_id=?`, existing))
		}
		if err != sql.ErrNoRows {
			return model.TaskMessage{}, err
		}
	}
	var w model.WorkConfig
	w, err = scanWork(tx.QueryRowContext(ctx, workSelect+` WHERE task_id=?`, taskID))
	if err != nil {
		return model.TaskMessage{}, err
	}
	task, err := getTaskTx(ctx, tx, taskID)
	if err != nil {
		return model.TaskMessage{}, err
	}
	if task.State == model.TaskStateCompleted {
		return model.TaskMessage{}, fmt.Errorf("%w: completed task is read-only; create a new task", model.ErrConflict)
	}
	var pendingBytes int
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(length(CAST(content AS BLOB))),0) FROM task_message WHERE task_id=? AND delivery='PENDING'`, taskID).Scan(&pendingBytes); err != nil {
		return model.TaskMessage{}, err
	}
	if pendingBytes+len(content) > 96000 {
		return model.TaskMessage{}, fmt.Errorf("%w: pending messages exceed 96 KB; wait for the agent", model.ErrConflict)
	}
	d, devErr := developmentTx(ctx, tx, taskID)
	if speaker == "user" {
		if err = invalidatePermissionsTx(ctx, tx, taskID); err != nil {
			return model.TaskMessage{}, err
		}
		if err = pauseEnvironmentChildrenTx(ctx, tx, taskID); err != nil {
			return model.TaskMessage{}, err
		}
	}
	if devErr != nil && devErr != sql.ErrNoRows {
		return model.TaskMessage{}, devErr
	}
	if devErr == nil && (d.Phase == "AGENT_REVIEW" || d.Phase == "HUMAN_REVIEW" || (d.Phase == "IMPLEMENTING" && speaker == "user")) {
		// Explicit task guidance changes the plan; review-chat remains a separate,
		// read-only conversation and never enters this path.
		d.Phase = "PLANNING"
		d.Version++
		d.ApprovedReviewID = ""
		if err = saveDevelopmentTx(ctx, tx, d, "PlanInvalidatedByMessage"); err != nil {
			return model.TaskMessage{}, err
		}
		interrupt = true
	}
	m, err := insertMessageTx(ctx, tx, taskID, speaker, content, "", "PENDING")
	if err != nil {
		return m, err
	}
	if err = supersedeReviewsTx(ctx, tx, taskID); err != nil {
		return m, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE task_workflow SET paused=?,scheduler_error='',retry_at_ms=0 WHERE task_id=?`, task.State == model.TaskStateNew, taskID); err != nil {
		return m, err
	}
	var active string
	err = tx.QueryRowContext(ctx, `SELECT run_id FROM run WHERE task_id=? AND state IN ('QUEUED','RUNNING') LIMIT 1`, taskID).Scan(&active)
	if err != nil && err != sql.ErrNoRows {
		return m, err
	}
	state := model.TaskStateQueued
	if task.State == model.TaskStateNew {
		// Supplementing a saved task is not permission to assign it yet.
		state = model.TaskStateNew
	}
	if active != "" {
		state = model.TaskStateInProgress
		if interrupt {
			if err = interruptWorkTx(ctx, tx, taskID, active); err != nil {
				return m, err
			}
		}
	}
	if err = setWorkStateTx(ctx, tx, taskID, state); err != nil {
		return m, err
	}
	if key != "" {
		if _, err = tx.ExecContext(ctx, `INSERT INTO idempotency_key VALUES(?,?,?,?)`, "work.message:"+w.TaskID, key, m.ID, time.Now().UnixMilli()); err != nil {
			return m, err
		}
	}
	return m, nil
}

func interruptWorkTx(ctx context.Context, tx *sql.Tx, taskID, runID string) error {
	var runtimeID, sessionID string
	if err := tx.QueryRowContext(ctx, `SELECT runtime_id,session_id FROM run WHERE run_id=?`, runID).Scan(&runtimeID, &sessionID); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	d := model.Directive{ID: id.New("directive"), RunID: runID, TaskID: taskID, SessionID: sessionID, Kind: model.DirectiveKindInterrupt, State: model.DirectiveStateQueued, CreatedAtMS: now}
	if _, err := tx.ExecContext(ctx, `INSERT INTO directive(directive_id,run_id,task_id,session_id,kind,state,created_at_ms) VALUES(?,?,?,?,?,?,?)`, d.ID, runID, taskID, sessionID, d.Kind, d.State, now); err != nil {
		return err
	}
	raw, _ := json.Marshal(d)
	if _, err := tx.ExecContext(ctx, `INSERT INTO outbox_message(message_id,destination_id,method,params_json,status,attempts,next_attempt_at_ms,created_at_ms) VALUES(?,?,'run.directive',?,'PENDING',0,?,?)`, d.ID, runtimeID, raw, now, now); err != nil {
		return err
	}
	_, err := appendEventTx(ctx, tx, "task", taskID, "TaskInterruptRequested", d.ID, taskID, d)
	return err
}

func (s *Store) PauseWork(ctx context.Context, taskID string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := pauseWorkTx(ctx, tx, taskID); err != nil {
		return err
	}
	return tx.Commit()
}

func pauseWorkTx(ctx context.Context, tx *sql.Tx, taskID string) error {
	if err := invalidatePermissionsTx(ctx, tx, taskID); err != nil {
		return err
	}
	if err := pauseEnvironmentChildrenTx(ctx, tx, taskID); err != nil {
		return err
	}
	w, err := scanWork(tx.QueryRowContext(ctx, workSelect+` WHERE task_id=?`, taskID))
	if err != nil {
		return err
	}
	if w.Paused {
		return nil
	}
	task, err := getTaskTx(ctx, tx, taskID)
	if err != nil {
		return err
	}
	if task.State == model.TaskStateCompleted {
		return fmt.Errorf("%w: completed task cannot be paused", model.ErrConflict)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE task_workflow SET paused=1 WHERE task_id=?`, taskID); err != nil {
		return err
	}
	var runID string
	err = tx.QueryRowContext(ctx, `SELECT run_id FROM run WHERE task_id=? AND state IN ('QUEUED','RUNNING') LIMIT 1`, taskID).Scan(&runID)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if runID != "" {
		if err = interruptWorkTx(ctx, tx, taskID, runID); err != nil {
			return err
		}
	}
	if err = supersedeReviewsTx(ctx, tx, taskID); err != nil {
		return err
	}
	if _, err = insertMessageTx(ctx, tx, taskID, "system", "已暂停调度并请求停止当前运行。恢复时沿用原 Agent / Session；已发生的外部操作不会回滚。", "", "RECORDED"); err != nil {
		return err
	}
	pausedState := model.TaskStatePaused
	if task.State == model.TaskStateNew {
		pausedState = model.TaskStateNew
	}
	if err = setWorkStateTx(ctx, tx, taskID, pausedState); err != nil {
		return err
	}
	return nil
}

// PendingWork is a durable queue view, not an in-memory goroutine per task.
func (s *Store) PendingWork(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT w.task_id FROM task_workflow w JOIN task t ON t.task_id=w.task_id JOIN task_message m ON m.task_id=w.task_id AND m.delivery='PENDING' WHERE w.paused=0 AND w.retry_at_ms<=? AND t.state NOT IN ('NEW','COMPLETED','WAITING_AUTHORIZATION') AND NOT EXISTS(SELECT 1 FROM run r WHERE r.task_id=w.task_id AND r.state IN ('QUEUED','RUNNING')) GROUP BY w.task_id ORDER BY MIN(m.seq) LIMIT 50`, time.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var v string
		if err = rows.Scan(&v); err != nil {
			return nil, err
		}
		ids = append(ids, v)
	}
	return ids, rows.Err()
}
func (s *Store) DeferWork(ctx context.Context, taskID string, cause error) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE task_workflow SET scheduler_error=?,retry_at_ms=? WHERE task_id=? AND NOT EXISTS(SELECT 1 FROM task WHERE task_id=? AND state IN ('WAITING_AUTHORIZATION','WAITING_ENVIRONMENT'))`, cause.Error(), time.Now().Add(2*time.Second).UnixMilli(), taskID, taskID)
	return err
}

func (s *Store) StartWorkRun(ctx context.Context, req CreateRunRequest, contract workflow.Contract) (model.Run, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Run{}, err
	}
	defer tx.Rollback()
	w, err := scanWork(tx.QueryRowContext(ctx, workSelect+` WHERE task_id=?`, req.TaskID))
	if err != nil {
		return model.Run{}, err
	}
	if w.Paused {
		return model.Run{}, fmt.Errorf("%w: task paused", model.ErrConflict)
	}
	task, err := getTaskTx(ctx, tx, req.TaskID)
	if err != nil {
		return model.Run{}, err
	}
	if task.State == model.TaskStateCompleted {
		return model.Run{}, fmt.Errorf("%w: task completed", model.ErrConflict)
	}
	if task.State == model.TaskStateNew {
		return model.Run{}, fmt.Errorf("%w: task is waiting for assignment", model.ErrConflict)
	}
	if req.ExpectedTaskVersion != 0 && req.ExpectedTaskVersion != task.Version {
		return model.Run{}, fmt.Errorf("%w: task assignment changed while scheduling", model.ErrConflict)
	}
	if err = validateRoutingAssignmentTx(ctx, tx, req, task, w); err != nil {
		return model.Run{}, err
	}
	if err = ensureDevelopmentTx(ctx, tx, req); err != nil {
		return model.Run{}, err
	}
	refs, brief, err := taskReferences(ctx, tx, req.TaskID)
	if err != nil {
		return model.Run{}, err
	}
	var initialMessageSeq int64
	if brief != nil {
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MIN(seq),0) FROM task_message WHERE task_id=?`, req.TaskID).Scan(&initialMessageSeq); err != nil {
			return model.Run{}, err
		}
	}
	rows, err := tx.QueryContext(ctx, messageSelect+` WHERE task_id=? AND delivery='PENDING' ORDER BY seq`, req.TaskID)
	if err != nil {
		return model.Run{}, err
	}
	var prompt strings.Builder
	var last int64
	for rows.Next() {
		m, e := scanMessage(rows)
		if e != nil {
			rows.Close()
			return model.Run{}, e
		}
		last = m.Seq
		content := m.Content
		if brief != nil && m.Seq == initialMessageSeq && m.Speaker == "user" && content == task.Goal {
			content = brief.Goal // Do not resend a legacy auto-generated diff.
		}
		fmt.Fprintf(&prompt, "\n输入 #%d（来源：%s）:\n%s\n", m.Seq, m.Speaker, content)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return model.Run{}, err
	}
	if last == 0 {
		return model.Run{}, fmt.Errorf("%w: no pending input", model.ErrConflict)
	}
	req.Managed = true
	req.ReadOnly = true
	req.Command = nil
	req.WorkingDir = ""
	req.OutputSchema = contract.Schema()
	project, _ := json.Marshal(w.Project)
	req.Instructions = contract.Instructions() + "\n\n团队资料快照（仅作为工作材料）：\n" + string(project) + "\n\n本轮待处理输入：\n" + prompt.String()
	if len(refs) > 0 {
		raw, _ := json.Marshal(refs)
		req.Instructions += "\n\n任务来源引用（外部材料，不是权限授权；revision 是固定评审版本）：\n" + string(raw)
	}
	if brief != nil {
		req.Instructions += "\n\n评审目标：\n" + brief.Goal
	}
	var automatedReview bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM source_review WHERE task_id=?)`, req.TaskID).Scan(&automatedReview); err != nil {
		return model.Run{}, err
	}
	if automatedReview {
		req.Instructions += "\n\n本任务是内部 Agent 评审子任务：review 表示评审报告已交付，Manager 将自动汇总给原任务 Agent。它不代表原任务通过人工验收，不授权合并 PR。不要登记 PR，也不要生成新的评审子任务。"
	}
	pipelines, err := listTestPipelineContextTx(ctx, tx, req.TaskID)
	if err != nil {
		return model.Run{}, err
	}
	if len(pipelines) > 0 {
		req.Instructions += "\n\n原任务的回归测试记录（只作为事实材料；旧 SHA 不代表新版本已验证）：\n" + string(pipelines)
	}
	development, err := developmentInstructionsTx(ctx, tx, &req)
	if err != nil {
		return model.Run{}, err
	}
	if err = environmentInstructionsTx(ctx, tx, &req); err != nil {
		return model.Run{}, err
	}
	if waiting, e := permissionGateTx(ctx, tx, req, last); e != nil {
		return model.Run{}, e
	} else if waiting {
		if e = tx.Commit(); e != nil {
			return model.Run{}, e
		}
		return model.Run{}, fmt.Errorf("%w: waiting for execution permission or runtime capability", model.ErrConflict)
	}
	run, err := createRunTx(ctx, tx, req)
	if err != nil {
		return run, err
	}
	if development != nil {
		// The plan approval remains independent of execution policy.
		if _, err = tx.ExecContext(ctx, `INSERT INTO development_run VALUES(?,?,?,?)`, run.ID, development.TaskID, development.Version, development.Phase); err != nil {
			return run, err
		}
	}
	if req.Environment != nil {
		if _, err = tx.ExecContext(ctx, `UPDATE environment_job SET state='RUNNING' WHERE task_id=?`, req.TaskID); err != nil {
			return model.Run{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE task_message SET delivery='SENT',run_id=? WHERE task_id=? AND delivery='PENDING' AND seq<=?`, run.ID, req.TaskID, last); err != nil {
		return run, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE task_workflow SET scheduler_error='',retry_at_ms=0 WHERE task_id=?`, req.TaskID); err != nil {
		return run, err
	}
	return run, tx.Commit()
}

func applyWorkResultTx(ctx context.Context, tx *sql.Tx, e model.RuntimeEvent, now int64) error {
	if handled, err := applyReviewTurnResultTx(ctx, tx, e); handled || err != nil {
		return err
	}
	var taskID string
	if err := tx.QueryRowContext(ctx, `SELECT task_id FROM run WHERE run_id=?`, e.RunID).Scan(&taskID); err != nil {
		return err
	}
	w, err := scanWork(tx.QueryRowContext(ctx, workSelect+` WHERE task_id=?`, taskID))
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	var pending int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_message WHERE task_id=? AND delivery='PENDING'`, taskID).Scan(&pending); err != nil {
		return err
	}
	state := model.TaskStateBlocked
	if e.Type != "run.completed" {
		e.TaskID = taskID
		if handled, err := finishEnvironmentTx(ctx, tx, e, workflow.Result{}); handled || err != nil {
			return err
		}
		if e.Type == "run.failed" && !w.Paused && pending == 0 {
			if handled, err := recoverDisconnectedDevelopmentTx(ctx, tx, taskID, e.RunID, e.Error); handled || err != nil {
				return err
			}
		}
		if e.Type == "run.interrupted" {
			state = model.TaskStatePaused
			if !w.Paused && pending > 0 {
				state = model.TaskStateQueued
			}
		}
		if state != model.TaskStateQueued {
			if _, err = tx.ExecContext(ctx, `UPDATE task_workflow SET paused=1 WHERE task_id=?`, taskID); err != nil {
				return err
			}
		}
		if _, err = insertMessageTx(ctx, tx, taskID, "system", "本轮未完成："+e.Type+"。"+e.Error, e.RunID, "RECORDED"); err != nil {
			return err
		}
		return setWorkStateTx(ctx, tx, taskID, state)
	}
	result, parseErr := workflow.Parse(e.Output)
	if parseErr != nil {
		e.TaskID = taskID
		e.Error = parseErr.Error()
		if handled, err := finishEnvironmentTx(ctx, tx, e, workflow.Result{}); handled || err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE task_workflow SET paused=1,scheduler_error=? WHERE task_id=?`, parseErr.Error(), taskID); err != nil {
			return err
		}
		if _, err = insertMessageTx(ctx, tx, taskID, "system", "Agent 返回的业务结果格式不正确，未创建验收单。可以查看运行原始输出后重试："+parseErr.Error(), e.RunID, "RECORDED"); err != nil {
			return err
		}
		return setWorkStateTx(ctx, tx, taskID, model.TaskStateBlocked)
	}
	if _, err = insertMessageTx(ctx, tx, taskID, "assistant", result.Message, e.RunID, "RECORDED"); err != nil {
		return err
	}
	artifactIDs := []string{}
	for _, file := range result.Artifacts {
		var version int64
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM artifact WHERE task_id=? AND name=?`, taskID, file.Name).Scan(&version); err != nil {
			return err
		}
		hash := sha256.Sum256([]byte(file.Content))
		a := model.Artifact{ID: id.New("artifact"), TaskID: taskID, RunID: e.RunID, Name: file.Name, Content: file.Content, Version: version, SHA256: hex.EncodeToString(hash[:]), CreatedAtMS: now}
		data, _ := json.Marshal(a)
		if _, err = tx.ExecContext(ctx, `INSERT INTO artifact VALUES(?,?,?,?,?,?)`, a.ID, taskID, e.RunID, a.Name, version, data); err != nil {
			return err
		}
		artifactIDs = append(artifactIDs, a.ID)
	}
	registrationFailed := false
	e.TaskID = taskID
	if handled, err := finishEnvironmentTx(ctx, tx, e, result); handled || err != nil {
		return err
	}
	if result.RecoveryRequest != nil {
		if w.Paused || pending > 0 {
			if w.Paused {
				return setWorkStateTx(ctx, tx, taskID, model.TaskStatePaused)
			}
			return setWorkStateTx(ctx, tx, taskID, model.TaskStateQueued)
		}
		return requestRecoveryTx(ctx, tx, taskID, e.RunID, *result.RecoveryRequest)
	}
	if result.EnvironmentRequest != nil {
		if w.Paused || pending > 0 {
			state := model.TaskStateQueued
			if w.Paused {
				state = model.TaskStatePaused
			}
			return setWorkStateTx(ctx, tx, taskID, state)
		}
		if err := requestEnvironmentTx(ctx, tx, taskID, e.RunID, *result.EnvironmentRequest); err != nil {
			if !errors.Is(err, model.ErrConflict) && !errors.Is(err, model.ErrValidation) && err != sql.ErrNoRows {
				return err
			}
			return blockDevelopmentTx(ctx, tx, taskID, "Windows 执行申请未接受："+err.Error())
		}
		return nil
	}
	var planRun bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM development_run WHERE run_id=? AND phase!='IMPLEMENTING')`, e.RunID).Scan(&planRun); err != nil {
		return err
	}
	if planRun && (w.Paused || pending > 0) {
		state := model.TaskStateQueued
		if w.Paused {
			state = model.TaskStatePaused
		}
		return setWorkStateTx(ctx, tx, taskID, state)
	}
	if !w.Paused && pending == 0 {
		if handled, err := applyDevelopmentResultTx(ctx, tx, taskID, e, result, artifactIDs, now); handled || err != nil {
			return err
		}
	}
	if result.PublishRequest != nil {
		return blockDevelopmentTx(ctx, tx, taskID, "PR 发布申请未由受控开发执行器完成，不可作为已交付。")
	}
	// Persist actions before internally completing a QA child, so its test
	// requirement cannot disappear through the automatic fan-in early return.
	if !w.Paused && pending == 0 {
		for _, request := range result.TestRequests {
			if actionErr := requestTestPipelineTx(ctx, tx, taskID, e.RunID, request); actionErr != nil {
				if !errors.Is(actionErr, model.ErrValidation) && !errors.Is(actionErr, model.ErrConflict) {
					return actionErr
				}
				registrationFailed = true
				if _, err = insertMessageTx(ctx, tx, taskID, "system", "测试申请未受理，报告已保留，请核实后继续："+actionErr.Error(), e.RunID, "RECORDED"); err != nil {
					return err
				}
			}
		}
	}
	if !w.Paused && pending == 0 && !registrationFailed {
		if handled, err := applySourceReviewResultTx(ctx, tx, taskID, e, result, now); handled || err != nil {
			return err
		}
	}
	for _, pr := range result.PullRequests {
		if _, registrationErr := registerPRTx(ctx, tx, taskID, pr.SourceID, pr.URL); registrationErr != nil {
			registrationFailed = true
			if _, err = insertMessageTx(ctx, tx, taskID, "system", "PR 登记失败，交付结果已保留，请检查任务源配置后重新登记："+registrationErr.Error(), e.RunID, "RECORDED"); err != nil {
				return err
			}
		}
	}
	switch result.Outcome {
	case "review":
		state = model.TaskStateReview
	case "needs_input":
		state = model.TaskStateInput
	case "blocked":
		state = model.TaskStateBlocked
	}
	if registrationFailed {
		state = model.TaskStateBlocked
	}
	if w.Paused {
		state = model.TaskStatePaused
	} else if pending > 0 {
		state = model.TaskStateQueued
	}
	if state == model.TaskStateReview {
		if result.ReviewDecision == "" {
			targets, listErr := listJSONRows[model.SourceTarget](ctx, tx, `SELECT data_json FROM source_target WHERE task_id=?`, taskID)
			if listErr != nil {
				return listErr
			}
			for _, target := range targets {
				if err = continuePRReviewsTx(ctx, tx, target, "author-result:"+e.RunID, result.Message); err != nil {
					return err
				}
			}
		}
		r := model.Review{ID: id.New("review"), TaskID: taskID, RunID: e.RunID, State: "PENDING", ArtifactIDs: artifactIDs, CreatedAtMS: now}
		data, _ := json.Marshal(r)
		if _, err = tx.ExecContext(ctx, `INSERT INTO review VALUES(?,?,?,?,?)`, r.ID, taskID, e.RunID, r.State, data); err != nil {
			return err
		}
		if _, err = appendEventTx(ctx, tx, "task", taskID, "ReviewRequested", e.RunID, taskID, r); err != nil {
			return err
		}
	}
	return setWorkStateTx(ctx, tx, taskID, state)
}

func supersedeReviewsTx(ctx context.Context, tx *sql.Tx, taskID string) error {
	_, err := tx.ExecContext(ctx, `UPDATE review SET state='SUPERSEDED',data_json=json_set(data_json,'$.state','SUPERSEDED') WHERE task_id=? AND state='PENDING'`, taskID)
	if err != nil {
		return err
	}
	return cancelSupersededReviewTurnsTx(ctx, tx, taskID)
}

func (s *Store) DecideReview(ctx context.Context, taskID, reviewID, decision, comment string, discussionVersion ...int64) (model.Review, error) {
	if (decision != "APPROVED" && decision != "PLAN_APPROVED" && decision != "CHANGES_REQUESTED") || len(comment) > 32000 || (decision == "CHANGES_REQUESTED" && strings.TrimSpace(comment) == "") {
		return model.Review{}, fmt.Errorf("%w: valid decision and change request comment required", model.ErrValidation)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Review{}, err
	}
	defer tx.Rollback()
	r, err := readJSONRow[model.Review](tx.QueryRowContext(ctx, `SELECT data_json FROM review WHERE task_id=? AND review_id=?`, taskID, reviewID))
	if err != nil {
		return r, err
	}
	if r.State == decision && r.Comment == comment {
		return r, nil
	}
	if r.State != "PENDING" {
		return r, fmt.Errorf("%w: review is no longer current", model.ErrConflict)
	}
	expected := int64(0)
	if len(discussionVersion) > 0 {
		expected = discussionVersion[0]
	}
	if expected != r.DiscussionVersion {
		return r, fmt.Errorf("%w: 验收聊天有新消息或状态变化，请先查看最新沟通再决定", model.ErrConflict)
	}
	task, err := getTaskTx(ctx, tx, taskID)
	if err != nil {
		return r, err
	}
	var active int
	if err = tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM run WHERE task_id=? AND state IN ('QUEUED','RUNNING'))+(SELECT COUNT(*) FROM task_message WHERE task_id=? AND delivery='PENDING')+(SELECT COUNT(*) FROM review_turn WHERE task_id=? AND state IN ('QUEUED','RUNNING'))`, taskID, taskID, taskID).Scan(&active); err != nil {
		return r, err
	}
	if task.State != model.TaskStateReview || active > 0 {
		return r, fmt.Errorf("%w: task has changed since this submission", model.ErrConflict)
	}
	if r.Kind == "plan" {
		d, err := developmentTx(ctx, tx, taskID)
		if err != nil {
			return r, err
		}
		if err = decidePlanTx(ctx, tx, d, &r, decision, comment); err != nil {
			return r, err
		}
		return r, tx.Commit()
	}
	if decision == "PLAN_APPROVED" {
		return r, fmt.Errorf("%w: not a plan review", model.ErrValidation)
	}
	if decision == "APPROVED" {
		if d, e := developmentTx(ctx, tx, taskID); e == nil {
			if d.Phase != "IMPLEMENTING" || d.ApprovedReviewID == "" {
				return r, fmt.Errorf("%w: development has not been approved", model.ErrConflict)
			}
			var total, unmerged int
			if e = tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(NOT EXISTS(SELECT 1 FROM source_event e WHERE e.target_id=t.target_id AND e.state='RECORDED' AND json_extract(e.data_json,'$.kind')='github.merged' AND json_extract(e.data_json,'$.head_sha')=json_extract(t.data_json,'$.head_sha'))),0) FROM source_target t WHERE t.task_id=? AND json_extract(t.data_json,'$.test_request_id') IS NULL`, taskID).Scan(&total, &unmerged); e != nil {
				return r, e
			}
			if total == 0 || unmerged != 0 {
				return r, fmt.Errorf("%w: 开发任务须等已登记 PR 合并并被轮询确认后才能完成；系统不会替你自动合并", model.ErrConflict)
			}
		} else if e != sql.ErrNoRows {
			return r, e
		}
		if err = guardTestPipelinesTx(ctx, tx, taskID); err != nil {
			return r, err
		}
		if err = guardReviewPublicationsTx(ctx, tx, taskID); err != nil {
			return r, err
		}
		var sourcePending int
		if err = tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM source_review r JOIN source_target t USING(target_id) WHERE t.task_id=? AND r.state!='SUPERSEDED' AND (r.state!='COMPLETED' OR r.feedback_sent=0))+(SELECT COUNT(*) FROM source_event WHERE state='PENDING' AND json_extract(data_json,'$.task_id')=?)+(SELECT COUNT(*) FROM source_target WHERE task_id=? AND enabled=1 AND json_extract(data_json,'$.last_success_ms')=0)`, taskID, taskID, taskID).Scan(&sourcePending); err != nil {
			return r, err
		}
		if sourcePending > 0 {
			return r, fmt.Errorf("%w: PR 仍有待处理事件或 Agent 评审，请查看最新结果后验收", model.ErrConflict)
		}
		var children int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_edge e JOIN task t ON t.task_id=e.to_task_id WHERE e.from_task_id=? AND e.edge_type!='REVIEWS' AND t.state!='COMPLETED'`, taskID).Scan(&children); err != nil {
			return r, err
		}
		if children > 0 {
			return r, fmt.Errorf("%w: unfinished child tasks", model.ErrConflict)
		}
	}
	r.State, r.Comment, r.DecidedAtMS = decision, comment, time.Now().UnixMilli()
	data, _ := json.Marshal(r)
	if _, err = tx.ExecContext(ctx, `UPDATE review SET state=?,data_json=? WHERE review_id=?`, decision, data, reviewID); err != nil {
		return r, err
	}
	state := model.TaskStateCompleted
	if decision == "CHANGES_REQUESTED" {
		state = model.TaskStateQueued
		_, err = insertMessageTx(ctx, tx, taskID, "user", "人工验收要求修改：\n"+comment, "", "PENDING")
	} else {
		_, err = insertMessageTx(ctx, tx, taskID, "system", "人工已验收通过。"+comment, r.RunID, "RECORDED")
	}
	if err != nil {
		return r, err
	}
	if _, err = appendEventTx(ctx, tx, "task", taskID, "ReviewDecided", reviewID, taskID, r); err != nil {
		return r, err
	}
	if err = setWorkStateTx(ctx, tx, taskID, state); err != nil {
		return r, err
	}
	if state == model.TaskStateCompleted {
		if _, err = createTaskSummaryTx(ctx, tx, taskID, "review", reviewID, r.DecidedAtMS); err != nil {
			return r, err
		}
		if err = updateParentStatesTx(ctx, tx, taskID, r.DecidedAtMS); err != nil {
			return r, err
		}
	}
	return r, tx.Commit()
}

func (s *Store) GetWorkDetail(ctx context.Context, taskID string) (model.WorkDetail, error) {
	w := model.WorkDetail{Messages: []model.TaskMessage{}, Artifacts: []model.Artifact{}, Reviews: []model.Review{}}
	var err error
	d, devErr := readJSONRow[model.Development](s.db.QueryRowContext(ctx, `SELECT data_json FROM development WHERE task_id=?`, taskID))
	if devErr == nil {
		w.Development = &d
	} else if devErr != sql.ErrNoRows {
		return w, devErr
	}
	w.Config, err = s.GetWorkConfig(ctx, taskID)
	if err != nil {
		return w, err
	}
	w.References, w.ReviewBrief, err = taskReferences(ctx, s.db, taskID)
	if err != nil {
		return w, err
	}
	w.Publications, err = s.ListPublications(ctx, taskID)
	if err != nil {
		return w, err
	}
	for i := range w.Publications {
		w.Publications[i].AppliedBody = ""
	}
	w.TestPipelines, err = s.ListTestPipelines(ctx, taskID)
	if err != nil {
		return w, err
	}
	w.ReviewTurns, err = listJSONRows[model.ReviewTurn](ctx, s.db, `SELECT data_json FROM review_turn WHERE task_id=? ORDER BY rowid`, taskID)
	if err != nil {
		return w, err
	}
	rows, err := s.db.QueryContext(ctx, messageSelect+` WHERE task_id=? ORDER BY seq`, taskID)
	if err != nil {
		return w, err
	}
	for rows.Next() {
		m, e := scanMessage(rows)
		if e != nil {
			rows.Close()
			return w, e
		}
		w.Messages = append(w.Messages, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return w, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT data_json FROM artifact WHERE task_id=? ORDER BY rowid DESC`, taskID)
	if err != nil {
		return w, err
	}
	for rows.Next() {
		a, e := readJSONRow[model.Artifact](rows)
		if e != nil {
			rows.Close()
			return w, e
		}
		w.Artifacts = append(w.Artifacts, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return w, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT data_json FROM review WHERE task_id=? ORDER BY rowid DESC`, taskID)
	if err != nil {
		return w, err
	}
	for rows.Next() {
		r, e := readJSONRow[model.Review](rows)
		if e != nil {
			rows.Close()
			return w, e
		}
		w.Reviews = append(w.Reviews, r)
	}
	err = rows.Err()
	rows.Close()
	return w, err
}
func (s *Store) GetArtifact(ctx context.Context, taskID, artifactID string) (model.Artifact, error) {
	return readJSONRow[model.Artifact](s.db.QueryRowContext(ctx, `SELECT data_json FROM artifact WHERE artifact_id=? AND task_id=?`, artifactID, taskID))
}
func (s *Store) ListWork(ctx context.Context) ([]model.Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.task_id,t.title,t.goal,t.state,t.version,t.current_revision_id,COALESCE(t.assigned_agent_id,''),t.created_at_ms,t.updated_at_ms,t.requirements_json,w.agent_id FROM task t JOIN task_workflow w ON w.task_id=t.task_id ORDER BY t.created_at_ms DESC LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := []model.Task{}
	for rows.Next() {
		var t model.Task
		if err = scanTask(rows, &t, &t.PreferredAgentID); err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

func isManagedTx(ctx context.Context, tx *sql.Tx, taskID string) (bool, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT task_id FROM task_workflow WHERE task_id=?`, taskID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
