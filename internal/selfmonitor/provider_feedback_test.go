package selfmonitor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/synapse/internal/health"
	"github.com/Automaat/synapse/internal/task"
)

// stubProviderGate records calls to ReportRateLimit for assertion.
type stubProviderGate struct {
	calls []providerRateLimitCall
}

type providerRateLimitCall struct {
	provider string
	after    time.Duration
	reason   string
}

func (s *stubProviderGate) IsHealthy(_ string) bool       { return true }
func (s *stubProviderGate) Failover(_ string) string      { return "" }
func (s *stubProviderGate) Reason(_ string) string        { return "" }
func (s *stubProviderGate) ReportAuthFailure(_, _ string) {}
func (s *stubProviderGate) ReportRateLimit(provider string, after time.Duration, reason string) {
	s.calls = append(s.calls, providerRateLimitCall{provider: provider, after: after, reason: reason})
}

// overloadedFixtureLines returns a Codex-format NDJSON stream that produces
// an overloaded_error class. ClaudeResult.ErrorType is NOT populated from
// Claude stream-json (extractResultFieldsTyped doesn't map it), so we use
// the Codex envelope format where "type":"error" carries error_type directly.
func overloadedFixtureLines() []string {
	return []string{
		`{"type":"thread.started","thread_id":"t1"}`,
		`{"type":"error","error_type":"overloaded_error","message":"API overloaded","code":529}`,
	}
}

func writeFixtureInAgentsDir(t *testing.T, logsDir, agentID string, lines []string) string {
	t.Helper()
	agentsDir := filepath.Join(logsDir, "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("mkdir agents: %v", err)
	}
	path := filepath.Join(agentsDir, agentID+"-2026-04-14T10-00-00.ndjson")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestProviderFeedback_ReportsRateLimitForOverloadedError(t *testing.T) {
	logsDir := t.TempDir()
	logPath := writeFixtureInAgentsDir(t, logsDir, "agent-overload", overloadedFixtureLines())

	gate := &stubProviderGate{}
	rep := &health.Report{
		Findings: []health.Finding{{
			Category:    health.CatAgentRetryLoop,
			Fingerprint: "agent_retry_loop:task-ol",
			TaskID:      "task-ol",
			AgentID:     "agent-overload",
			LogFile:     logPath,
		}},
	}

	svc := NewService(Deps{
		Health:       &stubHealth{Report: rep},
		ProviderGate: gate,
	})

	_, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if len(gate.calls) != 1 {
		t.Fatalf("ReportRateLimit calls = %d, want 1", len(gate.calls))
	}
	call := gate.calls[0]
	if call.provider != "claude" {
		t.Errorf("provider = %q, want claude", call.provider)
	}
	if call.after != 15*time.Minute {
		t.Errorf("retryAfter = %v, want 15m", call.after)
	}
	if !strings.Contains(call.reason, "overloaded_error") {
		t.Errorf("reason = %q, want to mention overloaded_error", call.reason)
	}
}

func TestProviderFeedback_UsesTaskRunProviderWhenAvailable(t *testing.T) {
	logsDir := t.TempDir()
	logPath := writeFixtureInAgentsDir(t, logsDir, "agent-codex", overloadedFixtureLines())

	gate := &stubProviderGate{}
	tasks := &stubTasks{
		byID: map[string]task.Task{
			"task-codex": {
				ID: "task-codex",
				AgentRuns: []task.AgentRun{
					{AgentID: "agent-codex", Provider: "codex", LogFile: logPath},
				},
			},
		},
	}
	rep := &health.Report{
		Findings: []health.Finding{{
			Category:    health.CatAgentRetryLoop,
			Fingerprint: "agent_retry_loop:task-codex",
			TaskID:      "task-codex",
			AgentID:     "agent-codex",
			LogFile:     logPath,
		}},
	}

	svc := NewService(Deps{
		Health:       &stubHealth{Report: rep},
		Tasks:        tasks,
		ProviderGate: gate,
	})

	_, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if len(gate.calls) != 1 {
		t.Fatalf("ReportRateLimit calls = %d, want 1", len(gate.calls))
	}
	if gate.calls[0].provider != "codex" {
		t.Errorf("provider = %q, want codex", gate.calls[0].provider)
	}
}

func TestProviderFeedback_SkipsNonRetryLoopCategory(t *testing.T) {
	categories := []health.Category{
		health.CatCostOutlier,
		health.CatStuckTask,
		health.CatTriageMismatch,
		health.CatFailureRate,
	}

	for _, cat := range categories {
		t.Run(string(cat), func(t *testing.T) {
			logPath := writeFixture(t, overloadedFixtureLines())

			gate := &stubProviderGate{}
			rep := &health.Report{
				Findings: []health.Finding{{
					Category:    cat,
					Fingerprint: string(cat) + ":t1",
					TaskID:      "t1",
					LogFile:     logPath,
				}},
			}

			svc := NewService(Deps{
				Health:       &stubHealth{Report: rep},
				ProviderGate: gate,
			})

			_, err := svc.Scan(context.Background())
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}

			if len(gate.calls) != 0 {
				t.Errorf("ReportRateLimit called for category %q, want skipped", cat)
			}
		})
	}
}

func TestProviderFeedback_SkipsWhenNoMatchingErrorClass(t *testing.T) {
	// permission_denied error: should not trigger provider feedback.
	logPath := writeFixture(t, fixtureLines()) // fixture has permission_denied

	gate := &stubProviderGate{}
	rep := &health.Report{
		Findings: []health.Finding{{
			Category:    health.CatAgentRetryLoop,
			Fingerprint: "agent_retry_loop:t1",
			TaskID:      "t1",
			LogFile:     logPath,
		}},
	}

	svc := NewService(Deps{
		Health:       &stubHealth{Report: rep},
		ProviderGate: gate,
	})

	_, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if len(gate.calls) != 0 {
		t.Errorf("ReportRateLimit called for permission_denied error class, want skipped")
	}
}

func TestProviderFeedback_SkipsWhenProviderGateNil(t *testing.T) {
	logPath := writeFixture(t, overloadedFixtureLines())

	rep := &health.Report{
		Findings: []health.Finding{{
			Category:    health.CatAgentRetryLoop,
			Fingerprint: "agent_retry_loop:t1",
			TaskID:      "t1",
			LogFile:     logPath,
		}},
	}

	svc := NewService(Deps{
		Health:       &stubHealth{Report: rep},
		ProviderGate: nil,
	})

	// Must not panic or error.
	_, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan with nil ProviderGate: %v", err)
	}
}

func TestProviderFeedback_SkipsWhenLogSummaryNil(t *testing.T) {
	// No LogFile, no AgentID → no LogSummary.
	gate := &stubProviderGate{}
	rep := &health.Report{
		Findings: []health.Finding{{
			Category:    health.CatAgentRetryLoop,
			Fingerprint: "agent_retry_loop:t1",
			TaskID:      "t1",
		}},
	}

	svc := NewService(Deps{
		Health:       &stubHealth{Report: rep},
		ProviderGate: gate,
	})

	_, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if len(gate.calls) != 0 {
		t.Errorf("ReportRateLimit called with nil LogSummary, want skipped")
	}
}
