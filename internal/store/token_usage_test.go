package store

import (
	"context"
	"path/filepath"
	"testing"

	"work-assistant/internal/model"
	"work-assistant/internal/workflow"
)

func TestTokenUsageMigrationBackfillsLegacyCodexProgress(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "control.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := s.CreateTask(ctx, "usage-migration", "legacy", "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RegisterRuntime(ctx, model.RuntimeHello{RuntimeID: "legacy-runtime", Epoch: "legacy-epoch", Hostname: "legacy", OS: "test", Arch: "test", Capabilities: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, CreateRunRequest{TaskID: task.ID, RuntimeID: "legacy-runtime", AdapterID: "codex-agent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: run.RuntimeID, Epoch: "legacy-epoch", RuntimeSeq: 1, RunID: run.ID, Type: "run.progress", Stream: "codex-usage", Message: `{"input_tokens":90,"cached_input_tokens":70,"output_tokens":10,"reasoning_output_tokens":3}`}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`DROP TABLE run_token_usage`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`DELETE FROM schema_version WHERE version=21`); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	usage, err := s.TaskTokenUsage(ctx, task.ID)
	if err != nil || usage.TotalTokens != 100 || usage.CachedInputTokens != 70 {
		t.Fatalf("backfilled usage = %#v, %v", usage, err)
	}
}

func TestTaskTokenUsagePersistsAndDoesNotDoubleCount(t *testing.T) {
	ctx := context.Background()
	s, agent, task := workFixture(t)
	run := startWork(t, s, agent, task)
	event := model.RuntimeEvent{
		RuntimeID: run.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 1, RunID: run.ID,
		TaskID: task.ID, Type: "run.progress", Usage: &model.TokenUsage{
			Provider: "codex", InputTokens: 100, CachedInputTokens: 60,
			OutputTokens: 25, ReasoningOutputTokens: 5,
		},
	}
	if duplicate, err := s.ApplyRuntimeEvent(ctx, event); err != nil || duplicate {
		t.Fatalf("usage event = duplicate %v, error %v", duplicate, err)
	}
	if duplicate, err := s.ApplyRuntimeEvent(ctx, event); err != nil || !duplicate {
		t.Fatalf("replayed usage event = duplicate %v, error %v", duplicate, err)
	}
	finishWork(t, s, run, 2, "review", "done")
	usage, err := s.TaskTokenUsage(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if usage.TotalTokens != 125 || usage.InputTokens != 100 || usage.CachedInputTokens != 60 || usage.OutputTokens != 25 || usage.ReasoningOutputTokens != 5 {
		t.Fatalf("totals = %#v", usage.TokenUsageTotals)
	}
	if usage.ReportedRuns != 1 || usage.UnreportedRuns != 0 || usage.PendingRuns != 0 || len(usage.Stages) != 1 || usage.Stages[0].Stage != "other" {
		t.Fatalf("coverage/stages = %#v", usage)
	}
	work, err := s.GetWorkDetail(ctx, task.ID)
	if err != nil || work.TokenUsage.TotalTokens != 125 {
		t.Fatalf("work detail usage = %#v, %v", work.TokenUsage, err)
	}
}

func TestTaskTokenUsageSeparatesPlanningAndIncludesChildren(t *testing.T) {
	ctx := context.Background()
	s, agent, parent := workFixture(t)
	if _, err := s.db.Exec(`INSERT INTO development(task_id,data_json) VALUES(?,?)`, parent.ID, `{"task_id":"`+parent.ID+`","phase":"PLANNING","version":1}`); err != nil {
		t.Fatal(err)
	}
	plan := startWork(t, s, agent, parent)
	if _, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: plan.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 1, RunID: plan.ID, Type: "run.progress", Usage: &model.TokenUsage{Provider: "codex", InputTokens: 10, OutputTokens: 2}}); err != nil {
		t.Fatal(err)
	}

	child, err := s.CreateWork(ctx, CreateWorkRequest{Title: "Review plan", Goal: "review", Requirements: model.TaskRequirements{RoleID: agent.RoleID}, Key: "usage-child"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO task_edge(edge_id,from_task_id,to_task_id,edge_type,created_at_ms) VALUES('usage-edge',?,?,?,1)`, parent.ID, child.ID, model.TaskEdgeDecomposedInto); err != nil {
		t.Fatal(err)
	}
	reviewer, err := s.CreateAgent(ctx, model.AgentProfile{Name: "usage reviewer", RoleID: agent.RoleID, RuntimeID: agent.RuntimeID, AdapterID: agent.AdapterID, ModelID: agent.ModelID, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	childRun, err := s.StartWorkRun(ctx, CreateRunRequest{TaskID: child.ID, AgentID: reviewer.ID, RuntimeID: reviewer.RuntimeID, AdapterID: reviewer.AdapterID, ModelID: reviewer.ModelID}, workflow.JSONContract{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: childRun.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 2, RunID: childRun.ID, Type: "run.progress", Usage: &model.TokenUsage{Provider: "codex", InputTokens: 20, OutputTokens: 3}}); err != nil {
		t.Fatal(err)
	}
	usage, err := s.TaskTokenUsage(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if usage.TotalTokens != 35 || usage.ReportedRuns != 2 || len(usage.Stages) != 2 {
		t.Fatalf("aggregate = %#v", usage)
	}
}
