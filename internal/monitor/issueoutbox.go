package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/github"
	"gopkg.in/yaml.v3"
)

// issueOutboxMaxDepth bounds how many distinct failed filings a single
// outbox will hold. Once full, new failures are logged and dropped rather
// than persisted — a stuck outbox must never grow unboundedly on disk.
// A package var (not const) so tests can shrink it instead of writing 200+
// fixture files.
var issueOutboxMaxDepth = 200

// issueOutboxMaxAttempts bounds how many times a single pending filing is
// retried before it is dropped as permanently failed. At one retry per
// flush (each Submit/SubmitIssue call, plus each monitor tick), this is a
// generous number of chances without retrying forever against a
// misconfiguration nobody is going to fix.
var issueOutboxMaxAttempts = 20

// issueSubmitter is the subset of *GHIssueSink a DurableGHIssueSink wraps.
// Narrowed to an interface so tests can substitute a fake without a real gh
// binary.
type issueSubmitter interface {
	SubmitIssue(ctx context.Context, title, body string, extraLabels []string) (created bool, url string, err error)
}

// outboxItem is one pending issue filing, persisted as a single YAML file.
type outboxItem struct {
	Fingerprint   string    `yaml:"fingerprint"`
	Title         string    `yaml:"title"`
	Body          string    `yaml:"body"`
	ExtraLabels   []string  `yaml:"extra_labels,omitempty"`
	Attempts      int       `yaml:"attempts"`
	FirstFailedAt time.Time `yaml:"first_failed_at"`
	LastAttemptAt time.Time `yaml:"last_attempt_at"`
	LastError     string    `yaml:"last_error,omitempty"`
}

func fingerprintTitle(title string) string {
	sum := sha256.Sum256([]byte(title))
	return hex.EncodeToString(sum[:])[:16]
}

// issueOutboxStore persists pending issue filings as one YAML file per
// fingerprint under dir, mirroring internal/agentqueue's store pattern.
// dir is always injected by the caller — this type never resolves
// config.HomeDir() itself.
type issueOutboxStore struct {
	dir string
}

func newIssueOutboxStore(dir string) (*issueOutboxStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create gh issue outbox dir %s: %w", dir, err)
	}
	return &issueOutboxStore{dir: dir}, nil
}

func (s *issueOutboxStore) filePath(fingerprint string) string {
	return filepath.Join(s.dir, fingerprint+".yaml")
}

func (s *issueOutboxStore) put(it outboxItem) error {
	data, err := yaml.Marshal(it)
	if err != nil {
		return fmt.Errorf("marshal outbox item: %w", err)
	}
	return fsutil.AtomicWrite(s.filePath(it.Fingerprint), data)
}

func (s *issueOutboxStore) del(fingerprint string) error {
	if err := os.Remove(s.filePath(fingerprint)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete outbox item: %w", err)
	}
	return nil
}

// load reads every pending item, skipping and logging any that fail to read
// or parse rather than failing the whole load.
func (s *issueOutboxStore) load(log *slog.Logger) []outboxItem {
	paths, err := fsutil.ListFiles(s.dir, ".yaml")
	if err != nil {
		log.Warn("monitor.issue_outbox.load.list-failed", "dir", s.dir, "err", err)
		return nil
	}
	out := make([]outboxItem, 0, len(paths))
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			log.Warn("monitor.issue_outbox.load.read-failed", "path", p, "err", err)
			continue
		}
		var it outboxItem
		if err := yaml.Unmarshal(data, &it); err != nil {
			log.Warn("monitor.issue_outbox.load.parse-failed", "path", p, "err", err)
			continue
		}
		if it.Fingerprint == "" {
			log.Warn("monitor.issue_outbox.load.empty-fingerprint", "path", p)
			continue
		}
		out = append(out, it)
	}
	return out
}

func (s *issueOutboxStore) depth() int {
	paths, err := fsutil.ListFiles(s.dir, ".yaml")
	if err != nil {
		return 0
	}
	return len(paths)
}

// DurableGHIssueSink wraps a GH issue sink so a filing that fails with an
// authentication error is persisted to a bounded, on-disk outbox and
// retried on the next call instead of being lost — the credentials that
// caused the failure (an unconfigured GitHub App, a dead ambient `gh auth
// login`) commonly recover without a Sybra restart, and even across a
// restart the outbox survives since it is read from disk, never held only
// in memory. See issue #2032.
//
// Only auth-classified failures are queued: a malformed label or a
// duplicate-title conflict won't resolve itself by retrying, so those are
// returned to the caller unchanged without consuming outbox space.
type DurableGHIssueSink struct {
	inner    issueSubmitter
	store    *issueOutboxStore
	logger   *slog.Logger
	name     string        // sink identity for logs, e.g. "monitor" or "human-review"
	auditLog *audit.Logger // optional; nil in tests that construct the struct directly
}

// NewDurableGHIssueSink wraps inner with a bounded on-disk retry outbox
// rooted at dir. name identifies the sink in log lines (e.g. "monitor",
// "human-review") since a process may run more than one. auditLog may be nil
// (no audit event is emitted, only the log line) — callers that want the
// failure surfaced as a health.Finding (see internal/health's
// checkGHIssueAuthFailure) must pass a non-nil logger.
func NewDurableGHIssueSink(inner *GHIssueSink, dir, name string, logger *slog.Logger, auditLog *audit.Logger) (*DurableGHIssueSink, error) {
	if logger == nil {
		logger = slog.Default()
	}
	store, err := newIssueOutboxStore(dir)
	if err != nil {
		return nil, err
	}
	return &DurableGHIssueSink{inner: inner, store: store, logger: logger, name: name, auditLog: auditLog}, nil
}

// Submit implements IssueSink.
func (d *DurableGHIssueSink) Submit(ctx context.Context, a Anomaly, body string) (bool, error) {
	created, _, err := d.SubmitIssue(ctx, IssueTitle(a.Kind, a.Fingerprint), body, []string{"bug"})
	return created, err
}

// SubmitIssue implements the humanReviewIssueFiler-compatible surface. It
// first drains what it can of the pending outbox, then attempts the
// caller's own submission.
func (d *DurableGHIssueSink) SubmitIssue(ctx context.Context, title, body string, extraLabels []string) (created bool, url string, err error) {
	d.flushPending(ctx)

	created, url, err = d.inner.SubmitIssue(ctx, title, body, extraLabels)
	if err == nil {
		return created, url, nil
	}
	if !github.IsAuthError(err) {
		return created, url, err
	}
	d.persist(title, body, extraLabels, err)
	return created, url, err
}

// flushPending retries every currently-pending outbox item once. Items that
// succeed are removed; items that fail again are re-persisted with a bumped
// attempt count, or dropped once issueOutboxMaxAttempts is exceeded.
func (d *DurableGHIssueSink) flushPending(ctx context.Context) {
	pending := d.store.load(d.logger)
	for _, it := range pending {
		created, _, err := d.inner.SubmitIssue(ctx, it.Title, it.Body, it.ExtraLabels)
		if err == nil {
			if delErr := d.store.del(it.Fingerprint); delErr != nil {
				d.logger.Warn("monitor.issue_outbox.retry.cleanup-failed", "sink", d.name, "fingerprint", it.Fingerprint, "err", delErr)
				continue
			}
			d.logger.Info("monitor.issue_outbox.retry.succeeded", "sink", d.name, "fingerprint", it.Fingerprint, "created", created, "attempts", it.Attempts+1)
			continue
		}
		it.Attempts++
		it.LastAttemptAt = time.Now().UTC()
		it.LastError = redactedErrorString(err)
		if it.Attempts >= issueOutboxMaxAttempts {
			if delErr := d.store.del(it.Fingerprint); delErr != nil {
				d.logger.Warn("monitor.issue_outbox.retry.cleanup-failed", "sink", d.name, "fingerprint", it.Fingerprint, "err", delErr)
			}
			d.logger.Error("monitor.issue_outbox.retry.exhausted", "sink", d.name, "fingerprint", it.Fingerprint, "title", it.Title, "attempts", it.Attempts, "err", it.LastError)
			continue
		}
		if putErr := d.store.put(it); putErr != nil {
			d.logger.Warn("monitor.issue_outbox.retry.persist-failed", "sink", d.name, "fingerprint", it.Fingerprint, "err", putErr)
		}
	}
}

// persist queues a newly-failed filing. It logs exactly once per distinct
// title — the first time a fingerprint is newly written — so a repeatedly
// failing filing produces one actionable log line per outage, not one per
// attempt.
func (d *DurableGHIssueSink) persist(title, body string, extraLabels []string, submitErr error) {
	fp := fingerprintTitle(title)
	now := time.Now().UTC()
	existing, hadExisting := d.lookup(fp)
	it := outboxItem{
		Fingerprint:   fp,
		Title:         title,
		Body:          body,
		ExtraLabels:   extraLabels,
		Attempts:      0,
		FirstFailedAt: now,
		LastAttemptAt: now,
		LastError:     redactedErrorString(submitErr),
	}
	if hadExisting {
		it.Attempts = existing.Attempts
		it.FirstFailedAt = existing.FirstFailedAt
	} else if d.store.depth() >= issueOutboxMaxDepth {
		d.logger.Error("monitor.issue_outbox.full", "sink", d.name, "depth", issueOutboxMaxDepth, "title", title, "err", it.LastError)
		return
	}
	if err := d.store.put(it); err != nil {
		d.logger.Warn("monitor.issue_outbox.persist-failed", "sink", d.name, "fingerprint", fp, "err", err)
		return
	}
	if !hadExisting {
		d.logger.Error("monitor.issue.auth_failed", "sink", d.name, "title", title, "err", it.LastError,
			"hint", "issue filing is unauthenticated; configure github.app or `gh auth login` — queued for retry")
		audit.LogEvent(d.auditLog, d.logger, audit.EventGHIssueAuthFailed, "", "", map[string]any{
			"sink": d.name, "err": it.LastError,
		})
	}
}

func (d *DurableGHIssueSink) lookup(fingerprint string) (outboxItem, bool) {
	data, err := os.ReadFile(d.store.filePath(fingerprint))
	if err != nil {
		return outboxItem{}, false
	}
	var it outboxItem
	if err := yaml.Unmarshal(data, &it); err != nil {
		return outboxItem{}, false
	}
	return it, true
}

// redactedErrorString returns err's message with GitHub token-shaped
// substrings stripped, so a persisted outbox entry or a log line built from
// it never carries live credentials.
func redactedErrorString(err error) string {
	if err == nil {
		return ""
	}
	return string(redactSecrets([]byte(err.Error())))
}
