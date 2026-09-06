package model

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var ErrConflict = errors.New("conflict")
var ErrValidation = errors.New("invalid input")

// RoleSpec is reusable policy, independent of a machine, model, or session.
// Description is display-only; only ExecutionInstructions enters the prompt.
type RoleSpec struct {
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	Capabilities   []string `json:"capabilities"`
	Instructions   string   `json:"instructions"`
	OutputContract string   `json:"output_contract"`
	Boundaries     []string `json:"boundaries"`
}

type Role struct {
	ID string `json:"role_id"`
	RoleSpec
	Version     int64 `json:"version"`
	CreatedAtMS int64 `json:"created_at_ms"`
}

type RoleMessage struct {
	Speaker     string `json:"speaker"`
	Content     string `json:"content"`
	RunID       string `json:"run_id,omitempty"`
	CreatedAtMS int64  `json:"created_at_ms"`
}

type RoleDraft struct {
	ID              string        `json:"draft_id"`
	TaskID          string        `json:"task_id"`
	State           string        `json:"state"`
	Version         int64         `json:"version"`
	Spec            RoleSpec      `json:"spec"`
	Messages        []RoleMessage `json:"messages"`
	Questions       []string      `json:"questions"`
	LastRunID       string        `json:"last_run_id,omitempty"`
	PublishedRoleID string        `json:"published_role_id,omitempty"`
	Error           string        `json:"error,omitempty"`
	CreatedAtMS     int64         `json:"created_at_ms"`
	UpdatedAtMS     int64         `json:"updated_at_ms"`
}

// AgentProfile is a stable employee identity. Multiple employees can share a role.
type AgentProfile struct {
	Version       int64  `json:"version"`
	ID            string `json:"agent_id"`
	Name          string `json:"name"`
	RoleID        string `json:"role_id"`
	RuntimeID     string `json:"runtime_id"`
	AdapterID     string `json:"adapter_id"`
	ModelID       string `json:"model_id,omitempty"`
	MaxConcurrent int    `json:"max_concurrent"`
	State         string `json:"state"`
	CreatedAtMS   int64  `json:"created_at_ms"`
	Role          Role   `json:"role"`
	ActiveRuns    int    `json:"active_runs"`
}

type TaskRequirements struct {
	RoleID           string   `json:"role_id,omitempty"`
	Capabilities     []string `json:"capabilities,omitempty"`
	ExcludedAgentIDs []string `json:"excluded_agent_ids,omitempty"`
}

func (r TaskRequirements) IsEmpty() bool {
	return r.RoleID == "" && len(r.Capabilities) == 0 && len(r.ExcludedAgentIDs) == 0
}

var capabilityName = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

func ValidateCapabilities(values []string) error {
	if len(values) > 32 {
		return fmt.Errorf("%w: at most 32 capabilities", ErrValidation)
	}
	seen := make(map[string]bool)
	for _, value := range values {
		if !capabilityName.MatchString(value) || seen[value] {
			return fmt.Errorf("%w: capability %q must be unique and use lowercase letters, digits, dot, dash or underscore", ErrValidation, value)
		}
		seen[value] = true
	}
	return nil
}

func (r RoleSpec) Validate(publish bool) error {
	if len(r.Name) > 200 || len(r.Description) > 4000 || len(r.Instructions) > 32000 || len(r.OutputContract) > 8000 || len(r.Boundaries) > 32 {
		return fmt.Errorf("%w: role fields exceed size limits", ErrValidation)
	}
	for _, boundary := range r.Boundaries {
		if len(boundary) > 2000 {
			return fmt.Errorf("%w: boundary exceeds 2000 bytes", ErrValidation)
		}
	}
	if publish && (strings.TrimSpace(r.Name) == "" || strings.TrimSpace(r.Instructions) == "" || strings.TrimSpace(r.OutputContract) == "" || len(r.Capabilities) == 0) {
		return fmt.Errorf("%w: name, instructions, output_contract and capabilities are required to publish", ErrValidation)
	}
	return ValidateCapabilities(r.Capabilities)
}

func (r TaskRequirements) Validate() error {
	if len(r.RoleID) > 200 || len(r.ExcludedAgentIDs) > 100 {
		return fmt.Errorf("%w: task requirements exceed limits", ErrValidation)
	}
	for _, agentID := range r.ExcludedAgentIDs {
		if agentID == "" || len(agentID) > 200 {
			return fmt.Errorf("%w: invalid excluded agent id", ErrValidation)
		}
	}
	return ValidateCapabilities(r.Capabilities)
}

func (r RoleSpec) ExecutionInstructions() string {
	text := "Role instructions:\n" + r.Instructions + "\n\nRequired deliverables:\n" + r.OutputContract
	if len(r.Boundaries) > 0 {
		text += "\n\nBoundaries (these do not grant tool or system permissions):\n- " + strings.Join(r.Boundaries, "\n- ")
	}
	return text
}
