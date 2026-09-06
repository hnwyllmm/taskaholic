package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"work-assistant/internal/model"
	"work-assistant/internal/workflow"
)

func completionOutput(t *testing.T, result string) string {
	t.Helper()
	raw, err := json.Marshal(workflow.Result{
		Outcome: "review", Message: "ready",
		Artifacts: []workflow.File{{Name: "result.md", Content: result}},
		Summary: &workflow.CompletionSummary{
			Result: result, Learnings: []string{"reusable lesson"}, Improvements: []string{"next improvement"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestTaskSummaryTimingAndReworkMetrics(t *testing.T) {
	ctx := context.Background()
	s, agent, task := workFixture(t)
	base := time.Now().Add(-2 * time.Minute).UnixMilli()
	if _, err := s.db.Exec(`UPDATE task SET created_at_ms=?,updated_at_ms=? WHERE task_id=?`, base, base, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE task_message SET created_at_ms=? WHERE task_id=?`, base, task.ID); err != nil {
		t.Fatal(err)
	}

	first := startWork(t, s, agent, task)
	if _, err := s.db.Exec(`UPDATE run SET created_at_ms=? WHERE run_id=?`, base+5_000, first.ID); err != nil {
		t.Fatal(err)
	}
	for _, event := range []model.RuntimeEvent{
		{RuntimeID: agent.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 1, RunID: first.ID, Type: "run.started", OccurredAt: base + 10_000},
		{RuntimeID: agent.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 2, RunID: first.ID, Type: "run.completed", OccurredAt: base + 30_000, Output: completionOutput(t, "first result")},
	} {
		if _, err := s.ApplyRuntimeEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`UPDATE event_log SET recorded_at_ms=CASE event_type WHEN 'RunStarted' THEN ? WHEN 'RunCompleted' THEN ? END WHERE aggregate_type='run' AND aggregate_id=? AND event_type IN ('RunStarted','RunCompleted')`, base+10_000, base+30_000, first.ID); err != nil {
		t.Fatal(err)
	}
	work, err := s.GetWorkDetail(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	firstReview, err := s.DecideReview(ctx, task.ID, work.Reviews[0].ID, "CHANGES_REQUESTED", "fix it")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE event_log SET recorded_at_ms=? WHERE aggregate_type='task' AND aggregate_id=? AND event_type='ReviewRequested' AND json_extract(payload_json,'$.review_id')=?`, base+30_000, task.ID, firstReview.ID); err != nil {
		t.Fatal(err)
	}
	firstReview.DecidedAtMS = base + 40_000
	firstReviewRaw, _ := json.Marshal(firstReview)
	if _, err = s.db.Exec(`UPDATE review SET data_json=? WHERE review_id=?`, firstReviewRaw, firstReview.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE task_message SET created_at_ms=? WHERE task_id=? AND speaker='user' AND seq=(SELECT MAX(seq) FROM task_message WHERE task_id=? AND speaker='user')`, base+40_000, task.ID, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE event_log SET recorded_at_ms=? WHERE aggregate_type='task' AND aggregate_id=? AND event_type='ReviewDecided' AND json_extract(payload_json,'$.review_id')=?`, base+40_000, task.ID, firstReview.ID); err != nil {
		t.Fatal(err)
	}

	second := startWork(t, s, agent, task)
	if _, err = s.db.Exec(`UPDATE run SET created_at_ms=? WHERE run_id=?`, base+45_000, second.ID); err != nil {
		t.Fatal(err)
	}
	for _, event := range []model.RuntimeEvent{
		{RuntimeID: agent.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 3, RunID: second.ID, Type: "run.started", OccurredAt: base + 50_000},
		{RuntimeID: agent.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 4, RunID: second.ID, Type: "run.completed", OccurredAt: base + 80_000, Output: completionOutput(t, "final result")},
	} {
		if _, err = s.ApplyRuntimeEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	work, err = s.GetWorkDetail(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondReview := work.Reviews[0]
	if _, err = s.db.Exec(`UPDATE event_log SET recorded_at_ms=CASE event_type WHEN 'RunStarted' THEN ? WHEN 'RunCompleted' THEN ? END WHERE aggregate_type='run' AND aggregate_id=? AND event_type IN ('RunStarted','RunCompleted')`, base+50_000, base+80_000, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE event_log SET recorded_at_ms=? WHERE aggregate_type='task' AND aggregate_id=? AND event_type='ReviewRequested' AND json_extract(payload_json,'$.review_id')=?`, base+80_000, task.ID, secondReview.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DecideReview(ctx, task.ID, secondReview.ID, "APPROVED", "accepted"); err != nil {
		t.Fatal(err)
	}
	summary, err := s.GetLatestTaskSummary(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	metrics := summary.Metrics
	if metrics.QueueWaitMS != 10_000 || metrics.RuntimeQueueWaitMS != 10_000 || metrics.ActiveRunTimeMS != 50_000 {
		t.Fatalf("duration metrics = %+v", metrics)
	}
	if metrics.RunCount != 2 || metrics.CompletedRunCount != 2 || metrics.ReviewRoundCount != 2 || metrics.ChangesRequestedCount != 1 || metrics.GuidanceMessageCount != 1 {
		t.Fatalf("count metrics = %+v", metrics)
	}
	if metrics.HumanReviewWaitMS < 45_000 || metrics.CycleTimeMS < 110_000 {
		t.Fatalf("human/cycle metrics = %+v", metrics)
	}
	foundRework := false
	for _, signal := range summary.Signals {
		foundRework = foundRework || signal.Code == "review_rework"
	}
	if !foundRework {
		t.Fatal("rework efficiency signal missing", summary.Signals)
	}
}

func TestV8BackfillsExistingCompletedTasks(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "control.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RegisterRuntime(ctx, model.RuntimeHello{RuntimeID: "role-runtime", Epoch: "epoch-role", Capabilities: map[string]any{"adapters": map[string]any{"codex-agent": map[string]any{"role_instructions": true, "structured_output": true, "read_only_runs": true}}}}); err != nil {
		t.Fatal(err)
	}
	agentRole := publishTestRole(t, s, "document.write")
	agent, err := s.CreateAgent(ctx, model.AgentProfile{Name: "backfill-writer", RoleID: agentRole.ID, RuntimeID: "role-runtime", AdapterID: "codex-agent", ModelID: "test-model", MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateWork(ctx, CreateWorkRequest{Title: "Old task", Goal: "Complete before v8"})
	if err != nil {
		t.Fatal(err)
	}
	run := startWork(t, s, agent, task)
	if _, err = s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: agent.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 1, RunID: run.ID, Type: "run.completed", Output: completionOutput(t, "old result")}); err != nil {
		t.Fatal(err)
	}
	work, _ := s.GetWorkDetail(ctx, task.ID)
	if _, err = s.DecideReview(ctx, task.ID, work.Reviews[0].ID, "APPROVED", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`DROP TABLE task_summary`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`DELETE FROM schema_version WHERE version=8`); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	summary, err := reopened.GetLatestTaskSummary(ctx, task.ID)
	if err != nil || summary.Result != "old result" || summary.SourceType != "review" {
		t.Fatalf("backfilled summary = %+v, %v", summary, err)
	}
	var version int
	if err = reopened.db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("schema version = %d, %v", version, err)
	}
}
