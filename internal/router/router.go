// Package router defines the control-plane extension point that assigns a run
// to a connected runtime. The default policy is deliberately deterministic;
// richer policies can consider load, labels, affinity, and prior ownership.
package router

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"work-assistant/internal/model"
)

type Request struct {
	Task      model.Task
	AdapterID string
}

type Selector interface {
	Select(context.Context, Request, []model.Runtime) (string, error)
}

type FirstOnline struct{}

func (FirstOnline) Select(_ context.Context, request Request, runtimes []model.Runtime) (string, error) {
	sort.Slice(runtimes, func(i, j int) bool { return runtimes[i].ID < runtimes[j].ID })
	for _, candidate := range runtimes {
		if candidate.State != "ONLINE" || !supportsAgent(candidate, request.AdapterID) {
			continue
		}
		return candidate.ID, nil
	}
	return "", errors.New("no online runtime supports the requested agent")
}

func supportsAgent(candidate model.Runtime, agentID string) bool {
	if agentID == "" {
		return true
	}
	var capabilities struct {
		Adapters map[string]json.RawMessage `json:"adapters"`
	}
	if json.Unmarshal(candidate.Capabilities, &capabilities) != nil {
		return false
	}
	_, ok := capabilities.Adapters[agentID]
	return ok
}
