package workflow

import "testing"

func TestDeclaresAlreadyFixedOnMain(t *testing.T) {
	for _, tc := range []struct {
		name             string
		signal           string
		wantAlreadyFixed bool
		wantDeclared     bool
	}{
		{
			name:             "fenced already fixed",
			signal:           "Checked the base branch.\n\n```sybra-recovery\n{\"decision\":\"already-fixed-on-main\",\"reason\":\"landed in an earlier PR\"}\n```",
			wantAlreadyFixed: true,
			wantDeclared:     true,
		},
		{
			name:             "fenced none",
			signal:           "```sybra-recovery\n{\"decision\":\"none\"}\n```",
			wantAlreadyFixed: false,
			wantDeclared:     true,
		},
		{
			name:             "last fenced block wins over a quoted contract",
			signal:           "The contract asks for:\n\n```sybra-recovery\n{\"decision\":\"already-fixed-on-main\",\"reason\":\"example\"}\n```\n\nI made no such finding:\n\n```sybra-recovery\n{\"decision\":\"none\"}\n```",
			wantAlreadyFixed: false,
			wantDeclared:     true,
		},
		{
			name:             "indented fence still parses",
			signal:           "  ```sybra-recovery\n  {\"decision\":\"already-fixed-on-main\",\"reason\":\"already on base\"}\n  ```",
			wantAlreadyFixed: true,
			wantDeclared:     true,
		},
		{
			name:             "bare json object on the status-reason path",
			signal:           `{"decision":"already-fixed-on-main","reason":"duplicate of an earlier task"}`,
			wantAlreadyFixed: true,
			wantDeclared:     true,
		},
		{
			name:             "affirmative prose is not a declaration",
			signal:           "Already fixed on main; no PR needed. Duplicate task, safe to close/mark done.",
			wantAlreadyFixed: false,
			wantDeclared:     false,
		},
		{
			name:             "negated prose is not a declaration",
			signal:           "I checked whether this was already fixed on main; it is NOT. Ran out of context before committing, parking for a human.",
			wantAlreadyFixed: false,
			wantDeclared:     false,
		},
		{
			name:             "incidental main.go mention is not a declaration",
			signal:           "Already fixed the nil deref in main.go and pushed the branch.",
			wantAlreadyFixed: false,
			wantDeclared:     false,
		},
		{
			name:             "unrelated json is not a declaration",
			signal:           "Ran the checks and got {\"passed\": true, \"failed\": 0}.",
			wantAlreadyFixed: false,
			wantDeclared:     false,
		},
		{
			name:             "empty",
			signal:           "",
			wantAlreadyFixed: false,
			wantDeclared:     false,
		},
		{
			name:             "whitespace",
			signal:           " \n\t ",
			wantAlreadyFixed: false,
			wantDeclared:     false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			alreadyFixed, declared, err := declaresAlreadyFixedOnMain(tc.signal)
			if err != nil {
				t.Fatalf("declaresAlreadyFixedOnMain(%q) returned error: %v", tc.signal, err)
			}
			if declared != tc.wantDeclared {
				t.Fatalf("declaresAlreadyFixedOnMain(%q) declared = %v, want %v", tc.signal, declared, tc.wantDeclared)
			}
			if alreadyFixed != tc.wantAlreadyFixed {
				t.Fatalf("declaresAlreadyFixedOnMain(%q) alreadyFixed = %v, want %v", tc.signal, alreadyFixed, tc.wantAlreadyFixed)
			}
		})
	}
}

func TestDeclaresAlreadyFixedOnMainUnreadableDeclaration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		signal string
	}{
		{
			name:   "malformed json",
			signal: "```sybra-recovery\n{\"decision\": already-fixed-on-main}\n```",
		},
		{
			name:   "unknown decision",
			signal: "```sybra-recovery\n{\"decision\":\"close-it\"}\n```",
		},
		{
			name:   "missing decision",
			signal: "```sybra-recovery\n{\"reason\":\"it is a duplicate\"}\n```",
		},
		{
			name:   "verbatim prompt placeholder is rejected",
			signal: "```sybra-recovery\n{\"decision\": \"<already-fixed-on-main|none>\", \"reason\": \"<one sentence>\"}\n```",
		},
		{
			name:   "unreadable declaration outranks corroborating prose",
			signal: "Already fixed on main, safe to close.\n\n```sybra-recovery\n{\"decision\":\n```",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			alreadyFixed, declared, err := declaresAlreadyFixedOnMain(tc.signal)
			if err == nil {
				t.Fatalf("declaresAlreadyFixedOnMain(%q) = (%v, %v), want an error", tc.signal, alreadyFixed, declared)
			}
			if alreadyFixed || declared {
				t.Fatalf("declaresAlreadyFixedOnMain(%q) = (%v, %v) on error, want (false, false)", tc.signal, alreadyFixed, declared)
			}
		})
	}
}

func TestRecoveryVerdictReason(t *testing.T) {
	signal := "```sybra-recovery\n{\"decision\":\"already-fixed-on-main\",\"reason\":\"landed in an earlier PR\"}\n```"
	if got := recoveryVerdictReason(signal); got != "landed in an earlier PR" {
		t.Fatalf("recoveryVerdictReason = %q, want %q", got, "landed in an earlier PR")
	}
	if got := recoveryVerdictReason("no declaration here"); got != "" {
		t.Fatalf("recoveryVerdictReason = %q, want empty", got)
	}
}
