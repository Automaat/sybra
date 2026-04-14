package selfmonitor

import (
	"context"
	"errors"
	"testing"

	"github.com/Automaat/synapse/internal/health"
	"github.com/Automaat/synapse/internal/task"
)

// stubJudge satisfies the Judge interface for unit tests.
type stubJudge struct {
	verdict Verdict
	err     error
}

func (s *stubJudge) Judge(_ context.Context, _ health.Finding, _ *LogSummary, _ *task.Task) (Verdict, error) {
	return s.verdict, s.err
}

func TestJudge_FillsVerdictWhenLogSummaryAvailable(t *testing.T) {
	j := &stubJudge{
		verdict: Verdict{
			Classification: VerdictConfirmed,
			RootCause:      "tool loop on Bash",
			Confidence:     0.9,
		},
	}

	logsDir := t.TempDir()
	logPath := writeFixture(t, fixtureLines())

	rep := &health.Report{
		Score: health.ScoreWarning,
		Findings: []health.Finding{{
			Category:    health.CatCostOutlier,
			Fingerprint: "cost_outlier:task-judge",
			TaskID:      "task-judge",
			LogFile:     logPath,
		}},
	}

	svc := NewService(Deps{
		Health:  &stubHealth{Report: rep},
		LogsDir: logsDir,
		Judge:   j,
	})

	r, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(r.Findings) != 1 {
		t.Fatalf("Findings = %d, want 1", len(r.Findings))
	}
	inv := r.Findings[0]
	if inv.Verdict.Classification != VerdictConfirmed {
		t.Errorf("Classification = %q, want confirmed", inv.Verdict.Classification)
	}
	if inv.Verdict.RootCause != "tool loop on Bash" {
		t.Errorf("RootCause = %q, want 'tool loop on Bash'", inv.Verdict.RootCause)
	}
}

func TestJudge_KeepsVerdictPendingOnError(t *testing.T) {
	j := &stubJudge{err: errors.New("claude unavailable")}

	logPath := writeFixture(t, fixtureLines())

	rep := &health.Report{
		Findings: []health.Finding{{
			Category:    health.CatCostOutlier,
			Fingerprint: "cost_outlier:task-judge-err",
			LogFile:     logPath,
		}},
	}

	svc := NewService(Deps{
		Health: &stubHealth{Report: rep},
		Judge:  j,
	})

	r, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(r.Findings) != 1 {
		t.Fatalf("Findings = %d, want 1", len(r.Findings))
	}
	if r.Findings[0].Verdict.Classification != VerdictPending {
		t.Errorf("Classification = %q, want pending after judge error", r.Findings[0].Verdict.Classification)
	}
}

func TestJudge_SkippedWhenNoLogSummary(t *testing.T) {
	called := false
	j := &stubJudge{verdict: Verdict{Classification: VerdictConfirmed}}
	_ = j

	// Wrap stubJudge to detect calls.
	spyJudge := &spyCallJudge{inner: j, onCall: func() { called = true }}

	rep := &health.Report{
		Findings: []health.Finding{{
			Category:    health.CatStuckTask,
			Fingerprint: "stuck_task:task-no-log",
			// No LogFile, no AgentID → no LogSummary.
		}},
	}

	svc := NewService(Deps{
		Health: &stubHealth{Report: rep},
		Judge:  spyJudge,
	})

	r, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if called {
		t.Error("Judge called when LogSummary is nil, want skipped")
	}
	if r.Findings[0].Verdict.Classification != VerdictPending {
		t.Errorf("Classification = %q, want pending when judge skipped", r.Findings[0].Verdict.Classification)
	}
}

// spyCallJudge wraps a Judge and calls onCall before delegating.
type spyCallJudge struct {
	inner  Judge
	onCall func()
}

func (s *spyCallJudge) Judge(ctx context.Context, f health.Finding, ls *LogSummary, t *task.Task) (Verdict, error) {
	s.onCall()
	return s.inner.Judge(ctx, f, ls, t)
}

func TestParseJudgeVerdict_ExtractsLastJSONObject(t *testing.T) {
	tests := []struct {
		name          string
		raw           []byte
		wantClass     string
		wantRootCause string
		wantErr       bool
	}{
		{
			name:          "clean JSON in result",
			raw:           []byte(`{"result":"{\"classification\":\"confirmed\",\"rootCause\":\"tool loop\",\"confidence\":0.85}"}`),
			wantClass:     VerdictConfirmed,
			wantRootCause: "tool loop",
		},
		{
			name:          "JSON preceded by prose",
			raw:           []byte(`{"result":"Analysis follows.\n{\"classification\":\"false_positive\",\"rootCause\":\"proportional cost\"}"}`),
			wantClass:     VerdictFalsePositive,
			wantRootCause: "proportional cost",
		},
		{
			name:    "empty result field",
			raw:     []byte(`{"result":""}`),
			wantErr: true,
		},
		{
			name:    "malformed envelope",
			raw:     []byte(`not json`),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := parseJudgeVerdict(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseJudgeVerdict: want error, got nil (verdict=%+v)", v)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseJudgeVerdict: %v", err)
			}
			if v.Classification != tt.wantClass {
				t.Errorf("Classification = %q, want %q", v.Classification, tt.wantClass)
			}
			if v.RootCause != tt.wantRootCause {
				t.Errorf("RootCause = %q, want %q", v.RootCause, tt.wantRootCause)
			}
		})
	}
}

func TestJudgeExtractLastJSON_ReturnsLastObject(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "single object",
			input: `{"a":"b"}`,
			want:  `{"a":"b"}`,
		},
		{
			name:  "prose then object",
			input: `some text {"classification":"confirmed"}`,
			want:  `{"classification":"confirmed"}`,
		},
		{
			name:  "two objects, returns last",
			input: `{"first":1} more text {"second":2}`,
			want:  `{"second":2}`,
		},
		{
			name:  "no object",
			input: `plain text only`,
			want:  "",
		},
		{
			name:  "nested object",
			input: `{"outer":{"inner":1}}`,
			want:  `{"outer":{"inner":1}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := judgeExtractLastJSON(tt.input)
			if got != tt.want {
				t.Errorf("judgeExtractLastJSON(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
