package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"work-assistant/internal/model"
	"work-assistant/internal/workflow"
)

// Only internal review tasks may be reopened, never the user's accepted task.
// Session binding is left intact. Eight completed rounds per revision bounds
// automatic discussion; manual holds and transport failures are never undone.
func continuePRReviewsTx(ctx context.Context, tx *sql.Tx, target model.SourceTarget, key, message string) error {
	parent, err := getTaskTx(ctx, tx, target.TaskID)
	if err != nil {
		return err
	}
	if parent.State == model.TaskStateCompleted {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT r.task_id FROM source_review r JOIN task_workflow w USING(task_id) WHERE r.target_id=? AND r.head_sha=? AND r.state IN ('COMPLETED','WAITING_TESTS','PENDING') AND w.paused=0`, target.ID, target.HeadSHA)
	if err != nil {
		return err
	}
	var tasks []string
	for rows.Next() {
		var task string
		if err = rows.Scan(&task); err != nil {
			rows.Close()
			return err
		}
		tasks = append(tasks, task)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, task := range tasks {
		current, err := getTaskTx(ctx, tx, task)
		if err != nil {
			return err
		}
		var exists bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM idempotency_key WHERE scope=? AND key=?)`, "work.message:"+task, key).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		var rounds int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM run WHERE task_id=? AND state='COMPLETED'`, task).Scan(&rounds); err != nil {
			return err
		}
		if strings.HasPrefix(key, "author-result:") {
			if current.State != model.TaskStateCompleted && current.State != model.TaskStateBlocked {
				continue
			}
			var output string
			if err = tx.QueryRowContext(ctx, `SELECT output FROM run WHERE task_id=? AND state='COMPLETED' ORDER BY created_at_ms DESC LIMIT 1`, task).Scan(&output); err != nil {
				return err
			}
			result, parseErr := workflow.Parse(output)
			if parseErr != nil {
				return parseErr
			}
			if result.ReviewDecision == "passed" || result.ReviewDecision == "waiting_tests" || result.ReviewDecision == "" {
				continue
			}
		}
		if rounds >= 8 {
			if _, err = tx.ExecContext(ctx, `UPDATE task_workflow SET paused=1,scheduler_error='同版本自动评审已达 8 轮，请核对未解决的问题后手动继续' WHERE task_id=?`, task); err != nil {
				return err
			}
			if err = setWorkStateTx(ctx, tx, task, model.TaskStateBlocked); err != nil {
				return err
			}
			if _, err = insertMessageTx(ctx, tx, target.TaskID, "system", "PR 自动评审已达 8 轮仍有新讨论，需要核对："+target.Entity+"\n评审任务："+task, "", "RECORDED"); err != nil {
				return err
			}
			continue
		}
		if current.State == model.TaskStateCompleted {
			if err = setWorkStateTx(ctx, tx, task, model.TaskStateQueued); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE source_review SET state='PENDING',feedback_sent=0 WHERE task_id=?`, task); err != nil {
			return err
		}
		if _, err = taskEventMessageTx(ctx, tx, task, "继续此 PR 的评审，沿用原 Session。重新核对版本、讨论和测试，不凭旧结论自动通过。\n"+target.Entity+"\n固定版本："+target.HeadSHA+"\n"+truncateRunes(message, 6000), key, "system"); err != nil {
			return err
		}
	}
	return nil
}

func guardReviewPublicationsTx(ctx context.Context, tx *sql.Tx, taskID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT r.role_id,r.head_sha,t.data_json,COALESCE((SELECT output FROM run WHERE task_id=r.task_id AND state='COMPLETED' ORDER BY created_at_ms DESC LIMIT 1),'') FROM source_review r JOIN source_target t USING(target_id) WHERE t.task_id=? AND r.head_sha=json_extract(t.data_json,'$.head_sha') AND r.state!='SUPERSEDED'`, taskID)
	if err != nil {
		return err
	}
	type check struct{ key, head, output string }
	var checks []check
	for rows.Next() {
		var role, head, output string
		var raw []byte
		if err = rows.Scan(&role, &head, &raw, &output); err != nil {
			rows.Close()
			return err
		}
		var target model.SourceTarget
		if err = json.Unmarshal(raw, &target); err != nil {
			rows.Close()
			return err
		}
		checks = append(checks, check{publicationKey("pr-review", target.Entity, role), head, output})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, c := range checks {
		result, err := workflow.Parse(c.output)
		if err != nil {
			return fmt.Errorf("%w: 评审还没有有效报告", model.ErrConflict)
		}
		if result.ReviewDecision == "" {
			continue
		} // Historical results keep their original acceptance policy.
		if result.ReviewDecision != "passed" {
			return fmt.Errorf("%w: 当前版本 Agent 评审未通过", model.ErrConflict)
		}
		p, err := readJSONRow[model.Publication](tx.QueryRowContext(ctx, `SELECT data_json FROM publication WHERE publication_key=?`, c.key))
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if p.HeadSHA != c.head || p.State != "SYNCED" || p.AppliedVersion != p.Version || p.ReportHash != publicationKey(c.output) || p.Verdict != "passed" {
			return fmt.Errorf("%w: 评审结果尚未成功回写 PR", model.ErrConflict)
		}
	}
	return nil
}
