package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/health"
	"github.com/Automaat/sybra/internal/selfmonitor"
)

// claudeFixtureLines mirrors the hand-written NDJSON stream the service
// package's test fixture uses: three identical Bash calls followed by two
// more to trip the stall detector, one tool_result is_error=true for the
// permission_denied class, and a result event with cost + rate_limit.
// Duplicated here to keep the CLI e2e test package self-contained rather
// than reaching into the selfmonitor package's internal test helpers.
func claudeFixtureLines() []string {
	return []string{
		`{"type":"system","subtype":"init","session_id":"s1"}`,
		`{"type":"assistant","session_id":"s1","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls /tmp"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"file1\nfile2"}]}}`,
		`{"type":"assistant","session_id":"s1","message":{"content":[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"ls /tmp"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t2","content":"permission denied: /tmp/secret","is_error":true}]}}`,
		`{"type":"assistant","session_id":"s1","message":{"content":[{"type":"tool_use","id":"t3","name":"Bash","input":{"command":"ls /tmp"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t3","content":"file1\nfile2"}]}}`,
		`{"type":"assistant","session_id":"s1","message":{"content":[{"type":"tool_use","id":"t4","name":"Bash","input":{"command":"ls /tmp"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t4","content":"file1\nfile2"}]}}`,
		`{"type":"assistant","session_id":"s1","message":{"content":[{"type":"tool_use","id":"t5","name":"Bash","input":{"command":"ls /tmp"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t5","content":"file1\nfile2"}]}}`,
		`{"type":"result","subtype":"error","session_id":"s1","total_cost_usd":0.42,"total_input_tokens":1200,"total_output_tokens":340}`,
	}
}

// writeHealthFile stages a health report under SYBRA_HOME at the path
// config.HealthReportPath() resolves to. Used by every CLI e2e test so the
// investigate subcommand has something for DiskHealthReader to consume.
func writeHealthFile(t *testing.T, home string, rep health.Report) {
	t.Helper()
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal health: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, "health-report.json"), data, 0o644); err != nil {
		t.Fatalf("write health: %v", err)
	}
}

func writeAgentLog(t *testing.T, home, agentID string, lines []string) string {
	t.Helper()
	dir := filepath.Join(home, "logs", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, agentID+"-2026-04-14T10-00-00.ndjson")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	return path
}

// seedConfigForLogs writes a config.yaml that points logging.dir at the
// test home's logs directory so cfg.Logging.Dir is set when the CLI loads
// the config. Without this, cfg.Logging.Dir defaults to the real user
// ~/.sybra/logs and the FindLogFile glob fallback points at the wrong
// place.
func seedConfigForLogs(t *testing.T, home string) {
	t.Helper()
	content := `logging:
  dir: ` + filepath.Join(home, "logs") + `
`
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// TestE2ECLIInvestigateDistillsLog walks the full `investigate` round-trip:
// stage a health report + agent log under SYBRA_HOME, run the CLI, and
// verify the JSON report carries the distilled LogSummary. This is the
// end-to-end contract the Claude Code triage skill depends on.
func TestE2ECLIInvestigateDistillsLog(t *testing.T) {
	home := setupStore(t)
	seedConfigForLogs(t, home)

	logPath := writeAgentLog(t, home, "agent-cli", claudeFixtureLines())

	writeHealthFile(t, home, health.Report{
		GeneratedAt: time.Now().UTC(),
		Score:       health.ScoreWarning,
		Findings: []health.Finding{{
			Category:    health.CatCostOutlier,
			Severity:    health.SeverityWarning,
			Title:       "eval agent cost $0.65",
			Fingerprint: "cost_outlier:task-cli",
			TaskID:      "task-cli",
			AgentID:     "agent-cli",
			LogFile:     logPath,
		}},
	})

	code, out := runCLI(t, "--json", "selfmonitor", "investigate")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}

	var rep selfmonitor.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(rep.Findings) != 1 {
		t.Fatalf("Findings = %d, want 1", len(rep.Findings))
	}
	inv := rep.Findings[0]
	if inv.LogSummary == nil {
		t.Fatal("LogSummary = nil, want distilled summary from CLI investigate")
	}
	if inv.LogSummary.TotalToolCalls != 5 {
		t.Errorf("TotalToolCalls = %d, want 5", inv.LogSummary.TotalToolCalls)
	}
	if !inv.LogSummary.StallDetected {
		t.Error("StallDetected = false, want true after 5 identical Bash calls")
	}
}

// TestE2ECLIScanRoundTripsPersistedReport covers the read-only `scan`
// subcommand: pre-populate the last-report.json on disk and verify the
// CLI parses and returns the same Report shape.
func TestE2ECLIScanRoundTripsPersistedReport(t *testing.T) {
	home := setupStore(t)
	dir := filepath.Join(home, "selfmonitor")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	want := selfmonitor.Report{
		SchemaVersion: selfmonitor.ReportSchemaVersion,
		HealthScore:   health.ScoreCritical,
		Suppressed:    2,
		DurationMS:    150,
		Findings: []selfmonitor.InvestigatedFinding{{
			Fingerprint: "cost_outlier:task-persisted",
			Finding: health.Finding{
				Category:    health.CatCostOutlier,
				Severity:    health.SeverityWarning,
				Title:       "persisted",
				Fingerprint: "cost_outlier:task-persisted",
			},
			Verdict: selfmonitor.Verdict{Classification: selfmonitor.VerdictPending},
		}},
	}
	data, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "last-report.json"), data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	code, out := runCLI(t, "--json", "selfmonitor", "scan")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var got selfmonitor.Report
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.HealthScore != health.ScoreCritical {
		t.Errorf("HealthScore = %q, want critical", got.HealthScore)
	}
	if got.Suppressed != 2 {
		t.Errorf("Suppressed = %d, want 2", got.Suppressed)
	}
	if len(got.Findings) != 1 || got.Findings[0].Fingerprint != "cost_outlier:task-persisted" {
		t.Errorf("Findings = %+v, want 1 persisted finding", got.Findings)
	}
}

// TestE2ECLILedgerRoundTrip stages ledger entries on disk, then asks the
// CLI for them filtered by fingerprint. Exercises the Open → History path
// the ledger subcommand relies on, without touching any real ~/.sybra.
func TestE2ECLILedgerRoundTrip(t *testing.T) {
	home := setupStore(t)

	ledgerPath := filepath.Join(home, "selfmonitor", "ledger.jsonl")
	ledger, err := selfmonitor.Open(ledgerPath)
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}

	fp := "cost_outlier:task-ledger"
	rows := []selfmonitor.LedgerEntry{
		{Fingerprint: fp, Verdict: selfmonitor.VerdictPending, CreatedAt: time.Now().UTC().Add(-2 * time.Hour)},
		{Fingerprint: fp, Verdict: selfmonitor.VerdictFalsePositive, CreatedAt: time.Now().UTC().Add(-1 * time.Hour)},
		{Fingerprint: "other:task-b", Verdict: selfmonitor.VerdictConfirmed, CreatedAt: time.Now().UTC()},
	}
	for _, r := range rows {
		if err := ledger.Append(r); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	code, out := runCLI(t, "--json", "selfmonitor", "ledger", "--fingerprint", fp)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var entries []selfmonitor.LedgerEntry
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	for _, e := range entries {
		if e.Fingerprint != fp {
			t.Errorf("leaked unrelated entry: %+v", e)
		}
	}
}

// TestE2ECLILedgerSinceFilter covers the --since duration parsing branch
// (including the "Nd" day suffix) by seeding rows with controlled
// timestamps and asking the CLI to filter them.
func TestE2ECLILedgerSinceFilter(t *testing.T) {
	home := setupStore(t)
	ledgerPath := filepath.Join(home, "selfmonitor", "ledger.jsonl")
	ledger, err := selfmonitor.Open(ledgerPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	fp := "stuck_task:task-since"
	now := time.Now().UTC()
	rows := []selfmonitor.LedgerEntry{
		{Fingerprint: fp, Verdict: selfmonitor.VerdictPending, CreatedAt: now.Add(-10 * 24 * time.Hour)},
		{Fingerprint: fp, Verdict: selfmonitor.VerdictConfirmed, CreatedAt: now.Add(-1 * time.Hour)},
	}
	for _, r := range rows {
		if err := ledger.Append(r); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	code, out := runCLI(t, "--json", "selfmonitor", "ledger", "--fingerprint", fp, "--since", "2d")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var entries []selfmonitor.LedgerEntry
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1 (only the recent row)", len(entries))
	}
	if entries[0].Verdict != selfmonitor.VerdictConfirmed {
		t.Errorf("Verdict = %q, want confirmed", entries[0].Verdict)
	}
}

func TestE2ECLILedgerUnfilteredSinceIncludesNonIssues(t *testing.T) {
	home := setupStore(t)
	ledgerPath := filepath.Join(home, "selfmonitor", "ledger.jsonl")
	ledger, err := selfmonitor.Open(ledgerPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	now := time.Now().UTC()
	rows := []selfmonitor.LedgerEntry{
		{
			Fingerprint: "old:task",
			Verdict:     selfmonitor.VerdictPending,
			CreatedAt:   now.Add(-72 * time.Hour),
		},
		{
			Fingerprint: "fresh-non-issue:task",
			Verdict:     selfmonitor.VerdictConfirmed,
			CreatedAt:   now.Add(-2 * time.Hour),
		},
		{
			Fingerprint: "fresh-open-issue:task",
			Verdict:     selfmonitor.VerdictNeedsHuman,
			IssueNumber: 42,
			IssueState:  "open",
			CreatedAt:   now.Add(-1 * time.Hour),
		},
	}
	for _, r := range rows {
		if err := ledger.Append(r); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	code, out := runCLI(t, "--json", "selfmonitor", "ledger", "--since", "24h")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var entries []selfmonitor.LedgerEntry
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2 recent rows", len(entries))
	}
	if entries[0].Fingerprint != "fresh-non-issue:task" {
		t.Fatalf("entries[0] = %q, want fresh non-issue row", entries[0].Fingerprint)
	}
	if entries[1].Fingerprint != "fresh-open-issue:task" {
		t.Fatalf("entries[1] = %q, want fresh open issue row", entries[1].Fingerprint)
	}
}

// TestE2ECLIInvestigateSuppressionReadsLedger wires a seeded ledger into
// the CLI investigate path: the finding should be counted as Suppressed
// instead of emitted, proving the CLI shares the same ledger file the
// background service uses.
func TestE2ECLIInvestigateSuppressionReadsLedger(t *testing.T) {
	home := setupStore(t)
	seedConfigForLogs(t, home)

	ledgerPath := filepath.Join(home, "selfmonitor", "ledger.jsonl")
	ledger, err := selfmonitor.Open(ledgerPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	fp := "cost_outlier:task-suppressed"
	for range 3 {
		if err := ledger.Append(selfmonitor.LedgerEntry{
			Fingerprint: fp,
			Verdict:     selfmonitor.VerdictFalsePositive,
			CreatedAt:   time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	writeHealthFile(t, home, health.Report{
		Findings: []health.Finding{{
			Category:    health.CatCostOutlier,
			Fingerprint: fp,
			TaskID:      "task-suppressed",
		}},
	})

	code, out := runCLI(t, "--json", "selfmonitor", "investigate")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var rep selfmonitor.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(rep.Findings) != 0 {
		t.Errorf("suppressed finding leaked through CLI: %+v", rep.Findings)
	}
	if rep.Suppressed != 1 {
		t.Errorf("Suppressed = %d, want 1", rep.Suppressed)
	}
}

// TestE2ECLIInvestigateNoHealthReportSoft guards the "fresh install" path:
// if there's no health-report.json yet, the CLI should still succeed with
// an empty Report rather than erroring out.
func TestE2ECLIInvestigateNoHealthReportSoft(t *testing.T) {
	setupStore(t)

	code, out := runCLI(t, "--json", "selfmonitor", "investigate")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var rep selfmonitor.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(rep.Findings) != 0 {
		t.Errorf("Findings = %d, want 0 on missing health report", len(rep.Findings))
	}
	if rep.SchemaVersion != selfmonitor.ReportSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", rep.SchemaVersion, selfmonitor.ReportSchemaVersion)
	}
}

// TestE2ECLILedgerBadSinceFails covers the error path where --since fails
// to parse. The CLI should exit non-zero with a helpful message, not panic
// or return an empty slice.
func TestE2ECLILedgerBadSinceFails(t *testing.T) {
	setupStore(t)
	code, _ := runCLI(t, "--json", "selfmonitor", "ledger", "--since", "garbage")
	if code == 0 {
		t.Error("expected non-zero exit for bad --since")
	}
}
