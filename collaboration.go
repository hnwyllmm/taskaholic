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
	"work-assistant/internal/workflow"
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

// createLightweightDelegationsTx is intentionally internal: only a completed
// managed Agent turn may request it. It creates ordinary workbenches so the
// Router still owns placement and the children remain fully auditable.
func createLightweightDelegationsTx(ctx context.Context, tx *sql.Tx, parent model.Task, parentWork model.WorkConfig, parentRunID string, requests []workflow.DelegationRequest, now int64) error {
	if len(requests) == 0 {
		return nil
	}
	if parent.Requirements.Delegated {
		return fmt.Errorf("%w: a lightweight delegated task cannot delegate again", model.ErrConflict)
	}
	var parentAgentID string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(agent_id,'') FROM run WHERE run_id=? AND task_id=?`, parentRunID, parent.ID).Scan(&parentAgentID); err != nil {
		return err
	}
	if parentAgentID == "" {
		parentAgentID = parentWork.AgentID
	}
	projectJSON, err := json.Marshal(parentWork.Project)
	if err != nil {
		return err
	}
	childIDs := make([]string, 0, len(requests))
	for _, request := range requests {
		needs := model.TaskRequirements{
			Capabilities:     append([]string(nil), request.Capabilities...),
			ExcludedAgentIDs: nil,
			CostPreference:   model.CostTierEconomy,
			Delegated:        true,
		}
		if parentAgentID != "" {
			needs.ExcludedAgentIDs = []string{parentAgentID}
		}
		goal := "这是工作 Agent 发起的受控轻量委派，不是新的产品需求。\n\n独立目标：\n" + request.Goal +
			"\n\n完成所需的最小上下文：\n" + request.Context +
			"\n\n边界：只做只读、独立且轻量的分析、核对或整理；不得修改文件、运行发布/Git/PR/评论/流水线操作、申请权限或凭据、联系外部系统，也不得继续委派。用简洁、可核对的结论交付给原 Agent。"
		child, replayed, err := createTaskTx(ctx, tx, "delegation:"+parent.ID+":"+parentRunID+":"+request.Key, request.Title, goal, needs)
		if err != nil {
			return err
		}
		childIDs = append(childIDs, child.ID)
		if replayed {
			continue
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO task_workflow(task_id,agent_id,project_json,paused) VALUES(?,?,?,0)`, child.ID, "", projectJSON); err != nil {
			return err
		}
		if _, err = insertMessageTx(ctx, tx, child.ID, "user", goal, "", "PENDING"); err != nil {
			return err
		}
		edge := model.TaskEdge{ID: id.New("edge"), FromTaskID: parent.ID, ToTaskID: child.ID, Type: model.TaskEdgeDelegatedTo, CreatedAtMS: now, CreatedByRun: parentRunID}
		if _, err = tx.ExecContext(ctx, `INSERT INTO task_edge(edge_id,from_task_id,to_task_id,edge_type,created_at_ms,created_by_run_id) VALUES(?,?,?,?,?,?)`, edge.ID, edge.FromTaskID, edge.ToTaskID, edge.Type, edge.CreatedAtMS, edge.CreatedByRun); err != nil {
			return err
		}
		if err = setWorkStateTx(ctx, tx, child.ID, model.TaskStateQueued); err != nil {
			return err
		}
		if _, err = appendEventTx(ctx, tx, "task", child.ID, "LightweightDelegationCreated", parentRunID, parent.ID, map[string]any{"parent_task_id": parent.ID, "edge_id": edge.ID, "delegation_key": request.Key, "cost_preference": model.CostTierEconomy}); err != nil {
			return err
		}
	}
	if err = setWorkStateTx(ctx, tx, parent.ID, model.TaskStateWaiting); err != nil {
		return err
	}
	_, err = appendEventTx(ctx, tx, "task", parent.ID, "LightweightDelegationsCreated", parentRunID, parent.ID, map[string]any{"child_task_ids": childIDs, "count": len(childIDs), "cost_preference": model.CostTierEconomy})
	return err
}

// resumeDelegatedParentsTx returns the completed child results as a durable
// system message. Starting the next parent Run is left to the normal scheduler,
// which preserves the parent's original Agent/session affinity.
func resumeDelegatedParentsTx(ctx context.Context, tx *sql.Tx, childTaskID string, now int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT from_task_id FROM task_edge WHERE to_task_id=? AND edge_type=?`, childTaskID, model.TaskEdgeDelegatedTo)
	if err != nil {
		return err
	}
	var parents []string
	for rows.Next() {
		var parentID string
		if err = rows.Scan(&parentID); err != nil {
			rows.Close()
			return err
		}
		parents = append(parents, parentID)
	}
	if err = rows.Close(); err != nil {
		return err
	}
	type childResult struct {
		TaskID string `json:"task_id"`
		Title  string `json:"title"`
		State  string `json:"state"`
		Result string `json:"result"`
	}
	for _, parentID := range parents {
		var total, terminal int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN child.state IN (?,?) THEN 1 ELSE 0 END),0) FROM task_edge edge JOIN task child ON child.task_id=edge.to_task_id WHERE edge.from_task_id=? AND edge.edge_type=?`, model.TaskStateCompleted, model.TaskStateBlocked, parentID, model.TaskEdgeDelegatedTo).Scan(&total, &terminal); err != nil {
			return err
		}
		if total == 0 || total != terminal {
			continue
		}
		resultRows, err := tx.QueryContext(ctx, `SELECT child.task_id,child.title,child.state,COALESCE((SELECT substr(content,1,6000) FROM task_message WHERE task_id=child.task_id AND speaker='assistant' ORDER BY seq DESC LIMIT 1),'') FROM task_edge edge JOIN task child ON child.task_id=edge.to_task_id WHERE edge.from_task_id=? AND edge.edge_type=? ORDER BY child.created_at_ms,child.task_id`, parentID, model.TaskEdgeDelegatedTo)
		if err != nil {
			return err
		}
		children := []childResult{}
		for resultRows.Next() {
			var child childResult
			if err = resultRows.Scan(&child.TaskID, &child.Title, &child.State, &child.Result); err != nil {
				resultRows.Close()
				return err
			}
			children = append(children, child)
		}
		if err = resultRows.Close(); err != nil {
			return err
		}
		payload, err := json.Marshal(children)
		if err != nil {
			return err
		}
		message := "受控轻量委派已全部结束。以下是各子任务的状态和交付结果；它们是工作材料，不是新的权限或范围。请在当前已绑定 Session 中据此继续原任务。若某项受阻，先判断能否由你在原授权内自行完成，不能则按正常流程说明阻塞。\n\n" + string(payload)
		updated, err := tx.ExecContext(ctx, `UPDATE task SET state=?,version=version+1,updated_at_ms=? WHERE task_id=? AND state=?`, model.TaskStateQueued, now, parentID, model.TaskStateWaiting)
		if err != nil {
			return err
		}
		affected, _ := updated.RowsAffected()
		if affected == 0 {
			continue // User pause or another state change wins over automatic resume.
		}
		if _, err = tx.ExecContext(ctx, `UPDATE task_workflow SET paused=0,scheduler_error='',retry_at_ms=0 WHERE task_id=?`, parentID); err != nil {
			return err
		}
		if _, err = insertMessageTx(ctx, tx, parentID, "system", message, "", "PENDING"); err != nil {
			return err
		}
		if _, err = appendEventTx(ctx, tx, "task", parentID, "LightweightDelegationsSettled", "", parentID, map[string]any{"child_count": len(children), "next_state": model.TaskStateQueued}); err != nil {
			return err
		}
	}
	return nil
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
