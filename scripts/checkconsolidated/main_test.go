package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/Automaat/sybra/internal/providerid"
	"github.com/Automaat/sybra/internal/taskstatus"
)

func findingsFor(t *testing.T, path, source string) []finding {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, source, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	info := collectTypeInfo("fixture", fset, []*ast.File{file})
	return inspectFile(fset, path, file, info)
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
	wants := map[findingKind]int{
		kindStringTruncation: 2, // explicit truncation plus the JSON span slice
		kindJSONExtraction:   1,
		kindTaskStatus:       1,
		kindProvider:         1,
	}
	for kind, want := range wants {
		if got := countKind(findings, kind); got != want {
			t.Errorf("%s findings = %d, want %d; all findings: %#v", kind, got, want, findings)
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

func TestDetectsOrdinaryBoundAndNamedString(t *testing.T) {
	t.Parallel()
	findings := findingsFor(t, "internal/example/drift.go", `package example
type message string
func source() string { return "payload" }
func truncate(s message, boundary int) (message, string) {
	return s[:boundary], source()[:boundary]
}
`)
	if got := countKind(findings, kindStringTruncation); got != 2 {
		t.Fatalf("truncation findings = %d, want 2: %#v", got, findings)
	}
}

func TestDetectsAssignmentBraceCounterReturningSpan(t *testing.T) {
	t.Parallel()
	findings := findingsFor(t, "internal/example/drift.go", `package example
func scan(s string) (int, int) {
	depth := 0
	start := 0
	for i, c := range s {
		switch c { case '{': depth = depth + 1; case '}': depth = depth - 1 }
		if depth == 0 { return start, i }
	}
	return -1, -1
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
		kind findingKind
	}{
		{"internal/textutil/new.go", `package textutil; func f(s string, n int) string { return s[:n] }`, kindStringTruncation},
		{"internal/llmjob/new.go", `package llmjob; import "strings"; func extractJSON(s string) string { return s[strings.IndexByte(s, '{'):strings.LastIndexByte(s, '}')] }`, kindJSONExtraction},
		{"internal/taskstatus/new.go", `package taskstatus; const x = "human-required"`, kindTaskStatus},
		{"internal/providerid/new.go", `package providerid; const x = "claude"`, kindProvider},
	}
	for _, tc := range cases {
		findings := findingsFor(t, tc.path, tc.src)
		if got := countKind(findings, tc.kind); got != 0 {
			t.Errorf("%s produced %s findings: %#v", tc.path, tc.kind, findings)
		}
	}
}

func TestAvoidsDocumentedNonPrimitiveShapes(t *testing.T) {
	t.Parallel()
	findings := findingsFor(t, "internal/example/parser.go", `package example
type wire struct { Status string `+"`json:\"human-required\"`"+` }
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

func TestTestFilesRequireExplicitWireLiteralExceptions(t *testing.T) {
	t.Parallel()
	findings := findingsFor(t, "internal/example/drift_test.go", `package example
var status = "human-required"
var provider = "claude"
func truncate(s string, limit int) string { return s[:limit] }
`)
	if got := countKind(findings, kindStringTruncation); got != 1 {
		t.Fatalf("truncation findings = %d, want 1: %#v", got, findings)
	}
	if got := countKind(findings, kindTaskStatus) + countKind(findings, kindProvider); got != 2 {
		t.Fatalf("wire literal findings in test file = %d, want 2: %#v", got, findings)
	}
}

func TestVocabulariesComeFromCanonicalPackages(t *testing.T) {
	t.Parallel()
	if got, want := len(taskStatuses), len(taskstatus.All()); got != want {
		t.Fatalf("task status vocabulary length = %d, want %d", got, want)
	}
	for _, status := range taskstatus.All() {
		if _, ok := taskStatuses[string(status)]; !ok {
			t.Errorf("task status vocabulary missing %q", status)
		}
	}
	if got, want := len(providerNames), len(providerid.All()); got != want {
		t.Fatalf("provider vocabulary length = %d, want %d", got, want)
	}
	for _, provider := range providerid.All() {
		if _, ok := providerNames[provider]; !ok {
			t.Errorf("provider vocabulary missing %q", provider)
		}
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
