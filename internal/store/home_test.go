package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"work-assistant/internal/concierge"
	"work-assistant/internal/model"
)

func homeTurn(t *testing.T, s *Store, c model.HomeChat, k string) model.HomeChat {
	t.Helper()
	r, err := s.StartHomeRun(context.Background(), c.ID, c.Version, "请分析当前工作", "test status", CreateRunRequest{AgentID: "home-assistant", RuntimeID: "role-runtime", AdapterID: "codex-agent", ModelID: "test-model", IdempotencyKey: k}, concierge.JSONAssistant{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func homeResult(t *testing.T, s *Store, c model.HomeChat, seq int64, p *model.HomeAction) model.HomeChat {
	t.Helper()
	raw, _ := json.Marshal(concierge.Result{Message: "建议请确认，尚未执行。", Proposal: p})
	if _, err := s.ApplyRuntimeEvent(context.Background(), model.RuntimeEvent{RuntimeID: "role-runtime", Epoch: "epoch-role", RuntimeSeq: seq, RunID: c.LastRunID, Type: "run.completed", Output: string(raw)}); err != nil {
		t.Fatal(err)
	}
	c, err := s.GetHomeChat(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func TestHomeChatIsNotWorkAndConfirmationIsAtomic(t *testing.T) {
	ctx := context.Background()
	s := roleTestStore(t)
	role := publishTestRole(t, s, "document.write")
	c, err := s.CreateHomeChat(ctx, "same")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := s.CreateHomeChat(ctx, "same")
	if again.ID != c.ID {
		t.Fatal("duplicate chat")
	}
	if _, err = s.CreateRun(ctx, CreateRunRequest{TaskID: c.TaskID, RuntimeID: "role-runtime"}); !errors.Is(err, model.ErrConflict) {
		t.Fatal("generic run bypass", err)
	}
	c = homeTurn(t, s, c, "one")
	firstRun, _ := s.GetRun(ctx, c.LastRunID)
	var spec model.RunSpec
	var raw []byte
	if err = s.db.QueryRow(`SELECT params_json FROM outbox_message WHERE method='run.start' AND json_extract(params_json,'$.run_id')=?`, firstRun.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &spec); err != nil || !spec.ReadOnly || len(spec.OutputSchema) == 0 || len(spec.Command) > 0 {
		t.Fatal("unsafe run", spec, err)
	}
	c = homeResult(t, s, c, 1, nil)
	if _, err = s.GetLatestTaskSummary(ctx, c.TaskID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("homepage chat turn was recorded as a business task summary", err)
	}
	work, _ := s.ListWork(ctx)
	if len(work) != 0 {
		t.Fatal("chat created work")
	}
	c = homeTurn(t, s, c, "two")
	nextRun, _ := s.GetRun(ctx, c.LastRunID)
	if nextRun.SessionID != firstRun.SessionID {
		t.Fatal("lost home session")
	}
	c = homeResult(t, s, c, 2, &model.HomeAction{Kind: "create_work", Title: "Document", Text: "Write a guide", RoleID: role.ID})
	work, _ = s.ListWork(ctx)
	if len(work) != 0 || len(c.Proposals) != 1 {
		t.Fatal("proposal executed early")
	}
	p := c.Proposals[0]
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := s.DecideHomeProposal(ctx, c.ID, p.ID, "CONFIRMED"); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	work, _ = s.ListWork(ctx)
	if len(work) != 1 {
		t.Fatal("duplicate confirmed work", len(work))
	}
	c, _ = s.GetHomeChat(ctx, c.ID)
	if c.Proposals[0].ResourceID != work[0].ID {
		t.Fatal("result link missing")
	}
	snapshot := filepath.Join(t.TempDir(), "backup.sqlite")
	if err = s.Backup(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	saved, err := restored.GetHomeChat(ctx, c.ID)
	if err != nil || len(saved.Messages) != len(c.Messages) || saved.Proposals[0].State != "CONFIRMED" {
		t.Fatal("backup incomplete", err)
	}
}
func TestHomeStaleForeignAndSupersededProposals(t *testing.T) {
	ctx := context.Background()
	s, _, task := workFixture(t)
	c, _ := s.CreateHomeChat(ctx, "")
	c = homeTurn(t, s, c, "1")
	c = homeResult(t, s, c, 1, &model.HomeAction{Kind: "message_work", Title: "补充", Text: "more context", TargetTaskID: task.ID})
	p := c.Proposals[0]
	if _, err := s.MessageWork(ctx, task.ID, "changed externally", "external", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideHomeProposal(ctx, c.ID, p.ID, "CONFIRMED"); !errors.Is(err, model.ErrConflict) {
		t.Fatal("stale action accepted", err)
	}
	d, _ := s.GetWorkDetail(ctx, task.ID)
	if len(d.Messages) != 2 {
		t.Fatal("stale action mutated work")
	}
	c = homeTurn(t, s, c, "2")
	if c.Proposals[0].State != "SUPERSEDED" {
		t.Fatal("old proposal remains active")
	}
	if _, err := s.DecideHomeProposal(ctx, c.ID, p.ID, "CONFIRMED"); !errors.Is(err, model.ErrConflict) {
		t.Fatal(err)
	}
	c = homeResult(t, s, c, 2, &model.HomeAction{Kind: "pause_work", Title: "暂停", Text: "pause", TargetTaskID: "not-in-snapshot"})
	if c.State != "FAILED" || len(c.Proposals) != 1 {
		t.Fatal("unknown task accepted")
	}
	c = homeTurn(t, s, c, "3")
	c = homeResult(t, s, c, 3, &model.HomeAction{Kind: "pause_work", Title: "暂停", Text: "pause", TargetTaskID: task.ID})
	c, err := s.DecideHomeProposal(ctx, c.ID, c.Proposals[1].ID, "CONFIRMED")
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := s.GetWorkConfig(ctx, task.ID)
	if !cfg.Paused {
		t.Fatal("pause not applied")
	}
}
func TestHomeRoleIsDraftOnlyAndDismissedIsSafe(t *testing.T) {
	ctx := context.Background()
	s := roleTestStore(t)
	c, _ := s.CreateHomeChat(ctx, "")
	spec := sampleRole("analysis")
	c = homeTurn(t, s, c, "1")
	c = homeResult(t, s, c, 1, &model.HomeAction{Kind: "create_role", Title: "分析员", Text: "准备草稿", RoleSpec: &spec})
	p := c.Proposals[0]
	c, err := s.DecideHomeProposal(ctx, c.ID, p.ID, "DISMISSED")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DecideHomeProposal(ctx, c.ID, p.ID, "CONFIRMED"); !errors.Is(err, model.ErrConflict) {
		t.Fatal("dismissed executed")
	}
	drafts, _ := s.ListRoleDrafts(ctx)
	if len(drafts) != 0 {
		t.Fatal("dismissal created draft")
	}
	c = homeTurn(t, s, c, "2")
	c = homeResult(t, s, c, 2, &model.HomeAction{Kind: "create_role", Title: "分析员", Text: "准备草稿", RoleSpec: &spec})
	c, err = s.DecideHomeProposal(ctx, c.ID, c.Proposals[1].ID, "CONFIRMED")
	if err != nil {
		t.Fatal(err)
	}
	roles, _ := s.ListRoles(ctx)
	agents, _ := s.ListAgents(ctx)
	if len(roles) != 0 || len(agents) != 0 {
		t.Fatal("role autopublished")
	}
	draft, err := s.GetRoleDraft(ctx, c.Proposals[1].ResourceID)
	if err != nil || draft.State != "DRAFT" || draft.Spec.Instructions != spec.Instructions {
		t.Fatal("missing draft", err)
	}
}
