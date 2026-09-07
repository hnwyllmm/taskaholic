package workflow

import (
	"strings"
	"testing"
)

func TestResultValidation(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `{"outcome":"review","message":"done","artifacts":null}`, `{"outcome":"complete","message":"done","artifacts":[]}`, `{"outcome":"review","message":"done","artifacts":[],"extra":1}`, `{"outcome":"review","message":"done","artifacts":[]} {}`, `{"outcome":"review","message":"done","artifacts":[{"name":"../secret","content":"x"}]}`, `{"outcome":"review","message":"done","artifacts":[{"name":"a","content":"x"},{"name":"a","content":"y"}]}`} {
		if _, err := Parse(raw); err == nil {
			t.Errorf("accepted invalid result %s", raw)
		}
	}
	for _, outcome := range []string{"review", "needs_input", "blocked"} {
		if _, err := Parse(`{"outcome":"` + outcome + `","message":"hello","artifacts":[]}`); err != nil {
			t.Fatal(err)
		}
	}
	valid := `{"outcome":"review","message":"done","artifacts":[],"summary":{"result":"shipped","learnings":["reuse the check"],"improvements":["automate it"]}}`
	result, err := Parse(valid)
	if err != nil || result.Summary == nil || result.Summary.Result != "shipped" {
		t.Fatalf("valid summary rejected: %+v %v", result, err)
	}
	for _, raw := range []string{
		`{"outcome":"review","message":"done","artifacts":[],"summary":{"result":"","learnings":[],"improvements":[]}}`,
		`{"outcome":"review","message":"done","artifacts":[],"summary":{"result":"done","learnings":[""],"improvements":[]}}`,
		`{"outcome":"review","message":"done","artifacts":[],"summary":{"result":"done","learnings":[],"improvements":[],"extra":true}}`,
	} {
		if _, err := Parse(raw); err == nil {
			t.Fatalf("invalid summary accepted: %s", raw)
		}
	}
	if schema := string((JSONContract{}).Schema()); !strings.Contains(schema, `"required":["outcome","message","artifacts","summary","pull_requests"]`) {
		t.Fatal("completion summary is not required by the advertised contract", schema)
	}
}
