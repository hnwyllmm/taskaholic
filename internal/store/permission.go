package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"work-assistant/internal/id"
	"work-assistant/internal/model"
)

const waitingAuthorization = "WAITING_AUTHORIZATION"
const waitingEnvironment = "WAITING_ENVIRONMENT"

func invalidatePermissionsTx(ctx context.Context, tx *sql.Tx, taskID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT data_json FROM permission_request WHERE task_id=? AND state IN ('PENDING','APPROVED')`, taskID)
	if err != nil {
		return err
	}
	var requests []model.PermissionRequest
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		var r model.PermissionRequest
		if err = json.Unmarshal(raw, &r); err != nil {
			rows.Close()
			return err
		}
		requests = append(requests, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, r := range requests {
		r.State = "STALE"
		if err = savePermissionTx(ctx, tx, &r, "ExecutionPermissionInvalidated"); err != nil {
			return err
		}
	}
	return nil
}

func migrateV19(db *sql.DB) error {
	var v int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&v); err != nil || v >= 19 {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`CREATE TABLE execution_policy(policy_id TEXT PRIMARY KEY, operation TEXT NOT NULL, runtime_id TEXT NOT NULL, repository TEXT NOT NULL, data_json TEXT NOT NULL, UNIQUE(operation,runtime_id,repository))`,
		`CREATE TABLE permission_request(request_id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES task(task_id), fingerprint TEXT NOT NULL UNIQUE, state TEXT NOT NULL, data_json TEXT NOT NULL)`,
		`INSERT INTO schema_version VALUES(19,unixepoch('subsec')*1000)`,
	} {
		if _, err = tx.Exec(q); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func validOperation(op string) bool {
	return op == "development.execute" || op == "windows_seekdb_phase0"
}
func validatePolicy(p model.ExecutionPolicy) error {
	if !validOperation(p.Operation) || (p.Effect != "allow" && p.Effect != "ask" && p.Effect != "deny") || p.RuntimeID == "" || p.Repository == "" || len(p.RuntimeID) > 160 || len(p.Repository) > 240 || strings.ContainsAny(p.RuntimeID+p.Repository, "\r\n\x00") {
		return fmt.Errorf("%w: invalid execution policy", model.ErrValidation)
	}
	if p.RuntimeID != "*" && strings.Contains(p.RuntimeID, "*") || p.Repository != "*" && (strings.Contains(p.Repository, "*") || strings.Count(p.Repository, "/") != 1) {
		return fmt.Errorf("%w: use an exact scope or *", model.ErrValidation)
	}
	return nil
}
func (s *Store) ExecutionPolicies(ctx context.Context) ([]model.ExecutionPolicy, error) {
	return listJSONRows[model.ExecutionPolicy](ctx, s.db, `SELECT data_json FROM execution_policy ORDER BY rowid`)
}
func (s *Store) PermissionRequests(ctx context.Context) ([]model.PermissionRequest, error) {
	return listJSONRows[model.PermissionRequest](ctx, s.db, `SELECT data_json FROM permission_request ORDER BY CASE state WHEN 'PENDING' THEN 0 ELSE 1 END,rowid DESC LIMIT 200`)
}

func savePolicyTx(ctx context.Context, tx *sql.Tx, p model.ExecutionPolicy) (model.ExecutionPolicy, error) {
	if err := validatePolicy(p); err != nil {
		return p, err
	}
	old, err := readJSONRow[model.ExecutionPolicy](tx.QueryRowContext(ctx, `SELECT data_json FROM execution_policy WHERE operation=? AND runtime_id=? AND repository=?`, p.Operation, p.RuntimeID, p.Repository))
	if err != nil && err != sql.ErrNoRows {
		return p, err
	}
	if err == nil {
		if p.Version != old.Version {
			return p, fmt.Errorf("%w: policy changed", model.ErrConflict)
		}
		p.ID = old.ID
	} else {
		if p.Version != 0 {
			return p, fmt.Errorf("%w: policy no longer exists", model.ErrConflict)
		}
		p.ID = id.New("policy")
	}
	p.Version++
	raw, _ := json.Marshal(p)
	_, err = tx.ExecContext(ctx, `INSERT INTO execution_policy VALUES(?,?,?,?,?) ON CONFLICT(policy_id) DO UPDATE SET data_json=excluded.data_json`, p.ID, p.Operation, p.RuntimeID, p.Repository, raw)
	if err == nil {
		_, err = appendEventTx(ctx, tx, "execution_policy", p.ID, "ExecutionPolicyChanged", "", "", p)
	}
	return p, err
}
func (s *Store) SaveExecutionPolicy(ctx context.Context, p model.ExecutionPolicy) (model.ExecutionPolicy, error) {
	var result model.ExecutionPolicy
	err := s.sourceWrite(ctx, func(tx *sql.Tx) error { var err error; result, err = savePolicyTx(ctx, tx, p); return err })
	return result, err
}

func policyEffectTx(ctx context.Context, tx *sql.Tx, r model.PermissionRequest) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT data_json FROM execution_policy WHERE operation=? AND runtime_id IN (?, '*') AND repository IN (?, '*')`, r.Operation, r.RuntimeID, r.Repository)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	effect := "ask"
	if r.Operation == "development.execute" {
		effect = "allow"
	} // Existing human-approved isolated development behavior.
	best := -1
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return "", err
		}
		var p model.ExecutionPolicy
		if err = json.Unmarshal(raw, &p); err != nil {
			return "", err
		}
		if p.Effect == "deny" {
			return "deny", nil
		}
		score := 0
		if p.RuntimeID != "*" {
			score++
		}
		if p.Repository != "*" {
			score++
		}
		if score > best || score == best && p.Effect == "ask" {
			best = score
			effect = p.Effect
		}
	}
	return effect, rows.Err()
}

func savePermissionTx(ctx context.Context, tx *sql.Tx, r *model.PermissionRequest, event string) error {
	r.Version++
	raw, _ := json.Marshal(r)
	if _, err := tx.ExecContext(ctx, `INSERT INTO permission_request VALUES(?,?,?,?,?) ON CONFLICT(request_id) DO UPDATE SET state=excluded.state,data_json=excluded.data_json`, r.ID, r.TaskID, r.Fingerprint, r.State, raw); err != nil {
		return err
	}
	_, err := appendEventTx(ctx, tx, "task", r.TaskID, event, r.ID, r.TaskID, r)
	return err
}

// Gate before creating the run/outbox. Pending input stays undelivered, so approval
// resumes the same session without manufacturing a new user direction.
func permissionGateTx(ctx context.Context, tx *sql.Tx, req CreateRunRequest, last int64) (bool, error) {
	var grant *model.ExecutionGrant
	op, parent := "", ""
	if req.Environment != nil {
		grant = &req.Environment.Grant
		op = req.Environment.Profile
		parent = req.Environment.ParentTaskID
	} else if req.ExecutionGrant != nil {
		grant = req.ExecutionGrant
		op = "development.execute"
	}
	if grant == nil {
		return false, nil
	}
	task, err := getTaskTx(ctx, tx, req.TaskID)
	if err != nil {
		return false, err
	}
	r := model.PermissionRequest{TaskID: req.TaskID, ParentTaskID: parent, Title: task.Title, AgentID: req.AgentID, RuntimeID: req.RuntimeID, Operation: op, Repository: grant.Repository, PlanHash: grant.PlanHash, ReviewID: grant.ReviewID}
	raw, _ := json.Marshal([]any{r.TaskID, r.AgentID, r.RuntimeID, r.Operation, r.Repository, r.PlanHash, r.ReviewID, last, task.CurrentRevisionID})
	hash := sha256.Sum256(raw)
	r.Fingerprint = hex.EncodeToString(hash[:])
	if op == "windows_seekdb_phase0" {
		var capsRaw []byte
		if err = tx.QueryRowContext(ctx, `SELECT capabilities_json FROM runtime WHERE runtime_id=?`, req.RuntimeID).Scan(&capsRaw); err != nil {
			return false, err
		}
		var caps struct {
			Executors map[string]model.ExecutionCapability `json:"executors"`
		}
		if err = json.Unmarshal(capsRaw, &caps); err != nil {
			return false, err
		}
		if !caps.Executors[op].Available {
			reason := "运行机器未提供可用的 Windows 执行能力；需要配置执行器，不是授予 Agent sudo。"
			if _, err = tx.ExecContext(ctx, `UPDATE task_workflow SET scheduler_error=?,retry_at_ms=? WHERE task_id=?`, reason, time.Now().Add(30*time.Second).UnixMilli(), req.TaskID); err != nil {
				return false, err
			}
			if task.State != waitingEnvironment {
				err = setWorkStateTx(ctx, tx, req.TaskID, waitingEnvironment)
			}
			return true, err
		}
	}
	effect, err := policyEffectTx(ctx, tx, r)
	if err != nil {
		return false, err
	}
	old, lookup := readJSONRow[model.PermissionRequest](tx.QueryRowContext(ctx, `SELECT data_json FROM permission_request WHERE fingerprint=?`, r.Fingerprint))
	if lookup != nil && lookup != sql.ErrNoRows {
		return false, lookup
	}
	if lookup == nil {
		r = old
	}
	if effect == "allow" || effect != "deny" && r.State == "APPROVED" {
		if effect == "allow" {
			if _, err = appendEventTx(ctx, tx, "task", req.TaskID, "ExecutionPolicyAllowed", "", req.TaskID, r); err != nil {
				return false, err
			}
		}
		if lookup == nil && r.State != "USED" {
			r.State = "USED"
			if err = savePermissionTx(ctx, tx, &r, "ExecutionPermissionConsumed"); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	if lookup == sql.ErrNoRows {
		if err = invalidatePermissionsTx(ctx, tx, req.TaskID); err != nil {
			return false, err
		}
		r.ID = id.New("permission")
		r.State = "PENDING"
		r.CreatedAtMS = time.Now().UnixMilli()
		r.Reason = "需要执行授权；不会改变 Agent 沙箱、系统 sudo 配置，也不会取代方案审批。"
		if op == "windows_seekdb_phase0" {
			r.Reason = "在专用 Windows 环境运行固定 Phase 0 探针，串行切换 LongPathsEnabled=0/1 并恢复原值。仅授权此受控操作，不授予 Agent 管理员权限。"
		}
	}
	if effect == "deny" {
		r.State = "DENIED"
		r.Reason = "匹配的执行策略禁止此操作。请在执行权限页修改策略后重新检查。"
	}
	if err = setWorkStateTx(ctx, tx, req.TaskID, waitingAuthorization); err != nil {
		return false, err
	}
	fresh, err := getTaskTx(ctx, tx, req.TaskID)
	if err != nil {
		return false, err
	}
	r.TaskVersion = fresh.Version
	if _, err = tx.ExecContext(ctx, `UPDATE task_workflow SET scheduler_error=? WHERE task_id=?`, r.Reason, req.TaskID); err != nil {
		return false, err
	}
	return true, savePermissionTx(ctx, tx, &r, "ExecutionPermissionRequested")
}

func (s *Store) DecidePermission(ctx context.Context, requestID string, version int64, decision string, remember bool) error {
	if decision != "approve" && decision != "deny" {
		return fmt.Errorf("%w: invalid decision", model.ErrValidation)
	}
	return s.sourceWrite(ctx, func(tx *sql.Tx) error {
		r, err := readJSONRow[model.PermissionRequest](tx.QueryRowContext(ctx, `SELECT data_json FROM permission_request WHERE request_id=?`, requestID))
		if err != nil {
			return err
		}
		if r.Version != version || r.State != "PENDING" {
			return fmt.Errorf("%w: request already decided or changed", model.ErrConflict)
		}
		t, err := getTaskTx(ctx, tx, r.TaskID)
		if err != nil {
			return err
		}
		var paused bool
		if err = tx.QueryRowContext(ctx, `SELECT paused FROM task_workflow WHERE task_id=?`, r.TaskID).Scan(&paused); err != nil {
			return err
		}
		parent := r.TaskID
		if r.ParentTaskID != "" {
			parent = r.ParentTaskID
		}
		d, err := developmentTx(ctx, tx, parent)
		if err != nil {
			return err
		}
		var parentPaused bool
		if err = tx.QueryRowContext(ctx, `SELECT paused FROM task_workflow WHERE task_id=?`, parent).Scan(&parentPaused); err != nil {
			return err
		}
		if paused || parentPaused || t.Version != r.TaskVersion || t.State != waitingAuthorization || d.Phase != "IMPLEMENTING" || d.PlanHash != r.PlanHash || d.ApprovedReviewID != r.ReviewID {
			return fmt.Errorf("%w: task paused or approval scope changed", model.ErrConflict)
		}
		if decision == "approve" {
			effect, err := policyEffectTx(ctx, tx, r)
			if err != nil {
				return err
			}
			if effect == "deny" {
				return fmt.Errorf("%w: policy denies operation", model.ErrConflict)
			}
			if remember {
				p := model.ExecutionPolicy{Operation: r.Operation, RuntimeID: r.RuntimeID, Repository: r.Repository, Effect: "allow"}
				old, e := readJSONRow[model.ExecutionPolicy](tx.QueryRowContext(ctx, `SELECT data_json FROM execution_policy WHERE operation=? AND runtime_id=? AND repository=?`, p.Operation, p.RuntimeID, p.Repository))
				if e == nil {
					p.Version = old.Version
				} else if e != sql.ErrNoRows {
					return e
				}
				if _, err = savePolicyTx(ctx, tx, p); err != nil {
					return err
				}
			}
			r.State = "APPROVED"
			if err = setWorkStateTx(ctx, tx, r.TaskID, model.TaskStateQueued); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE task_workflow SET scheduler_error='',retry_at_ms=0 WHERE task_id=?`, r.TaskID); err != nil {
				return err
			}
		} else {
			r.State = "DENIED"
			if _, err = tx.ExecContext(ctx, `UPDATE task_workflow SET scheduler_error='执行授权已拒绝，未执行操作' WHERE task_id=?`, r.TaskID); err != nil {
				return err
			}
		}
		return savePermissionTx(ctx, tx, &r, "ExecutionPermissionDecided")
	})
}

// Explicitly re-evaluate a waiting request after policy/runtime configuration changes.
func (s *Store) RecheckPermission(ctx context.Context, requestID string, version int64) error {
	return s.sourceWrite(ctx, func(tx *sql.Tx) error {
		r, err := readJSONRow[model.PermissionRequest](tx.QueryRowContext(ctx, `SELECT data_json FROM permission_request WHERE request_id=?`, requestID))
		if err != nil {
			return err
		}
		if r.Version != version {
			return fmt.Errorf("%w: request changed", model.ErrConflict)
		}
		var paused bool
		if err = tx.QueryRowContext(ctx, `SELECT paused FROM task_workflow WHERE task_id=?`, r.TaskID).Scan(&paused); err != nil {
			return err
		}
		t, err := getTaskTx(ctx, tx, r.TaskID)
		if err != nil {
			return err
		}
		if paused || t.State != waitingAuthorization {
			return fmt.Errorf("%w: task is no longer waiting for permission", model.ErrConflict)
		}
		r.State = "PENDING"
		if err = savePermissionTx(ctx, tx, &r, "ExecutionPermissionRecheck"); err != nil {
			return err
		}
		return setWorkStateTx(ctx, tx, r.TaskID, model.TaskStateQueued)
	})
}
