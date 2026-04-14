package monitor

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// ErrGHRateLimit is returned by IssueSink when gh reports an API rate limit.
// The Service catches it and continues the cycle without aborting.
var ErrGHRateLimit = errors.New("gh: API rate limit exceeded")

// IssueSink is responsible for filing or updating GitHub issues for an
// anomaly. It is also responsible for dedup against existing open issues —
// callers should not pre-filter by fingerprint cooldown for correctness, only
// for cost. Implementations must be safe for sequential calls within a tick.
type IssueSink interface {
	Submit(ctx context.Context, a Anomaly, body string) (created bool, err error)
}

// ghExecer abstracts gh invocation for tests. The default impl shells out via
// exec.CommandContext. Mirrors the pattern in internal/github/client.go.
type ghExecer interface {
	run(ctx context.Context, args ...string) ([]byte, error)
}

type defaultGHExecer struct{}

func (defaultGHExecer) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	return cmd.CombinedOutput()
}

// GHIssueSink is the production IssueSink: searches by title, comments on hit,
// creates with monitor + bug labels on miss. Labels are created once per
// process via labelsOnce.
type GHIssueSink struct {
	exec       ghExecer
	label      string
	labelsOnce sync.Once
}

// NewGHIssueSink returns a sink wired to the real gh CLI. label is the
// monitor anomaly label used for filtering and creation.
func NewGHIssueSink(label string) *GHIssueSink {
	if label == "" {
		label = "monitor"
	}
	return &GHIssueSink{exec: defaultGHExecer{}, label: label}
}

// Submit searches for an open issue matching the anomaly fingerprint title.
// On hit it comments; on miss it creates with --label monitor,bug. Returns
// (created=true) when a new issue was created, (false) when an existing issue
// was commented.
func (s *GHIssueSink) Submit(ctx context.Context, a Anomaly, body string) (bool, error) {
	s.labelsOnce.Do(func() { s.ensureLabels(ctx) })

	title := IssueTitle(a.Kind, a.Fingerprint)
	num, err := s.findOpenIssue(ctx, title)
	if err != nil {
		return false, err
	}
	if num > 0 {
		if _, err := s.exec.run(ctx, "issue", "comment", fmt.Sprint(num), "--body", body); err != nil {
			return false, classifyGHError(err)
		}
		return false, nil
	}
	if _, err := s.exec.run(ctx, "issue", "create",
		"--title", title,
		"--body", body,
		"--label", s.label+",bug",
	); err != nil {
		return false, classifyGHError(err)
	}
	return true, nil
}

func (s *GHIssueSink) ensureLabels(ctx context.Context) {
	// Best-effort. gh exits non-zero if the label exists; both outcomes are
	// fine. We swallow the error and rely on the create call to surface
	// label-related problems if any.
	_, _ = s.exec.run(ctx, "label", "create", s.label, "--color", "BFD4F2", "--description", "Opened by synapse monitor")
	_, _ = s.exec.run(ctx, "label", "create", "bug", "--color", "D73A4A", "--description", "Something isn't working")
}

func (s *GHIssueSink) findOpenIssue(ctx context.Context, title string) (int, error) {
	out, err := s.exec.run(ctx, "issue", "list",
		"--state", "open",
		"--label", s.label,
		"--search", "in:title \""+title+"\"",
		"--json", "number,title",
		"--limit", "5",
	)
	if err != nil {
		return 0, classifyGHError(err)
	}
	return parseFirstMatchingIssueNumber(out, title), nil
}

// parseFirstMatchingIssueNumber scans gh's `--json number,title` output and
// returns the number of the first issue whose title equals (case-sensitive)
// the requested title. Avoids importing encoding/json indirectly via a
// generic struct just for the test path.
func parseFirstMatchingIssueNumber(raw []byte, want string) int {
	// gh emits `[{"number":N,"title":"..."},...]` — minimal handcrafted
	// scan: find each "number":N followed by "title":"..." pair.
	s := string(raw)
	for {
		nIdx := strings.Index(s, "\"number\":")
		if nIdx < 0 {
			return 0
		}
		s = s[nIdx+len("\"number\":"):]
		// Skip whitespace.
		for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n') {
			s = s[1:]
		}
		// Parse the integer.
		end := 0
		for end < len(s) && s[end] >= '0' && s[end] <= '9' {
			end++
		}
		if end == 0 {
			return 0
		}
		num := 0
		for i := 0; i < end; i++ {
			num = num*10 + int(s[i]-'0')
		}
		s = s[end:]
		// Find the matching title field.
		tIdx := strings.Index(s, "\"title\":\"")
		if tIdx < 0 {
			return 0
		}
		s = s[tIdx+len("\"title\":\""):]
		closing := strings.Index(s, "\"")
		if closing < 0 {
			return 0
		}
		got := s[:closing]
		if got == want {
			return num
		}
		s = s[closing:]
	}
}

func classifyGHError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "API rate limit exceeded") || strings.Contains(msg, "secondary rate limit") {
		return ErrGHRateLimit
	}
	return err
}
