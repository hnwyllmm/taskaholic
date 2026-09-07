package store

import (
	"context"
	"path/filepath"
	"testing"
	"work-assistant/internal/model"
)

func enableReasoning(t *testing.T, s *Store, a model.AgentProfile) {
	t.Helper()
	if err := s.RegisterRuntime(context.Background(), model.RuntimeHello{RuntimeID: a.RuntimeID, Epoch: "epoch-role", Capabilities: map[string]any{"adapters": map[string]any{a.AdapterID: map[string]any{"reasoning_effort": true, "native_session": true, "role_instructions": true, "structured_output": true, "read_only_runs": true}}}}); err != nil {
		t.Fatal(err)
	}
}

func setEffort(t *testing.T, s *Store, a model.AgentProfile, modelID string, effort *string) model.AgentProfile {
	t.Helper()
	updated, err := s.UpdateAgent(context.Background(), a.ID, AgentUpdate{Name: a.Name, ModelID: modelID, ReasoningEffort: effort, MaxConcurrent: a.MaxConcurrent, State: a.State, ExpectedVersion: a.Version})
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func TestMemberEffortSnapshotSameSessionReviewAndBackup(t *testing.T) {
	ctx := context.Background()
	s, a, task := workFixture(t)
	enableReasoning(t, s, a)
	medium, high := "medium", "high"
	a = setEffort(t, s, a, a.ModelID, &medium)
	run := startWork(t, s, a, task)
	if run.ReasoningEffort != "medium" || runSpec(t, s, run.ID).ReasoningEffort != "medium" {
		t.Fatal(run)
	}
	a = setEffort(t, s, a, a.ModelID, &high)
	if queued, _ := s.GetRun(ctx, run.ID); queued.ReasoningEffort != "medium" {
		t.Fatal("member update rewrote queued run", queued)
	}
	if _, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: a.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 1, RunID: run.ID, SessionID: run.SessionID, Type: "session.bound", AgentSessionRef: "codex:original"}); err != nil {
		t.Fatal(err)
	}
	config := run.ExecutionSettings
	config.ExecutionConfigured = true
	config.ExecutionModelID = a.ModelID
	event := model.RuntimeEvent{RuntimeID: a.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 2, RunID: run.ID, Type: "run.configured", Execution: &config}
	if _, err := s.ApplyRuntimeEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	if duplicate, err := s.ApplyRuntimeEvent(ctx, event); err != nil || !duplicate {
		t.Fatal("configuration replay", duplicate, err)
	}
	finishWork(t, s, run, 3, "review", "delivered")
	w, err := s.GetWorkDetail(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := w.Reviews[0]
	q, err := s.MessageReview(ctx, task.ID, r.ID, "explain", "q", r.DiscussionVersion)
	if err != nil {
		t.Fatal(err)
	}
	chat, err := s.StartReviewTurn(ctx, q.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if chat.ReasoningEffort != "high" || chat.SessionID != run.SessionID || runSpec(t, s, chat.ID).AgentSessionRef != "codex:original" {
		t.Fatal("effort change lost session", chat)
	}
	finishReviewChat(t, s, chat, 4, "run.completed", `{"message":"explained"}`)
	r = currentReview(t, s, task.ID, r.ID)
	if _, err := s.DecideReview(ctx, task.ID, r.ID, "APPROVED", "done", r.DiscussionVersion); err != nil {
		t.Fatal(err)
	}
	summary, err := s.GetLatestTaskSummary(ctx, task.ID)
	if err != nil || summary.Executor.ReasoningEffort != "medium" || !summary.Executor.ExecutionConfigured {
		t.Fatal("summary must use delivery run, not later chat/member", summary, err)
	}
	copyPath := filepath.Join(t.TempDir(), "copy.sqlite")
	if err := s.Backup(ctx, copyPath); err != nil {
		t.Fatal(err)
	}
	copy, err := Open(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	stored, err := copy.GetRun(ctx, run.ID)
	if err != nil || stored.ReasoningEffort != "medium" || stored.ExecutionModelID != a.ModelID {
		t.Fatal(stored, err)
	}
	member, err := copy.GetAgent(ctx, a.ID)
	if err != nil || member.ReasoningEffort != "high" {
		t.Fatal(member, err)
	}
	session, err := copy.GetTaskSession(ctx, task.ID)
	if err != nil || session.AgentSessionRef != "codex:original" {
		t.Fatal(session, err)
	}
}

func TestEffortOverrideClearAndPinnedModelDefaults(t *testing.T) {
	ctx := context.Background()
	s, a, _ := workFixture(t)
	enableReasoning(t, s, a)
	high, low, empty := "high", "low", ""
	a = setEffort(t, s, a, a.ModelID, &high)
	task, _, err := s.CreateTask(ctx, "", "plain", "check")
	if err != nil {
		t.Fatal(err)
	}
	seq := int64(10)
	start := func(override *string) model.Run {
		t.Helper()
		run, err := s.CreateRun(ctx, CreateRunRequest{TaskID: task.ID, RuntimeID: a.RuntimeID, AdapterID: a.AdapterID, AgentID: a.ID, ModelID: "test-model", ReasoningEffort: override})
		if err != nil {
			t.Fatal(err)
		}
		seq++
		if _, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: a.RuntimeID, Epoch: "epoch-role", RuntimeSeq: seq, RunID: run.ID, Type: "run.completed"}); err != nil {
			t.Fatal(err)
		}
		return run
	}
	first := start(nil)
	if first.ReasoningEffort != "high" {
		t.Fatal(first)
	}
	a = setEffort(t, s, a, a.ModelID, nil)
	if a.ReasoningEffort != "high" {
		t.Fatal("omitted field cleared setting")
	}
	if r := start(&low); r.ReasoningEffort != "low" || r.SessionID != first.SessionID || r.ReasoningEffortSource != "run_override" {
		t.Fatal(r)
	}
	if r := start(&empty); r.ReasoningEffort != "" || r.ReasoningEffortSource != "run_override" {
		t.Fatal(r)
	}
	if r := start(nil); r.ReasoningEffort != "high" {
		t.Fatal("override leaked", r)
	}
	a = setEffort(t, s, a, "new-model", &low)
	if r := start(nil); r.ModelID != "test-model" || r.ReasoningEffort != "high" || r.ReasoningEffortSource != "session_default" {
		t.Fatal("new model default leaked into pinned session", r)
	}
	a = setEffort(t, s, a, a.ModelID, &empty)
	if a.ReasoningEffort != "" {
		t.Fatal(a)
	}
}

func TestReasoningMigrationPreservesLegacyData(t *testing.T) {
	ctx := context.Background()
	s, a, _ := workFixture(t)
	task, _, err := s.CreateTask(ctx, "", "legacy", "keep")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, CreateRunRequest{TaskID: task.ID, AgentID: a.ID, RuntimeID: a.RuntimeID, AdapterID: a.AdapterID, ModelID: a.ModelID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`ALTER TABLE run DROP COLUMN execution_json`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`DELETE FROM schema_version WHERE version>=12`); err != nil {
		t.Fatal(err)
	}
	if err = migrateV12(s.db); err != nil {
		t.Fatal(err)
	}
	if err = migrateV12(s.db); err != nil {
		t.Fatal("migration not idempotent", err)
	}
	got, err := s.GetRun(ctx, run.ID)
	if err != nil || got.SessionID != run.SessionID || got.ModelID != run.ModelID || got.ReasoningEffortSource != "" || got.ExecutionConfigured {
		t.Fatal("guessed historical effort", got, err)
	}
}
