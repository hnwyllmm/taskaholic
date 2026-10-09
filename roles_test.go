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

func TestRoleRouterPrefersEconomyMemberForDelegatedWork(t *testing.T) {
	runtime := model.Runtime{ID: "runtime", State: "ONLINE", Capabilities: json.RawMessage(`{"adapters":{"codex-agent":{"role_instructions":true}}}`)}
	makeAgent := func(id, tier string, active int) model.AgentProfile {
		return model.AgentProfile{ID: id, RuntimeID: "runtime", AdapterID: "codex-agent", State: "ACTIVE", MaxConcurrent: 1, ActiveRuns: active, CostTier: tier, Role: model.Role{RoleSpec: model.RoleSpec{Capabilities: []string{"document.write"}}}}
	}
	standard := makeAgent("a-standard", model.CostTierStandard, 0)
	economy := makeAgent("z-economy", model.CostTierEconomy, 0)
	selector := LeastLoaded{}
	request := AgentRequest{Requirements: model.TaskRequirements{Capabilities: []string{"document.write"}, CostPreference: model.CostTierEconomy, Delegated: true}}
	selected, err := selector.SelectAgent(context.Background(), request, []model.AgentProfile{standard, economy}, []model.Runtime{runtime})
	if err != nil || selected.ID != economy.ID {
		t.Fatalf("economy preference = %+v, %v", selected, err)
	}
	economy.ActiveRuns = 1
	selected, err = selector.SelectAgent(context.Background(), request, []model.AgentProfile{standard, economy}, []model.Runtime{runtime})
	if err != nil || selected.ID != standard.ID {
		t.Fatalf("economy fallback = %+v, %v", selected, err)
	}
}
