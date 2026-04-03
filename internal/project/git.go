package project

import (
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
)

func ParseGitHubURL(raw string) (owner, repo string, err error) {
	raw = strings.TrimSpace(raw)

	// SSH: git@github.com:owner/repo.git
	if path, ok := strings.CutPrefix(raw, "git@github.com:"); ok {
		path = strings.TrimSuffix(path, ".git")
		return splitOwnerRepo(path)
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("parse url: %w", err)
	}

	if u.Host != "github.com" {
		return "", "", fmt.Errorf("unsupported host: %s", u.Host)
	}

	path := strings.TrimPrefix(u.Path, "/")
	path = strings.TrimSuffix(path, ".git")
	return splitOwnerRepo(path)
}

func splitOwnerRepo(path string) (owner, repo string, err error) {
	parts := strings.SplitN(path, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid owner/repo path: %s", path)
	}
	return parts[0], parts[1], nil
}

func CloneBare(repoURL, destPath string) error {
	cmd := exec.Command("git", "clone", "--bare", repoURL, destPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git clone --bare: %w: %s", err, string(out))
	}
	return nil
}

func DefaultBranch(barePath string) (string, error) {
	cmd := exec.Command("git", "symbolic-ref", "HEAD")
	cmd.Dir = barePath
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git symbolic-ref HEAD: %w", err)
	}
	ref := strings.TrimSpace(string(out))
	// refs/heads/main → main
	return filepath.Base(ref), nil
}

func FetchOrigin(barePath string) error {
	cmd := exec.Command("git", "fetch", "origin")
	cmd.Dir = barePath
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git fetch origin: %w: %s", err, string(out))
	}
	return nil
}

func CreateWorktree(barePath, worktreePath, branch, baseBranch string) error {
	cmd := exec.Command("git", "worktree", "add", worktreePath, "-b", branch, baseBranch)
	cmd.Dir = barePath
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree add: %w: %s", err, string(out))
	}
	return nil
}

func RemoveWorktree(barePath, worktreePath string) error {
	cmd := exec.Command("git", "worktree", "remove", "--force", worktreePath)
	cmd.Dir = barePath
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree remove: %w: %s", err, string(out))
	}
	return nil
}
