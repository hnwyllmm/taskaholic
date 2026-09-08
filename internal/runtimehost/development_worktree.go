package runtimehost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// This host-owned mapping lives outside Agent-writable Session directories.
type developmentRepository struct {
	Directory   string `json:"directory"`
	UpstreamSSH string `json:"upstream_ssh"`
}

func sshRepositoryURL(url, repository string) bool {
	// SSH host aliases are intentional: dev uses a separate identity for its fork.
	if !strings.HasPrefix(url, "git@") || strings.ContainsAny(url, " \t\r\n") {
		return false
	}
	parts := strings.SplitN(strings.TrimPrefix(url, "git@"), ":", 2)
	return len(parts) == 2 && !strings.HasPrefix(parts[0], "-") && managedID.MatchString(strings.ReplaceAll(parts[0], ".", "-")) && strings.TrimSuffix(parts[1], ".git") == repository
}

func baseGit(ctx context.Context, directory string, args ...string) (string, error) {
	result, err := developmentCommand(ctx, directory, "git", append([]string{"-c", "core.hooksPath=/dev/null"}, args...)...)
	if err != nil {
		return "", fmt.Errorf("base repository %s: %w", args[0], err)
	}
	return result, nil
}

func (d *Daemon) prepareDevelopmentWorktree(ctx context.Context, w *developmentWorkspace) error {
	raw, err := os.ReadFile(filepath.Join(d.config.WorkRoot, "repositories.json"))
	if err != nil {
		return errors.New("configure host work root repositories.json with the approved repository's existing base directory and upstream_ssh")
	}
	var repositories map[string]developmentRepository
	if err = json.Unmarshal(raw, &repositories); err != nil {
		return errors.New("invalid host repositories.json")
	}
	cfg, ok := repositories[w.Repository]
	if !ok || !filepath.IsAbs(cfg.Directory) || !sshRepositoryURL(cfg.UpstreamSSH, w.Repository) {
		return errors.New("approved repository requires an absolute base directory and matching upstream SSH URL")
	}
	real, err := filepath.EvalSymlinks(cfg.Directory)
	if err != nil || real != cfg.Directory {
		return errors.New("base repository must exist and must not be symlinked")
	}
	top, err := baseGit(ctx, real, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	if top != real {
		return errors.New("configured directory is not the base repository root")
	}
	upstream, err := baseGit(ctx, real, "remote", "get-url", "upstream")
	if err != nil {
		return err
	}
	if !sshRepositoryURL(upstream, w.Repository) && upstream != "https://github.com/"+w.Repository+".git" && upstream != "https://github.com/"+w.Repository {
		return errors.New("base upstream does not match approved repository")
	}
	actor, err := developmentCommand(ctx, real, "gh", "api", "--hostname", "github.com", "user", "--jq", ".login")
	if err != nil {
		return fmt.Errorf("identify fork through gh: %w", err)
	}
	if !managedID.MatchString(actor) {
		return errors.New("invalid GitHub identity")
	}
	fork := actor + "/" + strings.Split(w.Repository, "/")[1]
	info, err := developmentCommand(ctx, real, "gh", "api", "--hostname", "github.com", "repos/"+fork)
	if err != nil {
		return fmt.Errorf("verify origin fork through gh: %w", err)
	}
	var repo struct {
		FullName string `json:"full_name"`
		Parent   struct {
			FullName string `json:"full_name"`
		} `json:"parent"`
		Permissions struct {
			Push bool `json:"push"`
		} `json:"permissions"`
	}
	if json.Unmarshal([]byte(info), &repo) != nil || !strings.EqualFold(repo.FullName, fork) || !strings.EqualFold(repo.Parent.FullName, w.Repository) || !repo.Permissions.Push {
		return errors.New("origin must be a writable fork of the approved upstream")
	}
	origin, err := baseGit(ctx, real, "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	if !sshRepositoryURL(origin, fork) {
		return errors.New("base origin must use SSH and match the fork verified by gh")
	}
	// No checkout/reset/clean and no remote configuration changes in the user's base.
	// Explicit SSH URL avoids changing its existing HTTPS upstream setting.
	if _, err = baseGit(ctx, real, "fetch", "--no-tags", cfg.UpstreamSSH, "refs/heads/"+w.BaseBranch+":refs/remotes/upstream/"+w.BaseBranch); err != nil {
		return fmt.Errorf("fetch approved upstream via SSH: %w", err)
	}
	sha, err := baseGit(ctx, real, "rev-parse", "refs/remotes/upstream/"+w.BaseBranch+"^{commit}")
	if err != nil {
		return err
	}
	w.BaseDirectory = real
	w.OriginURL = origin
	w.BaseSHA = sha
	if _, err = baseGit(ctx, real, "worktree", "add", "-b", w.Branch, w.Directory, sha); err != nil {
		return fmt.Errorf("create isolated task worktree: %w", err)
	}
	w.GitDir, err = baseGit(ctx, w.Directory, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	return validateDevelopmentWorktree(ctx, *w)
}

func validateDevelopmentWorktree(ctx context.Context, w developmentWorkspace) error {
	common, err := baseGit(ctx, w.BaseDirectory, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	actual, err := filepath.EvalSymlinks(w.GitDir)
	if err != nil || actual != w.GitDir || filepath.Dir(w.GitDir) != filepath.Join(common, "worktrees") {
		return errors.New("task Git metadata is not owned by its configured base repository")
	}
	marker, err := os.ReadFile(filepath.Join(w.Directory, ".git"))
	if err != nil || strings.TrimSpace(string(marker)) != "gitdir: "+w.GitDir {
		return errors.New("task worktree Git pointer changed; refusing repository operations")
	}
	branch, err := w.git(ctx, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return err
	}
	if branch != w.Branch {
		return errors.New("task worktree branch changed; refusing repository operations")
	}
	return nil
}
