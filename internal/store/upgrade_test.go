package store

import (
	"context"
	"errors"
	"testing"
	"work-assistant/internal/concierge"
	"work-assistant/internal/model"
)

func readyUpgrade(t *testing.T, state *Store, title string) model.Upgrade {
	t.Helper()
	u, err := state.CreateUpgrade(context.Background(), title, "implement and test it")
	if err != nil {
		t.Fatal(err)
	}
	u.State = "BUILDING"
	u, err = state.ChangeUpgrade(context.Background(), u, u.Version, "build")
	if err != nil {
		t.Fatal(err)
	}
	u.State = "READY"
	u.CandidateSHA256 = "candidate-digest"
	u.SourceSHA256 = "source-digest"
	u.Changes = []model.UpgradeChange{{Path: "internal/example.go", Before: "old", After: "new"}}
	u, err = state.ChangeUpgrade(context.Background(), u, u.Version, "ready")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestUpgradeRequiresImmutableApprovalAndDrainsRuns(t *testing.T) {
	ctx := context.Background()
	state := roleTestStore(t)
	u := readyUpgrade(t, state, "Add feature")
	if _, err := state.CreateUpgrade(ctx, "Other", "other"); !errors.Is(err, model.ErrConflict) {
		t.Fatal("parallel active upgrade accepted", err)
	}
	if _, err := state.RequestUpgradeInstall(ctx, u.ID, u.Version, "wrong"); !errors.Is(err, model.ErrConflict) {
		t.Fatal("unreviewed candidate accepted", err)
	}
	task, _, err := state.CreateTask(ctx, "", "running", "goal")
	if err != nil {
		t.Fatal(err)
	}
	run, err := state.CreateRun(ctx, CreateRunRequest{TaskID: task.ID, RuntimeID: "role-runtime", AdapterID: "exec-agent"})
	if err != nil {
		t.Fatal(err)
	}
	u, err = state.RequestUpgradeInstall(ctx, u.ID, u.Version, u.CandidateSHA256)
	if err != nil || u.State != "WAITING_IDLE" {
		t.Fatal("install approval", u, err)
	}
	if _, err = state.CreateRun(ctx, CreateRunRequest{TaskID: task.ID, RuntimeID: "role-runtime", AdapterID: "exec-agent"}); !errors.Is(err, model.ErrConflict) {
		t.Fatal("maintenance accepted a new run", err)
	}
	if _, err = state.BeginUpgradeInstall(ctx, u.ID); !errors.Is(err, model.ErrConflict) {
		t.Fatal("install did not wait for active run", err)
	}
	if _, err = state.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: run.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 1, RunID: run.ID, Type: "run.completed"}); err != nil {
		t.Fatal(err)
	}
	u, err = state.BeginUpgradeInstall(ctx, u.ID)
	if err != nil || u.State != "INSTALLING" {
		t.Fatal("begin after drain", u, err)
	}
	if err = state.FinishUpgrade(ctx, u.ID, "SUCCEEDED", "", "/backup"); err != nil {
		t.Fatal(err)
	}
	if maintenance, _ := state.Maintenance(ctx); maintenance != "" {
		t.Fatal("maintenance was not cleared")
	}
}

func TestHomeUpgradeProposalOnlyCreatesRecordedCandidateRequest(t *testing.T) {
	ctx := context.Background()
	state := roleTestStore(t)
	chat, _ := state.CreateHomeChat(ctx, "upgrade-chat")
	chat = homeTurn(t, state, chat, "one")
	chat = homeResult(t, state, chat, 1, &model.HomeAction{Kind: "upgrade_system", Title: "Add dashboard", Text: "Implement a dashboard and tests"})
	if upgrades, _ := state.ListUpgrades(ctx); len(upgrades) != 0 {
		t.Fatal("proposal modified system before confirmation")
	}
	chat, err := state.DecideHomeProposal(ctx, chat.ID, chat.Proposals[0].ID, "CONFIRMED")
	if err != nil {
		t.Fatal(err)
	}
	upgrades, err := state.ListUpgrades(ctx)
	if err != nil || len(upgrades) != 1 || upgrades[0].State != "QUEUED" || chat.Proposals[0].ResourceID != upgrades[0].ID {
		t.Fatal("upgrade request was not recorded", upgrades, err)
	}
	if _, err = state.StartHomeRun(ctx, chat.ID, chat.Version, "status", "status", CreateRunRequest{RuntimeID: "role-runtime", AdapterID: "codex-agent"}, concierge.JSONAssistant{}); err != nil {
		t.Fatal("pre-install upgrade request unexpectedly stopped chat", err)
	}
}
