package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTestRequestValidationAndLegacyCompatibility(t *testing.T) {
	request := TestRequest{Kind: "seekdb_regression", PRURL: "https://github.com/oceanbase/seekdb/pull/123", HeadSHA: strings.Repeat("a", 40), Reason: "Persistence change"}
	base := Result{Outcome: "review", ReviewDecision: "passed", Message: "QA result", Artifacts: []File{}, TestRequests: []TestRequest{request}}
	raw, _ := json.Marshal(base)
	if _, err := Parse(string(raw)); err != nil {
		t.Fatal("a reviewer verdict must be allowed to request a separate delivery test", err)
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
	legacy, _ := json.Marshal(Result{Outcome: "review", ReviewDecision: "waiting_tests", Message: "historical result", Artifacts: []File{}})
	if _, err := Parse(string(legacy)); err != nil {
		t.Fatal("stored legacy reviewer output must remain readable", err)
	}
	if schema := string((JSONContract{}).Schema()); strings.Contains(schema, `"waiting_tests"`) {
		t.Fatal("new reviewer contract still advertises CI as a verdict", schema)
	}
}

func TestPipelineFailureAssessmentValidation(t *testing.T) {
	related := `{"outcome":"blocked","message":"repair the affected iterator path","artifacts":[],"recovery_request":{"evidence":"mysqltest reaches the changed iterator and fails after release","next_step":"repair the release ordering and run the focused test"},"pipeline_failure_assessment":{"request_id":"test_request_1","relation":"related","decision":"repair","reason":"The failing path executes the modified iterator release.","evidence":"The trace and changed call path meet at the same release branch.","mysqltest_case_count":0,"mysqltest_jobs":[]}}`
	if _, err := Parse(related); err != nil {
		t.Fatal("related assessment with repair continuation rejected", err)
	}
	unrelated := `{"outcome":"needs_input","message":"the shared runner failure appears unrelated","artifacts":[],"pipeline_failure_assessment":{"request_id":"test_request_1","relation":"unrelated","decision":"hold","reason":"The failure is in an untouched persistence fixture.","evidence":"No changed file or call path reaches that fixture.","mysqltest_case_count":0,"mysqltest_jobs":[]}}`
	if _, err := Parse(unrelated); err != nil {
		t.Fatal("unrelated assessment rejected", err)
	}
	retry := `{"outcome":"blocked","message":"retry the unrelated test-contract failure","artifacts":[],"pipeline_failure_assessment":{"request_id":"test_request_1","relation":"unrelated","decision":"retry","reason":"The trace fails before the changed path.","evidence":"Both mysqltest logs report a shared fixture timeout.","mysqltest_case_count":3,"mysqltest_jobs":[{"job_id":11,"job_name":"mysqltest-a","failed_case_count":1,"failed_case_names":["t/a"]},{"job_id":12,"job_name":"mysqltest-b","failed_case_count":2,"failed_case_names":["t/b","t/c"]}]}}`
	if _, err := Parse(retry); err != nil {
		t.Fatal("per-job mysqltest retry assessment rejected", err)
	}
	for _, bad := range []string{
		strings.Replace(related, `"recovery_request":{"evidence":"mysqltest reaches the changed iterator and fails after release","next_step":"repair the release ordering and run the focused test"},`, "", 1),
		strings.Replace(unrelated, `"needs_input"`, `"blocked"`, 1),
		strings.Replace(unrelated, `"unrelated"`, `"guess"`, 1),
		strings.Replace(retry, `"unrelated"`, `"related"`, 1),
		strings.Replace(retry, `"mysqltest_case_count":3`, `"mysqltest_case_count":2`, 1),
		strings.Replace(unrelated, `"artifacts":[]`, `"artifacts":[],"test_requests":[{"kind":"seekdb_regression","pr_url":"https://github.com/oceanbase/seekdb/pull/123","head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","reason":"retry","retry_of":"test_request_1"}]`, 1),
	} {
		if _, err := Parse(bad); err == nil {
			t.Fatal("invalid pipeline assessment accepted", bad)
		}
	}
	if schema := string((JSONContract{}).Schema()); !strings.Contains(schema, `"pipeline_failure_assessment"`) {
		t.Fatal("pipeline failure assessment missing from schema", schema)
	}
}

func TestResultValidation(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `{"outcome":"review","message":"done","artifacts":null}`, `{"outcome":"complete","message":"done","artifacts":[]}`, `{"outcome":"review","message":"done","artifacts":[],"extra":1}`, `{"outcome":"review","message":"done","artifacts":[]} {}`, `{"outcome":"review","message":"done","artifacts":[{"name":"../secret","content":"x"}]}`, `{"outcome":"review","message":"done","artifacts":[{"name":"empty.md","content":" "}]}`, `{"outcome":"review","message":"done","artifacts":[{"name":"a","content":"x"},{"name":"a","content":"y"}]}`} {
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

func TestJSONContractIsStrictSchemaComplete(t *testing.T) {
	var schema struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal((JSONContract{}).Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	required := make(map[string]bool, len(schema.Required))
	for _, name := range schema.Required {
		required[name] = true
	}
	for name := range schema.Properties {
		if !required[name] {
			t.Fatalf("Codex strict schema property %q is missing from required", name)
		}
	}
	if !required["delegations"] {
		t.Fatal("lightweight delegation array must be explicit in every response")
	}
}

func TestResultIgnoresRuntimeCitationAndExactArtifactDuplicates(t *testing.T) {
	raw := `{"outcome":"review","message":"QA complete","artifacts":[{"name":"qa.md","content":"findings"},{"name":"memory-citation.txt","content":""},{"name":"qa.md","content":"findings"}],"summary":{"result":"reviewed","learnings":[],"improvements":[]}}`
	result, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Artifacts) != 1 || result.Artifacts[0].Name != "qa.md" || result.Artifacts[0].Content != "findings" {
		t.Fatalf("unexpected normalized artifacts: %+v", result.Artifacts)
	}
}

func TestDevelopmentPlanRequiresClassifiedValidation(t *testing.T) {
	base := Result{Outcome: "review", Message: "plan", Artifacts: []File{{Name: "plan.md", Content: "complete"}}, PlanScope: &PlanScope{Repository: "oceanbase/seekdb", BaseBranch: "master"}}
	raw, _ := json.Marshal(base)
	if _, err := Parse(string(raw)); err == nil {
		t.Fatal("development plan without validation strategy accepted")
	}
	base.ValidationPlan = &ValidationPlan{
		Reuse: []ValidationItem{{Scenario: "parser compatibility", Reason: "not touched", Evidence: "baseline commit abc"}},
		Rerun: []ValidationItem{{Scenario: "persistence", Reason: "changed module", Evidence: "existing regression"}},
		Add:   []ValidationItem{}, Exclude: []ValidationItem{}, FinalGate: []string{"build", "persistence regression"},
	}
	raw, _ = json.Marshal(base)
	got, err := Parse(string(raw))
	if err != nil || got.ValidationPlan == nil || len(got.ValidationPlan.Rerun) != 1 {
		t.Fatalf("valid validation strategy rejected: %+v %v", got.ValidationPlan, err)
	}
	base.ValidationPlan.FinalGate = nil
	raw, _ = json.Marshal(base)
	if _, err = Parse(string(raw)); err == nil {
		t.Fatal("validation strategy without final gate accepted")
	}
	if schema := string((JSONContract{}).Schema()); !strings.Contains(schema, `"validation_plan"`) || !strings.Contains(schema, `"final_gate"`) {
		t.Fatal("validation strategy missing from contract")
	}
}

func TestImplementationProgressMayEchoValidationPlan(t *testing.T) {
	base := Result{
		Outcome: "blocked", Message: "implementation continues", Artifacts: []File{},
		RecoveryRequest: &RecoveryRequest{Evidence: "build exited 1", NextStep: "fix and rerun"},
		ValidationPlan: &ValidationPlan{
			Reuse: []ValidationItem{},
			Rerun: []ValidationItem{{Scenario: "changed persistence path", Reason: "candidate changed", Evidence: "candidate build pending"}},
			Add:   []ValidationItem{}, Exclude: []ValidationItem{}, FinalGate: []string{"build candidate"},
		},
	}
	raw, _ := json.Marshal(base)
	if _, err := Parse(string(raw)); err != nil {
		t.Fatalf("implementation continuation with validation progress rejected: %v", err)
	}
	base.RecoveryRequest = nil
	raw, _ = json.Marshal(base)
	if _, err := Parse(string(raw)); err == nil {
		t.Fatal("unscoped validation plan accepted outside development lifecycle")
	}
}

func TestVerificationAmendmentIsNarrowAndStructured(t *testing.T) {
	plan := &ValidationPlan{
		Reuse:   []ValidationItem{{Scenario: "existing product checks", Reason: "product behavior is unchanged", Evidence: "approved plan"}},
		Rerun:   []ValidationItem{{Scenario: "affected logservice test", Reason: "test registration changes", Evidence: "reviewer finding"}},
		Add:     []ValidationItem{{Scenario: "lease contention regression", Reason: "reviewer requested durable coverage", Evidence: "missing focused regression"}},
		Exclude: []ValidationItem{}, FinalGate: []string{"build test target", "run focused regression"},
	}
	valid := Result{
		Outcome:               "amend_validation",
		Message:               "Add the focused repository regression requested by review.",
		Artifacts:             []File{},
		ValidationPlan:        plan,
		VerificationAmendment: &VerificationAmendment{Reason: "Only repository regression coverage changes", Paths: []string{"unittest/logservice/replay_status_test.cpp", "tools/obtest/t/logservice/replay_status.test"}},
		TaskUpdate:            &TaskUpdate{Kind: "feature", Reason: "Close review coverage gap", Approach: "Add focused tests"},
	}
	raw, _ := json.Marshal(valid)
	if _, err := Parse(string(raw)); err != nil {
		t.Fatalf("valid verification amendment rejected: %v", err)
	}
	valid.VerificationAmendment.Paths = []string{"src/logservice/replay_status.cpp"}
	raw, _ = json.Marshal(valid)
	if _, err := Parse(string(raw)); err == nil {
		t.Fatal("product-source change accepted as verification-only amendment")
	}
	valid.VerificationAmendment.Paths = []string{"unittest/logservice/replay_status_test.cpp"}
	valid.Outcome = "review"
	raw, _ = json.Marshal(valid)
	if _, err := Parse(string(raw)); err == nil {
		t.Fatal("verification amendment accepted with a non-amendment outcome")
	}
	if schema := string((JSONContract{}).Schema()); !strings.Contains(schema, `"amend_validation"`) || !strings.Contains(schema, `"verification_amendment"`) {
		t.Fatal("verification amendment missing from contract", schema)
	}
}

func TestPublishRequiresValidationEvidence(t *testing.T) {
	base := Result{Outcome: "review", Message: "implemented", Artifacts: []File{}, PublishRequest: &PublishRequest{Title: "Fix issue", Body: "Implementation and risks"}}
	raw, _ := json.Marshal(base)
	if _, err := Parse(string(raw)); err == nil {
		t.Fatal("publish without validation evidence accepted")
	}
	base.TaskUpdate = &TaskUpdate{Kind: "bug", Analysis: "root cause", Approach: "repair", Reason: "correct behavior", Validation: "final gate: build and regression passed"}
	raw, _ = json.Marshal(base)
	if _, err := Parse(string(raw)); err != nil {
		t.Fatal("publish with validation evidence rejected", err)
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

func TestRecoveryRequestValidation(t *testing.T) {
	good := `{"outcome":"blocked","message":"continue diagnosis","artifacts":[],"recovery_request":{"evidence":"build exit 1","next_step":"inspect repository script"}}`
	if _, err := Parse(good); err != nil {
		t.Fatal(err)
	}
	withExistingPR := strings.Replace(good, `"artifacts":[]`, `"artifacts":[],"pull_requests":[{"url":"https://github.com/oceanbase/seekdb/pull/123","source_id":"github"}]`, 1)
	if _, err := Parse(withExistingPR); err != nil {
		t.Fatal("continuation rejected an idempotent PR registration", err)
	}
	for _, bad := range []string{
		strings.Replace(good, "build exit 1", "", 1),
		strings.Replace(good, `"blocked"`, `"review"`, 1),
		strings.Replace(good, `"next_step":`, `"command":`, 1),
		strings.Replace(good, `"artifacts":[]`, `"artifacts":[],"environment_request":{"profile":"windows_seekdb_phase0","reason":"test"}`, 1),
	} {
		if _, err := Parse(bad); err == nil {
			t.Fatal("unsafe recovery accepted", bad)
		}
	}
	if !strings.Contains(string((JSONContract{}).Schema()), `"recovery_request"`) {
		t.Fatal("schema missing recovery")
	}
}

func TestCapabilityRequestValidation(t *testing.T) {
	good := `{"outcome":"blocked","message":"need network","artifacts":[],"capability_request":{"capability":"network_access","reason":"fetch the approved repository dependency"}}`
	if _, err := Parse(good); err != nil {
		t.Fatal(err)
	}
	withExistingPR := `{"outcome":"blocked","message":"need network","artifacts":[],"capability_request":{"capability":"network_access","reason":"run the approved product test"},"pull_requests":[{"url":"https://github.com/oceanbase/seekdb/pull/1380","source_id":"github-source"}],"task_update":{"kind":"other","analysis":"","approach":"","reason":"","validation":"build passed","blocked_reason":"socket denied"}}`
	if _, err := Parse(withExistingPR); err != nil {
		t.Fatal("existing PR made capability request invalid", err)
	}
	for _, bad := range []string{
		strings.Replace(good, `"network_access"`, `"sudo"`, 1),
		strings.Replace(good, `"reason":"fetch the approved repository dependency"`, `"reason":""`, 1),
		strings.Replace(good, `"blocked"`, `"review"`, 1),
		strings.Replace(good, `"artifacts":[]`, `"artifacts":[],"recovery_request":{"evidence":"denied","next_step":"retry"}`, 1),
	} {
		if _, err := Parse(bad); err == nil {
			t.Fatal("accepted invalid capability request", bad)
		}
	}
	if schema := string((JSONContract{}).Schema()); !strings.Contains(schema, `"capability_request"`) || !strings.Contains(schema, `"host_full_access"`) {
		t.Fatal("capability request missing from schema")
	}
}

func TestLightweightDelegationValidation(t *testing.T) {
	valid := Result{
		Outcome: "blocked", Message: "I will wait for the independent checks.", Artifacts: []File{},
		Delegations: []DelegationRequest{{Key: "api-inventory", Title: "Inventory public API", Goal: "List the affected public API surface.", Context: "Read the supplied repository materials only.", Capabilities: []string{"analysis"}}},
	}
	raw, _ := json.Marshal(valid)
	if _, err := Parse(string(raw)); err != nil {
		t.Fatalf("valid lightweight delegation rejected: %v", err)
	}
	valid.RecoveryRequest = &RecoveryRequest{Evidence: "x", NextStep: "y"}
	raw, _ = json.Marshal(valid)
	if _, err := Parse(string(raw)); err == nil {
		t.Fatal("delegation combined with recovery request")
	}
	valid.RecoveryRequest = nil
	valid.Delegations[0].Key = "not a valid key!"
	raw, _ = json.Marshal(valid)
	if _, err := Parse(string(raw)); err == nil {
		t.Fatal("invalid delegation key accepted")
	}
	if schema := string((JSONContract{}).Schema()); !strings.Contains(schema, `"delegations"`) {
		t.Fatal("delegation schema missing")
	}
}
