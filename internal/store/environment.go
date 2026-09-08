package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"work-assistant/internal/id"
	"work-assistant/internal/model"
	"work-assistant/internal/workflow"
)

func migrateV18(db *sql.DB) error {
	var v int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&v); err != nil || v >= 18 {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{`CREATE TABLE environment_job(task_id TEXT PRIMARY KEY REFERENCES task(task_id), parent_task_id TEXT NOT NULL REFERENCES task(task_id), source_run_id TEXT NOT NULL UNIQUE REFERENCES run(run_id), state TEXT NOT NULL, data_json TEXT NOT NULL)`, `INSERT INTO schema_version VALUES(18,unixepoch('subsec')*1000)`} {
		if _, err = tx.Exec(q); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func requestEnvironmentTx(ctx context.Context, tx *sql.Tx, taskID, runID string, r workflow.EnvironmentRequest) error {
	d, err := developmentTx(ctx, tx, taskID)
	if err != nil {
		return err
	}
	if d.Phase != "IMPLEMENTING" || d.ApprovedReviewID == "" || d.Repository != "oceanbase/seekdb" || r.Profile != "windows_seekdb_phase0" {
		return fmt.Errorf("%w: environment request needs current approved SeekDB development", model.ErrConflict)
	}
	var version int64
	var phase string
	if err = tx.QueryRowContext(ctx, `SELECT plan_version,phase FROM development_run WHERE run_id=? AND task_id=?`, runID, taskID).Scan(&version, &phase); err != nil {
		return err
	}
	if version != d.Version || phase != "IMPLEMENTING" {
		return fmt.Errorf("%w: stale development run cannot request environment actions", model.ErrConflict)
	}
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT task_id FROM environment_job WHERE source_run_id=?`, runID).Scan(&existing)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	var session, agent string
	if err = tx.QueryRowContext(ctx, `SELECT session_id,agent_id FROM run WHERE run_id=? AND task_id=? AND state='COMPLETED'`, runID, taskID).Scan(&session, &agent); err != nil {
		return err
	}
	parent, err := getTaskTx(ctx, tx, taskID)
	if err != nil {
		return err
	}
	child, err := createWorkTx(ctx, tx, CreateWorkRequest{Title: "Windows 验证 · " + truncateRunes(parent.Title, 100), Goal: "受控 Windows 执行子任务，不调用模型，不授予 Agent 宿主机权限。\n" + r.Reason, AgentID: agent, Source: "manager.environment", Key: "environment:" + runID})
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO task_edge VALUES(?,?,?,?,?,?)`, id.New("edge"), taskID, child.ID, model.TaskEdgeDecomposedInto, time.Now().UnixMilli(), runID); err != nil {
		return err
	}
	job := model.EnvironmentExecution{ParentTaskID: taskID, SourceSessionID: session, SourceRunID: runID, Profile: r.Profile, Grant: model.ExecutionGrant{ReviewID: d.ApprovedReviewID, PlanHash: d.PlanHash, Repository: d.Repository, BaseBranch: d.BaseBranch}}
	raw, _ := json.Marshal(job)
	if _, err = tx.ExecContext(ctx, `INSERT INTO environment_job VALUES(?,?,?,'QUEUED',?)`, child.ID, taskID, runID, raw); err != nil {
		return err
	}
	if _, err = insertMessageTx(ctx, tx, taskID, "system", "已创建受控 Windows 验证子任务："+child.ID+"。等待执行器回传结果，不需要开发 Agent 直接操作 sudo/WinRM。", runID, "RECORDED"); err != nil {
		return err
	}
	return setWorkStateTx(ctx, tx, taskID, model.TaskStateWaiting)
}

func environmentInstructionsTx(ctx context.Context, tx *sql.Tx, req *CreateRunRequest) error {
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM environment_job WHERE parent_task_id=? AND state IN ('QUEUED','RUNNING'))`, req.TaskID).Scan(&active); err != nil {
		return err
	}
	if active {
		return fmt.Errorf("%w: waiting for environment child; do not mutate its source snapshot", model.ErrConflict)
	}
	job, err := readJSONRow[model.EnvironmentExecution](tx.QueryRowContext(ctx, `SELECT data_json FROM environment_job WHERE task_id=?`, req.TaskID))
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	d, err := developmentTx(ctx, tx, job.ParentTaskID)
	if err != nil {
		return err
	}
	var paused bool
	if err = tx.QueryRowContext(ctx, `SELECT paused FROM task_workflow WHERE task_id=?`, job.ParentTaskID).Scan(&paused); err != nil {
		return err
	}
	if paused || d.Phase != "IMPLEMENTING" || d.ApprovedReviewID != job.Grant.ReviewID || d.PlanHash != job.Grant.PlanHash {
		return fmt.Errorf("%w: parent approval paused or changed", model.ErrConflict)
	}
	req.Environment = &job
	return nil
}

func finishEnvironmentTx(ctx context.Context, tx *sql.Tx, e model.RuntimeEvent, result workflow.Result) (bool, error) {
	job, err := readJSONRow[model.EnvironmentExecution](tx.QueryRowContext(ctx, `SELECT data_json FROM environment_job WHERE task_id=?`, e.TaskID))
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	status := "unavailable"
	message := "受控 Windows 执行未返回有效报告。" + e.Error
	if result.EnvironmentResult != nil && result.EnvironmentResult.Profile == job.Profile {
		status = result.EnvironmentResult.Status
		message = result.EnvironmentResult.Message + "\n源码快照 SHA256：" + result.EnvironmentResult.SnapshotSHA256 + "\n" + result.EnvironmentResult.Log
	}
	if status != "passed" && status != "failed" && status != "unavailable" && status != "interrupted" {
		status = "unavailable"
		message = "执行器返回未知结论，不能视为通过。"
	}
	// A completed execution is not a passing product test. Preserve its verdict.
	if _, err = tx.ExecContext(ctx, `UPDATE environment_job SET state=? WHERE task_id=?`, status, e.TaskID); err != nil {
		return true, err
	}
	if _, err = insertMessageTx(ctx, tx, e.TaskID, "system", "Windows 执行结论："+status+"\n"+truncateRunes(message, 12000), e.RunID, "RECORDED"); err != nil {
		return true, err
	}
	if err = setWorkStateTx(ctx, tx, e.TaskID, model.TaskStateCompleted); err != nil {
		return true, err
	}
	d, err := developmentTx(ctx, tx, job.ParentTaskID)
	if err != nil {
		return true, err
	}
	var paused, pending bool
	if err = tx.QueryRowContext(ctx, `SELECT paused,EXISTS(SELECT 1 FROM task_message WHERE task_id=? AND delivery='PENDING') FROM task_workflow WHERE task_id=?`, job.ParentTaskID, job.ParentTaskID).Scan(&paused, &pending); err != nil {
		return true, err
	}
	feedback := "Windows 执行器结果（不是人工批准，也不是产品完成）：" + status + "\n" + truncateRunes(message, 12000) + "\n详细报告在子任务 " + e.TaskID + "。请结合源码快照和日志继续原任务；测试失败需分析修复，不得虚报通过。"
	if paused || d.Phase != "IMPLEMENTING" || d.ApprovedReviewID != job.Grant.ReviewID || d.PlanHash != job.Grant.PlanHash || pending {
		_, err = insertMessageTx(ctx, tx, job.ParentTaskID, "system", "历史/暂停中的环境任务返回，未自动推进：\n"+feedback, e.RunID, "RECORDED")
		return true, err
	}
	_, err = messageWorkFromTx(ctx, tx, job.ParentTaskID, feedback, "environment-result:"+e.RunID, false, "system")
	return true, err
}

// Explicitly request an environment child for an already blocked approved task.
func (s *Store) RequestEnvironment(ctx context.Context, taskID string, expected int64, r workflow.EnvironmentRequest) error {
	if m, err := s.Maintenance(ctx); err != nil {
		return err
	} else if m != "" {
		return fmt.Errorf("%w: maintenance active", model.ErrConflict)
	}
	return s.sourceWrite(ctx, func(tx *sql.Tx) error {
		t, err := getTaskTx(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if t.Version != expected || t.State != model.TaskStateBlocked {
			return fmt.Errorf("%w: requires current blocked task version", model.ErrConflict)
		}
		if len(r.Reason) == 0 || len(r.Reason) > 2000 {
			return fmt.Errorf("%w: reason required", model.ErrValidation)
		}
		var busy bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_message WHERE task_id=? AND delivery='PENDING') OR EXISTS(SELECT 1 FROM run WHERE task_id=? AND state IN ('RUNNING','QUEUED'))`, taskID, taskID).Scan(&busy); err != nil {
			return err
		}
		if busy {
			return fmt.Errorf("%w: task busy", model.ErrConflict)
		}
		var run string
		if err = tx.QueryRowContext(ctx, `SELECT run_id FROM run WHERE task_id=? AND state='COMPLETED' ORDER BY created_at_ms DESC LIMIT 1`, taskID).Scan(&run); err != nil {
			return err
		}
		if err = requestEnvironmentTx(ctx, tx, taskID, run, r); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE task_workflow SET paused=0,scheduler_error='' WHERE task_id=?`, taskID)
		return err
	})
}

func pauseEnvironmentChildrenTx(ctx context.Context, tx *sql.Tx, parent string) error {
	rows, err := tx.QueryContext(ctx, `SELECT task_id FROM environment_job WHERE parent_task_id=? AND state IN ('QUEUED','RUNNING')`, parent)
	if err != nil {
		return err
	}
	var children []string
	for rows.Next() {
		var child string
		if err = rows.Scan(&child); err != nil {
			rows.Close()
			return err
		}
		children = append(children, child)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, child := range children {
		if err = pauseWorkTx(ctx, tx, child); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE environment_job SET state='interrupted' WHERE task_id=?`, child); err != nil {
			return err
		}
		var active bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM run WHERE task_id=? AND state IN ('QUEUED','RUNNING'))`, child).Scan(&active); err != nil {
			return err
		}
		if !active {
			if err = setWorkStateTx(ctx, tx, child, model.TaskStateCompleted); err != nil {
				return err
			}
		}
	}
	return nil
}

// A human-supplied diagnostic resumes an approved task without treating it as
// new requirements or fabricating successful tests. The original session owns memory.
func (s *Store) ResumeDevelopmentDiagnosis(ctx context.Context, taskID string, expected int64, diagnosis string) error {
	if len(diagnosis) == 0 || len(diagnosis) > 12000 {
		return model.ErrValidation
	}
	if m, err := s.Maintenance(ctx); err != nil {
		return err
	} else if m != "" {
		return model.ErrConflict
	}
	return s.sourceWrite(ctx, func(tx *sql.Tx) error {
		t, err := getTaskTx(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if t.Version != expected || t.State != model.TaskStateBlocked {
			return model.ErrConflict
		}
		d, err := developmentTx(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if d.Phase != "IMPLEMENTING" || d.ApprovedReviewID == "" {
			return model.ErrConflict
		}
		r, err := readJSONRow[model.Review](tx.QueryRowContext(ctx, `SELECT data_json FROM review WHERE review_id=? AND task_id=?`, d.ApprovedReviewID, taskID))
		if err != nil {
			return err
		}
		if r.State != "PLAN_APPROVED" || r.PlanHash != d.PlanHash || r.RunID != d.PlanRunID {
			return model.ErrConflict
		}
		var busy bool
		if err = tx.QueryRowContext(ctx, `SELECT (SELECT paused FROM task_workflow WHERE task_id=?) OR EXISTS(SELECT 1 FROM task_message WHERE task_id=? AND delivery='PENDING') OR EXISTS(SELECT 1 FROM run WHERE task_id=? AND state IN ('QUEUED','RUNNING')) OR EXISTS(SELECT 1 FROM environment_job WHERE parent_task_id=? AND state IN ('QUEUED','RUNNING'))`, taskID, taskID, taskID, taskID).Scan(&busy); err != nil {
			return err
		}
		if busy {
			return model.ErrConflict
		}
		_, err = messageWorkFromTx(ctx, tx, taskID, "用户补充的构建诊断（不是新的需求、权限或测试通过证明）：\n"+diagnosis+"\n沿用原 Session 和已批准方案；核验诊断后修正构建差异再申请验证，不升级工具链、不擅自扩大验收范围。", fmt.Sprintf("development-diagnosis:%s:%d", taskID, expected), false, "system")
		return err
	})
}
