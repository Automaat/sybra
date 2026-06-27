package github

import (
	"fmt"
	"strconv"
	"strings"
)

// CloseIssue closes a GitHub issue, optionally leaving a comment. repo is
// "owner/name".
func CloseIssue(repo string, number int, comment string) error {
	return closeIssueWith(defaultExecer, repo, number, comment)
}

func closeIssueWith(e execer, repo string, number int, comment string) error {
	args := []string{"issue", "close", strconv.Itoa(number), "--repo", repo, "--reason", "completed"}
	if comment != "" {
		args = append(args, "--comment", comment)
	}
	out, err := e.run(args...)
	if err != nil {
		return fmt.Errorf("gh issue close %d: %s: %w", number, strings.TrimSpace(string(out)), err)
	}
	return nil
}
