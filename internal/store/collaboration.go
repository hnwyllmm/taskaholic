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
)

func (s *Store) CreateSubtask(ctx context.Context, parentTaskID, createdByRunID, idempotencyKey, title, goal string, requirements ...model.TaskRequirements) (model.SubtaskResult, bool, error) {
	title = strings.TrimSpace(title)
	goal = strings.TrimSpace(goal)
	if parentTaskID == "" || title == "" || goal == "" {
		return model.SubtaskResult{}, false, errors.New("parent_task_id, title and goal are required")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.SubtaskResult{}, false, err
	}
	defer tx.Rollback()
	if managed, err := isManagedTx(ctx, tx, parentTaskID); err != nil {
		return model.SubtaskResult{}, false, err
	} else if managed {
		return model.SubtaskResult{}, false, fmt.Errorf("%w: managed task delegation is not enabled yet", model.ErrConflict)
	}
	var needs model.TaskRequirements
	if len(requirements) > 0 {
		needs = requirements[0]
	}
	if err := validateRequirementsTx(ctx, tx, needs); err != nil {
		return model.SubtaskResult{}, false, err
	}
	needsJSON, _ := json.Marshal(needs)
	var parentState string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM task WHERE task_id = ?`, parentTaskID).Scan(&parentState); err != nil {
		return model.SubtaskResult{}, false, err
	}
	scope := "task.subtask:" + parentTaskID
	if idempotencyKey != "" {
		var childID string
		err := tx.QueryRowContext(ctx, `SELECT resource_id FROM idempotency_key WHERE scope = ? AND key = ?`,
			scope, idempotencyKey).Scan(&childID)
		if err == nil {
			child, err := getTaskTx(ctx, tx, childID)
			if err != nil {
				return model.SubtaskResult{}, false, err
			}
			edge, err := getTaskEdgeTx(ctx, tx, parentTaskID, childID, model.TaskEdgeDecomposedInto)
			return model.SubtaskResult{Task: child, Edge: edge}, true, err
		}
		if err != sql.ErrNoRows {
			return model.SubtaskResult{}, false, err
		}
	}
	if parentState == model.TaskStateCompleted {
		return model.SubtaskResult{}, false, errors.New("completed task cannot be decomposed")
	}
	if createdByRunID != "" {
		var runTaskID string
		if err := tx.QueryRowContext(ctx, `SELECT task_id FROM run WHERE run_id = ?`, createdByRunID).Scan(&runTaskID); err != nil {
			return model.SubtaskResult{}, false, err
		}
		if runTaskID != parentTaskID {
			return model.SubtaskResult{}, false, errors.New("created_by_run_id does not belong to the parent task")
		}
	}

	now := time.Now().UTC().UnixMilli()
	childID := id.New("task")
	revisionID := id.New("rev")
	edgeID := id.New("edge")
	child := model.Task{
		Requirements: needs,
		ID:           childID, Title: title, Goal: goal, State: model.TaskStateNew, Version: 1,
		CurrentRevisionID: revisionID, CreatedAtMS: now, UpdatedAtMS: now,
	}
	edge := model.TaskEdge{
		ID: edgeID, FromTaskID: parentTaskID, ToTaskID: childID,
		Type: model.TaskEdgeDecomposedInto, CreatedAtMS: now, CreatedByRun: createdByRunID,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO task(task_id, title, goal, state, version, current_revision_id, created_at_ms, updated_at_ms, requirements_json)
		VALUES(?, ?, ?, ?, 1, ?, ?, ?, ?)`, childID, title, goal, child.State, revisionID, now, now, needsJSON); err != nil {
		return model.SubtaskResult{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO task_revision(revision_id, task_id, revision_number, goal, created_at_ms)
		VALUES(?, ?, 1, ?, ?)`, revisionID, childID, goal, now); err != nil {
		return model.SubtaskResult{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO task_edge(edge_id, from_task_id, to_task_id, edge_type, created_at_ms, created_by_run_id)
		VALUES(?, ?, ?, ?, ?, NULLIF(?, ''))`, edgeID, parentTaskID, childID, edge.Type, now, createdByRunID); err != nil {
		return model.SubtaskResult{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE task SET state = ?, version = version + 1, updated_at_ms = ? WHERE task_id = ?`,
		model.TaskStateWaiting, now, parentTaskID); err != nil {
		return model.SubtaskResult{}, false, err
	}
	if _, err := appendEventTx(ctx, tx, "task", childID, "TaskCreated", createdByRunID, parentTaskID, map[string]any{
		"title": title, "goal": goal, "revision_id": revisionID, "parent_task_id": parentTaskID,
	}); err != nil {
		return model.SubtaskResult{}, false, err
	}
	if _, err := appendEventTx(ctx, tx, "task", parentTaskID, "TaskDecomposed", createdByRunID, parentTaskID, map[string]any{
		"child_task_id": childID, "edge_id": edgeID,
	}); err != nil {
		return model.SubtaskResult{}, false, err
	}
	if idempotencyKey != "" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO idempotency_key(scope, key, resource_id, created_at_ms) VALUES(?, ?, ?, ?)`,
			scope, idempotencyKey, childID, now); err != nil {
			return model.SubtaskResult{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return model.SubtaskResult{}, false, err
	}
	return model.SubtaskResult{Task: child, Edge: edge}, false, nil
}

func (s *Store) ListTaskEdges(ctx context.Context, taskID string) ([]model.TaskEdge, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT edge_id, from_task_id, to_task_id, edge_type, created_at_ms, COALESCE(created_by_run_id, '')
		FROM task_edge WHERE from_task_id = ? OR to_task_id = ? ORDER BY created_at_ms`, taskID, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var edges []model.TaskEdge
	for rows.Next() {
		var edge model.TaskEdge
		if err := rows.Scan(&edge.ID, &edge.FromTaskID, &edge.ToTaskID, &edge.Type,
			&edge.CreatedAtMS, &edge.CreatedByRun); err != nil {
			return nil, err
		}
		edges = append(edges, edge)
	}
	return edges, rows.Err()
}

func (s *Store) CreateDirective(ctx context.Context, runID, kind, message, idempotencyKey string) (model.Directive, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	message = strings.TrimSpace(message)
	if kind == "" {
		kind = model.DirectiveKindMessage
	}
	if kind != model.DirectiveKindMessage && kind != model.DirectiveKindInterrupt {
		return model.Directive{}, errors.New("directive kind must be message or interrupt")
	}
	if kind == model.DirectiveKindMessage && message == "" {
		return model.Directive{}, errors.New("message directive requires content")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Directive{}, err
	}
	defer tx.Rollback()
	var runtimeID, taskID, sessionID, runState string
	if err := tx.QueryRowContext(ctx, `
		SELECT runtime_id, task_id, COALESCE(session_id, ''), state FROM run WHERE run_id = ?`, runID).
		Scan(&runtimeID, &taskID, &sessionID, &runState); err != nil {
		return model.Directive{}, err
	}
	if managed, err := isManagedTx(ctx, tx, taskID); err != nil {
		return model.Directive{}, err
	} else if managed {
		return model.Directive{}, fmt.Errorf("%w: use managed task messages or pause to preserve conversation delivery", model.ErrConflict)
	}
	if idempotencyKey != "" {
		var existingDirectiveID string
		scope := "run.directive:" + runID
		err := tx.QueryRowContext(ctx, `SELECT resource_id FROM idempotency_key WHERE scope = ? AND key = ?`,
			scope, idempotencyKey).Scan(&existingDirectiveID)
		if err == nil {
			return scanDirective(tx.QueryRowContext(ctx, directiveSelect+" WHERE directive_id = ?", existingDirectiveID))
		}
		if err != sql.ErrNoRows {
			return model.Directive{}, err
		}
	}
	if isTerminalRunState(runState) {
		return model.Directive{}, fmt.Errorf("run is already terminal: %s", runState)
	}
	now := time.Now().UTC().UnixMilli()
	directive := model.Directive{
		ID: id.New("directive"), RunID: runID, TaskID: taskID, SessionID: sessionID,
		Kind: kind, Message: message, State: model.DirectiveStateQueued, CreatedAtMS: now,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO directive(directive_id, run_id, task_id, session_id, kind, message, state, created_at_ms)
		VALUES(?, ?, ?, NULLIF(?, ''), ?, NULLIF(?, ''), ?, ?)`, directive.ID, runID, taskID,
		sessionID, kind, message, directive.State, now); err != nil {
		return model.Directive{}, err
	}
	params, _ := json.Marshal(directive)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO outbox_message(message_id, destination_id, method, params_json, status,
		                           attempts, next_attempt_at_ms, created_at_ms)
		VALUES(?, ?, 'run.directive', ?, 'PENDING', 0, ?, ?)`,
		directive.ID, runtimeID, params, now, now); err != nil {
		return model.Directive{}, err
	}
	if _, err := appendEventTx(ctx, tx, "run", runID, "DirectiveIssued", directive.ID, taskID, directive); err != nil {
		return model.Directive{}, err
	}
	if idempotencyKey != "" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO idempotency_key(scope, key, resource_id, created_at_ms) VALUES(?, ?, ?, ?)`,
			"run.directive:"+runID, idempotencyKey, directive.ID, now); err != nil {
			return model.Directive{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return model.Directive{}, err
	}
	return directive, nil
}

func (s *Store) GetDirective(ctx context.Context, directiveID string) (model.Directive, error) {
	return scanDirective(s.db.QueryRowContext(ctx, directiveSelect+" WHERE directive_id = ?", directiveID))
}

func (s *Store) ListDirectivesForTask(ctx context.Context, taskID string) ([]model.Directive, error) {
	rows, err := s.db.QueryContext(ctx, directiveSelect+" WHERE task_id = ? ORDER BY created_at_ms", taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var directives []model.Directive
	for rows.Next() {
		directive, err := scanDirective(rows)
		if err != nil {
			return nil, err
		}
		directives = append(directives, directive)
	}
	return directives, rows.Err()
}

const directiveSelect = `
	SELECT directive_id, run_id, task_id, COALESCE(session_id, ''), kind,
	       COALESCE(message, ''), state, COALESCE(error, ''), created_at_ms, applied_at_ms
	FROM directive`

func scanDirective(row rowScanner) (model.Directive, error) {
	var directive model.Directive
	var applied sql.NullInt64
	if err := row.Scan(&directive.ID, &directive.RunID, &directive.TaskID, &directive.SessionID,
		&directive.Kind, &directive.Message, &directive.State, &directive.Error,
		&directive.CreatedAtMS, &applied); err != nil {
		return model.Directive{}, err
	}
	if applied.Valid {
		directive.AppliedAtMS = &applied.Int64
	}
	return directive, nil
}

func getTaskEdgeTx(ctx context.Context, tx *sql.Tx, fromTaskID, toTaskID, edgeType string) (model.TaskEdge, error) {
	var edge model.TaskEdge
	err := tx.QueryRowContext(ctx, `
		SELECT edge_id, from_task_id, to_task_id, edge_type, created_at_ms, COALESCE(created_by_run_id, '')
		FROM task_edge WHERE from_task_id = ? AND to_task_id = ? AND edge_type = ?`,
		fromTaskID, toTaskID, edgeType).Scan(&edge.ID, &edge.FromTaskID, &edge.ToTaskID,
		&edge.Type, &edge.CreatedAtMS, &edge.CreatedByRun)
	return edge, err
}

func updateParentStatesTx(ctx context.Context, tx *sql.Tx, childTaskID string, now int64) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT from_task_id FROM task_edge WHERE to_task_id = ? AND edge_type = ?`,
		childTaskID, model.TaskEdgeDecomposedInto)
	if err != nil {
		return err
	}
	var parentIDs []string
	for rows.Next() {
		var parentID string
		if err := rows.Scan(&parentID); err != nil {
			rows.Close()
			return err
		}
		parentIDs = append(parentIDs, parentID)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, parentID := range parentIDs {
		var total, terminal, blocked int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*),
			       SUM(CASE WHEN t.state IN (?, ?) THEN 1 ELSE 0 END),
			       SUM(CASE WHEN t.state = ? THEN 1 ELSE 0 END)
			FROM task_edge e JOIN task t ON t.task_id = e.to_task_id
			WHERE e.from_task_id = ? AND e.edge_type = ?`,
			model.TaskStateCompleted, model.TaskStateBlocked, model.TaskStateBlocked,
			parentID, model.TaskEdgeDecomposedInto).Scan(&total, &terminal, &blocked); err != nil {
			return err
		}
		if total == 0 || terminal != total {
			continue
		}
		var parentRunID, parentRuntimeID, parentSessionID string
		hasActiveParentRun := false
		err = tx.QueryRowContext(ctx, `
			SELECT run_id, runtime_id, COALESCE(session_id, '') FROM run
			WHERE task_id = ? AND state IN (?, ?) ORDER BY created_at_ms DESC LIMIT 1`,
			parentID, model.RunStateQueued, model.RunStateRunning).
			Scan(&parentRunID, &parentRuntimeID, &parentSessionID)
		if err == nil {
			hasActiveParentRun = true
		} else if err != sql.ErrNoRows {
			return err
		}
		state := model.TaskStateAssigned
		eventType := "SubtasksCompleted"
		if blocked > 0 {
			state = model.TaskStateBlocked
			eventType = "SubtasksBlocked"
		}
		if hasActiveParentRun {
			state = model.TaskStateInProgress
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE task SET state = ?, version = version + 1, updated_at_ms = ?
			WHERE task_id = ? AND state = ?`, state, now, parentID, model.TaskStateWaiting)
		if err != nil {
			return err
		}
		if affected, _ := result.RowsAffected(); affected > 0 {
			if _, err := appendEventTx(ctx, tx, "task", parentID, eventType, "", parentID, map[string]any{
				"child_count": total, "blocked_count": blocked, "next_state": state,
			}); err != nil {
				return err
			}
			if hasActiveParentRun {
				if err := enqueueSubtaskResultDirectiveTx(ctx, tx, parentID, parentRunID,
					parentRuntimeID, parentSessionID, eventType, now); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func enqueueSubtaskResultDirectiveTx(ctx context.Context, tx *sql.Tx, parentTaskID, parentRunID, runtimeID, sessionID, outcome string, now int64) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT child.task_id, child.title, child.state
		FROM task_edge edge JOIN task child ON child.task_id = edge.to_task_id
		WHERE edge.from_task_id = ? AND edge.edge_type = ? ORDER BY child.created_at_ms`,
		parentTaskID, model.TaskEdgeDecomposedInto)
	if err != nil {
		return err
	}
	type childResult struct {
		TaskID string `json:"task_id"`
		Title  string `json:"title"`
		State  string `json:"state"`
	}
	var children []childResult
	for rows.Next() {
		var child childResult
		if err := rows.Scan(&child.TaskID, &child.Title, &child.State); err != nil {
			rows.Close()
			return err
		}
		children = append(children, child)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{
		"type": "subtasks.settled", "parent_task_id": parentTaskID,
		"outcome": outcome, "children": children,
	})
	if err != nil {
		return err
	}
	directive := model.Directive{
		ID: id.New("directive"), RunID: parentRunID, TaskID: parentTaskID, SessionID: sessionID,
		Kind: model.DirectiveKindMessage, Message: string(payload), State: model.DirectiveStateQueued,
		CreatedAtMS: now,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO directive(directive_id, run_id, task_id, session_id, kind, message, state, created_at_ms)
		VALUES(?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?)`, directive.ID, parentRunID,
		parentTaskID, sessionID, directive.Kind, directive.Message, directive.State, now); err != nil {
		return err
	}
	params, _ := json.Marshal(directive)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO outbox_message(message_id, destination_id, method, params_json, status,
		                           attempts, next_attempt_at_ms, created_at_ms)
		VALUES(?, ?, 'run.directive', ?, 'PENDING', 0, ?, ?)`, directive.ID, runtimeID, params, now, now); err != nil {
		return err
	}
	_, err = appendEventTx(ctx, tx, "run", parentRunID, "DirectiveIssued", directive.ID, parentTaskID, directive)
	return err
}

func isTerminalRunState(state string) bool {
	return state == model.RunStateCompleted || state == model.RunStateFailed || state == model.RunStateInterrupted
}
