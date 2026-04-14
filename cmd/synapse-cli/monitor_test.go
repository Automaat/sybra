package main

import (
	"strings"
	"testing"

	"github.com/Automaat/synapse/internal/monitor"
	"github.com/Automaat/synapse/internal/task"
)

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

func TestMonitorScanDetectsUntriagedAndOverDispatch(t *testing.T) {
	setupStore(t)

	// Plant 4 in-progress tasks (over the default DispatchLimit of 3) plus
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
	createHeadless("untriaged todo", task.StatusTodo, nil)
	createHeadless("triaged todo", task.StatusTodo, []string{"medium"})

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
