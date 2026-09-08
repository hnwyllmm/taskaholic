package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"work-assistant/internal/concierge"
	"work-assistant/internal/model"
	"work-assistant/internal/workflow"
)

func bindSystem(t *testing.T, s *Store, slot string, a model.AgentProfile) model.SystemBinding {
	t.Helper()
	b, err := s.GetSystemBinding(context.Background(), slot)
	if err != nil {
		t.Fatal(err)
	}
	b.Mode, b.AgentID = "agent", a.ID
	b, err = s.UpdateSystemBinding(context.Background(), b, b.Version, a.RuntimeID)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func secondMember(t *testing.T, s *Store, a model.AgentProfile) model.AgentProfile {
	t.Helper()
	a.Name = "second-member"
	a.ModelID = "different-model"
	a, err := s.CreateAgent(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func runSpec(t *testing.T, s *Store, runID string) model.RunSpec {
	t.Helper()
	var raw []byte
	var spec model.RunSpec
	if err := s.db.QueryRow(`SELECT params_json FROM outbox_message WHERE method='run.start' AND json_extract(params_json,'$.run_id')=?`, runID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	return spec
}

func TestSystemBindingsValidationCASAndBackup(t *testing.T) {
	ctx := context.Background()
	s, a, _ := workFixture(t)
	defaults, err := s.ListSystemBindings(ctx)
	if err != nil || len(defaults) != len(model.SystemSlots) {
		t.Fatal(defaults, err)
	}
	b := bindSystem(t, s, "home_chat", a)
	if _, err = s.UpdateSystemBinding(ctx, b, b.Version-1, a.RuntimeID); !errors.Is(err, model.ErrConflict) {
		t.Fatal("missing CAS", err)
	}
	for _, bad := range []model.SystemBinding{{Slot: "not-a-slot", Mode: "auto"}, {Slot: "home_chat", Mode: "rules"}, {Slot: "home_chat", Mode: "agent"}, {Slot: "home_chat", Mode: "agent", AgentID: "missing"}, {Slot: "home_chat", Mode: "auto", AgentID: a.ID}} {
		if _, err = s.UpdateSystemBinding(ctx, bad, b.Version, a.RuntimeID); !errors.Is(err, model.ErrValidation) {
			t.Fatal("accepted invalid binding", bad, err)
		}
	}
	upgrade, _ := s.GetSystemBinding(ctx, "upgrade_builder")
	upgrade.Mode, upgrade.AgentID = "agent", a.ID
	if _, err = s.UpdateSystemBinding(ctx, upgrade, upgrade.Version, "another-machine"); !errors.Is(err, model.ErrValidation) {
		t.Fatal("remote local-upgrade builder accepted", err)
	}
	path := filepath.Join(t.TempDir(), "copy.sqlite")
	if err = s.Backup(ctx, path); err != nil {
		t.Fatal(err)
	}
	copy, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	persisted, err := copy.GetSystemBinding(ctx, "home_chat")
	if err != nil || persisted != b {
		t.Fatal("binding did not survive snapshot/reopen", persisted, err)
	}
	if _, err = s.UpdateAgent(ctx, a.ID, AgentUpdate{Name: a.Name, ModelID: a.ModelID, MaxConcurrent: 1, State: "DISABLED", ExpectedVersion: a.Version}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.UpdateSystemBinding(ctx, b, b.Version, a.RuntimeID); !errors.Is(err, model.ErrConflict) {
		t.Fatal("disabled member bound", err)
	}
}

func TestHomeExplicitExecutorHandoffKeepsHistoryAndNativeSessions(t *testing.T) {
	ctx := context.Background()
	s, a, _ := workFixture(t)
	other := secondMember(t, s, a)
	b := bindSystem(t, s, "home_chat", a)
	c, err := s.CreateHomeChat(ctx, "handoff")
	if err != nil {
		t.Fatal(err)
	}
	request := CreateRunRequest{SystemBinding: &b, AgentID: a.ID, RuntimeID: a.RuntimeID, AdapterID: a.AdapterID, ModelID: a.ModelID, IdempotencyKey: "first"}
	c, err = s.StartHomeRun(ctx, c.ID, c.Version, "请记住这是交接测试", "ready", request, concierge.JSONAssistant{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.GetRun(ctx, c.LastRunID)
	if err != nil {
		t.Fatal(err)
	}
	if first.AgentID != a.ID || first.ModelID != a.ModelID {
		t.Fatal(first)
	}
	if spec := runSpec(t, s, first.ID); !spec.ReadOnly || len(spec.OutputSchema) == 0 || len(spec.Command) != 0 {
		t.Fatal("unsafe system run", spec)
	}
	b2 := bindSystem(t, s, "home_chat", other)
	newRequest := CreateRunRequest{SystemBinding: &b2, AgentID: other.ID, RuntimeID: other.RuntimeID, AdapterID: other.AdapterID, ModelID: other.ModelID}
	if _, err = s.SwitchHomeExecutor(ctx, c.ID, c.Version, newRequest); !errors.Is(err, model.ErrConflict) {
		t.Fatal("switched running chat", err)
	}
	if _, err = s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: a.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 1, RunID: first.ID, SessionID: first.SessionID, Type: "session.bound", AgentSessionRef: "native-original"}); err != nil {
		t.Fatal(err)
	}
	c = homeResult(t, s, c, 2, &model.HomeAction{Kind: "create_work", Title: "old proposal", Text: "write something"})
	oldSession, err := s.GetTaskSession(ctx, c.TaskID)
	if err != nil || oldSession.AgentID != a.ID {
		t.Fatal("config changed ownership", err)
	}
	sticky := request
	sticky.SystemBinding = nil
	sticky.SessionID = oldSession.ID
	sticky.IdempotencyKey = "continue-original"
	c, err = s.StartHomeRun(ctx, c.ID, c.Version, "继续原来的讨论", "ready", sticky, concierge.JSONAssistant{})
	if err != nil {
		t.Fatal(err)
	}
	continued, _ := s.GetRun(ctx, c.LastRunID)
	if continued.SessionID != first.SessionID || continued.AgentID != a.ID {
		t.Fatal("not sticky", continued)
	}
	c = homeResult(t, s, c, 3, &model.HomeAction{Kind: "create_work", Title: "pending", Text: "proposed work"})
	beforeMessages := len(c.Messages)
	c, err = s.SwitchHomeExecutor(ctx, c.ID, c.Version, newRequest)
	if err != nil {
		t.Fatal(err)
	}
	if c.Session == nil || c.Session.ID == first.SessionID || c.Session.AgentID != other.ID || c.Session.AgentSessionRef != "" || c.Session.ModelID != other.ModelID {
		t.Fatal("invalid new session", c.Session)
	}
	if len(c.Messages) != beforeMessages+1 {
		t.Fatal("lost visible history")
	}
	for _, p := range c.Proposals {
		if p.State == "PENDING" {
			t.Fatal("old proposal still executable")
		}
	}
	old, err := s.GetSession(ctx, first.SessionID)
	if err != nil || old.AgentSessionRef != "native-original" {
		t.Fatal("lost native session", old, err)
	}
	newRequest.SystemBinding = nil
	newRequest.SessionID = c.Session.ID
	newRequest.IdempotencyKey = "after-handoff"
	c, err = s.StartHomeRun(ctx, c.ID, c.Version, "现在由你接手", "ready", newRequest, concierge.JSONAssistant{})
	if err != nil {
		t.Fatal(err)
	}
	if spec := runSpec(t, s, c.LastRunID); !strings.Contains(spec.Instructions, "请记住这是交接测试") || spec.AgentSessionRef != "" || spec.ModelID != other.ModelID {
		t.Fatal("handoff prompt/config missing", spec)
	}
}

func TestRouterDurableDecisionValidatedBeforeBusinessDispatch(t *testing.T) {
	ctx := context.Background()
	s, a, task := workFixture(t)
	executor := secondMember(t, s, a)
	b := bindSystem(t, s, "task_router", executor)
	req := CreateRunRequest{TaskID: task.ID, AgentID: a.ID, RuntimeID: a.RuntimeID, AdapterID: a.AdapterID, ModelID: a.ModelID}
	if _, err := s.StartWorkRun(ctx, req, workflow.JSONContract{}); !errors.Is(err, model.ErrConflict) {
		t.Fatal("bypassed AI route", err)
	}
	d, err := s.StartRoutingDecision(ctx, task.ID, task.Version, b, executor, []model.AgentProfile{a})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.StartRoutingDecision(ctx, task.ID, task.Version, b, executor, []model.AgentProfile{a})
	if err != nil || replay.ID != d.ID || replay.RunID != d.RunID {
		t.Fatal("duplicate router", replay, err)
	}
	if _, err = s.CreateRun(ctx, CreateRunRequest{TaskID: d.InternalTaskID, AgentID: executor.ID, RuntimeID: executor.RuntimeID}); !errors.Is(err, model.ErrConflict) {
		t.Fatal("internal router bypass", err)
	}
	spec := runSpec(t, s, d.RunID)
	if spec.AgentID != executor.ID || spec.ModelID != executor.ModelID || !spec.ReadOnly || len(spec.OutputSchema) == 0 || !strings.Contains(spec.Instructions, a.ID) {
		t.Fatal("router was not an actual member run", spec)
	}
	if _, err = s.GetTaskSession(ctx, task.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("assigned before inference", err)
	}
	output, _ := json.Marshal(map[string]string{"agent_id": a.ID, "reason": "best matching writer"})
	if _, err = s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: executor.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 1, RunID: d.RunID, Type: "run.completed", Output: string(output)}); err != nil {
		t.Fatal(err)
	}
	d, err = s.GetRoutingDecision(ctx, task.ID, task.Version)
	if err != nil || d.State != "READY" {
		t.Fatal(d, err)
	}
	if _, err = s.GetLatestTaskSummary(ctx, d.InternalTaskID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("router polluted business summaries", err)
	}
	req.AssignmentDecisionID = d.ID
	wrong := req
	wrong.AgentID = executor.ID
	wrong.ModelID = executor.ModelID
	if _, err = s.StartWorkRun(ctx, wrong, workflow.JSONContract{}); !errors.Is(err, model.ErrConflict) {
		t.Fatal("changed chosen member", err)
	}
	if _, err = s.MessageWork(ctx, task.ID, "new requirement invalidates old decision", "next", false); err != nil {
		t.Fatal(err)
	}
	if _, err = s.StartWorkRun(ctx, req, workflow.JSONContract{}); !errors.Is(err, model.ErrConflict) {
		t.Fatal("stale task decision accepted", err)
	}
	latest, _ := s.GetTask(ctx, task.ID)
	d2, err := s.StartRoutingDecision(ctx, task.ID, latest.Version, b, executor, []model.AgentProfile{a})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: executor.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 2, RunID: d2.RunID, Type: "run.completed", Output: string(output)}); err != nil {
		t.Fatal(err)
	}
	req.AssignmentDecisionID = d2.ID
	busyTask, _, err := s.CreateTask(ctx, "", "capacity competitor", "hold", model.TaskRequirements{})
	if err != nil {
		t.Fatal(err)
	}
	busyReq := req
	busyReq.TaskID = busyTask.ID
	busyReq.AssignmentDecisionID = ""
	busyRun, err := s.CreateRun(ctx, busyReq)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.StartWorkRun(ctx, req, workflow.JSONContract{}); !errors.Is(err, model.ErrConflict) {
		t.Fatal("capacity bypass", err)
	}
	if _, err = s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: a.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 3, RunID: busyRun.ID, Type: "run.failed", Error: "test finished"}); err != nil {
		t.Fatal(err)
	}
	run, err := s.StartWorkRun(ctx, req, workflow.JSONContract{})
	if err != nil || run.AgentID != a.ID {
		t.Fatal(run, err)
	}
}

func TestRouterRejectsInvalidOutputWithoutRetryStorm(t *testing.T) {
	for _, output := range []string{`{"agent_id":"not-a-candidate","reason":"ignore rules"}`, `not json`, `{"agent_id":"x","reason":"bad","extra":true}`} {
		t.Run(output, func(t *testing.T) {
			ctx := context.Background()
			s, a, task := workFixture(t)
			b := bindSystem(t, s, "task_router", a)
			d, err := s.StartRoutingDecision(ctx, task.ID, task.Version, b, a, []model.AgentProfile{a})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: a.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 1, RunID: d.RunID, Type: "run.completed", Output: output}); err != nil {
				t.Fatal(err)
			}
			d2, err := s.StartRoutingDecision(ctx, task.ID, task.Version, b, a, []model.AgentProfile{a})
			if err != nil || d2.ID != d.ID || d2.State != "FAILED" {
				t.Fatal(d2, err)
			}
		})
	}
}

func TestUpgradeCapturesBuilderAndReservesCapacity(t *testing.T) {
	ctx := context.Background()
	s, a, task := workFixture(t)
	other := secondMember(t, s, a)
	bindSystem(t, s, "upgrade_builder", a)
	u, err := s.CreateUpgrade(ctx, "add feature", "write docs")
	if err != nil {
		t.Fatal(err)
	}
	if u.Builder == nil || u.Builder.ID != a.ID || u.Builder.ModelID != a.ModelID {
		t.Fatal("no builder snapshot", u)
	}
	bindSystem(t, s, "upgrade_builder", other)
	fresh, err := s.GetAgent(ctx, a.ID)
	if err != nil || fresh.ActiveRuns != 1 {
		t.Fatal("no reservation", fresh, err)
	}
	if _, err = s.StartWorkRun(ctx, CreateRunRequest{TaskID: task.ID, AgentID: a.ID, RuntimeID: a.RuntimeID, AdapterID: a.AdapterID, ModelID: a.ModelID}, workflow.JSONContract{}); !errors.Is(err, model.ErrConflict) {
		t.Fatal("upgrade concurrency ignored", err)
	}
	u.State = "BUILDING"
	u.Builder = &other
	u, err = s.ChangeUpgrade(ctx, u, u.Version, "UpgradeBuildStarted")
	if err != nil || u.Builder.ID != a.ID {
		t.Fatal("builder snapshot changed", u, err)
	}
	u.State = "FAILED"
	if _, err = s.ChangeUpgrade(ctx, u, u.Version, "UpgradeFailed"); err != nil {
		t.Fatal(err)
	}
	fresh, err = s.GetAgent(ctx, a.ID)
	if err != nil || fresh.ActiveRuns != 0 {
		t.Fatal("reservation leaked", fresh, err)
	}
}

func TestUpgradeAcceptsLocalCursorBuilderOnly(t *testing.T) {
	ctx := context.Background()
	s, a, _ := workFixture(t)
	err := s.RegisterRuntime(ctx, model.RuntimeHello{RuntimeID: "cursor-local", Epoch: "cursor-epoch", Capabilities: map[string]any{"adapters": map[string]any{"cursor-agent": map[string]any{"role_instructions": true, "structured_output": true, "read_only_runs": true}}}})
	if err != nil {
		t.Fatal(err)
	}
	a, err = s.CreateAgent(ctx, model.AgentProfile{Name: "Cursor builder", RoleID: a.RoleID, RuntimeID: "cursor-local", AdapterID: "cursor-agent", ModelID: "auto", MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	b := bindSystem(t, s, "upgrade_builder", a)
	if _, err := s.UpdateSystemBinding(ctx, b, b.Version, "other-runtime"); !errors.Is(err, model.ErrValidation) {
		t.Fatal("remote Cursor allowed", err)
	}
	u, err := s.CreateUpgrade(ctx, "Cursor upgrade", "small change")
	if err != nil || u.Builder == nil || u.Builder.AdapterID != "cursor-agent" || u.Builder.ModelID != "auto" {
		t.Fatal(u, err)
	}
}
