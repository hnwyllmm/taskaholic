package store

import (
	"context"
	"database/sql"

	"work-assistant/internal/model"
)

// Only containment edges define work-list hierarchy. A dependency or another
// association must not hide an independently actionable task. Both endpoints
// must have workbenches so every hidden child has a navigable parent.
const workHierarchyEdges = `SELECT e.from_task_id,e.to_task_id FROM task_edge e
	JOIN task_workflow p ON p.task_id=e.from_task_id
	JOIN task_workflow c ON c.task_id=e.to_task_id
	WHERE e.edge_type IN ('DECOMPOSED_INTO','DELEGATED_TO','REVIEWS')`

func (s *Store) ListRootWork(ctx context.Context) ([]model.WorkTaskItem, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Filter before LIMIT: many new review tasks cannot evict an older root.
	return listWorkItemsTx(ctx, tx, `SELECT t.task_id FROM task t
		JOIN task_workflow w ON w.task_id=t.task_id
		WHERE NOT EXISTS(SELECT 1 FROM hierarchy e WHERE e.to_task_id=t.task_id)
		ORDER BY t.created_at_ms DESC,t.task_id DESC LIMIT 500`)
}

func listWorkItemsTx(ctx context.Context, tx *sql.Tx, seed string, args ...any) ([]model.WorkTaskItem, error) {
	rows, err := tx.QueryContext(ctx, `WITH RECURSIVE hierarchy AS (`+workHierarchyEdges+`),
		seeds AS (`+seed+`),
		descendants(root_id,task_id) AS (
			SELECT task_id,task_id FROM seeds
			UNION
			SELECT d.root_id,e.to_task_id FROM descendants d
			JOIN hierarchy e ON e.from_task_id=d.task_id
		),
		progress AS (
			SELECT d.root_id,
				SUM(CASE WHEN COALESCE(r.state,'')!='SUPERSEDED' THEN 1 ELSE 0 END) total,
				SUM(CASE WHEN COALESCE(r.state,'')!='SUPERSEDED' AND t.state='COMPLETED' THEN 1 ELSE 0 END) completed,
				SUM(CASE WHEN COALESCE(r.state,'')!='SUPERSEDED' AND t.state IN ('BLOCKED','WAITING_ENVIRONMENT') THEN 1 ELSE 0 END) blocked,
				SUM(CASE WHEN COALESCE(r.state,'')!='SUPERSEDED' AND t.state='WAITING_REVIEW' THEN 1 ELSE 0 END) waiting_review,
				SUM(CASE WHEN COALESCE(r.state,'')!='SUPERSEDED' AND t.state IN ('WAITING_INPUT','WAITING_AUTHORIZATION') THEN 1 ELSE 0 END) waiting_input,
				SUM(CASE WHEN r.task_id IS NULL AND t.state='WAITING_TESTS' THEN 1 ELSE 0 END) waiting_tests,
				SUM(CASE WHEN r.state='SUPERSEDED' THEN 1 ELSE 0 END) superseded,
				MAX(t.updated_at_ms) updated_at_ms
			FROM descendants d JOIN task t ON t.task_id=d.task_id
			LEFT JOIN source_review r ON r.task_id=t.task_id
			WHERE d.task_id!=d.root_id GROUP BY d.root_id
		)
		SELECT t.task_id,t.title,t.goal,t.state,t.version,t.current_revision_id,
			COALESCE(t.assigned_agent_id,''),t.created_at_ms,t.updated_at_ms,t.requirements_json,w.agent_id,
			COALESCE(p.total,0),COALESCE(p.completed,0),COALESCE(p.blocked,0),
			COALESCE(p.waiting_review,0),COALESCE(p.waiting_input,0),COALESCE(p.waiting_tests,0),COALESCE(p.superseded,0),
			COALESCE(p.updated_at_ms,0),COALESCE(r.state,''),COALESCE(r.head_sha,''),
			COALESCE((SELECT MIN(rr.started_at_ms) FROM run rr WHERE rr.task_id=t.task_id),0),
			CASE WHEN t.state='COMPLETED' THEN COALESCE(
				(SELECT MAX(ts.completed_at_ms) FROM task_summary ts WHERE ts.task_id=t.task_id),
				t.updated_at_ms
			) ELSE 0 END
		FROM seeds s JOIN task t ON t.task_id=s.task_id
		JOIN task_workflow w ON w.task_id=t.task_id
		LEFT JOIN progress p ON p.root_id=t.task_id
		LEFT JOIN source_review r ON r.task_id=t.task_id
		ORDER BY t.created_at_ms DESC,t.task_id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []model.WorkTaskItem{}
	for rows.Next() {
		var item model.WorkTaskItem
		p := &item.Subtasks
		if err = scanTask(rows, &item.Task, &item.PreferredAgentID, &p.Total, &p.Completed,
			&p.Blocked, &p.WaitingReview, &p.WaitingInput, &p.WaitingTests, &p.Superseded, &p.UpdatedAtMS,
			&item.SourceReviewState, &item.ReviewHeadSHA, &item.FirstRunAtMS, &item.CompletedAtMS); err != nil {
			return nil, err
		}
		p.NeedsAttention = p.Blocked + p.WaitingReview + p.WaitingInput
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) GetWorkHierarchy(ctx context.Context, taskID string) (model.WorkHierarchy, error) {
	h := model.WorkHierarchy{TaskID: taskID, Parents: []model.WorkTaskLink{}, Roots: []model.WorkTaskLink{}, Children: []model.WorkTaskItem{}}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return h, err
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(ctx, `SELECT t.version FROM task t
		JOIN task_workflow w ON w.task_id=t.task_id WHERE t.task_id=?`, taskID).Scan(&h.TaskVersion); err != nil {
		return h, err
	}
	rows, err := tx.QueryContext(ctx, `WITH RECURSIVE hierarchy AS (`+workHierarchyEdges+`),
		ancestors(task_id) AS (
			SELECT from_task_id FROM hierarchy WHERE to_task_id=?
			UNION SELECT e.from_task_id FROM hierarchy e JOIN ancestors a ON e.to_task_id=a.task_id
		)
		SELECT t.task_id,t.title,
			EXISTS(SELECT 1 FROM hierarchy e WHERE e.to_task_id=? AND e.from_task_id=t.task_id),
			NOT EXISTS(SELECT 1 FROM hierarchy e WHERE e.to_task_id=t.task_id)
		FROM ancestors a JOIN task t ON t.task_id=a.task_id
		WHERE t.task_id!=? ORDER BY t.created_at_ms,t.task_id`, taskID, taskID, taskID)
	if err != nil {
		return h, err
	}
	for rows.Next() {
		var link model.WorkTaskLink
		var parent, root bool
		if err = rows.Scan(&link.ID, &link.Title, &parent, &root); err != nil {
			rows.Close()
			return h, err
		}
		if parent {
			h.Parents = append(h.Parents, link)
		}
		if root {
			h.Roots = append(h.Roots, link)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return h, err
	}
	h.Children, err = listWorkItemsTx(ctx, tx,
		`SELECT DISTINCT e.to_task_id AS task_id FROM hierarchy e WHERE e.from_task_id=? AND e.to_task_id!=?`, taskID, taskID)
	return h, err
}
