package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const (
	deliveryGatePending = "pending"
	deliveryGateFailed  = "failed"
)

// currentDeliveryGateTx projects external CI and requested regression tests
// onto the original development task. Review tasks never call or own this
// gate: they only contribute a role verdict and, for QA, may create a test
// request.
func currentDeliveryGateTx(ctx context.Context, tx *sql.Tx, taskID string) (string, string, error) {
	state, details := "", []string{}
	rows, err := tx.QueryContext(ctx, `SELECT target.entity,COALESCE(json_extract(target.cursor_json,'$.ci_state'),'')
		FROM source_target target JOIN task_source source USING(source_id)
		WHERE target.task_id=? AND target.enabled=1
		  AND json_extract(source.data_json,'$.kind')='github'
		  AND COALESCE(json_extract(target.data_json,'$.head_sha'),'')!=''`, taskID)
	if err != nil {
		return "", "", err
	}
	for rows.Next() {
		var entity, ci string
		if err = rows.Scan(&entity, &ci); err != nil {
			rows.Close()
			return "", "", err
		}
		switch ci {
		case "failed":
			state = deliveryGateFailed
			details = append(details, fmt.Sprintf("GitHub CI failed: %s", entity))
		case "pending", "":
			if state == "" {
				state = deliveryGatePending
			}
			details = append(details, fmt.Sprintf("GitHub CI is still running: %s", entity))
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return "", "", err
	}
	rows.Close()

	rows, err = tx.QueryContext(ctx, `SELECT target.entity,COALESCE((
			SELECT pipeline.state FROM test_pipeline pipeline
			WHERE pipeline.pr_target_id=target.target_id
			  AND pipeline.head_sha=json_extract(target.data_json,'$.head_sha')
			ORDER BY pipeline.attempt DESC LIMIT 1
		),'missing')
		FROM source_target target
		WHERE target.task_id=?
		  AND EXISTS(SELECT 1 FROM test_pipeline any_pipeline WHERE any_pipeline.pr_target_id=target.target_id)`, taskID)
	if err != nil {
		return "", "", err
	}
	for rows.Next() {
		var entity, pipeline string
		if err = rows.Scan(&entity, &pipeline); err != nil {
			rows.Close()
			return "", "", err
		}
		pipeline = strings.ToLower(pipeline)
		if pipeline == "success" {
			continue
		}
		if pipeline == "failed" || pipeline == "error" || pipeline == "canceled" || pipeline == "cancelled" {
			state = deliveryGateFailed
			details = append(details, fmt.Sprintf("requested regression did not pass (%s): %s", pipeline, entity))
			continue
		}
		if state == "" {
			state = deliveryGatePending
		}
		details = append(details, fmt.Sprintf("requested regression is still pending (%s): %s", pipeline, entity))
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return "", "", err
	}
	rows.Close()
	return state, strings.Join(details, "\n"), nil
}

func outstandingSourceReviewsTx(ctx context.Context, tx *sql.Tx, taskID string) (int, error) {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM source_review review JOIN source_target target USING(target_id)
		WHERE target.task_id=? AND review.state!='SUPERSEDED'
		  AND review.head_sha=json_extract(target.data_json,'$.head_sha')
		  AND review.state!='COMPLETED'`, taskID).Scan(&count)
	return count, err
}
