package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"work-assistant/internal/model"
	"work-assistant/internal/workflow"
)

const (
	autoRecoveryScope = "system.auto-recovery"
	autoRecoveryKey   = "v1"
)

// EnableAutomaticRecovery records the rollout boundary. Existing BLOCKED tasks
// stay untouched; tasks blocked after this point are handled durably across
// process restarts.
func (s *Store) EnableAutomaticRecovery(ctx context.Context) error {
	return s.sourceWrite(ctx, func(tx *sql.Tx) error {
		var marker string
		err := tx.QueryRowContext(ctx, `SELECT resource_id FROM idempotency_key WHERE scope=? AND key=?`, autoRecoveryScope, autoRecoveryKey).Scan(&marker)
		if err == nil {
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		now := time.Now().UnixMilli()
		var afterSeq int64
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(global_seq),0) FROM event_log`).Scan(&afterSeq); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO idempotency_key(scope,key,resource_id,created_at_ms) VALUES(?,?,?,?)`, autoRecoveryScope, autoRecoveryKey, strconv.FormatInt(afterSeq, 10), now); err != nil {
			return err
		}
		_, err = appendEventTx(ctx, tx, "system", "automatic-recovery", "AutomaticRecoveryEnabled", "", "", map[string]any{"enabled_at_ms": now, "after_global_seq": afterSeq})
		return err
	})
}

func (s *Store) automaticRecoveryBaseline(ctx context.Context) (int64, error) {
	var marker string
	err := s.db.QueryRowContext(ctx, `SELECT resource_id FROM idempotency_key WHERE scope=? AND key=?`, autoRecoveryScope, autoRecoveryKey).Scan(&marker)
	if err != nil {
		return 0, err
	}
	afterSeq, err := strconv.ParseInt(marker, 10, 64)
	if err != nil || afterSeq < 0 {
		return 0, fmt.Errorf("invalid automatic recovery baseline %q", marker)
	}
	return afterSeq, nil
}

// AutoRecoverBlockedWork converts an internal execution failure into another
// turn for the same Agent and logical Session. It does not approve permissions,
// alter a plan, reassign ownership, repeat an external write, or satisfy a
// human gate. A known exhausted native Codex conversation is the narrow
// exception to native-session affinity: its logical Session, worktree and
// approved plan stay in place, while the opaque native conversation is reset
// and replaced with a compact, durable handoff snapshot.
func (s *Store) AutoRecoverBlockedWork(ctx context.Context) (int, error) {
	if maintenance, err := s.Maintenance(ctx); err != nil {
		return 0, err
	} else if maintenance != "" {
		return 0, nil
	}
	afterSeq, err := s.automaticRecoveryBaseline(ctx)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.task_id
		FROM task t JOIN task_workflow w USING(task_id)
		LEFT JOIN development d USING(task_id)
		WHERE t.state='BLOCKED'
		  AND EXISTS(SELECT 1 FROM event_log e WHERE e.correlation_id=t.task_id AND e.global_seq>? AND e.event_type IN ('RunFailed','RunCompleted','RunInterrupted'))
		  AND COALESCE(json_extract(d.data_json,'$.phase'),'') NOT IN ('AGENT_REVIEW','HUMAN_REVIEW')
		  AND EXISTS(SELECT 1 FROM task_session s WHERE s.task_id=t.task_id AND s.unbound_at_ms IS NULL)
		  AND NOT EXISTS(SELECT 1 FROM run r WHERE r.task_id=t.task_id AND r.state IN ('QUEUED','RUNNING'))
		  AND NOT EXISTS(
		    SELECT 1 FROM event_log p
		    WHERE p.correlation_id=t.task_id AND p.event_type='WorkExplicitlyPaused'
		      AND p.global_seq>COALESCE((SELECT MAX(q.global_seq) FROM event_log q WHERE q.correlation_id=t.task_id AND q.event_type='RunQueued'),0)
		  )
		  AND NOT EXISTS(SELECT 1 FROM review r WHERE r.task_id=t.task_id AND r.state='PENDING')
		  AND NOT EXISTS(SELECT 1 FROM review_turn r WHERE r.task_id=t.task_id AND r.state IN ('QUEUED','RUNNING'))
		  AND NOT EXISTS(SELECT 1 FROM permission_request p WHERE p.task_id=t.task_id AND p.state='PENDING')
		  AND NOT EXISTS(SELECT 1 FROM environment_job e WHERE (e.task_id=t.task_id OR e.parent_task_id=t.task_id) AND e.state IN ('QUEUED','RUNNING'))
		  AND NOT EXISTS(SELECT 1 FROM publication p WHERE p.task_id=t.task_id AND p.state IN ('QUEUED','SUBMITTING','UNCERTAIN'))
		  AND NOT EXISTS(
		    SELECT 1 FROM test_pipeline p
		    WHERE (p.task_id=t.task_id OR json_extract(p.data_json,'$.requested_by_task_id')=t.task_id)
		      AND p.state IN ('QUEUED','SUBMITTING','UNCERTAIN','RETRY_QUEUED','RETRY_SUBMITTING','RETRY_UNCERTAIN','created','pending','preparing','waiting_for_resource','running','scheduled','canceling')
		      AND COALESCE((SELECT CASE WHEN json_valid(latest_run.output) THEN json_extract(latest_run.output,'$.outcome') ELSE '' END FROM run latest_run WHERE latest_run.task_id=t.task_id ORDER BY latest_run.created_at_ms DESC,latest_run.rowid DESC LIMIT 1),'')='blocked'
		  )
		  AND NOT EXISTS(
		    SELECT 1 FROM test_pipeline p
		    WHERE p.task_id=t.task_id
		      AND p.request_id=(SELECT latest.request_id FROM test_pipeline latest WHERE latest.pr_target_id=p.pr_target_id AND latest.head_sha=p.head_sha ORDER BY latest.attempt DESC LIMIT 1)
		      AND p.head_sha=json_extract((SELECT pr.data_json FROM source_target pr WHERE pr.target_id=p.pr_target_id),'$.head_sha')
		      AND p.state IN ('failed','ERROR','canceled','skipped')
		      AND (COALESCE(json_extract(p.data_json,'$.analysis_required'),0)=1 OR p.attempt>=3)
		      AND COALESCE((SELECT CASE WHEN json_valid(latest_run.output) THEN json_extract(latest_run.output,'$.outcome') ELSE '' END FROM run latest_run WHERE latest_run.task_id=t.task_id ORDER BY latest_run.created_at_ms DESC,latest_run.rowid DESC LIMIT 1),'')='blocked'
		  )
		ORDER BY t.updated_at_ms LIMIT 20`, afterSeq)
	if err != nil {
		return 0, err
	}
	var taskIDs []string
	for rows.Next() {
		var taskID string
		if err = rows.Scan(&taskID); err != nil {
			rows.Close()
			return 0, err
		}
		taskIDs = append(taskIDs, taskID)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	recovered := 0
	for _, taskID := range taskIDs {
		changed := false
		err = s.sourceWrite(ctx, func(tx *sql.Tx) error {
			var inner error
			changed, inner = autoRecoverBlockedWorkTx(ctx, tx, taskID, afterSeq)
			return inner
		})
		if err != nil {
			return recovered, err
		}
		if changed {
			recovered++
		}
	}
	return recovered, nil
}

// RecoverExhaustedNativeSessionWork repairs a task which has already been
// queued for automatic retry before a newer control-plane binary learned how
// to recover an exhausted native Codex conversation. It is deliberately
// narrower than generic recovery: only an idle, queued task with an active
// codex:* reference and a latest failed Run carrying a known context-exhaustion
// error is eligible. This makes deployment safe without requiring an operator
// to edit SQLite or wait out an old retry backoff.
func (s *Store) RecoverExhaustedNativeSessionWork(ctx context.Context) (int, error) {
	if maintenance, err := s.Maintenance(ctx); err != nil {
		return 0, err
	} else if maintenance != "" {
		return 0, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.task_id
		FROM task t
		JOIN task_workflow w USING(task_id)
		JOIN task_session ts ON ts.task_id=t.task_id AND ts.unbound_at_ms IS NULL
		JOIN session s ON s.session_id=ts.session_id
		WHERE t.state='QUEUED' AND w.paused=0 AND s.adapter_id='codex-agent'
		  AND s.agent_session_ref LIKE 'codex:%'
		  AND NOT EXISTS(SELECT 1 FROM run r WHERE r.task_id=t.task_id AND r.state IN ('QUEUED','RUNNING'))
		ORDER BY t.updated_at_ms
		LIMIT 20`)
	if err != nil {
		return 0, err
	}
	var taskIDs []string
	for rows.Next() {
		var taskID string
		if err := rows.Scan(&taskID); err != nil {
			rows.Close()
			return 0, err
		}
		taskIDs = append(taskIDs, taskID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	recovered := 0
	for _, taskID := range taskIDs {
		changed := false
		if err := s.sourceWrite(ctx, func(tx *sql.Tx) error {
			var inner error
			changed, inner = recoverQueuedExhaustedNativeSessionTx(ctx, tx, taskID)
			return inner
		}); err != nil {
			return recovered, err
		}
		if changed {
			recovered++
		}
	}
	return recovered, nil
}

func recoverQueuedExhaustedNativeSessionTx(ctx context.Context, tx *sql.Tx, taskID string) (bool, error) {
	var state string
	var paused bool
	if err := tx.QueryRowContext(ctx, `SELECT t.state,w.paused FROM task t JOIN task_workflow w USING(task_id) WHERE t.task_id=?`, taskID).Scan(&state, &paused); err != nil {
		return false, err
	}
	if state != model.TaskStateQueued || paused {
		return false, nil
	}
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM run WHERE task_id=? AND state IN ('QUEUED','RUNNING')`, taskID).Scan(&active); err != nil {
		return false, err
	}
	if active != 0 {
		return false, nil
	}
	changed, runID, err := resetLatestExhaustedCodexSessionTx(ctx, tx, taskID)
	if err != nil || !changed {
		return changed, err
	}
	now := time.Now().UnixMilli()
	if _, err = tx.ExecContext(ctx, `UPDATE task_workflow SET scheduler_error='',retry_at_ms=0 WHERE task_id=?`, taskID); err != nil {
		return false, err
	}
	_, err = appendEventTx(ctx, tx, "task", taskID, "NativeSessionContextHandoffExpedited", runID, taskID, map[string]any{
		"run_id": runID, "reason": "native_context_exhausted", "retry_at_ms": now,
	})
	return err == nil, err
}

func autoRecoverBlockedWorkTx(ctx context.Context, tx *sql.Tx, taskID string, afterSeq int64) (bool, error) {
	var taskState, schedulerError, phase string
	err := tx.QueryRowContext(ctx, `
		SELECT t.state,w.scheduler_error,COALESCE(json_extract(d.data_json,'$.phase'),'')
		FROM task t JOIN task_workflow w USING(task_id) LEFT JOIN development d USING(task_id)
		WHERE t.task_id=?`, taskID).Scan(&taskState, &schedulerError, &phase)
	if err != nil {
		return false, err
	}
	if taskState != model.TaskStateBlocked || phase == "AGENT_REVIEW" || phase == "HUMAN_REVIEW" {
		return false, nil
	}
	var gated bool
	if err = tx.QueryRowContext(ctx, `
		SELECT
		  EXISTS(SELECT 1 FROM event_log e WHERE e.correlation_id=? AND e.global_seq>? AND e.event_type IN ('RunFailed','RunCompleted','RunInterrupted'))=0 OR
		  EXISTS(SELECT 1 FROM task_session s WHERE s.task_id=? AND s.unbound_at_ms IS NULL)=0 OR
		  EXISTS(SELECT 1 FROM run r WHERE r.task_id=? AND r.state IN ('QUEUED','RUNNING')) OR
		  EXISTS(
		    SELECT 1 FROM event_log p
		    WHERE p.correlation_id=? AND p.event_type='WorkExplicitlyPaused'
		      AND p.global_seq>COALESCE((SELECT MAX(q.global_seq) FROM event_log q WHERE q.correlation_id=? AND q.event_type='RunQueued'),0)
		  ) OR
		  EXISTS(SELECT 1 FROM review r WHERE r.task_id=? AND r.state='PENDING') OR
		  EXISTS(SELECT 1 FROM review_turn r WHERE r.task_id=? AND r.state IN ('QUEUED','RUNNING')) OR
		  EXISTS(SELECT 1 FROM permission_request p WHERE p.task_id=? AND p.state='PENDING') OR
		  EXISTS(SELECT 1 FROM environment_job e WHERE (e.task_id=? OR e.parent_task_id=?) AND e.state IN ('QUEUED','RUNNING')) OR
		  EXISTS(SELECT 1 FROM publication p WHERE p.task_id=? AND p.state IN ('QUEUED','SUBMITTING','UNCERTAIN')) OR
		  EXISTS(
		    SELECT 1 FROM test_pipeline p
		    WHERE (p.task_id=? OR json_extract(p.data_json,'$.requested_by_task_id')=?)
		      AND p.state IN ('QUEUED','SUBMITTING','UNCERTAIN','RETRY_QUEUED','RETRY_SUBMITTING','RETRY_UNCERTAIN','created','pending','preparing','waiting_for_resource','running','scheduled','canceling')
		      AND COALESCE((SELECT CASE WHEN json_valid(latest_run.output) THEN json_extract(latest_run.output,'$.outcome') ELSE '' END FROM run latest_run WHERE latest_run.task_id=? ORDER BY latest_run.created_at_ms DESC,latest_run.rowid DESC LIMIT 1),'')='blocked'
		  ) OR
		  EXISTS(
		    SELECT 1 FROM test_pipeline p
		    WHERE p.task_id=?
		      AND p.request_id=(SELECT latest.request_id FROM test_pipeline latest WHERE latest.pr_target_id=p.pr_target_id AND latest.head_sha=p.head_sha ORDER BY latest.attempt DESC LIMIT 1)
		      AND p.head_sha=json_extract((SELECT pr.data_json FROM source_target pr WHERE pr.target_id=p.pr_target_id),'$.head_sha')
		      AND p.state IN ('failed','ERROR','canceled','skipped')
		      AND (COALESCE(json_extract(p.data_json,'$.analysis_required'),0)=1 OR p.attempt>=3)
		      AND COALESCE((SELECT CASE WHEN json_valid(latest_run.output) THEN json_extract(latest_run.output,'$.outcome') ELSE '' END FROM run latest_run WHERE latest_run.task_id=? ORDER BY latest_run.created_at_ms DESC,latest_run.rowid DESC LIMIT 1),'')='blocked'
		  )`,
		taskID, afterSeq, taskID, taskID, taskID, taskID, taskID, taskID, taskID, taskID, taskID, taskID, taskID, taskID, taskID, taskID, taskID).Scan(&gated); err != nil {
		return false, err
	}
	if gated {
		return false, nil
	}
	var runID, runState, runError, output string
	if err = tx.QueryRowContext(ctx, `SELECT run_id,state,COALESCE(error,''),COALESCE(output,'') FROM run WHERE task_id=? ORDER BY created_at_ms DESC,rowid DESC LIMIT 1`, taskID).Scan(&runID, &runState, &runError, &output); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	kind, reason := automaticRecoveryReason(runState, runError, output, schedulerError)
	var pending int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_message WHERE task_id=? AND delivery='PENDING'`, taskID).Scan(&pending); err != nil {
		return false, err
	}
	if changed, _, handoffErr := resetLatestExhaustedCodexSessionTx(ctx, tx, taskID); handoffErr != nil {
		return false, handoffErr
	} else if changed {
		kind = "native_context_handoff"
		// resetLatestExhaustedCodexSessionTx has written a durable handoff
		// snapshot. Do not overwrite it with a vague generic retry directive.
		pending++
	}
	var recent int
	now := time.Now()
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_log WHERE correlation_id=? AND event_type='WorkAutoRecoveryQueued' AND occurred_at_ms>=?`, taskID, now.Add(-time.Hour).UnixMilli()).Scan(&recent); err != nil {
		return false, err
	}
	delay := automaticRecoveryDelay(kind, reason, recent)
	retryAt := now.Add(delay).UnixMilli()
	messageID := ""
	if pending == 0 {
		content := automaticRecoveryMessage(kind, reason, phase, recent+1)
		message, messageErr := insertMessageTx(ctx, tx, taskID, "system", content, runID, "PENDING")
		if messageErr != nil {
			return false, messageErr
		}
		messageID = message.ID
	}
	if _, err = tx.ExecContext(ctx, `UPDATE task_workflow SET paused=0,scheduler_error='',retry_at_ms=? WHERE task_id=?`, retryAt, taskID); err != nil {
		return false, err
	}
	if err = setWorkStateTx(ctx, tx, taskID, model.TaskStateQueued); err != nil {
		return false, err
	}
	_, err = appendEventTx(ctx, tx, "task", taskID, "WorkAutoRecoveryQueued", runID, taskID, map[string]any{
		"kind": kind, "reason": reason, "run_id": runID, "message_id": messageID,
		"attempt_in_last_hour": recent + 1, "retry_at_ms": retryAt, "phase": phase,
	})
	return err == nil, err
}

func isNativeCodexContextExhaustion(reason string) bool {
	lower := strings.ToLower(reason)
	return strings.Contains(lower, "context window") ||
		strings.Contains(lower, "ran out of room in the model's context") ||
		strings.Contains(lower, "context_length") ||
		strings.Contains(lower, "maximum context length")
}

// resetLatestExhaustedCodexSessionTx keeps the task's logical Session ID,
// worktree affinity, Agent, model and approved development state. It only
// removes an exhausted opaque Codex reference so the next Run starts a new
// native conversation and binds it back to this same logical Session.
func resetLatestExhaustedCodexSessionTx(ctx context.Context, tx *sql.Tx, taskID string) (bool, string, error) {
	var runID, state, runError string
	err := tx.QueryRowContext(ctx, `SELECT run_id,state,COALESCE(error,'') FROM run WHERE task_id=? ORDER BY created_at_ms DESC,rowid DESC LIMIT 1`, taskID).Scan(&runID, &state, &runError)
	if err == sql.ErrNoRows {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	if state != model.RunStateFailed || !isNativeCodexContextExhaustion(runError) {
		return false, runID, nil
	}
	session, err := getActiveSessionTx(ctx, tx, taskID)
	if err == sql.ErrNoRows {
		return false, runID, nil
	}
	if err != nil {
		return false, runID, err
	}
	if session.AdapterID != "codex-agent" || !strings.HasPrefix(session.AgentSessionRef, "codex:") {
		return false, runID, nil
	}

	snapshot, err := nativeContextHandoffSnapshotTx(ctx, tx, taskID, runID)
	if err != nil {
		return false, runID, err
	}
	metadata := map[string]any{}
	if len(session.Metadata) > 0 && string(session.Metadata) != "null" {
		if err = json.Unmarshal(session.Metadata, &metadata); err != nil {
			return false, runID, fmt.Errorf("decode session metadata for native context handoff: %w", err)
		}
	}
	metadata["native_context_handoff"] = map[string]any{
		"at_ms":    time.Now().UnixMilli(),
		"reason":   "native_context_exhausted",
		"from_run": runID,
	}
	rawMetadata, err := json.Marshal(metadata)
	if err != nil {
		return false, runID, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE session SET agent_session_ref=NULL,metadata_json=?,updated_at_ms=? WHERE session_id=? AND agent_session_ref=?`, rawMetadata, time.Now().UnixMilli(), session.ID, session.AgentSessionRef); err != nil {
		return false, runID, err
	}
	message, err := insertMessageTx(ctx, tx, taskID, "system", snapshot, runID, "PENDING")
	if err != nil {
		return false, runID, err
	}
	_, err = appendEventTx(ctx, tx, "session", session.ID, "NativeSessionContextHandoffQueued", runID, taskID, map[string]any{
		"run_id": runID, "message_id": message.ID, "reason": "native_context_exhausted", "snapshot_bytes": len(snapshot),
	})
	return err == nil, runID, err
}

func nativeContextHandoffSnapshotTx(ctx context.Context, tx *sql.Tx, taskID, failedRunID string) (string, error) {
	task, err := getTaskTx(ctx, tx, taskID)
	if err != nil {
		return "", err
	}
	type handoffMessage struct {
		Speaker string `json:"speaker"`
		Content string `json:"content"`
	}
	type handoffArtifact struct {
		Name    string `json:"name"`
		Version int64  `json:"version"`
		SHA256  string `json:"sha256"`
		Content string `json:"content"`
	}
	type handoffSnapshot struct {
		Task struct {
			ID    string `json:"task_id"`
			Title string `json:"title"`
			Goal  string `json:"goal"`
		} `json:"task"`
		Development *model.Development    `json:"development,omitempty"`
		References  []model.TaskReference `json:"references,omitempty"`
		Messages    []handoffMessage      `json:"recent_messages,omitempty"`
		Artifacts   []handoffArtifact     `json:"recent_artifacts,omitempty"`
		FailedRunID string                `json:"failed_run_id"`
		Instruction string                `json:"instruction"`
	}
	var snapshot handoffSnapshot
	snapshot.Task.ID = task.ID
	snapshot.Task.Title = truncateRunes(task.Title, 400)
	snapshot.Task.Goal = truncateRunes(task.Goal, 2200)
	snapshot.FailedRunID = failedRunID
	snapshot.Instruction = "原隔离工作目录、批准方案、执行授权和外部记录仍有效。先检查现有工作树和以上材料；不要重复已确认的外部写入，也不要重新进行无关设计。"
	if development, developmentErr := developmentTx(ctx, tx, taskID); developmentErr == nil {
		snapshot.Development = &development
	} else if developmentErr != sql.ErrNoRows {
		return "", developmentErr
	}
	refs, _, refErr := taskReferences(ctx, tx, taskID)
	if refErr != nil {
		return "", refErr
	}
	if len(refs) > 6 {
		refs = refs[:6]
	}
	snapshot.References = refs

	rows, err := tx.QueryContext(ctx, `SELECT speaker,content FROM task_message WHERE task_id=? ORDER BY seq DESC LIMIT 6`, taskID)
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var message handoffMessage
		if err = rows.Scan(&message.Speaker, &message.Content); err != nil {
			rows.Close()
			return "", err
		}
		message.Content = truncateRunes(message.Content, 800)
		snapshot.Messages = append(snapshot.Messages, message)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return "", err
	}
	rows.Close()
	// Query returns newest first; restore chronology so the new Agent can read
	// the compact conversation as a coherent handoff.
	for left, right := 0, len(snapshot.Messages)-1; left < right; left, right = left+1, right-1 {
		snapshot.Messages[left], snapshot.Messages[right] = snapshot.Messages[right], snapshot.Messages[left]
	}

	rows, err = tx.QueryContext(ctx, `SELECT data_json FROM artifact WHERE task_id=? ORDER BY rowid DESC LIMIT 4`, taskID)
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return "", err
		}
		var artifact model.Artifact
		if err = json.Unmarshal(raw, &artifact); err != nil {
			rows.Close()
			return "", err
		}
		snapshot.Artifacts = append(snapshot.Artifacts, handoffArtifact{
			Name: artifact.Name, Version: artifact.Version, SHA256: artifact.SHA256, Content: truncateRunes(artifact.Content, 1000),
		})
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return "", err
	}
	rows.Close()

	raw, err := json.Marshal(snapshot)
	if err != nil {
		return "", err
	}
	// The compact bounds above keep normal snapshots below 32 KiB. Preserve a
	// valid JSON payload even if unusually dense UTF-8 material exceeds it.
	if len(raw) > 32*1024 {
		snapshot.Artifacts = nil
		if len(snapshot.Messages) > 3 {
			snapshot.Messages = snapshot.Messages[len(snapshot.Messages)-3:]
		}
		snapshot.Task.Goal = truncateRunes(snapshot.Task.Goal, 800)
		raw, err = json.Marshal(snapshot)
		if err != nil {
			return "", err
		}
	}
	return "系统检测到原生 Codex 会话已耗尽上下文。已保留同一个逻辑 Session、原 Agent、隔离工作目录、已批准方案和任务记录；下一轮会启动新的原生会话。以下是受限交接快照（只含当前任务材料）：\n```json\n" + string(raw) + "\n```\n请在原工作树继续，不要把这次会话交接当作需求、权限或方案变更。", nil
}

func automaticRecoveryReason(runState, runError, output, schedulerError string) (string, string) {
	reason := strings.TrimSpace(schedulerError)
	kind := "manager_block"
	if runState == model.RunStateFailed {
		kind = "run_failure"
		if strings.TrimSpace(runError) != "" {
			reason = runError
		}
	} else if result, err := workflow.Parse(output); err != nil {
		kind = "result_protocol"
		if reason == "" {
			reason = err.Error()
		}
	} else {
		if result.Outcome == "blocked" {
			kind = "agent_blocked"
		}
		if result.TaskUpdate != nil && strings.TrimSpace(result.TaskUpdate.BlockedReason) != "" {
			reason = result.TaskUpdate.BlockedReason
		} else if reason == "" {
			reason = result.Message
		}
	}
	if reason == "" {
		reason = "上一轮没有给出可执行的完成结果。"
	}
	return kind, truncateRunes(reason, 3000)
}

func automaticRecoveryDelay(kind, reason string, recent int) time.Duration {
	delay := 3 * time.Second
	lower := strings.ToLower(reason)
	if strings.Contains(lower, "usage limit") || strings.Contains(lower, "rate limit") || strings.Contains(lower, "quota") || strings.Contains(lower, "429") || strings.Contains(reason, "额度") {
		delay = time.Minute
	}
	// No attempt budget: repeated blockers continue, but progressively slower so
	// a malformed fast response cannot burn tokens in a tight loop.
	for i := 0; i < recent && delay < 5*time.Minute; i++ {
		delay *= 2
	}
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	if kind == "result_protocol" && delay > 30*time.Second {
		delay = 30 * time.Second
	}
	return delay
}

func automaticRecoveryMessage(kind, reason, phase string, attempt int) string {
	work := "继续完成原任务；检查上一轮状态、已有结果和外部操作回执后再行动，不要盲目重放。"
	if phase == "IMPLEMENTING" {
		work = "沿用已批准方案在原工作树继续实施。若是代码、构建或测试缺陷，由你定位、修复并运行受影响验证；Manager 不代写业务修复。"
	} else if phase == "PLANNING" {
		work = "保持方案阶段，只补齐分析、方案或结果协议，不得提前修改代码。"
	}
	return fmt.Sprintf("系统自动解阻（第 %d 次，本次类型：%s）。\n上一轮原因：%s\n%s\n沿用原 Agent、原 Session、当前任务范围和阶段。能自主解决时直接处理，不要把普通错误交给人；结果格式或外部引用有误时只修正并重新提交，不要重做已完成工作。确实缺少产品决策或凭据时返回 needs_input；需要新权限或执行环境时使用结构化 capability_request / environment_request。不要在仍有可执行路径时再次返回笼统 blocked。", attempt, kind, reason, work)
}
