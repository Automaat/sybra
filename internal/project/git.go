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
		// Branch already exists from a previous run — reuse it
		cmd2 := exec.Command("git", "worktree", "add", worktreePath, branch)
		cmd2.Dir = barePath
		if out2, err2 := cmd2.CombinedOutput(); err2 != nil {
			return fmt.Errorf("git worktree add: %w: %s (retry: %w: %s)", err, string(out), err2, string(out2))
		}
	}
	return nil
}

func ListWorktrees(barePath string) ([]Worktree, error) {
	cmd := exec.Command("git", "worktree", "list", "--porcelain")
	cmd.Dir = barePath
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git worktree list: %w", err)
	}
	return parseWorktreePorcelain(string(out)), nil
}

func parseWorktreePorcelain(raw string) []Worktree {
	var result []Worktree
	for block := range strings.SplitSeq(strings.TrimSpace(raw), "\n\n") {
		if strings.Contains(block, "\nbare") || strings.HasSuffix(block, "\nbare") {
			continue
		}
		var wt Worktree
		for line := range strings.SplitSeq(block, "\n") {
			if rest, ok := strings.CutPrefix(line, "worktree "); ok {
				wt.Path = rest
			} else if rest, ok := strings.CutPrefix(line, "HEAD "); ok {
				if len(rest) > 7 {
					rest = rest[:7]
				}
				wt.Head = rest
			} else if ref, ok := strings.CutPrefix(line, "branch "); ok {
				branch, _ := strings.CutPrefix(ref, "refs/heads/")
				wt.Branch = branch
				if name, ok := strings.CutPrefix(wt.Branch, "synapse/"); ok {
					// Task ID is always the last 8 chars (uuid[:8])
					if len(name) >= 8 {
						wt.TaskID = name[len(name)-8:]
					} else {
						wt.TaskID = name
					}
				}
			}
		}
		if wt.Path != "" {
			result = append(result, wt)
		}
	}
	return result
}

func RemoveWorktree(barePath, worktreePath string) error {
	cmd := exec.Command("git", "worktree", "remove", "--force", worktreePath)
	cmd.Dir = barePath
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree remove: %w: %s", err, string(out))
	}
	return nil
}
