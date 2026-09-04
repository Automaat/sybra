package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/attribution"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/taskstatus"
)

type stubThreadFetcher struct {
	threads []github.ReviewThread
	err     error
}

func (s stubThreadFetcher) FetchReviewThreads(context.Context, string, int) ([]github.ReviewThread, error) {
	return s.threads, s.err
}

func TestUntouchedBriefedThreads(t *testing.T) {
	t.Parallel()

	briefed := []BriefedReviewThread{
		{ID: "t1", Comments: 1},
		{ID: "t2", Comments: 1},
	}

	tests := []struct {
		name string
		live []github.ReviewThread
		want []string
	}{
		{
			name: "a reply grows the comment count",
			live: []github.ReviewThread{
				{ID: "t1", CommentCount: 2},
				{ID: "t2", CommentCount: 2},
			},
		},
		{
			name: "resolved counts as answered even with no new comment",
			live: []github.ReviewThread{
				{ID: "t1", CommentCount: 1, IsResolved: true},
				{ID: "t2", CommentCount: 1, IsResolved: true},
			},
		},
		{
			name: "outdated counts as answered, the anchored code moved",
			live: []github.ReviewThread{
				{ID: "t1", CommentCount: 1, IsOutdated: true},
				{ID: "t2", CommentCount: 1, IsOutdated: true},
			},
		},
		{
			name: "a vanished thread is not held against the run",
			live: []github.ReviewThread{{ID: "t1", CommentCount: 2}},
		},
		{
			name: "an unanswered thread keeps its brief-time comment count",
			live: []github.ReviewThread{
				{ID: "t1", CommentCount: 1},
				{ID: "t2", CommentCount: 1},
			},
			want: []string{"t1", "t2"},
		},
		{
			name: "a partially answered set reports only the untouched ones",
			live: []github.ReviewThread{
				{ID: "t1", CommentCount: 2},
				{ID: "t2", CommentCount: 1},
			},
			want: []string{"t2"},
		},
		{
			// A reviewer posting again mid-run restores the brief-time last
			// author, which an author-identity check would misread as "the
			// agent never replied". The count only grows, so it does not.
			name: "a reviewer follow-up after the reply still counts as answered",
			live: []github.ReviewThread{
				{ID: "t1", CommentCount: 3, LastAuthorLogin: "reviewer"},
				{ID: "t2", CommentCount: 3, LastAuthorLogin: "reviewer"},
			},
		},
		{
			// A reviewer deleting their own comment shrinks the count. The
			// thread still moved, so it must not be held against the run.
			name: "a deleted comment shrinks the count and still counts as answered",
			live: []github.ReviewThread{
				{ID: "t1", CommentCount: 0},
				{ID: "t2", CommentCount: 0},
			},
		},
		{
			name: "an empty live set answers nothing and blames nothing",
			live: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := untouchedBriefedThreads(briefed, tc.live)
			var ids []string
			for _, g := range got {
				ids = append(ids, g.ID)
			}
			if strings.Join(ids, ",") != strings.Join(tc.want, ",") {
				t.Errorf("untouched = %v, want %v", ids, tc.want)
			}
		})
	}
}

func TestIsDeferralReply(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "an applied reply passes",
			body: "**Applied** — injected a stepping clock (abc1234).",
		},
		{
			name: "an invalid reply passes, disagreement is not deferral",
			body: "**Skipped (invalid)** — already at store.go:42. Happy to revisit if I'm reading this wrong.",
		},
		{
			name: "an answer with evidence passes",
			body: "**Answered** — the nil case cannot happen; the caller checks at api.go:88.",
		},
		{
			name: "a deferred call is Go vocabulary, not a deferral",
			body: "**Applied** — the handle is now closed in a deferred call (abc1234).",
		},
		{
			name: "a separate process is not a separate PR",
			body: "**Applied** — moved the spawn into a separate process group (abc1234).",
		},
		{
			name: "a separate printf is not a separate PR",
			body: "**Applied** — split the log line into a separate printf (abc1234).",
		},
		{
			name: "its own prompt is not its own PR",
			body: "**Applied** — the runbook now renders its own prompt (abc1234).",
		},
		{
			name: "another problem is not another PR",
			body: "**Answered** — that is another problem entirely; api.go:88 already guards it. No change needed.",
		},
		{
			name: "a value deferred by design is not a deferral of the fix",
			body: "**Answered** — the value is deferred until first use by design; store.go:42.",
		},
		{
			name: "a follow-up promise is a deferral",
			body: "Valid point, but deferred: happy to pick this up as a follow-up.",
			want: true,
		},
		{
			name: "a separate-PR promise is a deferral",
			body: "Agreed, though this belongs in a separate PR.",
			want: true,
		},
		{
			name: "leaving it as-is for now is a deferral",
			body: "Real trade-off. Leaving the sleep as-is for now.",
			want: true,
		},
		{
			name: "left as-is for now is the same deferral in past tense",
			body: "Left the sleep as-is for now; the refactor is bigger than this PR.",
			want: true,
		},
		{
			name: "picking it up separately is a deferral",
			body: "Agreed — happy to pick this up separately.",
			want: true,
		},
		{
			name: "a follow-up issue is a deferral",
			body: "Good catch. I'll open a follow-up issue for this.",
			want: true,
		},
		{
			name: "out of scope is a deferral",
			body: "Out of scope here, but noted.",
			want: true,
		},
		{
			name: "a subsequent PR is a deferral",
			body: "Fair point. I'll do this in a subsequent PR.",
			want: true,
		},
		{
			name: "deferring to a later PR is a deferral",
			body: "Agreed; deferring to a later PR.",
			want: true,
		},
		{
			name: "not in this change is a deferral",
			body: "Not in this change — filing a ticket instead.",
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := tc.body + "\n\n" + attribution.Footer
			if got := isDeferralReply(body); got != tc.want {
				t.Errorf("isDeferralReply(%q) = %v, want %v", body, got, tc.want)
			}
		})
	}
}

func TestDeferredBriefedThreads(t *testing.T) {
	t.Parallel()

	const agent = "sybra-bot"
	briefed := []BriefedReviewThread{{ID: "t1", Comments: 1}, {ID: "t2", Comments: 1}}
	deferral := "Agreed, deferred to a follow-up.\n\n" + attribution.Footer
	applied := "**Applied** — done in abc1234.\n\n" + attribution.Footer

	tests := []struct {
		name      string
		login     string
		untouched []BriefedReviewThread
		live      []github.ReviewThread
		want      []string
	}{
		{
			name:  "fixed threads are not deferrals",
			login: agent,
			live: []github.ReviewThread{
				{ID: "t1", CommentCount: 2, LastAuthorLogin: agent, LastCommentBody: applied},
				{ID: "t2", CommentCount: 2, LastAuthorLogin: agent, LastCommentBody: applied},
			},
		},
		{
			name:  "a resolved thread is the reviewer's own call",
			login: agent,
			live: []github.ReviewThread{
				{ID: "t1", CommentCount: 2, IsResolved: true, LastAuthorLogin: agent, LastCommentBody: deferral},
			},
		},
		{
			name:  "an outdated thread means the anchored code moved",
			login: agent,
			live: []github.ReviewThread{
				{ID: "t1", CommentCount: 2, IsOutdated: true, LastAuthorLogin: agent, LastCommentBody: deferral},
			},
		},
		{
			// Sybra's own review agent stamps the harness footer on the review
			// comments it writes, so on a PR reviewed by another instance the
			// reviewer's text carries it too. Only the login separates them.
			name:  "a reviewer's own comment is never this run's deferral",
			login: agent,
			live: []github.ReviewThread{
				{ID: "t1", CommentCount: 2, LastAuthorLogin: "reviewer", LastCommentBody: deferral},
			},
		},
		{
			name: "an unknown login leaves the run unverified rather than parking it",
			live: []github.ReviewThread{
				{ID: "t1", CommentCount: 2, LastAuthorLogin: agent, LastCommentBody: deferral},
			},
		},
		{
			// Already reported as unanswered. Counting it again would name one
			// thread as two and call an unanswered thread answered.
			name:      "an unanswered thread is not also a deferral",
			login:     agent,
			untouched: []BriefedReviewThread{{ID: "t1", Comments: 1}},
			live: []github.ReviewThread{
				{ID: "t1", CommentCount: 1, LastAuthorLogin: agent, LastCommentBody: deferral},
			},
		},
		{
			name:  "a deferral reply is held against the run",
			login: agent,
			live: []github.ReviewThread{
				{ID: "t1", CommentCount: 2, LastAuthorLogin: agent, LastCommentBody: applied},
				{ID: "t2", CommentCount: 2, LastAuthorLogin: agent, LastCommentBody: deferral, Path: "internal/b.go", Line: 40},
			},
			want: []string{"t2"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var ids []string
			for _, g := range deferredBriefedThreads(briefed, tc.untouched, tc.live, tc.login) {
				ids = append(ids, g.ID)
			}
			if strings.Join(ids, ",") != strings.Join(tc.want, ",") {
				t.Errorf("deferred = %v, want %v", ids, tc.want)
			}
		})
	}
}

// A thread the agent replied to counts as answered by comment count alone, so
// without the deferral floor a run that conceded every point and changed
// nothing reached the reviewer as a finished fix.
func TestExecVerifyReviewThreads_DeferralParksTheTask(t *testing.T) {
	t.Parallel()

	const agent = "sybra-bot"
	deferral := "Valid point, but deferred — happy to pick this up as a follow-up.\n\n" + attribution.Footer
	tasks := newMemTasks()
	info := TaskInfo{ID: "task-1", Status: taskstatus.InProgress, PRNumber: 7, ProjectID: "o/r"}
	tasks.Put(info)
	engine := newEngineForEval(t, tasks)
	engine.pr.ThreadFetcher = stubThreadFetcher{threads: []github.ReviewThread{
		{ID: "t1", CommentCount: 2, LastAuthorLogin: agent, LastCommentBody: "**Applied** — done.\n\n" + attribution.Footer},
		{ID: "t2", CommentCount: 2, LastAuthorLogin: agent, LastCommentBody: deferral, Path: "internal/b.go", Line: 40},
	}}

	vars := map[string]string{
		PRReviewAgentLoginVar: agent,
		PRReviewThreadBriefVar: MarshalBriefedReviewThreads([]BriefedReviewThread{
			{ID: "t1", Comments: 1},
			{ID: "t2", Comments: 1},
		}),
	}
	step := &Step{ID: "verify_review_threads", Type: StepVerifyReviewThreads}
	if _, err := engine.execVerifyReviewThreads("task-1", step, &Execution{Variables: vars}, info); err != nil {
		t.Fatalf("execVerifyReviewThreads: %v", err)
	}

	got, _ := tasks.GetTask("task-1")
	if got.Status != taskstatus.HumanRequired {
		t.Fatalf("status = %q, want %q", got.Status, taskstatus.HumanRequired)
	}
	reason := tasks.reasons["task-1"]
	for _, want := range []string{"1 of 2", "deferral", "internal/b.go:40"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q missing %q", reason, want)
		}
	}
}

func TestExecVerifyReviewThreads(t *testing.T) {
	t.Parallel()

	briefed := MarshalBriefedReviewThreads([]BriefedReviewThread{
		{ID: "t1", Comments: 1},
		{ID: "t2", Comments: 1},
	})

	tests := []struct {
		name       string
		vars       map[string]string
		prNumber   int
		projectID  string
		live       []github.ReviewThread
		fetchErr   error
		wantStatus taskstatus.Status
		wantOutput string
	}{
		{
			name:       "no brief skips, which is every non-comments fix",
			vars:       map[string]string{},
			prNumber:   7,
			projectID:  "o/r",
			wantStatus: taskstatus.InProgress,
			wantOutput: "skipped: no review threads briefed",
		},
		{
			name:       "missing pr skips",
			vars:       map[string]string{PRReviewThreadBriefVar: briefed},
			projectID:  "o/r",
			wantStatus: taskstatus.InProgress,
			wantOutput: "skipped: missing pr or project",
		},
		{
			name:       "a fetch failure never parks the run",
			vars:       map[string]string{PRReviewThreadBriefVar: briefed},
			prNumber:   7,
			projectID:  "o/r",
			fetchErr:   errors.New("rate limited"),
			wantStatus: taskstatus.InProgress,
			wantOutput: "skipped: fetch failed",
		},
		{
			name:      "answered threads let the fix continue",
			vars:      map[string]string{PRReviewThreadBriefVar: briefed},
			prNumber:  7,
			projectID: "o/r",
			live: []github.ReviewThread{
				{ID: "t1", CommentCount: 2},
				{ID: "t2", CommentCount: 2},
			},
			wantStatus: taskstatus.InProgress,
			wantOutput: "review threads answered",
		},
		{
			name:      "unanswered threads park the task instead of looping",
			vars:      map[string]string{PRReviewThreadBriefVar: briefed},
			prNumber:  7,
			projectID: "o/r",
			live: []github.ReviewThread{
				{ID: "t1", CommentCount: 2},
				{ID: "t2", CommentCount: 1, Path: "internal/a.go", Line: 12},
			},
			wantStatus: taskstatus.HumanRequired,
			wantOutput: "unanswered review threads",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tasks := newMemTasks()
			info := TaskInfo{ID: "task-1", Status: taskstatus.InProgress, PRNumber: tc.prNumber, ProjectID: tc.projectID}
			tasks.Put(info)
			engine := newEngineForEval(t, tasks)
			engine.pr.ThreadFetcher = stubThreadFetcher{threads: tc.live, err: tc.fetchErr}

			step := &Step{ID: "verify_review_threads", Type: StepVerifyReviewThreads}
			out, err := engine.execVerifyReviewThreads("task-1", step, &Execution{Variables: tc.vars}, info)
			if err != nil {
				t.Fatalf("execVerifyReviewThreads: %v", err)
			}
			if !strings.Contains(out.Output, tc.wantOutput) {
				t.Errorf("output = %q, want it to contain %q", out.Output, tc.wantOutput)
			}
			got, _ := tasks.GetTask("task-1")
			if got.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", got.Status, tc.wantStatus)
			}
		})
	}
}

// The parked reason must name which feedback was dropped, not only how much:
// a bare count sends the operator back to the PR to diff it by hand.
func TestExecVerifyReviewThreads_ReasonNamesTheDroppedThreads(t *testing.T) {
	t.Parallel()

	fetcher := stubThreadFetcher{threads: []github.ReviewThread{
		{ID: "t1", CommentCount: 1, Path: "internal/a.go", Line: 12},
		{ID: "t2", CommentCount: 1, Path: "internal/b.go", Line: 40},
	}}

	tasks := newMemTasks()
	info := TaskInfo{ID: "task-1", Status: taskstatus.InProgress, PRNumber: 7, ProjectID: "o/r"}
	tasks.Put(info)
	engine := newEngineForEval(t, tasks)
	engine.pr.ThreadFetcher = fetcher

	vars := map[string]string{PRReviewThreadBriefVar: MarshalBriefedReviewThreads([]BriefedReviewThread{
		{ID: "t1", Comments: 1},
		{ID: "t2", Comments: 1},
	})}
	step := &Step{ID: "verify_review_threads", Type: StepVerifyReviewThreads}
	if _, err := engine.execVerifyReviewThreads("task-1", step, &Execution{Variables: vars}, info); err != nil {
		t.Fatalf("execVerifyReviewThreads: %v", err)
	}

	reason := tasks.reasons["task-1"]
	for _, want := range []string{"2 of 2", "internal/a.go:12", "internal/b.go:40"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q missing %q", reason, want)
		}
	}
}

func TestBriefedReviewThreadsRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []BriefedReviewThread
		raw  string
		want int
	}{
		{name: "empty marshals to empty so the step's skip check stays a plain test"},
		{name: "round trip", in: []BriefedReviewThread{{ID: "t1", Comments: 2}}, want: 1},
		{name: "malformed decodes to nothing to verify", raw: "{not json", want: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw := tc.raw
			if raw == "" {
				raw = MarshalBriefedReviewThreads(tc.in)
			}
			if got := len(UnmarshalBriefedReviewThreads(raw)); got != tc.want {
				t.Errorf("decoded %d threads, want %d", got, tc.want)
			}
		})
	}
}

// A brief persisted before BriefedReviewThread carried a comment count decodes
// to Comments: 0. Every live thread has at least one comment, so a workflow
// that spanned the deploy goes unverified instead of parking outright.
func TestUntouchedBriefedThreads_PreUpgradeBriefFailsOpen(t *testing.T) {
	t.Parallel()

	briefed := UnmarshalBriefedReviewThreads(`[{"id":"t1","last_author":"reviewer"}]`)
	if len(briefed) != 1 || briefed[0].Comments != 0 {
		t.Fatalf("old shape decoded unexpectedly: %+v", briefed)
	}
	live := []github.ReviewThread{{ID: "t1", CommentCount: 1, LastAuthorLogin: "reviewer"}}
	if got := untouchedBriefedThreads(briefed, live); len(got) != 0 {
		t.Errorf("pre-upgrade brief parked the run: %+v", got)
	}
}

// The resolved-on-remote escape hatch exists so a human is not parked for a PR
// that already landed. The gate must not undo it.
func TestExecVerifyReviewThreads_SkipsALandedPR(t *testing.T) {
	t.Parallel()

	fetcher := stubThreadFetcher{threads: []github.ReviewThread{
		{ID: "t1", CommentCount: 1, Path: "internal/a.go", Line: 12},
	}}

	tasks := newMemTasks()
	info := TaskInfo{ID: "task-1", Status: taskstatus.InReview, PRNumber: 7, ProjectID: "o/r"}
	tasks.Put(info)
	engine := newEngineForEval(t, tasks)
	engine.pr.ThreadFetcher = fetcher
	engine.pr.StateFetcher = stubPRStateFetcher{state: github.PRState{State: "MERGED"}}

	vars := map[string]string{PRReviewThreadBriefVar: MarshalBriefedReviewThreads(
		[]BriefedReviewThread{{ID: "t1", Comments: 1}})}
	step := &Step{ID: "verify_noop_review_threads", Type: StepVerifyReviewThreads}
	out, err := engine.execVerifyReviewThreads("task-1", step, &Execution{Variables: vars}, info)
	if err != nil {
		t.Fatalf("execVerifyReviewThreads: %v", err)
	}
	if !strings.Contains(out.Output, "already merged") {
		t.Errorf("output = %q, want the merged-PR skip", out.Output)
	}
	got, _ := tasks.GetTask("task-1")
	if got.Status != taskstatus.InReview {
		t.Errorf("status = %q, want a merged PR left in-review", got.Status)
	}
}

type stubPRStateFetcher struct{ state github.PRState }

func (s stubPRStateFetcher) FetchPRState(string, int) (github.PRState, error) {
	return s.state, nil
}

// PRState.Resolved() is MERGED || ReadyToMerge(), so an open PR with green CI
// and no conflicts reads as "resolved" - and that is the ordinary shape of a
// PR under review. Skipping on it would leave the gate inert on exactly the
// runs it exists to catch, since the comments dispatch keys on unanswered
// threads alone and does not care about mergeability or CI.
func TestExecVerifyReviewThreads_GreenOpenPRIsStillChecked(t *testing.T) {
	t.Parallel()

	fetcher := stubThreadFetcher{threads: []github.ReviewThread{
		{ID: "t1", CommentCount: 1, Path: "internal/a.go", Line: 12},
	}}

	tasks := newMemTasks()
	info := TaskInfo{ID: "task-1", Status: taskstatus.InProgress, PRNumber: 7, ProjectID: "o/r"}
	tasks.Put(info)
	engine := newEngineForEval(t, tasks)
	engine.pr.ThreadFetcher = fetcher
	engine.pr.StateFetcher = stubPRStateFetcher{state: github.PRState{State: "OPEN", Mergeable: "MERGEABLE"}}

	vars := map[string]string{PRReviewThreadBriefVar: MarshalBriefedReviewThreads(
		[]BriefedReviewThread{{ID: "t1", Comments: 1}})}
	step := &Step{ID: "verify_review_threads", Type: StepVerifyReviewThreads}
	out, err := engine.execVerifyReviewThreads("task-1", step, &Execution{Variables: vars}, info)
	if err != nil {
		t.Fatalf("execVerifyReviewThreads: %v", err)
	}
	if !strings.Contains(out.Output, "unanswered review threads") {
		t.Errorf("output = %q, want the green open PR to still be checked", out.Output)
	}
	got, _ := tasks.GetTask("task-1")
	if got.Status != taskstatus.HumanRequired {
		t.Errorf("status = %q, want human-required", got.Status)
	}
}
