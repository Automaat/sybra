package clusterlead

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/cluster"
	"github.com/Automaat/sybra/internal/task"
)

func TestMergeAuthoritySplit(t *testing.T) {
	t0 := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)

	canonical := task.Task{
		ID:           "task-1",
		ProjectID:    "owner/repo",
		AssignedNode: "pet-box",
		Title:        "leader ingested title",
		Issue:        "https://github.com/owner/repo/issues/9",
		TodoistID:    "td-1",
		Status:       task.StatusTodo,
		CreatedAt:    t0,
		UpdatedAt:    t0,
		MirrorRev:    4,
	}
	follower := task.Task{
		ID:           "task-1",
		ProjectID:    "attacker/evil",
		AssignedNode: "somewhere-else",
		Title:        "follower renamed it",
		Issue:        "https://github.com/attacker/evil/issues/1",
		Status:       task.StatusInReview,
		StatusReason: "review drafted",
		Branch:       "feat/x",
		WorktreeDir:  "/wt/task-1",
		PRNumber:     42,
		Reviewed:     true,
		Outcome:      "merged",
		Plan:         "the plan",
		CodeReview:   "the review",
		UpdatedAt:    t1,
	}

	out, ok := Merge(canonical, follower)
	if !ok {
		t.Fatal("newer follower state must apply")
	}

	if out.ProjectID != "owner/repo" || out.AssignedNode != "pet-box" ||
		out.Title != "leader ingested title" || out.Issue != canonical.Issue ||
		out.TodoistID != "td-1" || !out.CreatedAt.Equal(t0) {
		t.Errorf("identity fields must stay leader-authoritative: %+v", out)
	}

	if out.Status != task.StatusInReview || out.StatusReason != "review drafted" ||
		out.Branch != "feat/x" || out.WorktreeDir != "/wt/task-1" || out.PRNumber != 42 ||
		!out.Reviewed || out.Outcome != "merged" || out.Plan != "the plan" || out.CodeReview != "the review" {
		t.Errorf("execution fields must be follower-authoritative: %+v", out)
	}

	if out.MirrorRev != 5 {
		t.Errorf("MirrorRev = %d, want 5 (bumped once)", out.MirrorRev)
	}
	if out.MirrorUpdatedAt == nil || !out.MirrorUpdatedAt.Equal(t1) {
		t.Errorf("MirrorUpdatedAt must track the follower clock (%v), got %v", t1, out.MirrorUpdatedAt)
	}
}

func TestMergeDropsStaleAndOutOfOrder(t *testing.T) {
	t0 := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	applied := t0.Add(2 * time.Hour)
	canonical := task.Task{ID: "task-1", AssignedNode: "n", Status: task.StatusInReview, MirrorRev: 7, MirrorUpdatedAt: &applied}

	cases := []struct {
		name              string
		followerUpdatedAt time.Time
		wantOK            bool
	}{
		{"older than applied is dropped", applied.Add(-time.Hour), false},
		{"equal to applied is dropped", applied, false},
		{"newer than applied applies", applied.Add(time.Minute), true},
	}
	for _, c := range cases {
		follower := task.Task{ID: "task-1", AssignedNode: "n", Status: task.StatusTodo, UpdatedAt: c.followerUpdatedAt}
		out, ok := Merge(canonical, follower)
		if ok != c.wantOK {
			t.Errorf("%s: ok = %v, want %v", c.name, ok, c.wantOK)
		}
		if !ok {
			if out.MirrorRev != 7 || out.Status != task.StatusInReview {
				t.Errorf("%s: a dropped update must not mutate canonical: %+v", c.name, out)
			}
		} else if out.MirrorRev != 8 {
			t.Errorf("%s: applied MirrorRev = %d, want 8", c.name, out.MirrorRev)
		}
	}
}

func TestMergeFirstApplyWhenNeverMirrored(t *testing.T) {
	canonical := task.Task{ID: "task-1", AssignedNode: "n", Status: task.StatusTodo, MirrorRev: 0}
	follower := task.Task{ID: "task-1", AssignedNode: "n", Status: task.StatusInProgress, UpdatedAt: time.Unix(1, 0)}
	out, ok := Merge(canonical, follower)
	if !ok {
		t.Fatal("first-ever mirror (nil MirrorUpdatedAt) must apply")
	}
	if out.MirrorRev != 1 || out.Status != task.StatusInProgress {
		t.Errorf("first apply = %+v, want rev 1 + in-progress", out)
	}
}

func TestTaskIDFromEvent(t *testing.T) {
	cases := []struct {
		name      string
		eventName string
		eventData string
		want      string
	}{
		{"task path", "task:updated", "/data/sybra/tasks/abc123.md", "abc123"},
		{"quoted path", "task:created", `"/x/y/def456.md"`, "def456"},
		{"non-task event", "agent:state", "/x/abc.md", ""},
		{"empty", "task:updated", "", ""},
		{"traversal basename resolves to basename", "task:updated", "/x/../y.md", "y"},
	}
	for _, c := range cases {
		got := taskIDFromEvent(cluster.Event{Name: c.eventName, Data: c.eventData})
		if got != c.want {
			t.Errorf("%s: taskIDFromEvent = %q, want %q", c.name, got, c.want)
		}
	}
}
