package store

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"work-assistant/internal/model"
)

func TestTaskReferencesLegacyReviewInputsAndSessionContinuity(t *testing.T) {
	ctx := context.Background()
	s, author, _ := workFixture(t)
	ant := saveTestSource(t, s, "antmultica")
	targets, _ := s.ListSourceTargets(ctx)
	// Historical events only had the workspace URL; keep that immutable.
	event := model.SourceEvent{Key: "issue:one:v1", Kind: "antmultica.issue", Entity: "antmultica:workspace:one", Title: "SEEK-1 request", Message: "Original request", URL: "https://antmultica.alipay.com/seekdb"}
	pollStore(t, s, ant, targets[0], "", event)
	events, _ := s.ListSourceEvents(ctx)
	parent, err := s.GetTask(ctx, events[0].TaskID)
	if err != nil {
		t.Fatal(err)
	}
	first := startWork(t, s, author, parent)
	link := "https://antmultica.alipay.com/seekdb/issues/one"
	firstSpec := runSpec(t, s, first.ID)
	if !strings.Contains(firstSpec.Instructions, link) {
		t.Fatal("agent missing source link")
	}
	if !firstSpec.ReadOnly || !firstSpec.NetworkAccess || firstSpec.ExecutionGrant != nil || !strings.Contains(firstSpec.Instructions, "经 Manager 校验并持久化的 AntMultica 来源引用") || !strings.Contains(firstSpec.Instructions, "不得执行创建、更新、评论") {
		t.Fatalf("source-linked Agent missing managed read network boundary: %#v", firstSpec)
	}
	finishWork(t, s, first, 1, "review", "implementation")
	qaRole := publishTestRole(t, s, "qa.review")
	qa, err := s.CreateAgent(ctx, model.AgentProfile{Name: "QA", RoleID: qaRole.ID, RuntimeID: author.RuntimeID, AdapterID: author.AdapterID, ModelID: author.ModelID, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	github := saveTestSource(t, s, "github")
	target, err := s.RegisterPR(ctx, parent.ID, github.ID, "https://github.com/oceanbase/seekdb/pull/123")
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	legacy := "LEGACY_PATCH_PAYLOAD\n@@ -1 +1 @@\n+code copied from GitHub"
	pollStore(t, s, github, target, head, model.SourceEvent{Key: "head:" + head, Kind: "github.head", HeadSHA: head, Message: legacy})
	reviews, _ := s.ListSourceReviews(ctx)
	if len(reviews) != 1 {
		t.Fatal(reviews)
	}
	child, _ := s.GetTask(ctx, reviews[0].TaskID)
	if strings.Contains(child.Goal, "LEGACY_PATCH_PAYLOAD") || !strings.Contains(child.Goal, target.Entity) || !strings.Contains(child.Title, "#123") {
		t.Fatal("review still embeds event evidence", child)
	}
	// Simulate a pre-upgrade task and its undelivered initial message.
	if _, err = s.db.Exec(`UPDATE task SET goal=? WHERE task_id=?`, legacy, child.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE task_message SET content=? WHERE task_id=? AND speaker='user'`, legacy, child.ID); err != nil {
		t.Fatal(err)
	}
	before, _ := s.GetTask(ctx, child.ID)
	work, err := s.GetWorkDetail(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if work.ReviewBrief == nil || strings.Contains(work.ReviewBrief.Goal, "LEGACY_PATCH_PAYLOAD") || len(work.References) != 2 || work.References[0].URL != target.Entity || work.References[0].Revision != head || work.References[1].URL != link {
		t.Fatal(work)
	}
	after, _ := s.GetTask(ctx, child.ID)
	if !reflect.DeepEqual(before, after) || work.Messages[0].Content != legacy {
		t.Fatal("read rewrote historical content")
	}
	run := startWork(t, s, qa, child)
	spec := runSpec(t, s, run.ID)
	if strings.Contains(spec.TaskGoal+spec.Instructions, "LEGACY_PATCH_PAYLOAD") || !strings.Contains(spec.TaskGoal, head) || !strings.Contains(spec.Instructions, link) {
		t.Fatal("legacy diff leaked into new run")
	}
	if _, err = s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: qa.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 2, RunID: run.ID, SessionID: run.SessionID, Type: "session.bound", AgentSessionRef: "codex:review-memory"}); err != nil {
		t.Fatal(err)
	}
	finishWork(t, s, run, 3, "blocked", "missing evidence")
	if _, err = s.MessageWork(ctx, child.ID, legacy, "continue-review", false); err != nil {
		t.Fatal(err)
	}
	next := startWork(t, s, qa, child)
	if next.SessionID != run.SessionID || runSpec(t, s, next.ID).AgentSessionRef != "codex:review-memory" {
		t.Fatal("lost review session")
	}
	if !strings.Contains(runSpec(t, s, next.ID).Instructions, legacy) {
		t.Fatal("a later human message was mistaken for generated evidence")
	}
	backup := filepath.Join(t.TempDir(), "copy.sqlite")
	if err = s.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(backup)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	recovered, err := restored.GetWorkDetail(ctx, child.ID)
	if err != nil || !reflect.DeepEqual(recovered.References, work.References) {
		t.Fatal("links lost in backup", err)
	}
	newHead := strings.Repeat("b", 40)
	pollStore(t, s, github, targetByID(t, s, target.ID), newHead)
	oldReview, err := s.GetWorkDetail(ctx, child.ID)
	if err != nil || oldReview.References[0].Revision != head || !strings.Contains(oldReview.ReviewBrief.Goal, head) {
		t.Fatal("review reference followed mutable PR head", err)
	}
	rootWork, err := s.GetWorkDetail(ctx, parent.ID)
	if err != nil || rootWork.References[0].Revision != newHead {
		t.Fatal("parent PR reference missing current head", err)
	}
}

func TestTaskReferencesRejectUntrustedSourceURLs(t *testing.T) {
	for _, raw := range []string{"javascript:alert(1)", "https://antmultica.alipay.com.evil.test/seekdb", "https://user:pass@antmultica.alipay.com/seekdb", "https://antmultica.alipay.com/seekdb/issues/another"} {
		t.Run(raw, func(t *testing.T) {
			s := roleTestStore(t)
			ctx := context.Background()
			source := saveTestSource(t, s, "antmultica")
			targets, _ := s.ListSourceTargets(ctx)
			pollStore(t, s, source, targets[0], "", model.SourceEvent{Key: "one", Kind: "antmultica.issue", Entity: "antmultica:workspace:one", Title: "SEEK-1", Message: "request", URL: raw})
			events, _ := s.ListSourceEvents(ctx)
			work, err := s.GetWorkDetail(ctx, events[0].TaskID)
			if err != nil || len(work.References) != 0 {
				t.Fatal("unsafe source URL rendered", work.References, err)
			}
		})
	}
}
