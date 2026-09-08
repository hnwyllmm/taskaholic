package runtimehost

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"work-assistant/internal/model"
)

func TestSSHRepositoryIdentity(t *testing.T) {
	for _, url := range []string{"git@github.com:actor/seekdb.git", "git@github-actor:actor/seekdb.git"} {
		if !sshRepositoryURL(url, "actor/seekdb") {
			t.Fatal(url)
		}
	}
	for _, url := range []string{"git@github.com:oceanbase/seekdb.git", "https://github.com/actor/seekdb.git", "git@github.com:actor/seekdb.git\n", "git@--bad:actor/seekdb.git"} {
		if sshRepositoryURL(url, "actor/seekdb") {
			t.Fatal(url)
		}
	}
}

func TestDevelopmentUsesBaseWorktreeAndPreservesUserCheckout(t *testing.T) {
	ctx := context.Background()
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	base := filepath.Join(root, "base")
	if e = os.Mkdir(base, 0700); e != nil {
		t.Fatal(e)
	}
	git := func(dir string, args ...string) string {
		t.Helper()
		out, e := baseGit(ctx, dir, args...)
		if e != nil {
			t.Fatal(e)
		}
		return out
	}
	git(base, "init")
	git(base, "symbolic-ref", "HEAD", "refs/heads/master")
	if e = os.WriteFile(filepath.Join(base, "source.txt"), []byte("original\n"), 0600); e != nil {
		t.Fatal(e)
	}
	git(base, "add", "source.txt")
	git(base, "-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "base")
	sha := git(base, "rev-parse", "HEAD")
	git(base, "remote", "add", "origin", "git@github-actor:actor/seekdb.git")
	git(base, "remote", "add", "upstream", "https://github.com/oceanbase/seekdb.git")
	if e = os.WriteFile(filepath.Join(base, "user-notes.md"), []byte("keep"), 0600); e != nil {
		t.Fatal(e)
	}
	before := git(base, "status", "--porcelain")
	bin := filepath.Join(root, "bin")
	if e = os.Mkdir(bin, 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\ncase \"$*\" in *'.login'*) echo actor;; *) echo '{\"full_name\":\"actor/seekdb\",\"parent\":{\"full_name\":\"oceanbase/seekdb\"},\"permissions\":{\"push\":true}}';; esac\n"), 0700); e != nil {
		t.Fatal(e)
	}
	ssh := filepath.Join(bin, "test-ssh")
	if e = os.WriteFile(ssh, []byte("#!/bin/sh\nexec git-upload-pack \"$TEST_BASE_REPO\"\n"), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GIT_SSH_COMMAND", ssh)
	t.Setenv("GIT_SSH_VARIANT", "ssh")
	t.Setenv("TEST_BASE_REPO", base)
	work := filepath.Join(root, "work")
	if e = os.Mkdir(work, 0700); e != nil {
		t.Fatal(e)
	}
	if e = durableWorkspaceJSON(filepath.Join(work, "repositories.json"), map[string]developmentRepository{"oceanbase/seekdb": {Directory: base, UpstreamSSH: "git@github-actor:oceanbase/seekdb.git"}}); e != nil {
		t.Fatal(e)
	}
	d := Daemon{config: Config{WorkRoot: work}}
	spec := model.RunSpec{TaskID: "task_one", SessionID: "session_one", AdapterID: "codex-agent", ExecutionGrant: &model.ExecutionGrant{ReviewID: "review_one", PlanHash: strings.Repeat("a", 64), Repository: "oceanbase/seekdb", BaseBranch: "master"}}
	session := filepath.Join(work, "sessions", spec.SessionID)
	if e = os.MkdirAll(session, 0700); e != nil {
		t.Fatal(e)
	}
	w, receipt, e := d.prepareDevelopment(ctx, spec, session)
	if e != nil {
		t.Fatal(e)
	}
	if w.BaseSHA != sha || w.OriginURL != "git@github-actor:actor/seekdb.git" || w.BaseDirectory != base {
		t.Fatal(w)
	}
	if !strings.Contains(w.GitDir, "/worktrees/") {
		t.Fatal("not a worktree", w.GitDir)
	}
	if e = os.WriteFile(filepath.Join(w.Directory, "source.txt"), []byte("task change"), 0600); e != nil {
		t.Fatal(e)
	}
	again, r, e := d.prepareDevelopment(ctx, spec, session)
	if e != nil || r != receipt || again.GitDir != w.GitDir {
		t.Fatal("resume changed worktree", e)
	}
	if git(base, "status", "--porcelain") != before || git(base, "rev-parse", "HEAD") != sha {
		t.Fatal("user base modified")
	}
	body, e := os.ReadFile(filepath.Join(base, "source.txt"))
	if e != nil || string(body) != "original\n" {
		t.Fatal("shared source overwritten")
	}
	// A changed .git pointer is never trusted, even though the Agent can edit its tree.
	if e = os.WriteFile(filepath.Join(w.Directory, ".git"), []byte("gitdir: "+filepath.Join(base, ".git")), 0600); e != nil {
		t.Fatal(e)
	}
	if _, _, e = d.prepareDevelopment(ctx, spec, session); e == nil {
		t.Fatal("redirected metadata accepted")
	}
}

func TestDevelopmentDiagnosticsDoNotLeakStderr(t *testing.T) {
	if _, err := developmentCommand(context.Background(), t.TempDir(), "sh", "-c", "echo 'secret_token=do-not-print Permission denied (publickey)' >&2; exit 1"); err == nil || strings.Contains(err.Error(), "do-not-print") || !strings.Contains(err.Error(), "Permission denied (publickey)") {
		t.Fatal(err)
	}
}
