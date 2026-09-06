package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"work-assistant/internal/model"
	"work-assistant/internal/rolebuilder"
)

func roleTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	err = s.RegisterRuntime(context.Background(), model.RuntimeHello{RuntimeID: "role-runtime", Epoch: "epoch-role", Capabilities: map[string]any{"adapters": map[string]any{"codex-agent": map[string]any{"role_instructions": true, "structured_output": true, "read_only_runs": true}, "exec-agent": map[string]any{}}}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func sampleRole(capability string) model.RoleSpec {
	return model.RoleSpec{Name: capability, Description: "DISPLAY_ONLY_TEXT", Capabilities: []string{capability}, Instructions: "Check evidence before reporting.", OutputContract: "Findings with evidence", Boundaries: []string{"Do not merge."}}
}

func publishTestRole(t *testing.T, s *Store, capability string) model.Role {
	t.Helper()
	ctx := context.Background()
	draft, err := s.CreateRoleDraft(ctx, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	draft, err = s.UpdateRoleDraft(ctx, draft.ID, draft.Version, sampleRole(capability))
	if err != nil {
		t.Fatal(err)
	}
	role, err := s.PublishRoleDraft(ctx, draft.ID, draft.Version)
	if err != nil {
		t.Fatal(err)
	}
	return role
}

func TestRoleDraftGenerationLifecycleAndBackup(t *testing.T) {
	s := roleTestStore(t)
	ctx := context.Background()
	draft, err := s.CreateRoleDraft(ctx, "review agent", "", "create-key")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.CreateRoleDraft(ctx, "ignored", "", "create-key")
	if err != nil || replay.ID != draft.ID {
		t.Fatalf("draft replay: %+v %v", replay, err)
	}
	if _, err := s.PublishRoleDraft(ctx, draft.ID, draft.Version); !errors.Is(err, model.ErrValidation) {
		t.Fatalf("empty publish: %v", err)
	}
	if _, err := s.CreateRun(ctx, CreateRunRequest{TaskID: draft.TaskID, RuntimeID: "role-runtime"}); !errors.Is(err, model.ErrConflict) {
		t.Fatalf("builder bypass: %v", err)
	}
	draft, err = s.UpdateRoleDraft(ctx, draft.ID, draft.Version, sampleRole("code.review"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateRoleDraft(ctx, draft.ID, 1, sampleRole("code.implement")); !errors.Is(err, model.ErrConflict) {
		t.Fatalf("stale edit: %v", err)
	}
	request := CreateRunRequest{RuntimeID: "role-runtime", AdapterID: "codex-agent", ModelID: "test-model", IdempotencyKey: "turn-1"}
	generating, run, err := s.StartRoleDraftRun(ctx, draft.ID, draft.Version, "Require tests", request, rolebuilder.JSONBuilder{})
	if err != nil {
		t.Fatal(err)
	}
	if generating.State != "GENERATING" || len(generating.Messages) != 1 {
		t.Fatalf("generation: %+v", generating)
	}
	_, sameRun, err := s.StartRoleDraftRun(ctx, draft.ID, draft.Version, "Require tests", request, rolebuilder.JSONBuilder{})
	if err != nil || sameRun.ID != run.ID {
		t.Fatalf("generation replay: %+v %v", sameRun, err)
	}
	if _, err := s.PublishRoleDraft(ctx, draft.ID, generating.Version); !errors.Is(err, model.ErrConflict) {
		t.Fatalf("inflight publish: %v", err)
	}
	message, err := s.ClaimOutbox(ctx, "role-runtime", "test")
	if err != nil || message == nil {
		t.Fatalf("outbox: %v", err)
	}
	var spec model.RunSpec
	if err := json.Unmarshal(message.Params, &spec); err != nil {
		t.Fatal(err)
	}
	if !spec.ReadOnly || !json.Valid(spec.OutputSchema) || !strings.Contains(spec.Instructions, "Require tests") {
		t.Fatalf("builder RunSpec: %+v", spec)
	}
	if err := s.MarkOutboxDelivered(ctx, message.ID); err != nil {
		t.Fatal(err)
	}
	bind := model.RuntimeEvent{RuntimeID: "role-runtime", Epoch: "epoch-role", RuntimeSeq: 1, RunID: run.ID, TaskID: run.TaskID, SessionID: run.SessionID, Type: "session.bound", AgentSessionRef: "codex:native-builder"}
	if _, err := s.ApplyRuntimeEvent(ctx, bind); err != nil {
		t.Fatal(err)
	}
	failed := model.RuntimeEvent{RuntimeID: "role-runtime", Epoch: "epoch-role", RuntimeSeq: 2, RunID: run.ID, TaskID: run.TaskID, Type: "run.completed", Output: "not JSON"}
	if _, err := s.ApplyRuntimeEvent(ctx, failed); err != nil {
		t.Fatal(err)
	}
	draft, err = s.GetRoleDraft(ctx, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if draft.State != "FAILED" || draft.Spec.Name != "code.review" || draft.Error == "" {
		t.Fatalf("failure overwrote draft: %+v", draft)
	}
	request.IdempotencyKey = "turn-2"
	draft, nextRun, err := s.StartRoleDraftRun(ctx, draft.ID, draft.Version, "Return a valid draft", request, rolebuilder.JSONBuilder{})
	if err != nil {
		t.Fatal(err)
	}
	if nextRun.SessionID != run.SessionID || nextRun.AgentID != run.AgentID {
		t.Fatal("builder lost affinity")
	}
	message, err = s.ClaimOutbox(ctx, "role-runtime", "test")
	if err != nil || message == nil {
		t.Fatalf("next outbox: %v", err)
	}
	if err := json.Unmarshal(message.Params, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.AgentSessionRef != "codex:native-builder" {
		t.Fatalf("native ref: %s", spec.AgentSessionRef)
	}
	output, _ := json.Marshal(rolebuilder.Reply{Message: "Updated", Questions: []string{"Do you need Go-specific checks?"}, Draft: sampleRole("code.review")})
	event := model.RuntimeEvent{RuntimeID: "role-runtime", Epoch: "epoch-role", RuntimeSeq: 3, RunID: nextRun.ID, TaskID: nextRun.TaskID, Type: "run.completed", Output: string(output)}
	if _, err := s.ApplyRuntimeEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	if duplicate, err := s.ApplyRuntimeEvent(ctx, event); err != nil || !duplicate {
		t.Fatalf("dedupe %v %v", duplicate, err)
	}
	draft, err = s.GetRoleDraft(ctx, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if draft.State != "DRAFT" || len(draft.Messages) != 3 || len(draft.Questions) != 1 {
		t.Fatalf("result: %+v", draft)
	}
	role, err := s.PublishRoleDraft(ctx, draft.ID, draft.Version)
	if err != nil {
		t.Fatal(err)
	}
	secondRole, err := s.PublishRoleDraft(ctx, draft.ID, draft.Version)
	if err != nil || secondRole.ID != role.ID {
		t.Fatalf("publish replay: %+v %v", secondRole, err)
	}
	copyDraft, err := s.CreateRoleDraft(ctx, "", role.ID, "")
	if err != nil || copyDraft.Spec.Name != role.Name || len(copyDraft.Messages) != 0 {
		t.Fatalf("clone: %+v %v", copyDraft, err)
	}
	_, err = s.CreateAgent(ctx, model.AgentProfile{Name: "reviewer", RoleID: role.ID, RuntimeID: "role-runtime", AdapterID: "codex-agent"})
	if err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(t.TempDir(), "roles.sqlite")
	if err := s.Backup(ctx, backupPath); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	gotRole, err := restored.GetRole(ctx, role.ID)
	if err != nil || gotRole.Instructions != role.Instructions {
		t.Fatalf("restore role: %+v %v", gotRole, err)
	}
	gotDraft, err := restored.GetRoleDraft(ctx, draft.ID)
	if err != nil || gotDraft.State != "PUBLISHED" || len(gotDraft.Messages) != 3 {
		t.Fatalf("restore draft: %+v %v", gotDraft, err)
	}
	agents, err := restored.ListAgents(ctx)
	if err != nil || len(agents) != 1 {
		t.Fatalf("restore agents: %+v %v", agents, err)
	}
}

func TestRoleAssignmentConstraintsSnapshotAndCapacity(t *testing.T) {
	s := roleTestStore(t)
	ctx := context.Background()
	role := publishTestRole(t, s, "code.review")
	agent, err := s.CreateAgent(ctx, model.AgentProfile{Name: "reviewer", RoleID: role.ID, RuntimeID: "role-runtime", AdapterID: "codex-agent", ModelID: "review-model"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAgent(ctx, model.AgentProfile{Name: "exec-role", RoleID: role.ID, RuntimeID: "role-runtime", AdapterID: "exec-agent"}); !errors.Is(err, model.ErrValidation) {
		t.Fatalf("adapter ignored instructions: %v", err)
	}
	newTask := func(needs model.TaskRequirements) model.Task {
		task, _, err := s.CreateTask(ctx, "", "work", "do work", needs)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	request := func(task model.Task) CreateRunRequest {
		return CreateRunRequest{TaskID: task.ID, AgentID: agent.ID, RuntimeID: agent.RuntimeID, AdapterID: agent.AdapterID, ModelID: agent.ModelID}
	}
	wrongTask := newTask(model.TaskRequirements{Capabilities: []string{"code.implement"}})
	if _, err := s.CreateRun(ctx, request(wrongTask)); !errors.Is(err, model.ErrConflict) {
		t.Fatalf("wrong capability: %v", err)
	}
	excluded := newTask(model.TaskRequirements{Capabilities: []string{"code.review"}, ExcludedAgentIDs: []string{agent.ID}})
	if _, err := s.CreateRun(ctx, request(excluded)); !errors.Is(err, model.ErrConflict) {
		t.Fatalf("self review: %v", err)
	}
	needs := model.TaskRequirements{RoleID: role.ID, Capabilities: []string{"code.review"}}
	firstTask, secondTask := newTask(needs), newTask(needs)
	var wg sync.WaitGroup
	results := make(chan model.Run, 2)
	failures := make(chan error, 2)
	for _, task := range []model.Task{firstTask, secondTask} {
		wg.Add(1)
		go func(task model.Task) {
			defer wg.Done()
			run, err := s.CreateRun(ctx, request(task))
			if err != nil {
				failures <- err
			} else {
				results <- run
			}
		}(task)
	}
	wg.Wait()
	close(results)
	close(failures)
	if len(results) != 1 || len(failures) != 1 {
		t.Fatalf("capacity race: %d success, %d failed", len(results), len(failures))
	}
	if err := <-failures; !errors.Is(err, model.ErrConflict) {
		t.Fatalf("capacity error: %v", err)
	}
	run := <-results
	if run.Role == nil || run.Role.ID != role.ID {
		t.Fatalf("run snapshot: %+v", run)
	}
	message, err := s.ClaimOutbox(ctx, "role-runtime", "test")
	if err != nil || message == nil {
		t.Fatalf("outbox: %v", err)
	}
	var spec model.RunSpec
	if err := json.Unmarshal(message.Params, &spec); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(spec.Instructions, "DISPLAY_ONLY_TEXT") || !strings.Contains(spec.Instructions, role.Instructions) {
		t.Fatalf("instructions not isolated: %s", spec.Instructions)
	}
	if _, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: "role-runtime", Epoch: "epoch-role", RuntimeSeq: 1, RunID: run.ID, Type: "run.completed", Output: "review passed"}); err != nil {
		t.Fatal(err)
	}
	resumed, err := s.CreateRun(ctx, CreateRunRequest{TaskID: run.TaskID, RuntimeID: agent.RuntimeID, AdapterID: agent.AdapterID})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.SessionID != run.SessionID || resumed.AgentID != agent.ID || resumed.ModelID != agent.ModelID || resumed.Role == nil || resumed.Role.Instructions != role.Instructions {
		t.Fatalf("resume lost snapshot: %+v", resumed)
	}
	detail, err := s.GetTaskDetail(ctx, run.TaskID)
	if err != nil || detail.Task.Requirements.RoleID != role.ID {
		t.Fatalf("requirements persistence: %+v %v", detail, err)
	}
}
