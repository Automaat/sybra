package project

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Automaat/synapse/internal/executil"
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
	return executil.Run("", "git", "clone", "--bare", repoURL, destPath)
}

func DefaultBranch(barePath string) (string, error) {
	ref, err := executil.Output(barePath, "git", "symbolic-ref", "HEAD")
	if err != nil {
		return "", err
	}
	// refs/heads/main → main
	return filepath.Base(ref), nil
}

func FetchOrigin(barePath string) error {
	return executil.Run(barePath, "git", "fetch", "origin")
}

// SanitizeWorktree cleans up worktree state that would confuse agents:
//   - aborts any stuck rebase/merge/cherry-pick
//   - deletes local branches that shadow remote refs (e.g. local "origin/main")
func SanitizeWorktree(wtPath string) error {
	// Abort stuck rebase if any.
	if _, err := os.Stat(rebaseStateDir(wtPath)); err == nil {
		cmd := exec.Command("git", "rebase", "--abort")
		cmd.Dir = wtPath
		_ = cmd.Run() // best-effort
	}

	// Abort stuck merge if any.
	cmd := exec.Command("git", "rev-parse", "--git-path", "MERGE_HEAD")
	cmd.Dir = wtPath
	if out, err := cmd.Output(); err == nil {
		if _, statErr := os.Stat(strings.TrimSpace(string(out))); statErr == nil {
			abort := exec.Command("git", "merge", "--abort")
			abort.Dir = wtPath
			_ = abort.Run()
		}
	}

	// Delete local branches that shadow remote tracking refs.
	// A local branch named "origin/foo" shadows "refs/remotes/origin/foo".
	listCmd := exec.Command("git", "branch", "--format=%(refname:short)")
	listCmd.Dir = wtPath
	branchOut, err := listCmd.Output()
	if err != nil {
		return err
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(branchOut)), "\n") {
		if !strings.HasPrefix(line, "origin/") {
			continue
		}
		del := exec.Command("git", "branch", "-D", line)
		del.Dir = wtPath
		_ = del.Run()
	}
	return nil
}

// rebaseStateDir returns the path to the rebase-merge or rebase-apply dir.
func rebaseStateDir(wtPath string) string {
	// git worktrees store rebase state inside the .git dir (which is a file
	// pointing to the actual gitdir). Use rev-parse to resolve it.
	cmd := exec.Command("git", "rev-parse", "--git-dir")
	cmd.Dir = wtPath
	out, err := cmd.Output()
	if err != nil {
		return filepath.Join(wtPath, ".git", "rebase-merge")
	}
	gitDir := strings.TrimSpace(string(out))
	// Check both rebase-merge (interactive) and rebase-apply (am-style).
	for _, sub := range []string{"rebase-merge", "rebase-apply"} {
		p := filepath.Join(gitDir, sub)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return filepath.Join(gitDir, "rebase-merge")
}

func CreateWorktree(barePath, worktreePath, branch, baseBranch string) error {
	return executil.Run(barePath, "git", "worktree", "add", worktreePath, "-b", branch, baseBranch)
}

// CreateWorktreeExisting checks out an existing branch into a new worktree.
func CreateWorktreeExisting(barePath, worktreePath, branch string) error {
	return executil.Run(barePath, "git", "worktree", "add", worktreePath, branch)
}

// CreateWorktreeDetached creates a worktree in detached HEAD mode from a remote ref.
// Used for read-only checkouts like code reviews.
func CreateWorktreeDetached(barePath, worktreePath, ref string) error {
	return executil.Run(barePath, "git", "worktree", "add", "--detach", worktreePath, ref)
}

func ListWorktrees(barePath string) ([]Worktree, error) {
	out, err := executil.Output(barePath, "git", "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseWorktreePorcelain(out), nil
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

// PushUpstream pushes branch to origin with -u to set remote tracking.
func PushUpstream(worktreePath, branch string) error {
	return executil.Run(worktreePath, "git", "push", "-u", "origin", branch)
}

func RemoveWorktree(barePath, worktreePath string) error {
	return executil.Run(barePath, "git", "worktree", "remove", "--force", worktreePath)
}
