package concierge

import (
	"encoding/json"
	"testing"
)

func TestStructuredProposals(t *testing.T) {
	if !json.Valid(JSONAssistant{}.Schema()) {
		t.Fatal("invalid schema")
	}
	for _, tc := range []struct {
		raw   string
		valid bool
	}{
		{`{"message":"目前没有需要你处理的工作。","proposal":null}`, true},
		{`{"message":"请确认","proposal":{"kind":"create_work","title":"文档","text":"write doc","target_task_id":"","role_id":"writer","role_spec":null}}`, true},
		{`{"message":"请确认","proposal":{"kind":"approve","title":"批准","text":"yes","target_task_id":"task","role_id":"","role_spec":null}}`, false},
		{`{"message":"请确认","proposal":{"kind":"create_work","title":"文档","text":"write doc","target_task_id":"task","role_id":"writer","role_spec":null}}`, false},
		{`{"message":"请确认","proposal":{"kind":"pause_work","title":"暂停","text":"pause","target_task_id":"task","role_id":"","role_spec":null,"command":["rm"]}}`, false},
		{`{"message":"请确认准备候选版本","proposal":{"kind":"upgrade_system","title":"增加升级状态页","text":"增加升级状态及验收说明，相关测试通过。","target_task_id":"","role_id":"","role_spec":null}}`, true},
		{`{"message":"非法升级","proposal":{"kind":"upgrade_system","title":"升级","text":"change","target_task_id":"task","role_id":"","role_spec":null}}`, false},
		{`{"message":"x","proposal":null} {}`, false}, {`{}`, false},
	} {
		_, err := Parse(tc.raw)
		if (err == nil) != tc.valid {
			t.Fatalf("parse %s: %v", tc.raw, err)
		}
	}
}
