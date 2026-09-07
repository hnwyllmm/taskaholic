package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"work-assistant/internal/model"
	"work-assistant/internal/workflow"
)

func migrateV16(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version >= 16 {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`CREATE TABLE publication(publication_key TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES task(task_id), state TEXT NOT NULL, next_attempt_ms INTEGER NOT NULL, data_json TEXT NOT NULL)`,
		`CREATE INDEX publication_due ON publication(state,next_attempt_ms)`,
		`CREATE TABLE publication_history(seq INTEGER PRIMARY KEY AUTOINCREMENT, publication_key TEXT NOT NULL REFERENCES publication(publication_key), version INTEGER NOT NULL, created_at_ms INTEGER NOT NULL, data_json TEXT NOT NULL)`,
		`INSERT INTO schema_version VALUES(16,unixepoch('subsec')*1000)`,
	} {
		if _, err = tx.Exec(q); err != nil {
			return fmt.Errorf("migrate v16: %w", err)
		}
	}
	return tx.Commit()
}

func publicationKey(parts ...string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(parts, "\x00"))))
}
func savePublicationTx(ctx context.Context, tx *sql.Tx, p model.Publication) error {
	p.UpdatedAtMS = time.Now().UnixMilli()
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO publication VALUES(?,?,?,?,?) ON CONFLICT(publication_key) DO UPDATE SET task_id=excluded.task_id,state=excluded.state,next_attempt_ms=excluded.next_attempt_ms,data_json=excluded.data_json`, p.Key, p.TaskID, p.State, p.NextAttemptMS, raw); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE task SET version=version+1,updated_at_ms=? WHERE task_id=? OR task_id IN (SELECT t.task_id FROM source_review r JOIN source_target t USING(target_id) WHERE r.task_id=?)`, p.UpdatedAtMS, p.TaskID, p.TaskID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO publication_history(publication_key,version,created_at_ms,data_json) VALUES(?,?,?,?)`, p.Key, p.Version, p.UpdatedAtMS, raw)
	return err
}
func queuePublicationTx(ctx context.Context, tx *sql.Tx, p model.Publication) error {
	old, err := readJSONRow[model.Publication](tx.QueryRowContext(ctx, `SELECT data_json FROM publication WHERE publication_key=?`, p.Key))
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	// Never lose the exact attempted body needed to reconcile an ambiguous POST.
	if old.State == "SUBMITTING" || old.State == "UNCERTAIN" || old.State == "BLOCKED" {
		return nil
	}
	if old.Body == p.Body && old.HeadSHA == p.HeadSHA && old.ReportHash == p.ReportHash && old.Verdict == p.Verdict {
		return nil
	}
	p.Version = old.Version + 1
	p.AppliedVersion = old.AppliedVersion
	p.RemoteID = old.RemoteID
	p.RemoteURL = old.RemoteURL
	p.AppliedBody = old.AppliedBody
	p.State = "QUEUED"
	return savePublicationTx(ctx, tx, p)
}

// Reconcile derives destinations from durable bindings, never URLs in model prose.
// Pollers remain read-only. History is append-only and included in SQLite backups.
func (s *Store) ReconcilePublications(ctx context.Context) error {
	return s.sourceWrite(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT r.task_id,r.role_id,r.head_sha,r.state,t.data_json,role.data_json,COALESCE((SELECT output FROM run WHERE task_id=r.task_id AND state='COMPLETED' ORDER BY created_at_ms DESC LIMIT 1),'') FROM source_review r JOIN source_target t USING(target_id) JOIN role USING(role_id) WHERE r.state!='SUPERSEDED' AND r.head_sha=json_extract(t.data_json,'$.head_sha') ORDER BY r.rowid`)
		if err != nil {
			return err
		}
		type review struct {
			task, role, head, state, output string
			target                          model.SourceTarget
			name                            string
		}
		var reviews []review
		for rows.Next() {
			var r review
			var targetRaw, roleRaw []byte
			if err = rows.Scan(&r.task, &r.role, &r.head, &r.state, &targetRaw, &roleRaw, &r.output); err != nil {
				rows.Close()
				return err
			}
			var role model.Role
			if err = json.Unmarshal(targetRaw, &r.target); err != nil {
				rows.Close()
				return err
			}
			if err = json.Unmarshal(roleRaw, &role); err != nil {
				rows.Close()
				return err
			}
			r.name = role.Name
			reviews = append(reviews, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, r := range reviews {
			key := publicationKey("pr-review", r.target.Entity, r.role)
			verdict := "评审中（尚无结论）"
			message := ""
			var result workflow.Result
			if r.output != "" {
				result, err = workflow.Parse(r.output)
				if err != nil {
					result = workflow.Result{Message: "报告格式无效，不能据此认定评审通过"}
				}
				message = result.Message
				switch result.ReviewDecision {
				case "passed":
					verdict = "通过"
				case "changes_requested":
					verdict = "不通过 · 需要修改"
				case "waiting_tests":
					verdict = "等待测试"
				default:
					verdict = "未通过 · 检查未完成或旧报告未声明结论"
				}
			}
			var active bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM run WHERE task_id=? AND state IN ('RUNNING','QUEUED')) OR EXISTS(SELECT 1 FROM task_message WHERE task_id=? AND delivery='PENDING')`, r.task, r.task).Scan(&active); err != nil {
				return err
			}
			if active {
				verdict = "重新评审中（之前结论不代表本轮通过）"
			}
			if r.state != "COMPLETED" && result.ReviewDecision == "passed" {
				verdict = "未通过 · 本轮尚未完成"
			}
			w, err := scanWork(tx.QueryRowContext(ctx, workSelect+` WHERE task_id=?`, r.task))
			if err != nil {
				return err
			}
			if w.Paused {
				verdict = "已暂停（不作为当前通过结论）"
			}
			pipelines, err := listJSONRows[model.TestPipeline](ctx, tx, `SELECT data_json FROM test_pipeline WHERE pr_target_id=? AND head_sha=? ORDER BY attempt`, r.target.ID, r.head)
			if err != nil {
				return err
			}
			var tests strings.Builder
			for _, p := range pipelines {
				fmt.Fprintf(&tests, "\n- 测试 #%d：%s · %s", p.PipelineID, p.State, p.URL)
			}
			var testsRequired bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM test_pipeline WHERE pr_target_id=?)`, r.target.ID).Scan(&testsRequired); err != nil {
				return err
			}
			if testsRequired && (len(pipelines) == 0 || pipelines[len(pipelines)-1].State != "success") && result.ReviewDecision == "passed" {
				verdict = "未通过 · 必需测试尚未成功"
			}
			body := fmt.Sprintf("<!-- work-assistant:review:%s -->\n## Code review · %s\n\n评审版本：`%s`\n\n结论：**%s**\n\n%s\n\n### 测试记录\n%s\n\n---\n这是 Agent 对上述 commit 的评审记录，不是 GitHub Approve，也不代表人工验收或允许合并。新 commit 必须重新评审；本评论持续更新，历次内容保存在任务记录中。", key, r.name, r.head, verdict, message, tests.String())
			decision := "pending"
			if verdict == "通过" {
				decision = "passed"
			}
			if err = queuePublicationTx(ctx, tx, model.Publication{Key: key, TaskID: r.task, Platform: "github", URL: r.target.Entity, HeadSHA: r.head, Body: body, Sticky: true, ReportHash: publicationKey(r.output), Verdict: decision}); err != nil {
				return err
			}
		}
		return reconcileIssuePublicationsTx(ctx, tx)
	})
}

func reconcileIssuePublicationsTx(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT se.task_id,se.entity,s.data_json FROM source_entity se JOIN task_source s USING(source_id) WHERE json_extract(s.data_json,'$.kind') IN ('antmultica','github') ORDER BY se.rowid`)
	if err != nil {
		return err
	}
	type binding struct {
		task, entity string
		source       model.TaskSource
	}
	var bindings []binding
	for rows.Next() {
		var b binding
		var raw []byte
		if err = rows.Scan(&b.task, &b.entity, &raw); err != nil {
			rows.Close()
			return err
		}
		if err = json.Unmarshal(raw, &b.source); err != nil {
			rows.Close()
			return err
		}
		bindings = append(bindings, b)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, b := range bindings {
		p := model.Publication{TaskID: b.task, Platform: b.source.Kind}
		if b.source.Kind == "antmultica" {
			parts := strings.Split(b.entity, ":")
			if len(parts) != 3 || parts[0] != "antmultica" || parts[1] != b.source.Config.WorkspaceID {
				continue
			}
			p.WorkspaceID = parts[1]
			p.IssueID = parts[2]
			p.ActorID = b.source.Config.AssigneeID
		} else {
			_, _, _, url, err := model.ParseGitHubIssue(b.entity)
			if err != nil {
				continue
			}
			p.URL = url
		}
		refs, _, err := taskReferences(ctx, tx, b.task)
		if err != nil {
			return err
		}
		destination := ""
		var links strings.Builder
		for _, ref := range refs {
			if ref.Kind == "antmultica.issue" {
				destination = ref.URL
			}
			if ref.Kind == "github.pr" {
				fmt.Fprintf(&links, "\n- PR：%s", ref.URL)
			}
		}
		if p.URL == "" {
			p.URL = destination
		}
		if p.URL == "" {
			continue
		}
		task, err := getTaskTx(ctx, tx, b.task)
		if err != nil {
			return err
		}
		var runID, output string
		err = tx.QueryRowContext(ctx, `SELECT run_id,output FROM run WHERE task_id=? AND state='COMPLETED' ORDER BY created_at_ms DESC LIMIT 1`, b.task).Scan(&runID, &output)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if runID == "" {
			continue
		} // Do not copy historical descriptions/credentials into comments.
		result, err := workflow.Parse(output)
		if err != nil {
			continue
		}
		if result.TaskUpdate == nil {
			continue
		} // Old free-form reports were not authored for external publication.
		u := result.TaskUpdate
		pipelines, err := listJSONRows[model.TestPipeline](ctx, tx, `SELECT data_json FROM test_pipeline WHERE task_id=? ORDER BY rowid`, b.task)
		if err != nil {
			return err
		}
		for _, pipeline := range pipelines {
			fmt.Fprintf(&links, "\n- Pipeline #%d：%s · %s · 被测 commit %s", pipeline.PipelineID, pipeline.State, pipeline.URL, pipeline.HeadSHA)
		}
		key := publicationKey("issue-progress", b.entity, b.task, runID, task.State, links.String())
		body := fmt.Sprintf("<!-- work-assistant:progress:%s -->\n工作助手处理进展\n\n状态：%s\n任务类型：%s\n\n以下是最近一轮已提交的分析，正在进行的后续修改尚不包含在此报告内。\n\n问题分析：%s\n\n实现/修复方案：%s\n\n修改理由：%s\n\n验证结果：%s\n\n阻塞/无法修复说明：%s\n\n关联交付：%s\n\n状态为工作助手记录；提交 PR 不代表已合并或工单已解决。", key, task.State, u.Kind, u.Analysis, u.Approach, u.Reason, u.Validation, u.BlockedReason, links.String())
		p.Key = key
		p.Body = body
		if err = queuePublicationTx(ctx, tx, p); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ListPublications(ctx context.Context, taskID string) ([]model.Publication, error) {
	if taskID == "" {
		return listJSONRows[model.Publication](ctx, s.db, `SELECT data_json FROM publication ORDER BY rowid`)
	}
	return listJSONRows[model.Publication](ctx, s.db, `SELECT data_json FROM publication WHERE task_id=? OR task_id IN (SELECT r.task_id FROM source_review r JOIN source_target t USING(target_id) WHERE t.task_id=?) ORDER BY rowid`, taskID, taskID)
}
func (s *Store) ClaimPublication(ctx context.Context, key string) (model.Publication, error) {
	var p model.Publication
	err := s.sourceWrite(ctx, func(tx *sql.Tx) error {
		var err error
		p, err = readJSONRow[model.Publication](tx.QueryRowContext(ctx, `SELECT data_json FROM publication WHERE publication_key=?`, key))
		if err != nil {
			return err
		}
		if p.State == "SYNCED" || p.State == "BLOCKED" || p.NextAttemptMS > time.Now().UnixMilli() {
			return model.ErrConflict
		}
		// SUBMITTING surviving a process crash is ambiguous; never POST again.
		reconcile := p.State == "SUBMITTING" || p.State == "UNCERTAIN"
		p.State = "SUBMITTING"
		p.NextAttemptMS = time.Now().Add(time.Minute).UnixMilli()
		if err = savePublicationTx(ctx, tx, p); err != nil {
			return err
		}
		if reconcile {
			p.State = "UNCERTAIN"
		}
		return nil
	})
	return p, err
}
func (s *Store) FinishPublication(ctx context.Context, p model.Publication, remoteID, remoteURL, state, message string) error {
	return s.sourceWrite(ctx, func(tx *sql.Tx) error {
		current, err := readJSONRow[model.Publication](tx.QueryRowContext(ctx, `SELECT data_json FROM publication WHERE publication_key=?`, p.Key))
		if err != nil {
			return err
		}
		if current.Version != p.Version {
			return model.ErrConflict
		}
		current.State = state
		current.Error = message
		current.NextAttemptMS = time.Now().Add(time.Minute).UnixMilli()
		if remoteID != "" {
			current.RemoteID = remoteID
			current.RemoteURL = remoteURL
		}
		if state == "SYNCED" {
			current.AppliedVersion = p.Version
			current.AppliedBody = p.Body
			current.Error = ""
		}
		return savePublicationTx(ctx, tx, current)
	})
}
