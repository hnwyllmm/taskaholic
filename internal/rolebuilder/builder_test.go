package rolebuilder

import (
	"encoding/json"
	"strings"
	"testing"

	"work-assistant/internal/model"
)

func TestStructuredRoleReplyValidation(t *testing.T) {
	reply := Reply{Message: "草案已准备好", Questions: []string{}, Draft: model.RoleSpec{Name: "文档作者", Description: "展示简介", Capabilities: []string{"document.write"}, Instructions: "整理资料，给出引用。", OutputContract: "可验证的文档", Boundaries: []string{"不捏造事实"}}}
	data, _ := json.Marshal(reply)
	builder := JSONBuilder{}
	if got, err := builder.Parse(string(data)); err != nil || got.Draft.Name != reply.Draft.Name {
		t.Fatalf("valid reply: %+v, %v", got, err)
	}
	for name, value := range map[string]string{
		"empty": "", "null": "null", "fence": "```json\n" + string(data) + "\n```", "trailing": string(data) + "{}",
		"missing description":  strings.Replace(string(data), `"description":"展示简介",`, "", 1),
		"null description":     strings.Replace(string(data), `"description":"展示简介"`, `"description":null`, 1),
		"unknown":              strings.Replace(string(data), `"name":`, `"runtime_id":"host", "name":`, 1),
		"missing instructions": strings.Replace(string(data), `"整理资料，给出引用。"`, `""`, 1),
		"bad capability":       strings.Replace(string(data), `"document.write"`, `"Document Writing"`, 1),
		"oversize":             strings.Repeat("x", 129*1024),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := builder.Parse(value); err == nil {
				t.Fatal("invalid output accepted")
			}
		})
	}
	if !json.Valid(builder.Schema()) {
		t.Fatal("invalid schema")
	}
}

func TestDisplayDescriptionIsNotExecutionInstruction(t *testing.T) {
	role := model.RoleSpec{Description: "DISPLAY_ONLY_SECRET", Instructions: "EXECUTION_INSTRUCTION", OutputContract: "RESULT"}
	if strings.Contains(role.ExecutionInstructions(), role.Description) || !strings.Contains(role.ExecutionInstructions(), role.Instructions) {
		t.Fatal("description/instructions boundary violated")
	}
}
