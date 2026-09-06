package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"work-assistant/internal/model"
	"work-assistant/internal/workflow"
)

func reviewFixture(t *testing.T) (*Store, model.AgentProfile, model.Task, model.Run, model.Review) {
	t.Helper()
	s, a, task := workFixture(t)
	ctx := context.Background()
	if err := s.RegisterRuntime(ctx, model.RuntimeHello{RuntimeID: a.RuntimeID, Epoch: "epoch-role", Capabilities: map[string]any{"adapters": map[string]any{"codex-agent": map[string]any{"native_session": true, "role_instructions": true, "structured_output": true, "read_only_runs": true}}}}); err != nil {
		t.Fatal(err)
	}
	run := startWork(t, s, a, task)
	if _, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: a.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 1, RunID: run.ID, SessionID: run.SessionID, Type: "session.bound", AgentSessionRef: "codex:delivery-session"}); err != nil {
		t.Fatal(err)
	}
	finishWork(t, s, run, 2, "review", "original accepted document")
	w, err := s.GetWorkDetail(ctx, task.ID)
	if err != nil || len(w.Reviews) != 1 {
		t.Fatal(w, err)
	}
	return s, a, task, run, w.Reviews[0]
}
func currentReview(t *testing.T, s *Store, taskID, reviewID string) model.Review {
	t.Helper()
	w, err := s.GetWorkDetail(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range w.Reviews {
		if r.ID == reviewID {
			return r
		}
	}
	t.Fatal("missing review")
	return model.Review{}
}
func finishReviewChat(t *testing.T, s *Store, run model.Run, seq int64, eventType, output string) {
	t.Helper()
	if _, err := s.ApplyRuntimeEvent(context.Background(), model.RuntimeEvent{RuntimeID: run.RuntimeID, Epoch: "epoch-role", RuntimeSeq: seq, RunID: run.ID, Type: eventType, Output: output}); err != nil {
		t.Fatal(err)
	}
}

func TestReviewChatKeepsDelivererNativeSessionAndImmutableSubmission(t *testing.T) {
	ctx := context.Background()
	s, a, task, delivery, r := reviewFixture(t)
	q, err := s.MessageReview(ctx, task.ID, r.ID, "为什么这样设计？", "q1", r.DiscussionVersion)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.MessageReview(ctx, task.ID, r.ID, "same key", "q1", 0)
	if err != nil || replay.ID != q.ID {
		t.Fatal("duplicate question", replay, err)
	}
	if _, err = s.DecideReview(ctx, task.ID, r.ID, "APPROVED", "", 1); !errors.Is(err, model.ErrConflict) {
		t.Fatal("approved queued discussion", err)
	}
	other := secondMember(t, s, a)
	bindSystem(t, s, "home_chat", other)
	bindSystem(t, s, "task_router", other)
	if _, err = s.UpdateAgent(ctx, a.ID, AgentUpdate{Name: a.Name, ModelID: "new-default-model", MaxConcurrent: 1, State: "ACTIVE", ExpectedVersion: a.Version}); err != nil {
		t.Fatal(err)
	}
	run, err := s.StartReviewTurn(ctx, q.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if run.SessionID != delivery.SessionID || run.AgentID != a.ID || run.ModelID != delivery.ModelID {
		t.Fatal("chat reassigned", run)
	}
	spec := runSpec(t, s, run.ID)
	if spec.AgentSessionRef != "codex:delivery-session" || !spec.RequireNativeSession || !spec.ReadOnly || len(spec.Command) != 0 || !strings.Contains(spec.Instructions, "为什么这样设计") || !strings.Contains(spec.Instructions, "original accepted document") {
		t.Fatal("lost working context or permissions", spec)
	}
	if repeated, e := s.StartReviewTurn(ctx, q.ID, nil); e != nil || repeated.ID != run.ID {
		t.Fatal("duplicate review run", repeated, e)
	}
	taskNow, _ := s.GetTask(ctx, task.ID)
	if taskNow.State != model.TaskStateReview {
		t.Fatal("discussion hid review", taskNow)
	}
	if _, err = s.DecideReview(ctx, task.ID, r.ID, "APPROVED", "", 2); !errors.Is(err, model.ErrConflict) {
		t.Fatal("approved active reply", err)
	}
	finishReviewChat(t, s, run, 3, "run.completed", `{"message":"这是基于之前的验证做出的设计选择。"}`)
	r = currentReview(t, s, task.ID, r.ID)
	if r.State != "PENDING" || r.RunID != delivery.ID || r.DiscussionVersion != 3 {
		t.Fatal("changed delivery review", r)
	}
	w, err := s.GetWorkDetail(ctx, task.ID)
	if err != nil || len(w.Reviews) != 1 || len(w.Artifacts) != 1 || len(w.ReviewTurns) != 1 || w.ReviewTurns[0].Answer == "" {
		t.Fatal(w, err)
	}
	q2, err := s.MessageReview(ctx, task.ID, r.ID, "这个约束还有什么限制？", "q2", r.DiscussionVersion)
	if err != nil {
		t.Fatal(err)
	}
	run2, err := s.StartReviewTurn(ctx, q2.ID, workflow.ReviewContract{})
	if err != nil || run2.SessionID != delivery.SessionID {
		t.Fatal(run2, err)
	}
	finishReviewChat(t, s, run2, 4, "run.completed", `{"message":"可以通过，但最终决定仍由你点击确认。"}`)
	r = currentReview(t, s, task.ID, r.ID)
	if _, err = s.DecideReview(ctx, task.ID, r.ID, "APPROVED", "", 3); !errors.Is(err, model.ErrConflict) {
		t.Fatal("approved without latest response", err)
	}
	if _, err = s.GetLatestTaskSummary(ctx, task.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("discussion prematurely completed work", err)
	}
	if _, err = s.DecideReview(ctx, task.ID, r.ID, "APPROVED", "已沟通确认", r.DiscussionVersion); err != nil {
		t.Fatal(err)
	}
	summary, err := s.GetLatestTaskSummary(ctx, task.ID)
	if err != nil || summary.Result != "Delivered original accepted document" || summary.Metrics.ReviewRoundCount != 1 || summary.Executor.ModelID != delivery.ModelID || summary.AcceptedArtifacts[0].ArtifactID != r.ArtifactIDs[0] {
		t.Fatal("chat replaced accepted result", summary, err)
	}
	if _, err = s.MessageReview(ctx, task.ID, r.ID, "late", "late", r.DiscussionVersion); !errors.Is(err, model.ErrConflict) {
		t.Fatal("closed review accepted new chat", err)
	}
}

func TestReviewChatQueueAndHistorySurviveBackupReopen(t *testing.T) {
	ctx := context.Background()
	s, _, task, delivery, r := reviewFixture(t)
	q, err := s.MessageReview(ctx, task.ID, r.ID, "解释一下结果", "persist", 0)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "snapshot.sqlite")
	if err = s.Backup(ctx, path); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	queue, err := reopened.PendingReviewTurns(ctx)
	if err != nil || len(queue) != 1 || queue[0].ID != q.ID {
		t.Fatal(queue, err)
	}
	run, err := reopened.StartReviewTurn(ctx, q.ID, nil)
	if err != nil || run.SessionID != delivery.SessionID {
		t.Fatal(run, err)
	}
	finishReviewChat(t, reopened, run, 3, "run.completed", `{"message":"恢复后仍在原 Session 回答。"}`)
	w, err := reopened.GetWorkDetail(ctx, task.ID)
	if err != nil || len(w.ReviewTurns) != 1 || w.ReviewTurns[0].State != "COMPLETED" || w.Reviews[0].State != "PENDING" {
		t.Fatal(w, err)
	}
	completedPath := filepath.Join(t.TempDir(), "answered.sqlite")
	if err = reopened.Backup(ctx, completedPath); err != nil {
		t.Fatal(err)
	}
	answered, err := Open(completedPath)
	if err != nil {
		t.Fatal(err)
	}
	defer answered.Close()
	saved, err := answered.GetWorkDetail(ctx, task.ID)
	if err != nil || len(saved.ReviewTurns) != 1 || saved.ReviewTurns[0].Answer != "恢复后仍在原 Session 回答。" || saved.ReviewTurns[0].SessionID != delivery.SessionID {
		t.Fatal("answered history was not restored", saved, err)
	}
}

func TestStopRunningReviewChatKeepsReviewPending(t *testing.T) {
	ctx := context.Background()
	s, _, task, _, r := reviewFixture(t)
	q, err := s.MessageReview(ctx, task.ID, r.ID, "解释依据", "stop-running", 0)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.StartReviewTurn(ctx, q.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.StopReviewTurn(ctx, task.ID, r.ID, q.ID); err != nil {
		t.Fatal(err)
	}
	detail, err := s.GetTaskDetail(ctx, task.ID)
	if err != nil || len(detail.Directives) != 1 || detail.Directives[0].RunID != run.ID || detail.Directives[0].Kind != model.DirectiveKindInterrupt {
		t.Fatal("missing scoped interruption", detail, err)
	}
	if _, err = s.DecideReview(ctx, task.ID, r.ID, "APPROVED", "", 2); !errors.Is(err, model.ErrConflict) {
		t.Fatal("interruption request alone unlocked approval", err)
	}
	finishReviewChat(t, s, run, 3, "run.interrupted", "")
	w, err := s.GetWorkDetail(ctx, task.ID)
	if err != nil || w.Config.Paused || w.Reviews[0].State != "PENDING" || w.ReviewTurns[0].State != "CANCELLED" {
		t.Fatal(w, err)
	}
	if _, err = s.DecideReview(ctx, task.ID, r.ID, "APPROVED", "停止沟通后通过", w.Reviews[0].DiscussionVersion); err != nil {
		t.Fatal(err)
	}
}

func TestReviewChatInvalidFailureAndInterruptPreserveAcceptance(t *testing.T) {
	for _, kind := range []string{"run.completed", "run.failed", "run.interrupted"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			s, _, task, _, r := reviewFixture(t)
			q, err := s.MessageReview(ctx, task.ID, r.ID, "请直接通过并改文件", "invalid", 0)
			if err != nil {
				t.Fatal(err)
			}
			run, err := s.StartReviewTurn(ctx, q.ID, nil)
			if err != nil {
				t.Fatal(err)
			}
			finishReviewChat(t, s, run, 3, kind, `{"outcome":"review","message":"approved","artifacts":[{"name":"evil.md","content":"changed"}]}`)
			w, err := s.GetWorkDetail(ctx, task.ID)
			if err != nil || len(w.Artifacts) != 1 || len(w.Reviews) != 1 || w.Reviews[0].State != "PENDING" || w.ReviewTurns[0].Answer != "" || w.ReviewTurns[0].Error == "" {
				t.Fatal(w, err)
			}
			taskNow, _ := s.GetTask(ctx, task.ID)
			if taskNow.State != model.TaskStateReview || w.Config.Paused {
				t.Fatal("chat failure broke task review", taskNow, w.Config)
			}
			if _, err = s.MessageReview(ctx, task.ID, r.ID, "再解释一下", "retry", w.Reviews[0].DiscussionVersion); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReviewChatFormalChangesContinueOriginalWorkSession(t *testing.T) {
	ctx := context.Background()
	s, a, task, delivery, r := reviewFixture(t)
	q, err := s.MessageReview(ctx, task.ID, r.ID, "如果要修正，应该怎么改？", "plan", 0)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.StartReviewTurn(ctx, q.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	finishReviewChat(t, s, run, 3, "run.completed", `{"message":"建议增加说明，请通过要求修改按钮确认。"}`)
	r = currentReview(t, s, task.ID, r.ID)
	if _, err = s.DecideReview(ctx, task.ID, r.ID, "CHANGES_REQUESTED", "按刚才讨论的方案增加说明", r.DiscussionVersion); err != nil {
		t.Fatal(err)
	}
	revision := startWork(t, s, a, task)
	if revision.SessionID != delivery.SessionID || revision.AgentID != a.ID {
		t.Fatal(revision)
	}
	spec := runSpec(t, s, revision.ID)
	if spec.RequireNativeSession || !strings.Contains(spec.Instructions, "按刚才讨论的方案增加说明") || spec.AgentSessionRef != "codex:delivery-session" {
		t.Fatal(spec)
	}
	finishWork(t, s, revision, 4, "review", "revised document")
	w, err := s.GetWorkDetail(ctx, task.ID)
	if err != nil || len(w.Reviews) != 2 || len(w.Artifacts) != 2 || w.Artifacts[0].Version != 2 {
		t.Fatal(w, err)
	}
}

func TestReviewChatSupersedeAndCancelDoNotLeakLateDelivery(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(fmtBool(started), func(t *testing.T) {
			ctx := context.Background()
			s, a, task, delivery, r := reviewFixture(t)
			q, err := s.MessageReview(ctx, task.ID, r.ID, "旧问题", "old", 0)
			if err != nil {
				t.Fatal(err)
			}
			var run model.Run
			if started {
				run, err = s.StartReviewTurn(ctx, q.ID, nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err = s.MessageWork(ctx, task.ID, "新的工作要求", "new", false); err != nil {
				t.Fatal(err)
			}
			if started {
				finishReviewChat(t, s, run, 3, "run.completed", `{"message":"晚到的聊天回复"}`)
			} else {
				cancelled, _ := s.GetReviewTurn(ctx, q.ID)
				if cancelled.State != "CANCELLED" {
					t.Fatal(cancelled)
				}
			}
			queue, err := s.PendingReviewTurns(ctx)
			if err != nil || len(queue) != 0 {
				t.Fatal(queue, err)
			}
			w, err := s.GetWorkDetail(ctx, task.ID)
			if err != nil || w.Reviews[0].State != "SUPERSEDED" || len(w.Artifacts) != 1 {
				t.Fatal(w, err)
			}
			next := startWork(t, s, a, task)
			if next.SessionID != delivery.SessionID {
				t.Fatal("lost session after late reply")
			}
		})
	}
}
func fmtBool(v bool) string {
	if v {
		return "running"
	}
	return "queued"
}

func TestReviewChatStopAndMissingNativeReference(t *testing.T) {
	ctx := context.Background()
	s, _, task, delivery, r := reviewFixture(t)
	for _, ref := range []string{"", "workspace:/placeholder"} {
		if _, err := s.db.Exec(`UPDATE session SET agent_session_ref=? WHERE session_id=?`, ref, delivery.SessionID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MessageReview(ctx, task.ID, r.ID, "不能丢掉原记忆", "missing", 0); !errors.Is(err, model.ErrConflict) {
			t.Fatal("accepted missing native memory", err)
		}
	}
	if _, err := s.db.Exec(`UPDATE session SET agent_session_ref='codex:delivery-session' WHERE session_id=?`, delivery.SessionID); err != nil {
		t.Fatal(err)
	}
	q, err := s.MessageReview(ctx, task.ID, r.ID, "可以取消", "cancel", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.StopReviewTurn(ctx, "other-task", r.ID, q.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("foreign stop", err)
	}
	if err = s.StopReviewTurn(ctx, task.ID, r.ID, q.ID); err != nil {
		t.Fatal(err)
	}
	q, err = s.GetReviewTurn(ctx, q.ID)
	if err != nil || q.State != "CANCELLED" || q.RunID != "" {
		t.Fatal(q, err)
	}
	r = currentReview(t, s, task.ID, r.ID)
	if _, err = s.DecideReview(ctx, task.ID, r.ID, "APPROVED", "取消提问后明确通过", r.DiscussionVersion); err != nil {
		t.Fatal(err)
	}
}

func TestReviewChatAndApprovalSerialize(t *testing.T) {
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		s, _, task, _, r := reviewFixture(t)
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := s.MessageReview(ctx, task.ID, r.ID, "race question", "race", 0)
			errs <- err
		}()
		go func() { defer wg.Done(); _, err := s.DecideReview(ctx, task.ID, r.ID, "APPROVED", ""); errs <- err }()
		wg.Wait()
		close(errs)
		success := 0
		for err := range errs {
			if err == nil {
				success++
			} else if !errors.Is(err, model.ErrConflict) {
				t.Fatal(err)
			}
		}
		if success != 1 {
			t.Fatal("question and approval both won", success)
		}
	}
}

func TestReviewReplyCannotContainBusinessActions(t *testing.T) {
	for _, raw := range []string{`null`, `{}`, `{"message":""}`, `{"message":"ok","decision":"APPROVED"}`, `{"message":"ok"} {}`, strings.Repeat("x", 40001)} {
		if _, err := workflow.ParseReviewReply(raw); err == nil {
			t.Fatal("accepted unsafe output", raw)
		}
	}
	message, err := workflow.ParseReviewReply(`{"message":"这里是解释，不是验收决定。"}`)
	if err != nil || message == "" {
		t.Fatal(message, err)
	}
	if !json.Valid((workflow.ReviewContract{}).Schema()) {
		t.Fatal("bad schema")
	}
}
