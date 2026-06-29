package monitor

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/Automaat/sybra/internal/attribution"
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
	repo       string
	labelsOnce sync.Once
}

// NewGHIssueSink returns a sink wired to the real gh CLI. label is the
// monitor anomaly label used for filtering and creation. repo is the
// "owner/name" GitHub repository where issues are filed; it is passed via
// --repo so the sink is independent of the process working directory.
func NewGHIssueSink(label, repo string) *GHIssueSink {
	if label == "" {
		label = "monitor"
	}
	return &GHIssueSink{exec: defaultGHExecer{}, label: label, repo: repo}
}

// Submit searches for an open issue matching the anomaly fingerprint title.
// On hit it comments; on miss it creates with --label monitor,bug. Returns
// (created=true) when a new issue was created, (false) when an existing issue
// was commented.
func (s *GHIssueSink) Submit(ctx context.Context, a Anomaly, body string) (bool, error) {
	created, _, err := s.SubmitIssue(ctx, IssueTitle(a.Kind, a.Fingerprint), body, []string{"bug"})
	return created, err
}

// SubmitIssue is the generic, anomaly-agnostic dedup-and-file primitive used
// by Submit and by other in-process automations (e.g. human-review). Behavior
// is identical to Submit: finds an open issue with the sink's primary label
// and matching title, comments on hit, creates on miss.
//
// extraLabels are appended to the sink's primary label on create. The dedup
// search still filters by the primary label, so callers that want a separate
// issue stream should construct their own sink with a dedicated label.
//
// Returns (created, url, err): url is the GitHub URL of the existing
// (commented) or newly created issue, parsed from gh's stdout. url may be
// empty if gh's output cannot be parsed but the operation otherwise
// succeeded.
func (s *GHIssueSink) SubmitIssue(ctx context.Context, title, body string, extraLabels []string) (created bool, url string, err error) {
	s.labelsOnce.Do(func() { s.ensureLabels(ctx) })

	num, foundURL, err := s.findOpenIssue(ctx, title)
	if err != nil {
		return false, "", err
	}
	if num > 0 {
		if _, runErr := s.exec.run(ctx, append(s.repoArgs(), "issue", "comment", strconv.Itoa(num), "--body", attribution.Append(body))...); runErr != nil {
			return false, foundURL, classifyGHError(runErr)
		}
		return false, foundURL, nil
	}
	var lb strings.Builder
	lb.WriteString(s.label)
	for _, extra := range extraLabels {
		extra = strings.TrimSpace(extra)
		if extra == "" || extra == s.label {
			continue
		}
		lb.WriteString(",")
		lb.WriteString(extra)
	}
	out, err := s.exec.run(ctx, append(s.repoArgs(), "issue", "create",
		"--title", title,
		"--body", attribution.Append(body),
		"--label", lb.String(),
	)...)
	if err != nil {
		return false, "", classifyGHError(err)
	}
	return true, parseIssueCreateURL(out), nil
}

// parseIssueCreateURL pulls the first line of `gh issue create` stdout that
// looks like a github.com issue URL. gh prints the URL on its own line; on
// older versions it may also emit a "Creating issue ..." prefix.
func parseIssueCreateURL(raw []byte) string {
	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "https://") && strings.Contains(line, "/issues/") {
			return line
		}
	}
	return ""
}

// repoArgs returns ["--repo", s.repo] when a repo is configured, or nil.
func (s *GHIssueSink) repoArgs() []string {
	if s.repo == "" {
		return nil
	}
	return []string{"--repo", s.repo}
}

func (s *GHIssueSink) ensureLabels(ctx context.Context) {
	// Best-effort. gh exits non-zero if the label exists; both outcomes are
	// fine. We swallow the error and rely on the create call to surface
	// label-related problems if any.
	_, _ = s.exec.run(ctx, append(s.repoArgs(), "label", "create", s.label, "--color", "BFD4F2", "--description", "Opened by sybra monitor")...)
	_, _ = s.exec.run(ctx, append(s.repoArgs(), "label", "create", "bug", "--color", "D73A4A", "--description", "Something isn't working")...)
}

func (s *GHIssueSink) findOpenIssue(ctx context.Context, title string) (number int, url string, err error) {
	out, err := s.exec.run(ctx, append(s.repoArgs(), "issue", "list",
		"--state", "open",
		"--label", s.label,
		"--search", "in:title \""+title+"\"",
		"--json", "number,title,url",
		"--limit", "5",
	)...)
	if err != nil {
		return 0, "", classifyGHError(err)
	}
	num, url := parseFirstMatchingIssue(out, title)
	return num, url, nil
}

// parseFirstMatchingIssue scans gh's `--json number,title,url` output and
// returns the (number, url) of the first issue whose title equals
// (case-sensitive) the requested title. Returns (0, "") on no match.
// Avoids importing encoding/json indirectly via a generic struct just for
// the test path.
func parseFirstMatchingIssue(raw []byte, want string) (number int, url string) {
	// gh emits `[{"number":N,"title":"...","url":"..."},...]` —
	// minimal handcrafted scan: read each object's number, title, and (if
	// the title matches) url. Field order in gh's output is alphabetical
	// (number, title, url).
	s := string(raw)
	for {
		nIdx := strings.Index(s, "\"number\":")
		if nIdx < 0 {
			return 0, ""
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
			return 0, ""
		}
		num := 0
		for i := range end {
			num = num*10 + int(s[i]-'0')
		}
		s = s[end:]
		// Find the matching title field.
		tIdx := strings.Index(s, "\"title\":\"")
		if tIdx < 0 {
			return 0, ""
		}
		s = s[tIdx+len("\"title\":\""):]
		closing := strings.Index(s, "\"")
		if closing < 0 {
			return 0, ""
		}
		got := s[:closing]
		s = s[closing:]
		if got != want {
			continue
		}
		// Title matched — pull url if present (alphabetical, so it follows).
		uIdx := strings.Index(s, "\"url\":\"")
		if uIdx < 0 {
			return num, ""
		}
		s = s[uIdx+len("\"url\":\""):]
		urlPart, _, ok := strings.Cut(s, "\"")
		if !ok {
			return num, ""
		}
		return num, urlPart
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
