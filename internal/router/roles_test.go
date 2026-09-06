package router

import (
	"context"
	"encoding/json"
	"testing"

	"work-assistant/internal/model"
)

func TestRoleRouterMatchesCapabilitiesExcludesAuthorAndHonorsCapacity(t *testing.T) {
	runtime := model.Runtime{ID: "runtime", State: "ONLINE", Capabilities: json.RawMessage(`{"adapters":{"codex-agent":{"role_instructions":true}}}`)}
	makeAgent := func(id, cap string, active int) model.AgentProfile {
		return model.AgentProfile{ID: id, RoleID: cap, RuntimeID: "runtime", AdapterID: "codex-agent", State: "ACTIVE", MaxConcurrent: 2, ActiveRuns: active, Role: model.Role{RoleSpec: model.RoleSpec{Capabilities: []string{cap}}}}
	}
	agents := []model.AgentProfile{makeAgent("author", "code.implement", 0), makeAgent("reviewer-1", "code.review", 1), makeAgent("reviewer-2", "code.review", 0)}
	selector := LeastLoaded{}
	needs := model.TaskRequirements{Capabilities: []string{"code.review"}, ExcludedAgentIDs: []string{"author"}}
	selected, err := selector.SelectAgent(context.Background(), AgentRequest{Requirements: needs}, agents, []model.Runtime{runtime})
	if err != nil || selected.ID != "reviewer-2" {
		t.Fatalf("selection: %+v %v", selected, err)
	}
	needs.ExcludedAgentIDs = append(needs.ExcludedAgentIDs, "reviewer-2")
	selected, err = selector.SelectAgent(context.Background(), AgentRequest{Requirements: needs}, agents, []model.Runtime{runtime})
	if err != nil || selected.ID != "reviewer-1" {
		t.Fatalf("exclusion: %+v %v", selected, err)
	}
	if _, err = selector.SelectAgent(context.Background(), AgentRequest{Requirements: needs, AgentID: "author"}, agents, []model.Runtime{runtime}); err == nil {
		t.Fatal("explicit id bypassed requirements")
	}
	if _, err = selector.SelectAgent(context.Background(), AgentRequest{Requirements: needs}, agents, nil); err == nil {
		t.Fatal("offline agent selected")
	}
	agents[1].ActiveRuns = 2
	if _, err = selector.SelectAgent(context.Background(), AgentRequest{Requirements: needs}, agents, []model.Runtime{runtime}); err == nil {
		t.Fatal("busy agent selected")
	}
}
