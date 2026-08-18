package workflow

import (
	"errors"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/taskstatus"
)

// swapReviewThreadFetch points the step at a canned thread set for one test.
// It writes a package-level seam, so a test using it must not call
// t.Parallel() - concurrent tests would overwrite each other's stub.
func swapReviewThreadFetch(t *testing.T, threads []github.ReviewThread, err error) {
	t.Helper()
	prev := fetchReviewThreads
	fetchReviewThreads = func(string, int) ([]github.ReviewThread, error) { return threads, err }
	t.Cleanup(func() { fetchReviewThreads = prev })
}

func TestUntouchedBriefedThreads(t *testing.T) {
	t.Parallel()

	briefed := []BriefedReviewThread{
		{ID: "t1", LastAuthor: "reviewer"},
		{ID: "t2", LastAuthor: "reviewer"},
	}

	tests := []struct {
		name string
		live []github.ReviewThread
		want []string
	}{
		{
			name: "answered threads carry a new last author",
			live: []github.ReviewThread{
				{ID: "t1", LastAuthorLogin: "harness"},
				{ID: "t2", LastAuthorLogin: "harness"},
			},
		},
		{
			name: "resolved counts as answered even with the original last author",
			live: []github.ReviewThread{
				{ID: "t1", LastAuthorLogin: "reviewer", IsResolved: true},
				{ID: "t2", LastAuthorLogin: "reviewer", IsResolved: true},
			},
		},
		{
			name: "outdated counts as answered, the anchored code moved",
			live: []github.ReviewThread{
				{ID: "t1", LastAuthorLogin: "reviewer", IsOutdated: true},
				{ID: "t2", LastAuthorLogin: "reviewer", IsOutdated: true},
			},
		},
		{
			name: "a vanished thread is not held against the run",
			live: []github.ReviewThread{{ID: "t1", LastAuthorLogin: "harness"}},
		},
		{
			name: "untouched threads keep their brief-time last author",
			live: []github.ReviewThread{
				{ID: "t1", LastAuthorLogin: "reviewer"},
				{ID: "t2", LastAuthorLogin: "reviewer"},
			},
			want: []string{"t1", "t2"},
		},
		{
			name: "a partially answered set reports only the untouched ones",
			live: []github.ReviewThread{
				{ID: "t1", LastAuthorLogin: "harness"},
				{ID: "t2", LastAuthorLogin: "reviewer"},
			},
			want: []string{"t2"},
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

func TestExecVerifyReviewThreads(t *testing.T) {
	briefed := MarshalBriefedReviewThreads([]BriefedReviewThread{
		{ID: "t1", LastAuthor: "reviewer"},
		{ID: "t2", LastAuthor: "reviewer"},
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
				{ID: "t1", LastAuthorLogin: "harness"},
				{ID: "t2", LastAuthorLogin: "harness"},
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
				{ID: "t1", LastAuthorLogin: "harness"},
				{ID: "t2", LastAuthorLogin: "reviewer", Path: "internal/a.go", Line: 12},
			},
			wantStatus: taskstatus.HumanRequired,
			wantOutput: "unanswered review threads",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			swapReviewThreadFetch(t, tc.live, tc.fetchErr)

			tasks := newMemTasks()
			info := TaskInfo{ID: "task-1", Status: taskstatus.InProgress, PRNumber: tc.prNumber, ProjectID: tc.projectID}
			tasks.Put(info)
			engine := newEngineForEval(t, tasks)

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
	swapReviewThreadFetch(t, []github.ReviewThread{
		{ID: "t1", LastAuthorLogin: "reviewer", Path: "internal/a.go", Line: 12},
		{ID: "t2", LastAuthorLogin: "reviewer", Path: "internal/b.go", Line: 40},
	}, nil)

	tasks := newMemTasks()
	info := TaskInfo{ID: "task-1", Status: taskstatus.InProgress, PRNumber: 7, ProjectID: "o/r"}
	tasks.Put(info)
	engine := newEngineForEval(t, tasks)

	vars := map[string]string{PRReviewThreadBriefVar: MarshalBriefedReviewThreads([]BriefedReviewThread{
		{ID: "t1", LastAuthor: "reviewer"},
		{ID: "t2", LastAuthor: "reviewer"},
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
		{name: "round trip", in: []BriefedReviewThread{{ID: "t1", LastAuthor: "a"}}, want: 1},
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
