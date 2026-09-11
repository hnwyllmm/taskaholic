package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"work-assistant/internal/model"
)

func TestSourceInboxAndCursorSurviveDatabaseReopen(t *testing.T) {
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "control.sqlite")
	s, err := Open(file)
	if err != nil {
		t.Fatal(err)
	}
	source := saveTestSource(t, s, "antmultica")
	targets, err := s.ListSourceTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cursor := json.RawMessage(`{"issues":{"one":"revision"}}`)
	event := model.SourceEvent{Key: "issue:one:v1", Kind: "antmultica.issue", Entity: "workspace:one", Title: "Restore test", Message: "durable input"}
	if err = s.CommitSourcePoll(ctx, source, targets[0], cursor, "", []model.SourceEvent{event}, 0, false); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "control.sqlite")
	if err = s.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, dbfile := range []string{file, backup} {
		reopened, err := Open(dbfile)
		if err != nil {
			t.Fatal(err)
		}
		target := targetByID(t, reopened, targets[0].ID)
		if string(target.Cursor) != string(cursor) {
			t.Fatal("cursor lost on reopen/backup")
		}
		if err = reopened.ProcessSourceEvents(ctx); err != nil {
			t.Fatal(err)
		}
		if err = reopened.ProcessSourceEvents(ctx); err != nil {
			t.Fatal(err)
		}
		tasks, err := reopened.ListWork(ctx)
		if err != nil || len(tasks) != 1 {
			t.Fatal("inbox did not recover exactly once", tasks, err)
		}
		if err = reopened.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func saveTestSource(t *testing.T, s *Store, kind string) model.TaskSource {
	t.Helper()
	source := model.TaskSource{ID: kind, Kind: kind, Name: kind, Enabled: true, IntervalSeconds: 5}
	if kind == "antmultica" {
		source.Config = model.SourceConfig{WorkspaceID: "workspace", WorkspaceSlug: "seekdb", AssigneeID: "user", IterationKey: "迭代", IterationValue: "1.5.0"}
	}
	saved, err := s.SaveTaskSource(context.Background(), source, 0)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}
func pollStore(t *testing.T, s *Store, source model.TaskSource, target model.SourceTarget, head string, events ...model.SourceEvent) {
	t.Helper()
	cursor, _ := json.Marshal(map[string]any{"head": head, "stamp": time.Now().UnixNano()})
	if err := s.CommitSourcePoll(context.Background(), source, target, cursor, head, events, time.Now().UnixMilli(), false); err != nil {
		t.Fatal(err)
	}
	if err := s.ProcessSourceEvents(context.Background()); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListSourceEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range list {
		if e.Error != "" {
			t.Fatal(e.Kind, e.Error)
		}
	}
}
func targetByID(t *testing.T, s *Store, targetID string) model.SourceTarget {
	t.Helper()
	targets, err := s.ListSourceTargets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range targets {
		if v.ID == targetID {
			return v
		}
	}
	t.Fatal("target missing")
	return model.SourceTarget{}
}

func TestSourceImportIdempotencyRestartAndIterationConfig(t *testing.T) {
	ctx := context.Background()
	s := roleTestStore(t)
	source := saveTestSource(t, s, "antmultica")
	targets, targetErr := s.ListSourceTargets(ctx)
	if targetErr != nil || len(targets) != 1 {
		t.Fatal(targets, targetErr)
	}
	target := targets[0]
	event := model.SourceEvent{Key: "issue:one:v1", Kind: "antmultica.issue", Entity: "workspace:one", Title: "SEEK-1", Message: "Analyze this requirement"}
	pollStore(t, s, source, target, "", event)
	events, _ := s.ListSourceEvents(ctx)
	taskID := events[0].TaskID
	if taskID == "" {
		t.Fatal(events)
	}
	before, _ := s.GetWorkDetail(ctx, taskID)
	pollStore(t, s, source, targetByID(t, s, target.ID), "", event)
	after, _ := s.GetWorkDetail(ctx, taskID)
	if len(before.Messages) != len(after.Messages) {
		t.Fatal("duplicate task input")
	}
	tasks, _ := s.ListWork(ctx)
	if len(tasks) != 1 {
		t.Fatal("duplicate work", tasks)
	}
	source.Config.IterationValue = "1.6.0"
	changed, err := s.SaveTaskSource(ctx, source, source.Version)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Config.IterationValue != "1.6.0" {
		t.Fatal(changed)
	}
	if _, err = s.SaveTaskSource(ctx, source, source.Version); !errors.Is(err, model.ErrConflict) {
		t.Fatal("CAS missing", err)
	}
	target = targetByID(t, s, target.ID)
	if err = s.CommitSourcePoll(ctx, source, target, json.RawMessage(`{"stale":true}`), "", nil, 0, false); !errors.Is(err, model.ErrConflict) {
		t.Fatal("stale configuration accepted", err)
	}
	// Simulate the restart boundary: the durable cursor, mapping and inbox
	// remain authoritative even if the provider redelivers an old observation.
	if err = s.ProcessSourceEvents(ctx); err != nil {
		t.Fatal(err)
	}
	event.Key = "issue:one:v2"
	event.Message = "Updated scope"
	pollStore(t, s, changed, target, "", event)
	tasks, _ = s.ListWork(ctx)
	if len(tasks) != 1 || tasks[0].ID != taskID {
		t.Fatal("iteration edit replaced task")
	}
	after, _ = s.GetWorkDetail(ctx, taskID)
	if len(after.Messages) != len(before.Messages)+1 {
		t.Fatal("missing update")
	}
}

func TestAntMulticaLifecycleUpdatesExistingTaskAndAuditEvidence(t *testing.T) {
	ctx := context.Background()
	s := roleTestStore(t)
	source := saveTestSource(t, s, "antmultica")
	target := targetByID(t, s, mustSingleTargetID(t, s))
	entity := "antmultica:workspace:one"
	pollStore(t, s, source, target, "", model.SourceEvent{Key: "issue:one:v1", Kind: "antmultica.issue", Entity: entity, Title: "SEEK-1", Message: "active issue"})
	events, err := s.ListSourceEvents(ctx)
	if err != nil || len(events) != 1 || events[0].TaskID == "" {
		t.Fatal(events, err)
	}
	taskID := events[0].TaskID
	target = targetByID(t, s, target.ID)
	pollStore(t, s, source, target, "", model.SourceEvent{Key: "antmultica.closed:one:v2", Kind: "antmultica.closed", Entity: entity, Message: "issue closed"})
	target = targetByID(t, s, target.ID)
	pollStore(t, s, source, target, "", model.SourceEvent{Key: "antmultica.reopened:one:v3", Kind: "antmultica.reopened", Entity: entity, Message: "issue reopened"})
	var latest string
	if err = s.db.QueryRow(`SELECT event_type FROM event_log WHERE aggregate_type='task' AND aggregate_id=? AND event_type IN ('ExternalTaskClosed','ExternalTaskReopened') ORDER BY global_seq DESC LIMIT 1`, taskID).Scan(&latest); err != nil || latest != "ExternalTaskReopened" {
		t.Fatalf("source lifecycle was not auditable: %q, %v", latest, err)
	}
	work, err := s.GetWorkDetail(ctx, taskID)
	if err != nil || len(work.Messages) < 3 || work.Messages[len(work.Messages)-1].Content != "issue reopened" {
		t.Fatalf("reopen did not update the existing task: %#v, %v", work.Messages, err)
	}
	tasks, err := s.ListWork(ctx)
	if err != nil || len(tasks) != 1 || tasks[0].ID != taskID {
		t.Fatalf("lifecycle transition created a duplicate task: %#v, %v", tasks, err)
	}
}

func TestSourceAdapterCanSupplyDeterministicTaskProfileFacts(t *testing.T) {
	ctx := context.Background()
	s := roleTestStore(t)
	source := saveTestSource(t, s, "antmultica")
	target := targetByID(t, s, mustSingleTargetID(t, s))
	event := model.SourceEvent{
		Key:          "issue:profile:v1",
		Kind:         "antmultica.issue",
		Entity:       "workspace:profile",
		Title:        "SEEK-42",
		Message:      "Fix the confirmed regression",
		TaskType:     "bug",
		Repository:   "oceanbase/seekdb",
		WorkflowType: "standard",
	}
	pollStore(t, s, source, target, "", event)
	events, err := s.ListSourceEvents(ctx)
	if err != nil || len(events) != 1 || events[0].TaskID == "" {
		t.Fatal(events, err)
	}
	assignment, _, err := s.PrepareTaskOptimization(ctx, events[0].TaskID)
	if err != nil {
		t.Fatal(err)
	}
	profile := assignment.Profile
	if profile.SourceType != "antmultica" || profile.TaskType != "bug" || profile.Repository != "oceanbase/seekdb" || profile.Workflow != "standard" {
		t.Fatalf("source facts were not pinned exactly: %#v", profile)
	}

	badTarget := targetByID(t, s, target.ID)
	err = s.CommitSourcePoll(ctx, source, badTarget, json.RawMessage(`{"next":true}`), "", []model.SourceEvent{{
		Key: "issue:invalid-profile", Kind: "antmultica.issue", Entity: "workspace:bad", Title: "bad", Message: "bad", TaskType: "guessed",
	}}, 0, false)
	if !errors.Is(err, model.ErrValidation) {
		t.Fatal("invalid source profile was accepted", err)
	}
}

func mustSingleTargetID(t *testing.T, s *Store) string {
	t.Helper()
	targets, err := s.ListSourceTargets(context.Background())
	if err != nil || len(targets) != 1 {
		t.Fatal(targets, err)
	}
	return targets[0].ID
}

func TestPRUpdatesKeepOwnerSessionAndHumanPause(t *testing.T) {
	ctx := context.Background()
	s, author, task := workFixture(t)
	run := startWork(t, s, author, task)
	_, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: author.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 1, RunID: run.ID, SessionID: run.SessionID, Type: "session.bound", AgentSessionRef: "native:original"})
	if err != nil {
		t.Fatal(err)
	}
	source := saveTestSource(t, s, "github")
	target, err := s.RegisterPR(ctx, task.ID, source.ID, "https://github.com/oceanbase/seekdb/pull/123")
	if err != nil {
		t.Fatal(err)
	}
	e := model.SourceEvent{Key: "comment:1", Kind: "github.comment", Message: "Please handle new feedback"}
	pollStore(t, s, source, target, strings.Repeat("a", 40), e)
	work, _ := s.GetWorkDetail(ctx, task.ID)
	if work.Messages[len(work.Messages)-1].Delivery != "PENDING" {
		t.Fatal("lost in-flight input")
	}
	detail, _ := s.GetTaskDetail(ctx, task.ID)
	if len(detail.Runs) != 1 {
		t.Fatal("parallel run created")
	}
	finishWork(t, s, run, 2, "review", "v1")
	next := startWork(t, s, author, task)
	session, _ := s.GetTaskSession(ctx, task.ID)
	if next.SessionID != run.SessionID || session.AgentSessionRef != "native:original" || next.AgentID != author.ID {
		t.Fatal("affinity lost", next, session)
	}
	finishWork(t, s, next, 3, "review", "v2")
	if err = s.PauseWork(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	e.Key = "comment:2"
	pollStore(t, s, source, targetByID(t, s, target.ID), strings.Repeat("a", 40), e)
	config, _ := s.GetWorkConfig(ctx, task.ID)
	paused, _ := s.GetTask(ctx, task.ID)
	if !config.Paused || paused.State != model.TaskStatePaused {
		t.Fatal("external event unpaused task")
	}
	// Resumption is a human decision; it still keeps native memory.
	_, err = s.MessageWork(ctx, task.ID, "resume", "resume", false)
	if err != nil {
		t.Fatal(err)
	}
	last := startWork(t, s, author, task)
	if last.SessionID != run.SessionID {
		t.Fatal("resume lost session")
	}
	finishWork(t, s, last, 4, "review", "v3")
	work, _ = s.GetWorkDetail(ctx, task.ID)
	if _, err = s.DecideReview(ctx, task.ID, work.Reviews[0].ID, "APPROVED", ""); err != nil {
		t.Fatal(err)
	}
	e.Key = "comment:3"
	pollStore(t, s, source, targetByID(t, s, target.ID), strings.Repeat("a", 40), e)
	done, _ := s.GetTask(ctx, task.ID)
	if done.State != model.TaskStateCompleted {
		t.Fatal("completed task reopened")
	}
	events, _ := s.ListSourceEvents(ctx)
	if events[0].State != "RECORDED" {
		t.Fatal("late event not retained", events)
	}
}

func TestPRReviewFanoutFaninAndSupersession(t *testing.T) {
	ctx := context.Background()
	s, author, parent := workFixture(t)
	run := startWork(t, s, author, parent)
	finishWork(t, s, run, 1, "review", "PR submitted")
	var reviewers []model.AgentProfile
	for i := range 3 {
		role := publishTestRole(t, s, []string{"architecture.review", "qa.review", "code.review"}[i])
		a, err := s.CreateAgent(ctx, model.AgentProfile{Name: fmt.Sprintf("reviewer-%d", i), RoleID: role.ID, RuntimeID: author.RuntimeID, AdapterID: author.AdapterID, ModelID: author.ModelID, MaxConcurrent: 1})
		if err != nil {
			t.Fatal(err)
		}
		reviewers = append(reviewers, a)
	}
	source := saveTestSource(t, s, "github")
	target, err := s.RegisterPR(ctx, parent.ID, source.ID, "https://github.com/oceanbase/seekdb/pull/123")
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	e := model.SourceEvent{Key: "head:" + head, Kind: "github.head", HeadSHA: head, Message: "diff"}
	pollStore(t, s, source, target, head, e)
	reviews, _ := s.ListSourceReviews(ctx)
	if len(reviews) != 3 {
		t.Fatal("expected three independent reviews", reviews)
	}
	task, _ := s.GetTask(ctx, parent.ID)
	if task.State != model.TaskStateWaiting {
		t.Fatal("parent not waiting", task.State)
	}
	for i, review := range reviews {
		task, _ := s.GetTask(ctx, review.TaskID)
		parentConfig, _ := s.GetWorkConfig(ctx, parent.ID)
		childConfig, err := s.GetWorkConfig(ctx, task.ID)
		if err != nil || childConfig.Project != parentConfig.Project || childConfig.AgentID != "" || task.AssignedAgentID != "" {
			t.Fatal("review must inherit task context but remain unassigned until Router selection", childConfig, err)
		}
		if len(task.Requirements.ExcludedAgentIDs) != 1 || task.Requirements.ExcludedAgentIDs[0] != author.ID {
			t.Fatal("author not excluded")
		}
		var a model.AgentProfile
		for _, candidate := range reviewers {
			if candidate.RoleID == review.RoleID {
				a = candidate
			}
		}
		rr := startWork(t, s, a, task)
		finishWork(t, s, rr, int64(i+2), "review", fmt.Sprintf("finding %d", i))
		completed, _ := s.GetTask(ctx, task.ID)
		if completed.State != model.TaskStateCompleted {
			t.Fatal("internal review waits for human", completed)
		}
		if _, err = s.GetLatestTaskSummary(ctx, task.ID); err != nil {
			t.Fatal("missing reviewer summary", err)
		}
	}
	source.Enabled = false
	source, err = s.SaveTaskSource(ctx, source, source.Version)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CollectSourceReviews(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.CollectSourceReviews(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.ProcessSourceEvents(ctx); err != nil {
		t.Fatal(err)
	}
	detail, _ := s.GetWorkDetail(ctx, parent.ID)
	feedback := 0
	for _, m := range detail.Messages {
		if strings.Contains(m.Content, "Agent 评审已全部完成") {
			feedback++
		}
	}
	if feedback != 1 {
		t.Fatal("fan-in not exactly once", feedback)
	}
	continued := startWork(t, s, author, parent)
	if continued.SessionID != run.SessionID {
		t.Fatal("author memory lost")
	}
	finishWork(t, s, continued, 8, "review", "addressed")
	source.Enabled = true
	source, err = s.SaveTaskSource(ctx, source, source.Version)
	if err != nil {
		t.Fatal(err)
	}
	newHead := strings.Repeat("b", 40)
	e.Key = "head:" + newHead
	e.HeadSHA = newHead
	pollStore(t, s, source, targetByID(t, s, target.ID), newHead, e)
	reviews, _ = s.ListSourceReviews(ctx)
	old, current := 0, 0
	for _, r := range reviews {
		if r.State == "SUPERSEDED" {
			old++
		} else {
			current++
		}
	}
	if old != 3 || current != 3 {
		t.Fatal("head revisions mixed", reviews)
	}
	roots, err := s.ListRootWork(ctx)
	if err != nil || len(roots) != 1 || roots[0].ID != parent.ID ||
		roots[0].Subtasks.Total != 3 || roots[0].Subtasks.Superseded != 3 || roots[0].Subtasks.Completed != 0 {
		t.Fatal("old-version reviews leaked into current work progress", roots, err)
	}
	hierarchy, err := s.GetWorkHierarchy(ctx, parent.ID)
	if err != nil || len(hierarchy.Children) != 6 {
		t.Fatal("historical review workbenches disappeared", len(hierarchy.Children), err)
	}
	for _, child := range hierarchy.Children {
		if child.ReviewHeadSHA == "" || child.SourceReviewState == "" {
			t.Fatal("review version metadata missing", child)
		}
	}
	// A delayed failure for the previous SHA is retained but never sent.
	before, _ := s.GetWorkDetail(ctx, parent.ID)
	pollStore(t, s, source, targetByID(t, s, target.ID), newHead, model.SourceEvent{Key: "late-ci", Kind: "github.ci_failed", HeadSHA: head, Message: "old failure"})
	after, _ := s.GetWorkDetail(ctx, parent.ID)
	if len(after.Messages) != len(before.Messages) {
		t.Fatal("stale failure restarted author")
	}
}

func TestPRRegistrationCannotHijackOrBypassSourceControls(t *testing.T) {
	ctx := context.Background()
	s, a, task := workFixture(t)
	source := saveTestSource(t, s, "github")
	if _, err := s.RegisterPR(ctx, task.ID, source.ID, "https://github.com/o/r/pull/1"); !errors.Is(err, model.ErrConflict) {
		t.Fatal("unowned task accepted", err)
	}
	startWork(t, s, a, task)
	pr, err := s.RegisterPR(ctx, task.ID, source.ID, "https://github.com/o/r/pull/1")
	if err != nil {
		t.Fatal(err)
	}
	same, err := s.RegisterPR(ctx, task.ID, source.ID, "https://github.com/O/R/pull/1")
	if err != nil || same.ID != pr.ID {
		t.Fatal("registration not idempotent", same, err)
	}
	other, err := s.CreateWork(ctx, CreateWorkRequest{Title: "other", Goal: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RegisterPR(ctx, other.ID, source.ID, pr.Entity); !errors.Is(err, model.ErrConflict) {
		t.Fatal("PR owner hijacked", err)
	}
	for _, url := range []string{"https://evil.test/o/r/pull/1", "https://github.com.evil.test/o/r/pull/1", "https://user:pass@github.com/o/r/pull/1", "http://github.com/o/r/pull/1", "https://github.com/o/r/pull/1?token=secret"} {
		if _, err = s.RegisterPR(ctx, task.ID, source.ID, url); !errors.Is(err, model.ErrValidation) {
			t.Fatal("unsafe URL", url, err)
		}
	}
	stale := pr
	if err = s.SetSourceTargetEnabled(ctx, pr.ID, false); err != nil {
		t.Fatal(err)
	}
	if err = s.CommitSourcePoll(ctx, source, stale, json.RawMessage("{}"), "", nil, 0, false); !errors.Is(err, model.ErrConflict) {
		t.Fatal("disabled target accepted in-flight poll", err)
	}
}
