// Package rolebuilder defines the generation policy, not an inference provider.
// Execution goes through the normal distributed runtime/adapter protocol.
package rolebuilder

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"work-assistant/internal/model"
)

type Builder interface {
	Prompt(model.RoleDraft, string) string
	Schema() json.RawMessage
}

type Reply struct {
	Message   string         `json:"message"`
	Questions []string       `json:"questions"`
	Draft     model.RoleSpec `json:"draft"`
}

type JSONBuilder struct{}

func (JSONBuilder) Prompt(draft model.RoleDraft, message string) string {
	spec, _ := json.Marshal(draft.Spec)
	return `You help the user design a reusable work-assistant role. Reply in the user's language.
Create or revise a concrete draft; ask up to three concise questions only if useful.
Roles are not limited to coding: design, review, security, testing, writing, research and coordination are all possible.
Description is display-only. Put actual responsibilities, first checks, workflow, escalation and review expectations in instructions.
Capabilities are stable routing labels (e.g. code.implement, code.review, security.review, document.write); use lowercase letters, dots, underscores or dashes. Reuse existing suitable labels rather than inventing synonyms.
Use output_contract for acceptance criteria and deliverables, and boundaries for what not to do or what requires human approval.
Do not execute the described role, run commands, read files, invoke tools, or publish anything. Only design configuration. Never place credentials in the role. A role does not grant permissions. Do not include runtime, model or machine configuration in your reply.
Return ONLY the JSON object required by the supplied schema, including message, questions and a complete draft. No markdown fences.
The current draft below is the user's latest edited version and takes precedence over earlier drafts in this conversation.
Current draft: ` + string(spec) + "\nUser request: " + message
}

func (JSONBuilder) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","additionalProperties":false,"required":["message","questions","draft"],"properties":{"message":{"type":"string"},"questions":{"type":"array","items":{"type":"string"}},"draft":{"type":"object","additionalProperties":false,"required":["name","description","capabilities","instructions","output_contract","boundaries"],"properties":{"name":{"type":"string"},"description":{"type":"string"},"capabilities":{"type":"array","items":{"type":"string"}},"instructions":{"type":"string"},"output_contract":{"type":"string"},"boundaries":{"type":"array","items":{"type":"string"}}}}}}`)
}

func (JSONBuilder) Parse(output string) (Reply, error) {
	var reply Reply
	if len(output) > 128*1024 {
		return reply, errors.New("role builder output exceeds 128 KiB")
	}
	decoder := json.NewDecoder(bytes.NewBufferString(output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&reply); err != nil {
		return reply, fmt.Errorf("invalid role builder JSON: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return reply, errors.New("role builder returned trailing content")
	}
	var envelope map[string]json.RawMessage
	_ = json.Unmarshal([]byte(output), &envelope)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(envelope["draft"], &fields)
	for _, name := range []string{"name", "description", "capabilities", "instructions", "output_contract", "boundaries"} {
		if raw := bytes.TrimSpace(fields[name]); len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
			return reply, fmt.Errorf("role builder draft must include non-null %s", name)
		}
	}
	if strings.TrimSpace(reply.Message) == "" || len(reply.Message) > 8000 || reply.Questions == nil || len(reply.Questions) > 3 || reply.Draft.Boundaries == nil {
		return reply, errors.New("role builder must return message, up to three questions, and draft with boundaries")
	}
	for _, question := range reply.Questions {
		if len(question) > 2000 {
			return reply, errors.New("role builder question is too long")
		}
	}
	if err := reply.Draft.Validate(true); err != nil {
		return reply, err
	}
	return reply, nil
}
