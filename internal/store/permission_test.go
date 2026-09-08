package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"work-assistant/internal/model"
	"work-assistant/internal/workflow"
)

func permissionFixture(t *testing.T) (*Store, model.AgentProfile, model.Task, model.Run, model.Task) {
	t.Helper()
	ctx := context.Background()
	s, dev, parent, impl := approvedEnvironmentFixture(t)
	if _, err := s.SaveExecutionPolicy(ctx, model.ExecutionPolicy{Operation: "windows_seekdb_phase0", RuntimeID: dev.RuntimeID, Repository: "oceanbase/seekdb", Effect: "ask", Version: 1}); err != nil {
		t.Fatal(err)
	}
	developmentFinish(t, s, impl, 3, workflow.Result{Outcome: "blocked", Message: "need matrix", Artifacts: []workflow.File{}, EnvironmentRequest: &workflow.EnvironmentRequest{Profile: "windows_seekdb_phase0", Reason: "policy validation"}})
	var childID string
	if err := s.db.QueryRow(`SELECT task_id FROM environment_job WHERE parent_task_id=?`, parent.ID).Scan(&childID); err != nil {
		t.Fatal(err)
	}
	child, err := s.GetTask(ctx, childID)
	if err != nil {
		t.Fatal(err)
	}
	return s, dev, parent, impl, child
}
func attemptPermissionRun(s *Store, a model.AgentProfile, t model.Task) (model.Run, error) {
	return s.StartWorkRun(context.Background(), CreateRunRequest{TaskID: t.ID, AgentID: a.ID, RuntimeID: a.RuntimeID, AdapterID: a.AdapterID, ModelID: a.ModelID}, workflow.JSONContract{})
}

func TestPermissionWaitApprovalOriginalSessionAndBackup(t *testing.T) {
	ctx := context.Background()
	s, dev, parent, impl, child := permissionFixture(t)
	if _, err := attemptPermissionRun(s, dev, child); !errors.Is(err, model.ErrConflict) {
		t.Fatal(err)
	}
	current, _ := s.GetTask(ctx, child.ID)
	if current.State != waitingAuthorization {
		t.Fatal(current.State)
	}
	pending, _ := s.PendingWork(ctx)
	for _, p := range pending {
		if p == child.ID {
			t.Fatal("authorization busy loop")
		}
	}
	var runs int
	s.db.QueryRow(`SELECT COUNT(*) FROM run WHERE task_id=?`, child.ID).Scan(&runs)
	if runs != 0 {
		t.Fatal("run dispatched before permission")
	}
	requests, err := s.PermissionRequests(ctx)
	if err != nil || len(requests) != 1 || requests[0].State != "PENDING" {
		t.Fatal(requests, err)
	}
	r := requests[0]
	copy := filepath.Join(t.TempDir(), "backup.sqlite")
	if err = s.Backup(ctx, copy); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(copy)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	rr, _ := restored.PermissionRequests(ctx)
	if len(rr) != 1 || rr[0].ID != r.ID {
		t.Fatal("lost authorization on backup")
	}
	if err = s.DecidePermission(ctx, r.ID, r.Version-1, "approve", false); !errors.Is(err, model.ErrConflict) {
		t.Fatal("stale approval accepted", err)
	}
	if err = s.DecidePermission(ctx, r.ID, r.Version, "approve", false); err != nil {
		t.Fatal(err)
	}
	if err = s.DecidePermission(ctx, r.ID, r.Version, "approve", false); !errors.Is(err, model.ErrConflict) {
		t.Fatal("double approval accepted", err)
	}
	run, err := attemptPermissionRun(s, dev, child)
	if err != nil {
		t.Fatal(err)
	}
	requests, _ = s.PermissionRequests(ctx)
	if requests[0].State != "USED" {
		t.Fatal("permission not consumed")
	}
	developmentFinish(t, s, run, 4, workflow.Result{Outcome: "review", Message: "failed report", Artifacts: []workflow.File{}, EnvironmentResult: &model.EnvironmentResult{Profile: "windows_seekdb_phase0", Status: "failed", Message: "assertion failed"}})
	next := startWork(t, s, dev, parent)
	if next.SessionID != impl.SessionID {
		t.Fatal("parent session changed")
	}
	if d := developmentState(t, s, parent.ID); d.Phase != "IMPLEMENTING" || d.ApprovedReviewID == "" {
		t.Fatal("plan approval lost")
	}
	// A later validation request is not covered by one-time approval.
	developmentFinish(t, s, next, 5, workflow.Result{Outcome: "blocked", Message: "fixed probe", Artifacts: []workflow.File{}, EnvironmentRequest: &workflow.EnvironmentRequest{Profile: "windows_seekdb_phase0", Reason: "revised source"}})
	var nextID string
	s.db.QueryRow(`SELECT task_id FROM environment_job WHERE source_run_id=?`, next.ID).Scan(&nextID)
	second, _ := s.GetTask(ctx, nextID)
	if _, err = attemptPermissionRun(s, dev, second); !errors.Is(err, model.ErrConflict) {
		t.Fatal("one-time permission leaked", err)
	}
}

func TestPermissionRememberDenyAndScope(t *testing.T) {
	ctx := context.Background()
	s, dev, _, _, child := permissionFixture(t)
	attemptPermissionRun(s, dev, child)
	requests, _ := s.PermissionRequests(ctx)
	r := requests[0]
	deny := model.ExecutionPolicy{Operation: r.Operation, RuntimeID: "*", Repository: r.Repository, Effect: "deny"}
	deny, err := s.SaveExecutionPolicy(ctx, deny)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DecidePermission(ctx, r.ID, r.Version, "approve", true); !errors.Is(err, model.ErrConflict) {
		t.Fatal("deny bypass", err)
	}
	deny.Effect = "ask"
	if _, err = s.SaveExecutionPolicy(ctx, deny); err != nil {
		t.Fatal(err)
	}
	if err = s.DecidePermission(ctx, r.ID, r.Version, "approve", true); err != nil {
		t.Fatal(err)
	}
	if _, err = attemptPermissionRun(s, dev, child); err != nil {
		t.Fatal(err)
	}
	err = s.sourceWrite(ctx, func(tx *sql.Tx) error {
		effect, e := policyEffectTx(ctx, tx, r)
		if e != nil || effect != "allow" {
			t.Fatal(effect, e)
		}
		r.RuntimeID = "other-runtime"
		effect, e = policyEffectTx(ctx, tx, r)
		if e != nil || effect != "ask" {
			t.Fatal("permission broadened", effect, e)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPermissionPauseInvalidatesAndUnavailableIsNotAuthorization(t *testing.T) {
	ctx := context.Background()
	s, dev, parent, _, child := permissionFixture(t)
	if _, err := s.db.Exec(`UPDATE runtime SET capabilities_json=json_set(capabilities_json,'$.executors.windows_seekdb_phase0.available',json('false')) WHERE runtime_id=?`, dev.RuntimeID); err != nil {
		t.Fatal(err)
	}
	attemptPermissionRun(s, dev, child)
	current, _ := s.GetTask(ctx, child.ID)
	if current.State != waitingEnvironment {
		t.Fatal(current.State)
	}
	requests, _ := s.PermissionRequests(ctx)
	if len(requests) != 0 {
		t.Fatal("missing tool treated as authorization")
	}
	s.db.Exec(`UPDATE runtime SET capabilities_json=json_set(capabilities_json,'$.executors.windows_seekdb_phase0.available',json('true')) WHERE runtime_id=?`, dev.RuntimeID)
	attemptPermissionRun(s, dev, child)
	requests, _ = s.PermissionRequests(ctx)
	if len(requests) != 1 {
		t.Fatal(requests)
	}
	r := requests[0]
	if err := s.sourceWrite(ctx, func(tx *sql.Tx) error { return pauseWorkTx(ctx, tx, parent.ID) }); err != nil {
		t.Fatal(err)
	}
	if err := s.DecidePermission(ctx, r.ID, r.Version, "approve", false); !errors.Is(err, model.ErrConflict) {
		t.Fatal("paused task authorized", err)
	}
	requests, _ = s.PermissionRequests(ctx)
	if requests[0].State != "STALE" {
		t.Fatal(requests)
	}
}

func TestAgentCapabilityApprovalResumesOriginalSessionOnce(t *testing.T) {
	ctx := context.Background()
	s, dev, parent, impl := approvedEnvironmentFixture(t)
	developmentFinish(t, s, impl, 3, workflow.Result{
		Outcome: "blocked", Message: "network is required", Artifacts: []workflow.File{},
		CapabilityRequest: &workflow.CapabilityRequest{Capability: "network_access", Reason: "fetch dependency metadata for the approved implementation"},
	})
	current, err := s.GetTask(ctx, parent.ID)
	if err != nil || current.State != waitingAuthorization {
		t.Fatal(current.State, err)
	}
	requests, err := s.PermissionRequests(ctx)
	if err != nil || len(requests) != 1 || requests[0].Operation != "agent.network_access" || requests[0].State != "PENDING" {
		t.Fatal(requests, err)
	}
	request := requests[0]
	if err = s.DecidePermission(ctx, request.ID, request.Version, "approve", false); err != nil {
		t.Fatal(err)
	}
	next := startWork(t, s, dev, parent)
	if next.SessionID != impl.SessionID {
		t.Fatal("capability approval replaced the native session")
	}
	var raw []byte
	if err = s.db.QueryRow(`SELECT params_json FROM outbox_message WHERE json_extract(params_json,'$.run_id')=?`, next.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var spec model.RunSpec
	if err = json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.ExecutionGrant == nil || len(spec.ExecutionGrant.Capabilities) != 1 || spec.ExecutionGrant.Capabilities[0] != "network_access" {
		t.Fatalf("approved capability not injected: %#v", spec.ExecutionGrant)
	}
	requests, _ = s.PermissionRequests(ctx)
	if requests[0].State != "USED" {
		t.Fatal("one-shot capability not consumed", requests[0])
	}
}
