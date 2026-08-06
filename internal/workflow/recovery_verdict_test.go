package workflow

import "testing"

// quotedFenceDisclaimed is the shape that defeated every extraction-based
// attempt: the agent echoes a filled-in declaration from the task body and
// then says in prose that it is not making one.
const quotedFenceDisclaimed = "I read the task body, which contains the contract example:\n\n" +
	"```sybra-recovery\n{\"decision\": \"already-fixed-on-main\", \"reason\": \"the change is already on the base branch\"}\n```\n\n" +
	"I am NOT declaring that. I made no commits and the change is NOT on main."

func TestDeclaresAlreadyFixedOnMain(t *testing.T) {
	for _, tc := range []struct {
		name             string
		signal           string
		wantAlreadyFixed bool
		wantDeclared     bool
	}{
		{
			name:             "whole-string declaration",
			signal:           `{"decision":"already-fixed-on-main","reason":"landed in an earlier PR"}`,
			wantAlreadyFixed: true,
			wantDeclared:     true,
		},
		{
			name:             "whole-string declaration with surrounding whitespace",
			signal:           "  \n{\"decision\":\"already-fixed-on-main\",\"reason\":\"landed earlier\"}\n ",
			wantAlreadyFixed: true,
			wantDeclared:     true,
		},
		{
			name:             "whole-string none",
			signal:           `{"decision":"none"}`,
			wantAlreadyFixed: false,
			wantDeclared:     true,
		},
		{
			name:             "affirmative prose is not a declaration",
			signal:           alreadyFixedOnMainProse,
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
			name:             "json quoted inside prose is not a declaration",
			signal:           `Per the contract I would emit {"decision": "already-fixed-on-main", "reason": "change already on base"} only if the work were already landed. It is not landed, so I am NOT declaring that.`,
			wantAlreadyFixed: false,
			wantDeclared:     false,
		},
		{
			name:             "json trailing a prose summary is not a declaration",
			signal:           "I could not implement the change and made no commits.\n\n{\"decision\": \"already-fixed-on-main\", \"reason\": \"example I am NOT making\"}",
			wantAlreadyFixed: false,
			wantDeclared:     false,
		},
		{
			name:             "quoted fence disclaimed in prose is not a declaration",
			signal:           quotedFenceDisclaimed,
			wantAlreadyFixed: false,
			wantDeclared:     false,
		},
		{
			name:             "fenced block alone is not a declaration",
			signal:           "```sybra-recovery\n{\"decision\":\"already-fixed-on-main\"}\n```",
			wantAlreadyFixed: false,
			wantDeclared:     false,
		},
		{
			name:             "json fence is not a declaration",
			signal:           "```json\n{\"decision\":\"already-fixed-on-main\"}\n```",
			wantAlreadyFixed: false,
			wantDeclared:     false,
		},
		{
			name:             "markdown table mentioning the decision is not a declaration",
			signal:           "| field | value |\n|---|---|\n| decision | already-fixed-on-main |",
			wantAlreadyFixed: false,
			wantDeclared:     false,
		},
		{
			name:             "unrelated whole-string json is not a declaration",
			signal:           `{"passed": true, "failed": 0}`,
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
			signal: `{"decision": already-fixed-on-main}`,
		},
		{
			name:   "unknown decision",
			signal: `{"decision":"close-it"}`,
		},
		{
			name:   "empty decision",
			signal: `{"decision":""}`,
		},
		{
			name:   "unicode lookalike hyphens",
			signal: `{"decision":"already‑fixed‑on‑main"}`,
		},
		{
			name:   "verbatim prompt placeholder is rejected",
			signal: `{"decision": "<already-fixed-on-main|none>", "reason": "<one sentence>"}`,
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
	signal := `{"decision":"already-fixed-on-main","reason":"landed in an earlier PR"}`
	if got := recoveryVerdictReason(signal); got != "landed in an earlier PR" {
		t.Fatalf("recoveryVerdictReason = %q, want %q", got, "landed in an earlier PR")
	}
	if got := recoveryVerdictReason("no declaration here"); got != "" {
		t.Fatalf("recoveryVerdictReason = %q, want empty", got)
	}
}
