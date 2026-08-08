package monitor

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/Automaat/sybra/internal/errclass"

	"github.com/Automaat/sybra/internal/attribution"
	"github.com/Automaat/sybra/internal/github"
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

// IssueCloser is an optional IssueSink capability: closes whatever open
// issue (or local task, for monitorRoutingSink's work-project path) matches
// an anomaly's fingerprint, once its condition has cleared. Not every sink
// implements it (NoopSink and test fakes don't); callers type-assert and
// skip closing when it's absent.
type IssueCloser interface {
	CloseIfOpen(ctx context.Context, a Anomaly, comment string) (closed bool, err error)
}

type IncidentArtifact struct {
	Number int
	URL    string
}

// IncidentSink applies a material incident revision to one canonical
// artifact. Identity is the stable body marker, not a mutable title.
type IncidentSink interface {
	ApplyIncident(context.Context, Incident, IncidentChange, string) (created bool, artifact IncidentArtifact, err error)
	ResolveIncident(context.Context, Incident, string) (closed bool, err error)
	MapDuplicateIncidents(context.Context, Incident, []int, string) error
}

// ghExecer abstracts gh invocation for tests. The default impl routes through
// github.RunWithEnv. Mirrors the pattern in internal/github/client.go.
type ghExecer interface {
	run(ctx context.Context, args ...string) ([]byte, error)
}

type defaultGHExecer struct{}

// ghEnv is indirected (rather than calling github.GHEnv directly) so tests
// can inject a synthetic token without a real App-auth mint. Share the same
// credential source as every other gh call in the process (internal/github's
// ghExecer/ghRunCtx): the cached GitHub App installation token when one is
// configured, so this sink isn't silently dependent on an ambient `gh auth
// login`/GH_TOKEN that the App-auth setup was specifically meant to replace.
// See #2032.
var ghEnv = github.GHEnv

// run routes through github.RunWithEnv — the same request gate (pacing,
// rate-limit bookkeeping, auth-circuit breaker) every other gh call in the
// process gets — instead of shelling out directly, so this sink's traffic
// isn't invisible to the shared rate budget. See #2496.
func (defaultGHExecer) run(ctx context.Context, args ...string) ([]byte, error) {
	return github.RunWithEnv(ctx, ghEnv(), args...)
}

// GHIssueSink is the production IssueSink: searches by title, comments on hit,
// creates with monitor + bug labels on miss. Labels are created once per
// process via labelsOnce.
type GHIssueSink struct {
	exec       ghExecer
	label      string
	repo       string
	labelsOnce sync.Once
	applyMu    sync.Mutex
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

func IncidentTitle(in Incident) string {
	return "[monitor] incident " + in.FailureCode + ": " + strings.TrimPrefix(in.Fingerprint, "incident:")
}

func incidentMarker(fp string) string { return "<!-- sybra-incident:v1:" + fp + " -->" }

type ghIncident struct {
	Number      int
	URL         string
	State       string
	HasRevision bool
	Duplicates  []int
}

func incidentRevisionMarker(in Incident) string {
	return fmt.Sprintf("<!-- sybra-incident-revision:v1:%s:%d:%s -->", in.Fingerprint, in.Revision, in.State)
}

func (s *GHIssueSink) findIncident(ctx context.Context, fp, revisionMarker string) (ghIncident, error) {
	marker := incidentMarker(fp)
	out, err := s.exec.run(ctx, append(s.repoArgs(), "issue", "list", "--state", "all", "--label", s.label,
		"--json", "number,url,state,body", "--limit", "1000")...)
	if err != nil {
		return ghIncident{}, classifyGHError("gh incident list", out, err)
	}
	type incidentRow struct {
		Number int    `json:"number"`
		URL    string `json:"url"`
		State  string `json:"state"`
		Body   string `json:"body"`
	}
	var rows []incidentRow
	if err := json.Unmarshal(out, &rows); err != nil {
		return ghIncident{}, fmt.Errorf("decode gh incident list: %w", err)
	}
	var matches []incidentRow
	for _, row := range rows {
		if strings.Contains(row.Body, marker) {
			matches = append(matches, row)
		}
	}
	if len(matches) == 0 {
		return ghIncident{}, nil
	}
	slices.SortFunc(matches, func(a, b incidentRow) int { return cmp.Compare(a.Number, b.Number) })
	canonical := matches[0]
	found := ghIncident{Number: canonical.Number, URL: canonical.URL, State: strings.ToUpper(canonical.State)}
	for _, duplicate := range matches[1:] {
		if !strings.EqualFold(duplicate.State, "CLOSED") {
			found.Duplicates = append(found.Duplicates, duplicate.Number)
		}
	}
	found.HasRevision = revisionMarker != "" && strings.Contains(canonical.Body, revisionMarker)
	if revisionMarker != "" && !found.HasRevision {
		viewOut, viewErr := s.exec.run(ctx, append(s.repoArgs(), "issue", "view", strconv.Itoa(canonical.Number), "--json", "comments")...)
		if viewErr != nil {
			return ghIncident{}, classifyGHError("gh incident view", viewOut, viewErr)
		}
		var viewed struct {
			Comments []struct {
				Body string `json:"body"`
			} `json:"comments"`
		}
		if decodeErr := json.Unmarshal(viewOut, &viewed); decodeErr != nil {
			return ghIncident{}, fmt.Errorf("decode gh incident comments: %w", decodeErr)
		}
		for _, comment := range viewed.Comments {
			found.HasRevision = strings.Contains(comment.Body, revisionMarker)
			if found.HasRevision {
				break
			}
		}
	}
	return found, nil
}

func (s *GHIssueSink) ApplyIncident(ctx context.Context, in Incident, change IncidentChange, body string) (bool, IncidentArtifact, error) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	s.labelsOnce.Do(func() { s.ensureLabels(ctx) })
	revisionMarker := incidentRevisionMarker(in)
	found, err := s.findIncident(ctx, in.Fingerprint, revisionMarker)
	if err != nil {
		return false, IncidentArtifact{}, err
	}
	body = incidentMarker(in.Fingerprint) + "\n" + revisionMarker + "\n" + body
	if len(found.Duplicates) > 0 {
		canonical := in
		canonical.IssueURL = found.URL
		if mapErr := s.MapDuplicateIncidents(ctx, canonical, found.Duplicates, "same stable incident fingerprint marker"); mapErr != nil {
			return false, IncidentArtifact{}, mapErr
		}
	}
	if found.Number == 0 {
		out, createErr := s.exec.run(ctx, append(s.repoArgs(), "issue", "create", "--title", IncidentTitle(in), "--body", attribution.Append(body), "--label", s.label+",bug")...)
		if createErr != nil {
			return false, IncidentArtifact{}, classifyGHError("gh incident create", out, createErr)
		}
		createdURL := parseIssueCreateURL(out)
		// A second process may have won the same list-then-create race. Re-query
		// and converge all marker-identical artifacts on the oldest canonical.
		canonical, reconcileErr := s.findIncident(ctx, in.Fingerprint, revisionMarker)
		if reconcileErr == nil && canonical.Number != 0 {
			if len(canonical.Duplicates) > 0 {
				linked := in
				linked.IssueURL = canonical.URL
				if mapErr := s.MapDuplicateIncidents(ctx, linked, canonical.Duplicates, "same stable incident fingerprint marker"); mapErr != nil {
					return false, IncidentArtifact{}, mapErr
				}
			}
			return true, IncidentArtifact{Number: canonical.Number, URL: canonical.URL}, nil
		}
		return true, IncidentArtifact{URL: createdURL}, nil
	}
	if found.State == "CLOSED" && in.State == IncidentActive {
		out, reopenErr := s.exec.run(ctx, append(s.repoArgs(), "issue", "reopen", strconv.Itoa(found.Number), "--comment", attribution.Append(body))...)
		if reopenErr != nil {
			return false, IncidentArtifact{}, classifyGHError("gh incident reopen", out, reopenErr)
		}
		return false, IncidentArtifact{Number: found.Number, URL: found.URL}, nil
	}
	if found.HasRevision {
		return false, IncidentArtifact{Number: found.Number, URL: found.URL}, nil
	}
	if change != IncidentUnchanged {
		out, commentErr := s.exec.run(ctx, append(s.repoArgs(), "issue", "comment", strconv.Itoa(found.Number), "--body", attribution.Append(body))...)
		if commentErr != nil {
			return false, IncidentArtifact{}, classifyGHError("gh incident comment", out, commentErr)
		}
	}
	return false, IncidentArtifact{Number: found.Number, URL: found.URL}, nil
}

func (s *GHIssueSink) ResolveIncident(ctx context.Context, in Incident, comment string) (bool, error) {
	revisionMarker := incidentRevisionMarker(in)
	found, err := s.findIncident(ctx, in.Fingerprint, revisionMarker)
	if err != nil || found.Number == 0 || found.State == "CLOSED" {
		return false, err
	}
	out, closeErr := s.exec.run(ctx, append(s.repoArgs(), "issue", "close", strconv.Itoa(found.Number), "--reason", "completed", "--comment", attribution.Append(revisionMarker+"\n"+comment))...)
	if closeErr != nil {
		return false, classifyGHError("gh incident close", out, closeErr)
	}
	return true, nil
}

func (s *GHIssueSink) MapDuplicateIncidents(ctx context.Context, in Incident, duplicates []int, coverage string) error {
	if strings.TrimSpace(coverage) == "" || in.IssueURL == "" {
		return errors.New("incident duplicate mapping requires canonical URL and reproduction coverage")
	}
	for _, number := range duplicates {
		body := fmt.Sprintf("Covered by canonical incident %s. Reproduction coverage: %s", in.IssueURL, coverage)
		out, err := s.exec.run(ctx, append(s.repoArgs(), "issue", "close", strconv.Itoa(number), "--reason", "not planned", "--comment", attribution.Append(body))...)
		if err != nil {
			return classifyGHError("gh duplicate incident close", out, err)
		}
	}
	return nil
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
		out, runErr := s.exec.run(ctx, append(s.repoArgs(), "issue", "comment", strconv.Itoa(num), "--body", attribution.Append(body))...)
		if runErr != nil {
			return false, foundURL, classifyGHError("gh issue comment", out, runErr)
		}
		return false, foundURL, nil
	}
	labels := issueCreateLabels(s.label, extraLabels)
	s.ensureExtraLabels(ctx, labels[1:])
	args := append(s.repoArgs(), "issue", "create",
		"--title", title,
		"--body", attribution.Append(body),
	)
	for _, label := range labels {
		args = append(args, "--label", label)
	}
	out, err := s.exec.run(ctx, args...)
	if err != nil {
		return false, "", classifyGHError("gh issue create", out, err)
	}
	return true, parseIssueCreateURL(out), nil
}

// CloseIfOpen implements IssueCloser: closes the open issue matching the
// anomaly's fingerprint title, if one exists. Used to auto-resolve a
// deterministic-kind issue (e.g. lost_agent) once its condition has stayed
// clear for the configured number of consecutive scans — the same intent as
// the #2433 merged-PR task auto-close, applied to monitor-filed issues.
func (s *GHIssueSink) CloseIfOpen(ctx context.Context, a Anomaly, comment string) (bool, error) {
	num, _, err := s.findOpenIssue(ctx, IssueTitle(a.Kind, a.Fingerprint))
	if err != nil {
		return false, err
	}
	if num == 0 {
		return false, nil
	}
	args := append(s.repoArgs(), "issue", "close", strconv.Itoa(num), "--reason", "completed", "--comment", attribution.Append(comment))
	out, err := s.exec.run(ctx, args...)
	if err != nil {
		return false, classifyGHError("gh issue close", out, err)
	}
	return true, nil
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

// ensureExtraLabels best-effort creates caller-supplied labels (e.g. an
// issue_labels array from a human-review LLM verdict) that ensureLabels
// doesn't already cover. Without this, gh issue create hard-fails outright
// when a caller names a label that doesn't exist on the repo yet, discarding
// an otherwise-valid issue and forcing the filing path down its fallback
// task/issue route. Same best-effort discipline as ensureLabels: an
// already-exists error is fine, and any other error is swallowed so the
// create call below still surfaces label problems if the label genuinely
// can't be used.
//
// Only called on the create path (num == 0 in SubmitIssue) since labels are
// only attached there, not on the comment path — this also avoids spending a
// gh call per extra label on the far more common dedup-hit path.
//
// The "--" before extra stops gh from parsing a caller-supplied label that
// happens to start with "-" (e.g. LLM output like "-1-of-3") as a flag; without
// it gh fails with "unknown shorthand flag", the create is silently swallowed,
// and the later issue-create call reproduces the same "label not found"
// failure this fix targets, just for a dash-prefixed label.
func (s *GHIssueSink) ensureExtraLabels(ctx context.Context, extraLabels []string) {
	for _, extra := range extraLabels {
		extra = strings.TrimSpace(extra)
		if extra == "" || extra == s.label || extra == "bug" {
			continue
		}
		_, _ = s.exec.run(ctx, append(s.repoArgs(), "label", "create", "--", extra)...)
	}
}

func issueCreateLabels(primary string, extraLabels []string) []string {
	labels := []string{primary}
	seen := map[string]struct{}{primary: {}}
	for _, extra := range extraLabels {
		extra = strings.TrimSpace(extra)
		if extra == "" {
			continue
		}
		if _, dup := seen[extra]; dup {
			continue
		}
		labels = append(labels, extra)
		seen[extra] = struct{}{}
	}
	return labels
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
		return 0, "", classifyGHError("gh issue list", out, err)
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

func classifyGHError(op string, out []byte, err error) error {
	if err == nil {
		return nil
	}
	out = redactSecrets(out)
	msg := err.Error() + "\n" + string(out)
	if errclass.Classify(msg, errclass.MonitorCooldownBiased) == errclass.RateLimited {
		return ErrGHRateLimit
	}
	if detail := sanitizeGHOutput(out); detail != "" {
		return fmt.Errorf("%s: %s: %w", op, detail, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// ghTokenPattern matches GitHub's token formats (personal access, OAuth, App
// installation, refresh). Applied to gh's combined output before it's ever
// wrapped into an error, logged, or written to an audit event, so a `gh`
// subprocess that echoes its own credentials (verbose/debug output, an odd
// error message) can't leak them downstream. See #2032.
var ghTokenPattern = regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr|github_pat)_[A-Za-z0-9_]{20,}\b`)

// redactSecrets strips GitHub token-shaped substrings from gh's combined
// output, plus the exact currently-cached App installation token as a
// belt-and-suspenders exact-match pass for token shapes the pattern above
// doesn't cover.
func redactSecrets(out []byte) []byte {
	s := ghTokenPattern.ReplaceAllString(string(out), "[redacted]")
	if token := github.CurrentAppToken(); token != "" {
		s = strings.ReplaceAll(s, token, "[redacted]")
	}
	return []byte(s)
}

func sanitizeGHOutput(out []byte) string {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return ""
	}
	lower := strings.ToLower(s)
	if strings.Contains(lower, "<!doctype html") || strings.Contains(lower, "<html") {
		for i := len(s) - 1; i >= 0; i-- {
			if s[i] != '\n' {
				continue
			}
			if line := strings.TrimSpace(s[i+1:]); strings.HasPrefix(line, "gh:") {
				return line
			}
		}
		return "GitHub returned an HTML error page"
	}
	lines := strings.Split(s, "\n")
	const maxLines = 5
	if len(lines) > maxLines {
		lines = append(lines[:maxLines], "...")
	}
	return strings.Join(lines, "\n")
}
