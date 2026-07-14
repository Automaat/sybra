package sybra

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/stats"
	"github.com/Automaat/sybra/internal/task"
)

func TestAggregateTasksDone(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC) // A Monday

	t1Time := now.Add(-1 * time.Hour)       // Today
	t2Time := now.Add(-2 * 24 * time.Hour)  // Last week (Saturday)
	t3Time := now.Add(-5 * 24 * time.Hour)  // Last week (Wednesday)
	t4Time := now.Add(-20 * 24 * time.Hour) // Last month
	t5Time := now.AddDate(0, -2, 0)         // A long time ago

	list := []task.Task{
		{Status: task.StatusDone, UpdatedAt: t1Time},
		{Status: task.StatusDone, UpdatedAt: t2Time}, // closed at will be set
		{Status: task.StatusDone, UpdatedAt: t3Time},
		{Status: task.StatusDone, UpdatedAt: t4Time},
		{Status: task.StatusDone, UpdatedAt: t5Time},
		{Status: task.StatusInProgress, UpdatedAt: now}, // Ignored
	}

	// Make t2 ClosedAt be today, to test ClosedAt taking precedence over UpdatedAt
	t2ClosedAt := now.Add(-2 * time.Hour)
	list[1].ClosedAt = &t2ClosedAt

	var resp stats.StatsResponse
	aggregateTasksDone(&resp, doneTaskClosures(list, "", now), now)

	// AllTime: 5 done tasks
	if resp.AllTime.TasksDone != 5 {
		t.Errorf("AllTime: got %d, want 5", resp.AllTime.TasksDone)
	}

	// Today: t1 and t2
	if resp.Today.TasksDone != 2 {
		t.Errorf("Today: got %d, want 2", resp.Today.TasksDone)
	}

	// ThisWeek (since June 14, Sunday): t1, t2
	if resp.ThisWeek.TasksDone != 2 {
		t.Errorf("ThisWeek: got %d, want 2", resp.ThisWeek.TasksDone)
	}

	// ThisMonth (since June 1): t1, t2, t3
	if resp.ThisMonth.TasksDone != 3 {
		t.Errorf("ThisMonth: got %d, want 3", resp.ThisMonth.TasksDone)
	}
}

func TestClosedTasksDaily(t *testing.T) {
	loc := time.FixedZone("app", 2*60*60)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, loc)
	closedMorning := time.Date(2026, 6, 14, 8, 0, 0, 0, time.UTC)
	closedEvening := time.Date(2026, 6, 14, 18, 0, 0, 0, time.UTC)
	updatedFallback := time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)
	oldUpdated := time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC)
	preferredClosed := time.Date(2026, 6, 15, 9, 0, 0, 0, time.UTC)

	list := []task.Task{
		{Status: task.StatusDone, UpdatedAt: closedMorning},
		{Status: task.StatusDone, UpdatedAt: closedEvening},
		{Status: task.StatusDone, UpdatedAt: updatedFallback},
		{Status: task.StatusDone, UpdatedAt: oldUpdated, ClosedAt: &preferredClosed},
		{Status: task.StatusInProgress, UpdatedAt: now},
	}

	got := closedTasksDaily(doneTaskClosures(list, "", now), now)
	want := []stats.TaskSeriesPoint{
		{Date: "2026-06-13", Count: 1},
		{Date: "2026-06-14", Count: 2},
		{Date: "2026-06-15", Count: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("closedTasksDaily() = %+v, want %+v", got, want)
	}
}

func TestClosedTasksDailyEmpty(t *testing.T) {
	got := closedTasksDaily(nil, time.Now())
	if len(got) != 0 {
		t.Fatalf("closedTasksDaily(nil) = %+v, want empty", got)
	}
}

func TestClosedTasksDailyUsesBackendLocalDateKeys(t *testing.T) {
	loc := time.FixedZone("app", 2*60*60)
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, loc)
	closed := time.Date(2026, 6, 1, 22, 30, 0, 0, time.UTC)
	list := []task.Task{{Status: task.StatusDone, UpdatedAt: time.Date(2026, 6, 1, 1, 0, 0, 0, time.UTC), ClosedAt: &closed}}

	got := closedTasksDaily(doneTaskClosures(list, "", now), now)
	want := []stats.TaskSeriesPoint{{Date: "2026-06-02", Count: 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("closedTasksDaily() = %+v, want %+v", got, want)
	}
}

func TestStatsServiceGetStatsAssignsClosedTasksDaily(t *testing.T) {
	statsStore, err := stats.NewStore(filepath.Join(t.TempDir(), "stats.json"))
	if err != nil {
		t.Fatal(err)
	}
	tasksDir := t.TempDir()
	taskStore, err := task.NewStore(tasksDir)
	if err != nil {
		t.Fatal(err)
	}
	taskMgr := task.NewManager(taskStore, nil)

	closed := time.Now().Add(-1 * time.Hour)
	writeStatsTask(t, tasksDir, task.Task{
		ID:        "done",
		Title:     "done task",
		Status:    task.StatusDone,
		TaskType:  task.TaskTypeNormal,
		AgentMode: task.AgentModeHeadless,
		CreatedAt: closed.Add(-1 * time.Hour),
		UpdatedAt: closed.Add(-30 * time.Minute),
		ClosedAt:  &closed,
	})
	writeStatsTask(t, tasksDir, task.Task{
		ID:        "todo",
		Title:     "todo task",
		Status:    task.StatusTodo,
		TaskType:  task.TaskTypeNormal,
		AgentMode: task.AgentModeHeadless,
		CreatedAt: closed,
		UpdatedAt: closed,
	})

	resp := (&StatsService{stats: statsStore, tasks: taskMgr}).GetStats()
	want := []stats.TaskSeriesPoint{{Date: closed.In(time.Now().Location()).Format(time.DateOnly), Count: 1}}
	if !reflect.DeepEqual(resp.ClosedTasksDaily, want) {
		t.Fatalf("ClosedTasksDaily = %+v, want %+v", resp.ClosedTasksDaily, want)
	}
	if resp.AllTime.TasksDone != 1 {
		t.Fatalf("AllTime.TasksDone = %d, want 1", resp.AllTime.TasksDone)
	}
}

func TestStatsServiceGetStatsCountsAuditDoneTasksMissingFromLiveList(t *testing.T) {
	statsStore, err := stats.NewStore(filepath.Join(t.TempDir(), "stats.json"))
	if err != nil {
		t.Fatal(err)
	}
	tasksDir := t.TempDir()
	taskStore, err := task.NewStore(tasksDir)
	if err != nil {
		t.Fatal(err)
	}
	taskMgr := task.NewManager(taskStore, nil)
	auditDir := t.TempDir()

	now := time.Now()
	liveClosed := now.Add(-1 * time.Hour)
	writeStatsTask(t, tasksDir, task.Task{
		ID:        "live-done",
		Title:     "live done task",
		Status:    task.StatusDone,
		TaskType:  task.TaskTypeNormal,
		AgentMode: task.AgentModeHeadless,
		CreatedAt: liveClosed.Add(-1 * time.Hour),
		UpdatedAt: liveClosed,
		ClosedAt:  &liveClosed,
	})
	deletedClosed := liveClosed.Add(-10 * time.Minute)
	reopenedClosed := liveClosed.Add(-20 * time.Minute)
	writeStatsAuditFile(t, auditDir, deletedClosed.Format(time.DateOnly)+".ndjson", []audit.Event{
		{
			Timestamp: deletedClosed,
			Type:      audit.EventTaskStatusChanged,
			TaskID:    "deleted-done",
			Data:      map[string]any{"from": "in-review", "to": "done"},
		},
		{
			Timestamp: reopenedClosed,
			Type:      audit.EventTaskStatusChanged,
			TaskID:    "reopened",
			Data:      map[string]any{"from": "in-review", "to": "done"},
		},
		{
			Timestamp: reopenedClosed.Add(time.Minute),
			Type:      audit.EventTaskStatusChanged,
			TaskID:    "reopened",
			Data:      map[string]any{"from": "done", "to": "todo"},
		},
	})

	resp := (&StatsService{stats: statsStore, tasks: taskMgr, auditDir: auditDir}).GetStats()
	if resp.AllTime.TasksDone != 2 {
		t.Fatalf("AllTime.TasksDone = %d, want 2 (live done + deleted done, excluding reopened)", resp.AllTime.TasksDone)
	}
	wantDay := liveClosed.In(time.Now().Location()).Format(time.DateOnly)
	if len(resp.ClosedTasksDaily) != 1 || resp.ClosedTasksDaily[0].Date != wantDay || resp.ClosedTasksDaily[0].Count != 2 {
		t.Fatalf("ClosedTasksDaily = %+v, want one %s bucket with 2", resp.ClosedTasksDaily, wantDay)
	}
}

func TestStatsServiceGetStatsKeepsClosedTasksDailyArrayWhenTaskListFails(t *testing.T) {
	statsStore, err := stats.NewStore(filepath.Join(t.TempDir(), "stats.json"))
	if err != nil {
		t.Fatal(err)
	}
	tasksDir := t.TempDir()
	taskStore, err := task.NewStore(tasksDir)
	if err != nil {
		t.Fatal(err)
	}
	taskMgr := task.NewManager(taskStore, nil)
	if err := os.RemoveAll(tasksDir); err != nil {
		t.Fatal(err)
	}

	resp := (&StatsService{stats: statsStore, tasks: taskMgr}).GetStats()
	if resp.ClosedTasksDaily == nil {
		t.Fatal("ClosedTasksDaily is nil, want empty slice")
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"closedTasksDaily":[]`) {
		t.Fatalf("marshaled response = %s, want closedTasksDaily array", data)
	}
}

func TestAggregateByProjectType(t *testing.T) {
	byProject := []stats.GroupedStat{
		{Key: "Automaat/sybra", Stats: stats.Summary{
			TotalCostUSD: 1.0, TotalRuns: 4, TotalDurationS: 200,
			TotalInputTokens: 1000, TotalOutputTokens: 500,
			TotalCacheCreationInputTokens: 300, TotalCacheReadInputTokens: 400,
			TotalPremiumRequests: 2.5,
			AvgCostPerRun:        0.25, AvgDurationS: 50,
		}},
		{Key: "Automaat/zsh-clean-history", Stats: stats.Summary{
			TotalCostUSD: 0.5, TotalRuns: 2, TotalDurationS: 100,
			TotalCacheCreationInputTokens: 30, TotalCacheReadInputTokens: 40,
			TotalPremiumRequests: 1.5,
			AvgCostPerRun:        0.25, AvgDurationS: 50,
		}},
		{Key: "kumahq/kuma", Stats: stats.Summary{
			TotalCostUSD: 3.0, TotalRuns: 6, TotalDurationS: 600,
			AvgCostPerRun: 0.5, AvgDurationS: 100,
		}},
		{Key: "no/such-project", Stats: stats.Summary{
			TotalCostUSD: 0.1, TotalRuns: 1, TotalDurationS: 10,
			AvgCostPerRun: 0.1, AvgDurationS: 10,
		}},
	}
	types := map[string]string{
		"Automaat/sybra":             "pet",
		"Automaat/zsh-clean-history": "pet",
		"kumahq/kuma":                "work",
	}

	out := aggregateByProjectType(byProject, types)

	if len(out) != 3 {
		t.Fatalf("want 3 buckets (pet, work, (unknown)); got %d: %+v", len(out), out)
	}

	// Sorted by cost desc: work (3.0) > pet (1.5) > (unknown) (0.1)
	if out[0].Key != "work" || out[1].Key != "pet" || out[2].Key != "(unknown)" {
		t.Errorf("ordering: got %s,%s,%s; want work,pet,(unknown)", out[0].Key, out[1].Key, out[2].Key)
	}

	pet := find(t, out, "pet")
	if pet.TotalRuns != 6 {
		t.Errorf("pet.totalRuns: got %d, want 6", pet.TotalRuns)
	}
	if !nearly(pet.TotalCostUSD, 1.5) {
		t.Errorf("pet.totalCost: got %f, want 1.5", pet.TotalCostUSD)
	}
	if !nearly(pet.AvgCostPerRun, 1.5/6) {
		t.Errorf("pet.avgCostPerRun: got %f, want %f", pet.AvgCostPerRun, 1.5/6)
	}
	if !nearly(pet.AvgDurationS, 300.0/6) {
		t.Errorf("pet.avgDurationS: got %f, want 50", pet.AvgDurationS)
	}
	if pet.TotalInputTokens != 1000 || pet.TotalOutputTokens != 500 {
		t.Errorf("pet tokens: got in=%d out=%d, want 1000/500", pet.TotalInputTokens, pet.TotalOutputTokens)
	}
	if pet.TotalCacheCreationInputTokens != 330 || pet.TotalCacheReadInputTokens != 440 {
		t.Errorf("pet cache tokens: got write=%d read=%d, want 330/440", pet.TotalCacheCreationInputTokens, pet.TotalCacheReadInputTokens)
	}
	if !nearly(pet.TotalPremiumRequests, 4.0) {
		t.Errorf("pet premium requests: got %f, want 4.0", pet.TotalPremiumRequests)
	}

	work := find(t, out, "work")
	if work.TotalRuns != 6 || !nearly(work.TotalCostUSD, 3.0) {
		t.Errorf("work bucket wrong: %+v", work)
	}

	unknown := find(t, out, "(unknown)")
	if unknown.TotalRuns != 1 {
		t.Errorf("unknown.totalRuns: got %d, want 1", unknown.TotalRuns)
	}
}

func TestAggregateByProjectType_EmptyMap(t *testing.T) {
	byProject := []stats.GroupedStat{
		{Key: "a/b", Stats: stats.Summary{TotalCostUSD: 1, TotalRuns: 1, TotalDurationS: 10}},
	}
	out := aggregateByProjectType(byProject, map[string]string{})
	if len(out) != 1 || out[0].Key != "(unknown)" {
		t.Fatalf("expected single (unknown) bucket; got %+v", out)
	}
}

func TestAggregateByProjectType_SkipsZeroBuckets(t *testing.T) {
	out := aggregateByProjectType(nil, map[string]string{"a/b": "pet"})
	if len(out) != 0 {
		t.Errorf("empty input → empty output; got %+v", out)
	}
}

func find(t *testing.T, gs []stats.GroupedStat, key string) stats.Summary {
	t.Helper()
	for _, g := range gs {
		if g.Key == key {
			return g.Stats
		}
	}
	t.Fatalf("no bucket with key %q in %+v", key, gs)
	return stats.Summary{}
}

func writeStatsTask(t *testing.T, dir string, tk task.Task) {
	t.Helper()
	data, err := task.Marshal(tk)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, tk.ID+".md"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeStatsAuditFile(t *testing.T, dir, name string, events []audit.Event) {
	t.Helper()
	var data []byte
	for _, ev := range events {
		line, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, line...)
		data = append(data, '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func nearly(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
