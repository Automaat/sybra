package clusterlead

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"slices"

	"github.com/Automaat/sybra/internal/task"
)

// A follower's task id reaches writeSidecars — which builds eight paths from
// it — before Put's own ValidateID runs, and followers are peers reachable
// over HTTP. Rejecting on ingest is what makes a hostile id write nothing.
func TestApplyFollowerTaskRejectsUnsafeIDsWithoutWriting(t *testing.T) {
	t0 := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		id   string
	}{
		{name: "parent traversal", id: "../escape"},
		{name: "nested traversal", id: "../../etc/passwd"},
		{name: "forward separator", id: "a/b"},
		{name: "backslash separator", id: `a\b`},
		{name: "absolute path", id: "/etc/passwd"},
		{name: "empty", id: ""},
		{name: "dot", id: "."},
		{name: "dot dot", id: ".."},
		{name: "leading dash", id: "-rf"},
		{name: "over the length cap", id: strings.Repeat("a", 65)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := leaderConfig("http://unused", []string{"owner/pet"})
			roster, _ := NewRoster(cfg, nil)
			mgr := newManager(t)
			dir := mgr.Store().Dir()
			mirror := NewMirror(cfg, mgr, roster, nil, time.Second)

			before := treeSnapshot(t, dir)

			if mirror.applyFollowerTask("pet-box", task.Task{
				ID: tt.id, Title: "hostile", Status: task.StatusTodo,
				ProjectID: "owner/pet", UpdatedAt: t0,
				Plan: "plan body", CodeReview: "review body",
			}) {
				t.Fatalf("applyFollowerTask accepted id %q", tt.id)
			}

			// Compare the whole listing, not its length: a regression that
			// removed one path and added another would balance out.
			if after := treeSnapshot(t, dir); !slices.Equal(after, before) {
				t.Fatalf("id %q changed the tasks tree:\nbefore=%v\nafter=%v", tt.id, before, after)
			}
		})
	}
}

// A well-formed id still applies, so the guard is a filter and not a wall.
func TestApplyFollowerTaskStillAdoptsValidIDs(t *testing.T) {
	cfg := leaderConfig("http://unused", []string{"owner/pet"})
	roster, _ := NewRoster(cfg, nil)
	mgr := newManager(t)
	mirror := NewMirror(cfg, mgr, roster, nil, time.Second)

	if !mirror.applyFollowerTask("pet-box", task.Task{
		ID: "task-abc123", Title: "legit", Status: task.StatusTodo,
		ProjectID: "owner/pet", UpdatedAt: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
	}) {
		t.Fatal("a valid follower task must still adopt")
	}
	if _, err := mgr.Get("task-abc123"); err != nil {
		t.Fatalf("adopted task not readable: %v", err)
	}
}

// treeSnapshot lists every path under the tasks dir and its parent, so a write
// that escapes upward is visible rather than silently outside the walk.
func treeSnapshot(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	root := filepath.Dir(dir)
	if err := filepath.WalkDir(root, func(p string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		out = append(out, p)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}
