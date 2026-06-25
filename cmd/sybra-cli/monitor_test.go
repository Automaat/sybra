package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/monitor"
	"github.com/Automaat/sybra/internal/task"
)

// backdateCreatedAt rewrites a task file's created_at so detectUntriaged's
// creation grace period does not exempt it — used by tests that assert
// untriaged detection rather than the grace window itself.
func backdateCreatedAt(t *testing.T, home, id string, age time.Duration) {
	t.Helper()
	p := filepath.Join(home, "tasks", id+".md")
	tk, err := task.Parse(p)
	if err != nil {
		t.Fatalf("parse %s: %v", id, err)
	}
	tk.CreatedAt = time.Now().Add(-age).UTC()
	data, err := task.Marshal(tk)
	if err != nil {
		t.Fatalf("marshal %s: %v", id, err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", id, err)
	}
}

func TestMonitorScanEmptyBoard(t *testing.T) {
	setupStore(t)

	code, out := runCLI(t, "--json", "monitor", "scan")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var report monitor.Report
	mustUnmarshal(t, out, &report)
	if len(report.Anomalies) != 0 {
		t.Errorf("clean board should report 0 anomalies, got %d", len(report.Anomalies))
	}
	if report.Counts.Todo != 0 || report.Counts.InProgress != 0 {
		t.Errorf("unexpected counts: %+v", report.Counts)
	}
}

// writeConfig writes a config.yaml into the test's SYBRA_HOME so a scan picks
// up explicit overrides instead of falling back to packaged defaults.
func writeConfig(t *testing.T, home, yaml string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func TestMonitorScanDetectsUntriagedAndOverDispatch(t *testing.T) {
	dir := setupStore(t)

	// Pin the dispatch limit so this test is independent of the default
	// agent concurrency (which is far above the 4 planted tasks).
	writeConfig(t, dir, "monitor:\n  dispatch_limit: 3\n")

	// Plant 4 in-progress tasks (over the pinned DispatchLimit of 3) plus
	// an untriaged todo task. Tags are empty on one; a second has a full
	// triage so only the first triggers untriaged. The in-progress tasks
	// get the "medium" tag + headless mode so they do not also trip
	// untriaged.
	createHeadless := func(title string, status task.Status, tags []string) string {
		args := []string{"--json", "create", "--title", title}
		if len(tags) > 0 {
			args = append(args, "--tags", strings.Join(tags, ","))
		}
		code, out := runCLI(t, args...)
		if code != 0 {
			t.Fatalf("create %q: exit %d: %s", title, code, out)
		}
		var t0 task.Task
		mustUnmarshal(t, out, &t0)
		if status != task.StatusTodo {
			code, out = runCLI(t, "--json", "update", t0.ID, "--status", string(status))
			if code != 0 {
				t.Fatalf("update %q: exit %d: %s", t0.ID, code, out)
			}
		}
		return t0.ID
	}

	createHeadless("in-progress a", task.StatusInProgress, []string{"medium"})
	createHeadless("in-progress b", task.StatusInProgress, []string{"medium"})
	createHeadless("in-progress c", task.StatusInProgress, []string{"medium"})
	createHeadless("in-progress d", task.StatusInProgress, []string{"medium"})
	untriagedID := createHeadless("untriaged todo", task.StatusTodo, nil)
	createHeadless("triaged todo", task.StatusTodo, []string{"medium"})

	// detectUntriaged exempts freshly created tasks; backdate this one past
	// the grace window so it is still flagged (this test asserts detection).
	backdateCreatedAt(t, dir, untriagedID, 30*time.Minute)

	code, out := runCLI(t, "--json", "monitor", "scan")
	if code != 0 {
		t.Fatalf("scan exit %d: %s", code, out)
	}
	var report monitor.Report
	mustUnmarshal(t, out, &report)

	if report.Counts.InProgress != 4 {
		t.Errorf("counts.inProgress: want 4, got %d", report.Counts.InProgress)
	}
	if report.Counts.Todo != 2 {
		t.Errorf("counts.todo: want 2, got %d", report.Counts.Todo)
	}

	gotKinds := make(map[monitor.AnomalyKind]int)
	for _, a := range report.Anomalies {
		gotKinds[a.Kind]++
	}
	if gotKinds[monitor.KindOverDispatchLimit] != 1 {
		t.Errorf("want 1 over_dispatch_limit, got %d", gotKinds[monitor.KindOverDispatchLimit])
	}
	// Four lost_agent hits too (in-progress without any live agent in CLI
	// process — CLI scan has no agentLister). Untriaged count: only the
	// "untriaged todo" row trips it; the triaged todo has both tags and
	// mode set.
	if gotKinds[monitor.KindUntriaged] != 1 {
		t.Errorf("want 1 untriaged, got %d (all kinds: %v)", gotKinds[monitor.KindUntriaged], gotKinds)
	}
	if gotKinds[monitor.KindLostAgent] != 4 {
		t.Errorf("want 4 lost_agent (CLI scan has no live-agent signal), got %d", gotKinds[monitor.KindLostAgent])
	}
}

func TestMonitorScanHumanSummaryLine(t *testing.T) {
	setupStore(t)
	code, out := runCLI(t, "monitor", "scan")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.HasPrefix(out, "monitor: ") {
		t.Errorf("expected 'monitor:' prefix, got: %q", out)
	}
	if !strings.Contains(out, "drift=0") {
		t.Errorf("expected drift=0 on empty board, got: %q", out)
	}
}

func TestMonitorScanUnknownSubcommand(t *testing.T) {
	setupStore(t)
	code, out := runCLIStderr(t, "--json", "monitor", "bogus")
	if code == 0 {
		t.Fatalf("expected non-zero exit for unknown subcommand, got %d (out=%q)", code, out)
	}
}

func TestMonitorScanNoArgsFails(t *testing.T) {
	setupStore(t)
	code, _ := runCLIStderr(t, "monitor")
	if code == 0 {
		t.Fatal("expected non-zero exit with no subcommand")
	}
}

// runCLIStderr mirrors runCLI but only the exit code matters for error
// assertions; stderr is suppressed during the test run.
func runCLIStderr(t *testing.T, args ...string) (exitCode int, stdout string) {
	t.Helper()
	return runCLI(t, args...)
}
