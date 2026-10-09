package agent

import (
	"context"

	"work-assistant/internal/model"
)

// Event is an adapter-local observation. The runtime wraps it in its own
// durable, monotonically sequenced event envelope before sending it upstream.
type Event struct {
	Execution       *model.ExecutionSettings
	Activity        *model.Action
	Usage           *model.TokenUsage
	Attributes      map[string]any
	Type            string
	Message         string
	Stream          string
	DirectiveID     string
	AgentSessionRef string
	Error           string
}

type Result struct {
	Output   string
	ExitCode int
	Err      error
}

// Adapter is the extension boundary for concrete coding agents. The MVP ships
// an exec adapter, while Codex, Claude Code, or an internal agent can implement
// this interface without changing the control protocol.
type Adapter interface {
	Name() string
	Capabilities() map[string]any
	Run(context.Context, model.RunSpec, string, <-chan model.Directive, func(Event)) Result
}

// ModelProvider is optional: adapters without discovery still accept manually
// configured model IDs. Implementations must honor cancellation and bound output.
type ModelProvider interface {
	ListModels(context.Context) ([]model.ModelOption, error)
}
