package main

import (
	"go/parser"
	"go/token"
	"testing"
)

func findingsFor(t *testing.T, path, source string) []finding {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, source, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return inspectFile(fset, path, file)
}

func countKind(findings []finding, kind findingKind) int {
	n := 0
	for _, f := range findings {
		if f.kind == kind {
			n++
		}
	}
	return n
}

func TestDetectsEveryConsolidatedPrimitiveDrift(t *testing.T) {
	t.Parallel()
	findings := findingsFor(t, "internal/example/drift.go", `package example
import "strings"
func truncate(s string, maxBytes int) string { return s[:maxBytes] }
func extractJSON(s string) string {
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	return s[start:end]
}
var status = "human-required"
var provider = "claude"
`)
	for _, kind := range []findingKind{kindStringTruncation, kindJSONExtraction, kindTaskStatus, kindProvider} {
		if got := countKind(findings, kind); got != 1 {
			t.Errorf("%s findings = %d, want 1; all findings: %#v", kind, got, findings)
		}
	}
}

func TestDetectsBalancedBraceScanner(t *testing.T) {
	t.Parallel()
	findings := findingsFor(t, "internal/example/drift.go", `package example
func scan(s string) string {
	depth := 0
	start := 0
	for _, c := range s {
		switch c { case '{': depth++; case '}': depth-- }
	}
	return s[start:]
}
`)
	if got := countKind(findings, kindJSONExtraction); got != 1 {
		t.Fatalf("JSON findings = %d, want 1: %#v", got, findings)
	}
}

func TestCanonicalPackagesOwnThePrimitives(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path string
		src  string
	}{
		{"internal/textutil/new.go", `package textutil; func f(s string, n int) string { return s[:n] }`},
		{"internal/llmjob/new.go", `package llmjob; import "strings"; func extractJSON(s string) string { return s[strings.IndexByte(s, '{'):strings.LastIndexByte(s, '}')] }`},
		{"internal/taskstatus/new.go", `package taskstatus; const x = "human-required"`},
		{"internal/providerid/new.go", `package providerid; const x = "claude"`},
	}
	for _, tc := range cases {
		if got := findingsFor(t, tc.path, tc.src); len(got) != 0 {
			t.Errorf("%s produced findings: %#v", tc.path, got)
		}
	}
}

func TestAvoidsDocumentedNonPrimitiveShapes(t *testing.T) {
	t.Parallel()
	findings := findingsFor(t, "internal/example/parser.go", `package example
type wire struct { Status string `+"`json:\"human-required\"`"+` }
func token(s string, idx int) string { return s[:idx] }
func codeBraceDelta(s string) int {
	depth := 0
	for _, c := range s { switch c { case '{': depth++; case '}': depth-- } }
	return depth
}
`)
	if len(findings) != 0 {
		t.Fatalf("parser/struct-tag shapes produced findings: %#v", findings)
	}
}

func TestTestFilesOnlyExemptWireLiterals(t *testing.T) {
	t.Parallel()
	findings := findingsFor(t, "internal/example/drift_test.go", `package example
var status = "human-required"
var provider = "claude"
func truncate(s string, limit int) string { return s[:limit] }
`)
	if got := countKind(findings, kindStringTruncation); got != 1 {
		t.Fatalf("truncation findings = %d, want 1: %#v", got, findings)
	}
	if got := countKind(findings, kindTaskStatus) + countKind(findings, kindProvider); got != 0 {
		t.Fatalf("wire literal findings in test file = %d, want 0: %#v", got, findings)
	}
}

func TestEveryExceptionHasAReasonAndExactPositiveCount(t *testing.T) {
	t.Parallel()
	if err := validateAllowances(); err != nil {
		t.Fatal(err)
	}
}

func TestExceptionLedgerRejectsBothAddedAndStaleOccurrences(t *testing.T) {
	t.Parallel()
	key := allowanceKey{kind: kindProvider, path: "internal/example/vendor.go", value: "claude"}
	ledger := map[allowanceKey]allowance{
		key: {count: 1, reason: "external vendor executable name"},
	}
	base := finding{kind: key.kind, path: key.path, value: key.value, line: 3}
	if issues := auditFindings([]finding{base}, ledger); len(issues) != 0 {
		t.Fatalf("exact baseline produced issues: %#v", issues)
	}
	if issues := auditFindings([]finding{base, base}, ledger); len(issues) != 1 || issues[0].finding == nil {
		t.Fatalf("added occurrence issues = %#v, want one source finding", issues)
	}
	if issues := auditFindings(nil, ledger); len(issues) != 1 || issues[0].finding != nil {
		t.Fatalf("stale occurrence issues = %#v, want one stale-ledger error", issues)
	}
}
