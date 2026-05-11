package sybra

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/task"
)

type fakeIssueSink struct {
	mu      sync.Mutex
	calls   int
	created bool
	url     string
	err     error

	gotTitle  string
	gotBody   string
	gotLabels []string
}

func (f *fakeIssueSink) SubmitIssue(_ context.Context, title, body string, labels []string) (created bool, url string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.gotTitle = title
	f.gotBody = body
	f.gotLabels = labels
	return f.created, f.url, f.err
}

func newReviewTestEnv(t *testing.T) (*humanReviewHandler, *task.Manager, *fakeIssueSink, func()) {
	t.Helper()
	dir := t.TempDir()
	store, err := task.NewStore(dir)
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	tasks := task.NewManager(store, task.EmitterFunc(func(string, any) {}))
	cfg := &config.Config{}
	cfg.HumanReview.Enabled = true
	cfg.HumanReview.SybraRepoDir = dir
	cfg.HumanReview.MaxPerHour = 3
	sink := &fakeIssueSink{created: true, url: "https://github.com/Automaat/sybra/issues/42"}
	logger := slog.New(slog.DiscardHandler)
	h := newHumanReviewHandler(cfg, tasks, nil, nil, logger, sink, dir, filepath.Join(dir, "missing.log"), nil)
	return h, tasks, sink, func() {}
}

func TestParseVerdict(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		input   string
		want    verdictDecision
		wantErr bool
	}{
		{
			name: "human decision",
			input: "Looks like a real ambiguity.\n\n```sybra-verdict\n" +
				`{"decision":"human","summary":"needs scope clarification"}` +
				"\n```\n",
			want: verdictDecision{Decision: "human", Summary: "needs scope clarification"},
		},
		{
			name: "sybra_bug decision",
			input: "Found a workflow bug.\n\n```sybra-verdict\n" +
				`{"decision":"sybra_bug","summary":"verify_commits flipped despite push","issue_title":"fix(workflow): verify_commits race","issue_body":"## What\nrace","issue_labels":["workflow"]}` +
				"\n```\n",
			want: verdictDecision{
				Decision: "sybra_bug", Summary: "verify_commits flipped despite push",
				IssueTitle: "fix(workflow): verify_commits race", IssueBody: "## What\nrace",
				IssueLabels: []string{"workflow"},
			},
		},
		{
			name:    "missing block",
			input:   "no fenced verdict here",
			wantErr: true,
		},
		{
			name:    "invalid json",
			input:   "```sybra-verdict\n{broken\n```",
			wantErr: true,
		},
		{
			name:    "unknown decision",
			input:   "```sybra-verdict\n{\"decision\":\"maybe\"}\n```",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseVerdict(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseVerdict: %v", err)
			}
			if got.Decision != tc.want.Decision || got.Summary != tc.want.Summary {
				t.Errorf("decision/summary: got %+v want %+v", got, tc.want)
			}
			if got.IssueTitle != tc.want.IssueTitle || got.IssueBody != tc.want.IssueBody {
				t.Errorf("issue: got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestRateLimiter(t *testing.T) {
	t.Parallel()
	h, _, _, cleanup := newReviewTestEnv(t)
	defer cleanup()

	now := time.Now()
	h.now = func() time.Time { return now }

	for i := range 3 {
		h.mu.Lock()
		ok := h.allowSpawnLocked()
		if ok {
			h.recent = append(h.recent, h.now())
		}
		h.mu.Unlock()
		if !ok {
			t.Fatalf("spawn %d should be allowed", i)
		}
	}
	h.mu.Lock()
	overLimit := h.allowSpawnLocked()
	h.mu.Unlock()
	if overLimit {
		t.Errorf("4th spawn should be rate-limited")
	}

	// Advance past the window: old entries expire and a slot frees up.
	h.now = func() time.Time { return now.Add(humanReviewWindow + time.Minute) }
	h.mu.Lock()
	allowed := h.allowSpawnLocked()
	h.mu.Unlock()
	if !allowed {
		t.Errorf("after window, spawn should be allowed again")
	}
}

func TestOnComplete_HumanVerdict_AppendsNote(t *testing.T) {
	t.Parallel()
	h, tasks, sink, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Refactor billing", "Original body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusHumanRequired)}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}

	ag := &agent.Agent{TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{
		Type: "assistant",
		Content: "Verdict below.\n\n```sybra-verdict\n" +
			`{"decision":"human","summary":"needs product input on scope"}` +
			"\n```\n",
	})
	h.inflight[tk.ID] = "agent-1"
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Errorf("status: got %q want human-required", got.Status)
	}
	if got.BlockedByIssue != "" {
		t.Errorf("BlockedByIssue: want empty, got %q", got.BlockedByIssue)
	}
	if !strings.Contains(got.Body, "Auto-review verdict: needs human") {
		t.Errorf("expected verdict header in body; got:\n%s", got.Body)
	}
	if !strings.Contains(got.Body, "needs product input on scope") {
		t.Errorf("expected summary in body; got:\n%s", got.Body)
	}
	if sink.calls != 0 {
		t.Errorf("sink should not be called for human verdict; calls=%d", sink.calls)
	}
	if _, busy := h.inflight[tk.ID]; busy {
		t.Errorf("inflight should be cleared after onComplete")
	}
}

func TestOnComplete_SybraBugVerdict_FilesIssueAndBlocks(t *testing.T) {
	t.Parallel()
	h, tasks, sink, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Workflow misfire", "Original body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusHumanRequired)}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}

	ag := &agent.Agent{TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{
		Type: "assistant",
		Content: "Diagnosis.\n\n```sybra-verdict\n" + `{
  "decision": "sybra_bug",
  "summary": "verify_commits never re-checked the branch",
  "issue_title": "fix(workflow): verify_commits race",
  "issue_body": "## What\nrace condition",
  "issue_labels": ["workflow", "regression"]
}` + "\n```\n",
	})
	h.inflight[tk.ID] = "agent-2"
	h.onComplete(ag)

	if sink.calls != 1 {
		t.Fatalf("sink calls: got %d want 1", sink.calls)
	}
	if sink.gotTitle != "fix(workflow): verify_commits race" {
		t.Errorf("sink title: got %q", sink.gotTitle)
	}
	if !strings.Contains(sink.gotBody, "race condition") {
		t.Errorf("sink body missing diagnosis: %q", sink.gotBody)
	}
	if len(sink.gotLabels) != 2 || sink.gotLabels[0] != "workflow" {
		t.Errorf("sink labels: got %v", sink.gotLabels)
	}

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if got.Status != task.StatusBlocked {
		t.Errorf("status: got %q want blocked", got.Status)
	}
	if got.BlockedByIssue != sink.url {
		t.Errorf("BlockedByIssue: got %q want %q", got.BlockedByIssue, sink.url)
	}
	if !strings.Contains(got.Body, "Auto-review verdict: blocked by Sybra bug") {
		t.Errorf("expected blocked-by-Sybra-bug header; got:\n%s", got.Body)
	}
	if !strings.Contains(got.Body, sink.url) {
		t.Errorf("expected issue URL in body; got:\n%s", got.Body)
	}
}

func TestOnComplete_SinkError_LeavesHumanRequired(t *testing.T) {
	t.Parallel()
	h, tasks, sink, cleanup := newReviewTestEnv(t)
	defer cleanup()
	sink.err = errors.New("rate limited")

	tk, err := tasks.Create("Whatever", "Body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusHumanRequired)}); err != nil {
		t.Fatalf("flip: %v", err)
	}

	ag := &agent.Agent{TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{
		Type:    "assistant",
		Content: "```sybra-verdict\n" + `{"decision":"sybra_bug","summary":"x","issue_title":"fix(x): y","issue_body":"z"}` + "\n```",
	})
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Errorf("status: got %q want human-required (sink error path)", got.Status)
	}
	if got.BlockedByIssue != "" {
		t.Errorf("BlockedByIssue should be empty on sink error; got %q", got.BlockedByIssue)
	}
	if !strings.Contains(got.Body, "issue submission failed") {
		t.Errorf("expected failure note in body; got:\n%s", got.Body)
	}
}

func TestMaybeSpawn_WorkProject_SkippedNoAgent(t *testing.T) {
	t.Parallel()
	h, tasks, sink, cleanup := newReviewTestEnv(t)
	defer cleanup()

	const workProject = "work-owner/work-repo"
	h.canFilePublic = func(projectID string) bool {
		return projectID != workProject
	}

	tk, err := tasks.Create("Has work content", "Body with work-repo refs.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{
		ProjectID: task.Ptr(workProject),
		Status:    task.Ptr(task.StatusHumanRequired),
	}); err != nil {
		t.Fatalf("assign work project: %v", err)
	}

	// agents and audit are nil in the test env; if maybeSpawn reaches the
	// spawn path it would panic. The guard must short-circuit before that.
	h.maybeSpawn(tk.ID, "in-progress")

	if sink.calls != 0 {
		t.Errorf("sink should not be called for work-typed project; calls=%d", sink.calls)
	}
	if _, busy := h.inflight[tk.ID]; busy {
		t.Errorf("inflight should be empty when work project is skipped")
	}
	if len(h.recent) != 0 {
		t.Errorf("rate-limit slot should not be consumed when work project is skipped; recent=%v", h.recent)
	}
}

func TestOnComplete_MalformedVerdict_AppendsRaw(t *testing.T) {
	t.Parallel()
	h, tasks, sink, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Mystery", "Body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusHumanRequired)}); err != nil {
		t.Fatalf("flip: %v", err)
	}

	ag := &agent.Agent{TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{Type: "result", Content: "no fenced verdict here"})
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Errorf("status: got %q want human-required", got.Status)
	}
	if !strings.Contains(got.Body, "unparseable verdict") {
		t.Errorf("expected unparseable header; got:\n%s", got.Body)
	}
	if sink.calls != 0 {
		t.Errorf("sink should not be called on malformed verdict; calls=%d", sink.calls)
	}
}
