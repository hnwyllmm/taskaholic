package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"work-assistant/internal/id"
	"work-assistant/internal/model"
)

func hierarchyWork(t *testing.T, s *Store, title, state string) model.Task {
	t.Helper()
	task, err := s.CreateWork(context.Background(), CreateWorkRequest{Title: title, Goal: "goal " + title, DeferAssignment: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE task SET state=? WHERE task_id=?`, state, task.ID); err != nil {
		t.Fatal(err)
	}
	task.State = state
	return task
}

func hierarchyEdge(t *testing.T, s *Store, parent, child, kind string) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO task_edge(edge_id,from_task_id,to_task_id,edge_type,created_at_ms)
		VALUES(?,?,?,?,1)`, id.New("edge"), parent, child, kind); err != nil {
		t.Fatal(err)
	}
}

func TestWorkHierarchyUsesContainmentAndPreservesTaskIdentity(t *testing.T) {
	ctx := context.Background()
	s := roleTestStore(t)
	root := hierarchyWork(t, s, "Original requirement", model.TaskStateWaiting)
	other := hierarchyWork(t, s, "Independent work", model.TaskStateNew)
	reviewer := hierarchyWork(t, s, "Review", model.TaskStateBlocked)
	child := hierarchyWork(t, s, "Implementation", model.TaskStateCompleted)
	grandchild := hierarchyWork(t, s, "Nested validation", model.TaskStateReview)
	hierarchyEdge(t, s, root.ID, reviewer.ID, "REVIEWS")
	hierarchyEdge(t, s, root.ID, child.ID, model.TaskEdgeDecomposedInto)
	hierarchyEdge(t, s, reviewer.ID, grandchild.ID, model.TaskEdgeDecomposedInto)
	hierarchyEdge(t, s, child.ID, grandchild.ID, model.TaskEdgeDecomposedInto)
	hierarchyEdge(t, s, root.ID, other.ID, "DEPENDS_ON")
	before, _ := s.GetTask(ctx, root.ID)
	items, err := s.ListRootWork(ctx)
	if err != nil || len(items) != 2 {
		t.Fatal("children leaked or independent dependency disappeared", items, err)
	}
	for _, item := range items {
		if item.ID == root.ID && (item.Subtasks.Total != 3 || item.Subtasks.Completed != 1 || item.Subtasks.NeedsAttention != 2) {
			t.Fatal("descendant progress must deduplicate shared descendants", item.Subtasks)
		}
	}
	h, err := s.GetWorkHierarchy(ctx, root.ID)
	if err != nil || len(h.Children) != 2 || len(h.Parents) != 0 {
		t.Fatal("root detail", h, err)
	}
	h, err = s.GetWorkHierarchy(ctx, grandchild.ID)
	if err != nil || len(h.Children) != 0 || len(h.Parents) != 2 || len(h.Roots) != 1 || h.Roots[0].ID != root.ID {
		t.Fatal("nested links lost original root", h, err)
	}
	after, _ := s.GetTask(ctx, root.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("read projection changed task state or ownership")
	}
	if _, err = s.GetWorkHierarchy(ctx, "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("missing workbench must not look like an empty root", err)
	}
	// Unrelated association is not a navigable workbench parent.
	legacy, _, err := s.CreateTask(ctx, "", "Legacy task", "not managed")
	if err != nil {
		t.Fatal(err)
	}
	hierarchyEdge(t, s, legacy.ID, other.ID, model.TaskEdgeDecomposedInto)
	items, err = s.ListRootWork(ctx)
	if err != nil || len(items) != 2 {
		t.Fatal("hidden under an inaccessible legacy parent", items, err)
	}
}

func TestRootLimitIsAppliedAfterFilteringChildren(t *testing.T) {
	ctx := context.Background()
	s := roleTestStore(t)
	root := hierarchyWork(t, s, "Older root", model.TaskStateWaiting)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 0; i < 505; i++ {
		child, err := createWorkTx(ctx, tx, CreateWorkRequest{Title: fmt.Sprintf("Review %03d", i), Goal: "review", DeferAssignment: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(`INSERT INTO task_edge(edge_id,from_task_id,to_task_id,edge_type,created_at_ms)
			VALUES(?,?,?,'REVIEWS',1)`, id.New("edge"), root.ID, child.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	roots, err := s.ListRootWork(ctx)
	if err != nil || len(roots) != 1 || roots[0].ID != root.ID || roots[0].Subtasks.Total != 505 {
		t.Fatal("child volume evicted the original root", len(roots), err)
	}
	h, err := s.GetWorkHierarchy(ctx, root.ID)
	if err != nil || len(h.Children) != 505 {
		t.Fatal("children silently truncated", len(h.Children), err)
	}
}
