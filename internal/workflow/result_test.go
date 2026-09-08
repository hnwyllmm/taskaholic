package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTestRequestValidationAndLegacyCompatibility(t *testing.T) {
	request := TestRequest{Kind: "seekdb_regression", PRURL: "https://github.com/oceanbase/seekdb/pull/123", HeadSHA: strings.Repeat("a", 40), Reason: "Persistence change"}
	base := Result{Outcome: "review", Message: "QA result", Artifacts: []File{}, TestRequests: []TestRequest{request}}
	raw, _ := json.Marshal(base)
	if _, err := Parse(string(raw)); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*TestRequest){func(r *TestRequest) { r.PRURL = "https://github.com/oceanbase/seekdb-bindings/pull/123" }, func(r *TestRequest) { r.HeadSHA = "master" }, func(r *TestRequest) { r.Kind = "arbitrary-http" }, func(r *TestRequest) { r.Reason = "" }} {
		r := request
		change(&r)
		base.TestRequests = []TestRequest{r}
		raw, _ = json.Marshal(base)
		if _, err := Parse(string(raw)); err == nil {
			t.Fatal("invalid test request accepted", r)
		}
	}
	base.TestRequests = []TestRequest{request, request}
	raw, _ = json.Marshal(base)
	if _, err := Parse(string(raw)); err == nil {
		t.Fatal("multiple mutations in one submission")
	}
}

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
	if schema := string((JSONContract{}).Schema()); !strings.Contains(schema, `"environment_request"`) || !strings.Contains(schema, `"summary"`) {
		t.Fatal("completion summary is not required by the advertised contract", schema)
	}
}

func TestEnvironmentRequestValidation(t *testing.T) {
	good := `{"outcome":"blocked","message":"need Windows","artifacts":[],"environment_request":{"profile":"windows_seekdb_phase0","reason":"policy matrix"}}`
	if _, err := Parse(good); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{strings.Replace(good, "policy matrix", "", 1), strings.Replace(good, "windows_seekdb_phase0", "shell", 1), strings.Replace(good, `"blocked"`, `"review"`, 1), strings.Replace(good, `"reason":"policy matrix"`, `"reason":"policy matrix","command":"sudo something"`, 1)} {
		if _, err := Parse(bad); err == nil {
			t.Fatal("invalid environment request accepted", bad)
		}
	}
}
