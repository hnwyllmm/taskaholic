package runtimehost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"work-assistant/internal/model"
	"work-assistant/internal/workflow"
)

// The Agent can edit only its Session tree. Git metadata and publication
// receipts live outside that writable tree and are operated by the runtime.
type developmentWorkspace struct {
	command         func(context.Context, string, string, ...string) (string, error)
	Directory       string `json:"directory"`
	GitDir          string `json:"git_dir"`
	Repository      string `json:"repository"`
	BaseBranch      string `json:"base_branch"`
	Branch          string `json:"branch"`
	BaseSHA         string `json:"base_sha"`
	PRURL           string `json:"pr_url"`
	CreateAttempted bool   `json:"create_attempted"`
}

func (w developmentWorkspace) run(ctx context.Context, binary string, args ...string) (string, error) {
	if w.command != nil {
		return w.command(ctx, w.Directory, binary, args...)
	}
	return developmentCommand(ctx, w.Directory, binary, args...)
}

var managedID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,160}$`)

func durableWorkspaceJSON(path string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".receipt-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// Never return command stderr: credential helpers and transports can include
// sensitive values. Detailed git/PR facts are returned by successful reads.
func developmentCommand(ctx context.Context, directory, binary string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = directory
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_LFS_SKIP_SMUDGE=1")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s action failed (details suppressed); check host access, repository and branch", binary)
	}
	return strings.TrimSpace(string(out)), nil
}
func (w developmentWorkspace) git(ctx context.Context, args ...string) (string, error) {
	prefix := []string{"--git-dir=" + w.GitDir, "--work-tree=" + w.Directory, "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential"}
	return w.run(ctx, "git", append(prefix, args...)...)
}

func (d *Daemon) prepareDevelopment(ctx context.Context, spec model.RunSpec, sessionDirectory string) (developmentWorkspace, string, error) {
	var w developmentWorkspace
	g := spec.ExecutionGrant
	if g == nil || spec.ReadOnly || spec.AdapterID != "codex-agent" || !managedID.MatchString(spec.TaskID) || !managedID.MatchString(spec.SessionID) || g.ReviewID == "" || len(g.PlanHash) != 64 {
		return w, "", errors.New("invalid isolated development grant")
	}
	if err := model.ValidateDevelopmentRepository(g.Repository, g.BaseBranch); err != nil {
		return w, "", err
	}
	metadata := filepath.Join(d.config.WorkRoot, "development", spec.SessionID)
	if err := os.MkdirAll(metadata, 0700); err != nil {
		return w, "", err
	}
	actual, err := filepath.EvalSymlinks(metadata)
	if err != nil || actual != metadata {
		return w, "", errors.New("development metadata must not be symlinked")
	}
	receipt := filepath.Join(metadata, "workspace.json")
	raw, err := os.ReadFile(receipt)
	if err == nil {
		if err = json.Unmarshal(raw, &w); err != nil {
			return w, receipt, err
		}
		if w.Repository != g.Repository || w.BaseBranch != g.BaseBranch || w.Directory != filepath.Join(sessionDirectory, "repository") || w.GitDir != filepath.Join(metadata, "git") {
			return w, receipt, errors.New("approved repository changed; preserve old checkout and configure a new isolated workspace")
		}
		real, e := filepath.EvalSymlinks(w.Directory)
		if e != nil || real != w.Directory {
			return w, receipt, errors.New("development checkout is missing or symlinked")
		}
		return w, receipt, nil
	}
	if !os.IsNotExist(err) {
		return w, receipt, err
	}
	w = developmentWorkspace{Repository: g.Repository, BaseBranch: g.BaseBranch, Directory: filepath.Join(sessionDirectory, "repository"), GitDir: filepath.Join(metadata, "git"), Branch: "work-assistant/" + spec.TaskID}
	if _, err = os.Lstat(w.Directory); !os.IsNotExist(err) {
		return w, receipt, errors.New("unregistered checkout exists; refusing to overwrite it")
	}
	if _, err = os.Lstat(w.GitDir); !os.IsNotExist(err) {
		return w, receipt, errors.New("incomplete Git preparation exists; preserved for recovery")
	}
	_, err = developmentCommand(ctx, sessionDirectory, "git", "-c", "core.hooksPath=/dev/null", "-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential", "clone", "--depth=1", "--single-branch", "--branch", g.BaseBranch, "--separate-git-dir", w.GitDir, "https://github.com/"+g.Repository+".git", w.Directory)
	if err != nil {
		return w, receipt, err
	}
	w.BaseSHA, err = w.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return w, receipt, err
	}
	if _, err = w.git(ctx, "checkout", "-b", w.Branch); err != nil {
		return w, receipt, err
	}
	return w, receipt, durableWorkspaceJSON(receipt, w)
}

// Publishing uses an existing authenticated fork, a task-unique branch and
// fast-forward-only push. Never merges, rewrites another branch or creates a
// fork implicitly. An uncertain PR POST is reconciled, never blindly retried.
func publishDevelopment(ctx context.Context, w *developmentWorkspace, receipt string, spec model.RunSpec, result *workflow.Result) error {
	p := result.PublishRequest
	if p == nil {
		return nil
	}
	if u := result.TaskUpdate; u != nil && u.Kind == "bug" && (strings.TrimSpace(u.Analysis) == "" || strings.TrimSpace(u.Approach) == "" || strings.TrimSpace(u.Reason) == "") {
		return errors.New("BUG publishing requires analysis, approach and repair reason before any external action")
	}
	actor, err := w.run(ctx, "gh", "api", "--hostname", "github.com", "user", "--jq", ".login")
	if err != nil {
		return err
	}
	if !managedID.MatchString(actor) {
		return errors.New("invalid GitHub publisher identity")
	}
	repoName := strings.Split(w.Repository, "/")[1]
	fork := actor + "/" + repoName
	remote, err := w.run(ctx, "gh", "api", "--hostname", "github.com", "repos/"+fork)
	if err != nil {
		return errors.New("publishing requires an existing authenticated GitHub fork")
	}
	var info struct {
		FullName string `json:"full_name"`
		Parent   struct {
			FullName string `json:"full_name"`
		} `json:"parent"`
	}
	if err = json.Unmarshal([]byte(remote), &info); err != nil || !strings.EqualFold(info.FullName, fork) || !strings.EqualFold(info.Parent.FullName, w.Repository) {
		return errors.New("publisher repository is not a verified fork of the approved repository")
	}
	// Refuse obvious credential/private-key files. This is an extra guard, not
	// a replacement for repository secret scanning or downstream security review.
	names, err := w.git(ctx, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return err
	}
	changed, err := w.git(ctx, "diff", "--name-only")
	if err != nil {
		return err
	}
	for _, name := range strings.Split(names+"\n"+changed, "\n") {
		base := strings.ToLower(filepath.Base(name))
		if base == ".env" || strings.HasPrefix(base, ".env.") || strings.HasSuffix(base, ".pem") || base == "id_rsa" || base == "id_ed25519" {
			return errors.New("publication includes a potentially sensitive file; remove it before retrying")
		}
	}
	if _, err = w.git(ctx, "add", "-A", "--", "."); err != nil {
		return err
	}
	staged, err := w.git(ctx, "diff", "--cached", "--stat")
	if err != nil {
		return err
	}
	if staged != "" {
		if _, err = w.git(ctx, "-c", "user.name="+actor, "-c", "user.email="+actor+"@users.noreply.github.com", "commit", "-m", p.Title); err != nil {
			return err
		}
	}
	head, err := w.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if head == w.BaseSHA {
		return errors.New("no implementation changes to publish; a document report is not a PR")
	}
	marker := "<!-- work-assistant:development:" + spec.TaskID + " -->"
	query, err := w.run(ctx, "gh", "api", "--hostname", "github.com", "--method", "GET", "repos/"+w.Repository+"/pulls", "-f", "state=all", "-f", "head="+actor+":"+w.Branch, "-f", "base="+w.BaseBranch, "-f", "per_page=100")
	if err != nil {
		return err
	}
	var prs []struct {
		URL   string `json:"html_url"`
		Body  string `json:"body"`
		State string `json:"state"`
	}
	if err = json.Unmarshal([]byte(query), &prs); err != nil {
		return err
	}
	found := ""
	for _, pr := range prs {
		if !strings.Contains(pr.Body, marker) {
			return errors.New("task branch is already associated with an unowned PR")
		}
		if pr.State != "open" {
			return errors.New("task PR is already closed; do not recreate automatically")
		}
		if found != "" {
			return errors.New("multiple PRs found for task branch")
		}
		found = pr.URL
	}
	if found == "" && w.CreateAttempted {
		return errors.New("previous PR creation outcome is uncertain; no duplicate PR created, inspect GitHub before retrying")
	}
	if _, err = w.git(ctx, "push", "https://github.com/"+fork+".git", "HEAD:refs/heads/"+w.Branch); err != nil {
		return err
	}
	if found == "" {
		if w.CreateAttempted {
			return errors.New("previous PR creation outcome is uncertain; no duplicate PR created, inspect GitHub before retrying")
		}
		w.CreateAttempted = true
		if err = durableWorkspaceJSON(receipt, w); err != nil {
			return err
		}
		body := p.Body + "\n\n" + marker + "\nApproved plan: " + spec.ExecutionGrant.PlanHash
		found, err = w.run(ctx, "gh", "pr", "create", "--repo", w.Repository, "--head", actor+":"+w.Branch, "--base", w.BaseBranch, "--title", p.Title, "--body", body)
		if err != nil {
			return errors.New("PR creation outcome uncertain; receipt retained, next attempt will reconcile GitHub")
		}
	}
	owner, repo, _, canonical, err := model.ParseGitHubPR(found)
	if err != nil || !strings.EqualFold(owner+"/"+repo, w.Repository) {
		return errors.New("publisher returned an unexpected PR URL")
	}
	w.PRURL = canonical
	if err = durableWorkspaceJSON(receipt, w); err != nil {
		return err
	}
	result.PullRequests = []workflow.PullRequest{{URL: canonical}}
	result.PublishRequest = nil
	result.Message += "\n\n已发布 PR：" + canonical + "\nCommit：" + head
	return nil
}
