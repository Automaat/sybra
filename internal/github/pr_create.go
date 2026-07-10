package github

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
)

// CreatePRRequest describes a new pull request to open for an
// already-pushed branch.
type CreatePRRequest struct {
	// Repo is the base repo the PR is opened against, "owner/name".
	Repo string
	// Head is the `gh pr create --head` value: a bare branch name, or
	// "fork-owner:branch" when the branch lives on a fork.
	Head  string
	Draft bool
	Title string
	Body  string
}

// createPRRunner runs `gh pr create` in dir and returns its combined output.
// A package var so tests can inject a fake without a real gh/network.
var createPRRunner = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return ghGate.execute(func() ([]byte, error) {
		cmd := exec.CommandContext(ctx, "gh", args...)
		cmd.Dir = dir
		if env := ghEnv(); env != nil {
			cmd.Env = env
		}
		return cmd.CombinedOutput()
	})
}

// CreatePR opens a pull request for a branch that has already been pushed,
// running `gh` inside dir (the task's worktree) so it resolves the same
// repo/fork context an interactive `gh pr create` would. Returns the new PR
// number and its head commit SHA.
func CreatePR(ctx context.Context, dir string, req CreatePRRequest) (number int, headSHA string, err error) {
	args := []string{
		"pr", "create",
		"--repo", req.Repo,
		"--head", req.Head,
		"--title", req.Title,
		"--body", req.Body,
	}
	if req.Draft {
		args = append(args, "--draft")
	}
	out, runErr := createPRRunner(ctx, dir, args...)
	if runErr != nil {
		return 0, "", fmt.Errorf("gh pr create: %s: %w", sanitizeGHOutput(out), runErr)
	}
	_, number = ParsePRURL(lastNonEmptyLine(string(out)))
	if number == 0 {
		return 0, "", fmt.Errorf("gh pr create: could not parse pr number from output: %s", sanitizeGHOutput(out))
	}
	sha, shaErr := FetchPRHeadSHAContext(ctx, req.Repo, number)
	if shaErr != nil {
		return number, "", fmt.Errorf("pr #%d created but head sha lookup failed: %w", number, shaErr)
	}
	return number, sha, nil
}

// lastNonEmptyLine returns the last non-blank line of s. `gh pr create`
// prints the new PR URL as its final line of stdout, possibly preceded by
// warnings.
func lastNonEmptyLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for _, raw := range slices.Backward(lines) {
		if line := strings.TrimSpace(raw); line != "" {
			return line
		}
	}
	return ""
}
