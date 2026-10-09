package taskaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"work-assistant/internal/model"
)

type fakeGitHubComments struct {
	comments                       []githubComment
	head, prURL, issueURL, actor   string
	posts, patches                 int
	failWrite, failRead, malformed bool
}

func newFakeComments() (*fakeGitHubComments, model.Publication) {
	p := model.Publication{Key: "test-key", URL: "https://github.com/oceanbase/seekdb/pull/123", HeadSHA: strings.Repeat("a", 40), Sticky: true, State: "SUBMITTING", Body: "<!-- work-assistant:review:test-key -->\n通过"}
	return &fakeGitHubComments{comments: []githubComment{}, head: p.HeadSHA, prURL: p.URL, issueURL: "https://api.github.com/repos/oceanbase/seekdb/issues/123", actor: "same-as-pr-author"}, p
}
func (f *fakeGitHubComments) run(ctx context.Context, binary string, input []byte, args ...string) ([]byte, error) {
	if binary != "gh" || len(args) < 6 || args[0] != "api" || args[1] != "--hostname" || args[2] != "github.com" || args[3] != "--method" {
		return nil, fmt.Errorf("unsafe invocation %v", args)
	}
	method, endpoint := args[4], args[5]
	if method == "GET" && f.failRead {
		return nil, errors.New("read timeout")
	}
	encode := func(v any) ([]byte, error) { return json.Marshal(v) }
	if method == "GET" {
		if endpoint == "user" {
			return encode(map[string]string{"login": f.actor})
		}
		if strings.HasSuffix(endpoint, "/pulls/123") || strings.HasSuffix(endpoint, "/issues/123") {
			return encode(map[string]any{"html_url": f.prURL, "state": "open", "head": map[string]string{"sha": f.head}})
		}
		if strings.Contains(endpoint, "comments?per_page=100&page=1") {
			if f.malformed {
				return []byte(`{}`), nil
			}
			return encode(f.comments)
		}
		if strings.Contains(endpoint, "issues/comments/") {
			for _, c := range f.comments {
				if strings.HasSuffix(endpoint, fmt.Sprint(c.ID)) {
					return encode(c)
				}
			}
			return nil, errors.New("404")
		}
		return nil, fmt.Errorf("unexpected read %s", endpoint)
	}
	if len(args) != 8 || args[6] != "--input" || args[7] != "-" || input == nil {
		return nil, errors.New("body must use stdin")
	}
	var body struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(input, &body); err != nil {
		return nil, err
	}
	var c githubComment
	switch method {
	case "POST":
		if endpoint != "repos/oceanbase/seekdb/issues/123/comments" {
			return nil, errors.New("not an ordinary comment")
		}
		f.posts++
		c = githubComment{ID: 101, Body: body.Body, IssueURL: f.issueURL, URL: f.prURL + "#issuecomment-101"}
		c.User.Login = f.actor
		f.comments = append(f.comments, c)
	case "PATCH":
		if endpoint != "repos/oceanbase/seekdb/issues/comments/101" {
			return nil, errors.New("wrong comment")
		}
		f.patches++
		for i := range f.comments {
			if f.comments[i].ID == 101 {
				f.comments[i].Body = body.Body
				c = f.comments[i]
			}
		}
	default:
		return nil, fmt.Errorf("forbidden method %s", method)
	}
	if f.failWrite {
		return nil, errors.New("response lost after server commit")
	}
	return encode(c)
}
func TestStickyCommentCreateUpdateAndCrashReconciliation(t *testing.T) {
	ctx := context.Background()
	fake, p := newFakeComments()
	g := GitHubComments{Run: fake.run, Binary: "gh"}
	fake.failWrite = true
	if _, err := g.Publish(ctx, p); err == nil {
		t.Fatal("lost response accepted")
	}
	fake.failWrite = false
	p.State = "UNCERTAIN"
	receipt, err := g.Publish(ctx, p)
	if err != nil || receipt.ID != "101" || fake.posts != 1 {
		t.Fatal(receipt, err, fake.posts)
	}
	p.RemoteID = receipt.ID
	p.AppliedBody = p.Body
	p.Body = "<!-- work-assistant:review:test-key -->\n不通过，需要修复"
	p.State = "SUBMITTING"
	receipt, err = g.Publish(ctx, p)
	if err != nil || receipt.ID != "101" || fake.posts != 1 || fake.patches != 1 {
		t.Fatal(receipt, err, fake)
	}
	p.AppliedBody = p.Body
	p.Body = "<!-- work-assistant:review:test-key -->\n测试通过，评审通过"
	fake.failWrite = true
	if _, err = g.Publish(ctx, p); err == nil {
		t.Fatal("expected uncertain PATCH")
	}
	fake.failWrite = false
	p.State = "UNCERTAIN"
	if _, err = g.Publish(ctx, p); err != nil || fake.posts != 1 || fake.patches != 2 {
		t.Fatal(err, fake)
	}
}
func TestStickyCommentSafetyGuards(t *testing.T) {
	for _, test := range []string{"stale-head", "uncertain-missing", "manual-edit", "wrong-author", "wrong-issue", "malformed", "read-failure", "bad-url"} {
		t.Run(test, func(t *testing.T) {
			fake, p := newFakeComments()
			g := GitHubComments{Run: fake.run, Binary: "gh"}
			if strings.HasPrefix(test, "wrong-") || test == "manual-edit" {
				r, err := g.Publish(context.Background(), p)
				if err != nil {
					t.Fatal(err)
				}
				p.RemoteID = r.ID
				p.AppliedBody = p.Body
				p.Body += " new"
				fake.posts = 0
			}
			switch test {
			case "stale-head":
				fake.head = strings.Repeat("b", 40)
			case "uncertain-missing":
				p.State = "UNCERTAIN"
			case "manual-edit":
				fake.comments[0].Body += " human edit"
			case "wrong-author":
				fake.comments[0].User.Login = "someone-else"
			case "wrong-issue":
				fake.comments[0].IssueURL += "4"
			case "malformed":
				fake.malformed = true
			case "read-failure":
				fake.failRead = true
			case "bad-url":
				p.URL = "https://evil.example/secret"
			}
			if _, err := g.Publish(context.Background(), p); err == nil {
				t.Fatal("unsafe publication accepted")
			}
			if fake.posts != 0 || fake.patches != 0 {
				t.Fatal("wrote despite guard", fake)
			}
		})
	}
}
func TestGitHubIssueUsesOrdinaryCommentsAndRecoversWithoutRepost(t *testing.T) {
	fake, p := newFakeComments()
	p.URL = "https://github.com/oceanbase/seekdb/issues/123"
	p.Sticky = false
	p.HeadSHA = ""
	p.Body = "<!-- work-assistant:progress:test-key -->\n问题分析和修复理由"
	fake.prURL = p.URL
	g := GitHubComments{Run: fake.run, Binary: "gh"}
	if _, err := g.Publish(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	p.State = "UNCERTAIN"
	if _, err := g.Publish(context.Background(), p); err != nil || fake.posts != 1 {
		t.Fatal(err, fake.posts)
	}
}

func TestGitHubRepositoryCaseDoesNotCreateFalseIdentityConflict(t *testing.T) {
	fake, p := newFakeComments()
	fake.prURL = "https://github.com/OceanBase/SeekDB/pull/123"
	fake.issueURL = "https://api.github.com/repos/OceanBase/SeekDB/issues/123"
	g := GitHubComments{Run: fake.run, Binary: "gh"}
	if _, err := g.Publish(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	p.State = "UNCERTAIN"
	if _, err := g.Publish(context.Background(), p); err != nil || fake.posts != 1 {
		t.Fatal(err, fake.posts)
	}
}
func TestAntProgressStdinIdentityAndReconciliation(t *testing.T) {
	p := model.Publication{Key: "k", WorkspaceID: "workspace", IssueID: "issue", ActorID: "person", URL: "https://antmultica.alipay.com/seekdb/issues/issue", Body: "<!-- work-assistant:progress:k -->\nRoot cause\nRepair reason"}
	var comments = []antComment{}
	posts := 0
	run := func(ctx context.Context, binary string, input []byte, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if binary != "multica" || !strings.HasPrefix(joined, "--server-url https://antmultica.alipay.com --workspace-id workspace issue ") {
			t.Fatal("unsafe arguments", args)
		}
		if strings.Contains(joined, "issue get issue") {
			return []byte(`{"id":"issue","workspace_id":"workspace"}`), nil
		}
		if strings.Contains(joined, "comment list issue --roots-only --full --output json") {
			return json.Marshal(comments)
		}
		if strings.Contains(joined, "comment add issue --content-stdin --output json") {
			if string(input) != p.Body {
				t.Fatal("lost stdin body")
			}
			posts++
			comments = append(comments, antComment{ID: "comment", IssueID: p.IssueID, AuthorID: p.ActorID, AuthorType: "member", Content: string(input)})
			return nil, errors.New("lost response")
		}
		t.Fatal("unexpected command", args)
		return nil, nil
	}
	a := AntMulticaComments{Run: run, Binary: "multica"}
	if _, err := a.Publish(context.Background(), p); err == nil {
		t.Fatal("lost response")
	}
	p.State = "UNCERTAIN"
	receipt, err := a.Publish(context.Background(), p)
	if err != nil || receipt.ID != "comment" || posts != 1 {
		t.Fatal(receipt, err, posts)
	}
	comments = nil
	receipt, err = a.Publish(context.Background(), p)
	if err == nil || posts != 1 {
		t.Fatal("ambiguous state reposted", receipt, err)
	}
}

func TestAntStatusIsReadBeforeWriteVerifiedAndIdempotent(t *testing.T) {
	p := model.Publication{Key: "status", WorkspaceID: "workspace", IssueID: "issue", ActorID: "person", URL: "https://antmultica.alipay.com/seekdb/issues/issue", DesiredStatus: "in_progress"}
	issue := antIssue{ID: p.IssueID, WorkspaceID: p.WorkspaceID, AssigneeID: p.ActorID, AssigneeType: "member", Status: "todo", StatusCategory: "todo"}
	writes := 0
	loseResponse := false
	run := func(_ context.Context, binary string, input []byte, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if binary != "multica" || input != nil || !strings.HasPrefix(joined, "--server-url https://antmultica.alipay.com --workspace-id workspace issue ") {
			t.Fatal("unsafe status command", binary, args)
		}
		if strings.Contains(joined, "issue get issue --output json") {
			return json.Marshal(issue)
		}
		if strings.Contains(joined, "issue status issue in_progress --no-start --output json") {
			writes++
			issue.Status, issue.StatusCategory = "in_progress", "in_progress"
			if loseResponse {
				return nil, errors.New("lost response")
			}
			return []byte(`{}`), nil
		}
		t.Fatal("unexpected status command", args)
		return nil, nil
	}
	a := AntMulticaComments{Run: run, Binary: "multica"}
	receipt, err := a.Publish(context.Background(), p)
	if err != nil || receipt.ID != p.IssueID || writes != 1 {
		t.Fatal(receipt, err, writes)
	}
	if _, err = a.Publish(context.Background(), p); err != nil || writes != 1 {
		t.Fatal("already-current status was written again", err, writes)
	}

	issue.Status, issue.StatusCategory = "todo", "todo"
	loseResponse = true
	_, err = a.Publish(context.Background(), p)
	var actionErr *PublicationError
	if !errors.As(err, &actionErr) || actionErr.State != "UNCERTAIN" || writes != 2 {
		t.Fatal("lost response was not made uncertain", err, writes)
	}
	p.State = "UNCERTAIN"
	loseResponse = false
	if _, err = a.Publish(context.Background(), p); err != nil || writes != 2 {
		t.Fatal("uncertain status was not reconciled before rewriting", err, writes)
	}

	issue.Status, issue.StatusCategory = "done", "done"
	_, err = a.Publish(context.Background(), p)
	if !errors.As(err, &actionErr) || actionErr.State != "SKIPPED" || writes != 2 {
		t.Fatal("terminal issue was reopened", err, writes)
	}
	issue.Status, issue.StatusCategory, issue.AssigneeID = "todo", "todo", "another-person"
	_, err = a.Publish(context.Background(), p)
	if !errors.As(err, &actionErr) || actionErr.State != "BLOCKED" || writes != 2 {
		t.Fatal("reassigned issue was changed", err, writes)
	}
}
