package router

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"

	"work-assistant/internal/model"
)

// AgentSelector is independent of runtime placement and can be replaced by a
// policy or AI ranking plugin. Hard constraints are rechecked by the store.
type AgentSelector interface {
	SelectAgent(context.Context, AgentRequest, []model.AgentProfile, []model.Runtime) (model.AgentProfile, error)
}

type AgentRequest struct {
	Requirements model.TaskRequirements
	AgentID      string
	RuntimeID    string
	AdapterID    string
	ModelID      string
}

type LeastLoaded struct{}

func (LeastLoaded) SelectAgent(_ context.Context, request AgentRequest, agents []model.AgentProfile, runtimes []model.Runtime) (model.AgentProfile, error) {
	available := make(map[string]model.Runtime)
	for _, runtime := range runtimes {
		if runtime.State == "ONLINE" {
			available[runtime.ID] = runtime
		}
	}
	agents = slices.Clone(agents)
	sort.Slice(agents, func(i, j int) bool {
		left, right := agents[i], agents[j]
		if left.ActiveRuns*right.MaxConcurrent != right.ActiveRuns*left.MaxConcurrent {
			return left.ActiveRuns*right.MaxConcurrent < right.ActiveRuns*left.MaxConcurrent
		}
		return left.ID < right.ID
	})
	eligible := make([]model.AgentProfile, 0, len(agents))
	for _, agent := range agents {
		if agent.State != "ACTIVE" || agent.ActiveRuns >= agent.MaxConcurrent || !Matches(request.Requirements, agent) {
			continue
		}
		if request.AgentID != "" && request.AgentID != agent.ID {
			continue
		}
		if request.RuntimeID != "" && request.RuntimeID != agent.RuntimeID {
			continue
		}
		if request.AdapterID != "" && request.AdapterID != agent.AdapterID {
			continue
		}
		if request.ModelID != "" && request.ModelID != agent.ModelID {
			continue
		}
		if !SupportsFeature(available[agent.RuntimeID], agent.AdapterID, "role_instructions") {
			continue
		}
		eligible = append(eligible, agent)
	}
	if request.Requirements.CostPreference == model.CostTierEconomy {
		for _, agent := range eligible {
			if model.NormalizeCostTier(agent.CostTier, agent.ModelID) == model.CostTierEconomy {
				return agent, nil
			}
		}
		// An economy preference is intentionally soft: suitability and forward
		// progress beat waiting forever for a low-cost member that is offline or
		// fully occupied.
	}
	if len(eligible) > 0 {
		return eligible[0], nil
	}
	return model.AgentProfile{}, fmt.Errorf("%w: no online role agent satisfies requirements and has capacity", model.ErrConflict)
}

func Matches(needs model.TaskRequirements, agent model.AgentProfile) bool {
	if needs.RoleID != "" && needs.RoleID != agent.RoleID {
		return false
	}
	if slices.Contains(needs.ExcludedAgentIDs, agent.ID) {
		return false
	}
	for _, capability := range needs.Capabilities {
		if !slices.Contains(agent.Role.Capabilities, capability) {
			return false
		}
	}
	return true
}

func SupportsFeature(runtime model.Runtime, adapterID, feature string) bool {
	var capabilities struct {
		Adapters map[string]map[string]any `json:"adapters"`
	}
	if json.Unmarshal(runtime.Capabilities, &capabilities) != nil {
		return false
	}
	value, _ := capabilities.Adapters[adapterID][feature].(bool)
	return value
}
