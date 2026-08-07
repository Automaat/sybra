// Command checkconsolidated prevents production code from recreating primitives
// that have a canonical shared package. It deliberately uses syntax-aware
// inspection instead of line grep so formatting and raw/interpreted literals do
// not change the result.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type findingKind string

const (
	kindStringTruncation findingKind = "string-byte-truncation"
	kindJSONExtraction   findingKind = "json-brace-extraction"
	kindTaskStatus       findingKind = "task-status-literal"
	kindProvider         findingKind = "provider-name-literal"
)

type finding struct {
	kind  findingKind
	path  string
	value string
	line  int
}

type allowanceKey struct {
	kind  findingKind
	path  string
	value string
}

type allowance struct {
	count  int
	reason string
}

type gateError struct {
	finding *finding
	message string
}

// allowances is the complete exception ledger. Counts are exact: removing an
// old exception or adding another copy both fail until this list is changed in
// review. Keep reasons specific enough that a reviewer can tell whether a new
// occurrence has the same semantics.
var allowances = buildAllowances()

func buildAllowances() map[allowanceKey]allowance {
	out := make(map[allowanceKey]allowance)
	add := func(kind findingKind, path, reason string, counts map[string]int) {
		for value, count := range counts {
			out[allowanceKey{kind: kind, path: path, value: value}] = allowance{count: count, reason: reason}
		}
	}

	// String slicing exceptions. These operate on an ASCII-only token, slice
	// at parser-discovered boundaries, or are test fixtures for byte-oriented
	// protocols; none truncates arbitrary human/provider text.
	add(kindStringTruncation, "cmd/gen-config-docs/main.go", "Generated Go/YAML identifiers are normalized ASCII; the slice changes first-letter case.", map[string]int{"slice": 1})
	add(kindStringTruncation, "cmd/gen-events/main.go", "Generated event function identifiers are ASCII; the slice changes first-letter case.", map[string]int{"slice": 1})
	add(kindStringTruncation, "cmd/sybra-cli/main.go", "Git revisions are hexadecimal ASCII and this produces a display-only short SHA.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/agent/discovery.go", "Provider session identifiers are ASCII protocol tokens and are shortened only for display.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/agent/k8s_job_runner.go", "The value is an already-normalized ASCII Kubernetes DNS label.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/agent/tool_result_bound.go", "The value is a hexadecimal digest used in an artifact filename.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/autoupdate/autoupdate.go", "Git revisions are hexadecimal ASCII and this produces a display-only short SHA.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/monitor/fingerprint.go", "The fingerprint slug is normalized to lowercase ASCII before its fixed-width cut.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/project/git.go", "Branch suffixes and Git revisions are validated ASCII protocol identifiers.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/provider/reset_hint.go", "Month names come from an ASCII provider protocol and the slice reads a three-byte abbreviation.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/stats/pricing.go", "Model IDs are ASCII protocol tokens; this slice parses a known pricing suffix.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/sybra/completion/evidence.go", "The evidence filename is regex-normalized to ASCII before enforcing the filesystem limit.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/sybra/config_sparse.go", "Sparse-config paths are slices at parser-derived YAML line/path boundaries.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/task/slug.go", "Task slugs are regex-normalized to lowercase ASCII before enforcing the identifier limit.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/worktree/attempt.go", "Git revisions are hexadecimal ASCII and this produces a branch suffix.", map[string]int{"slice": 1})

	add(kindStringTruncation, "internal/agent/procsandbox_darwin_integration_test.go", "Integration fixtures split fixed-format sandbox profile text and byte buffers.", map[string]int{"slice": 3})
	add(kindStringTruncation, "internal/project/repair_test.go", "Git-repair fixtures deliberately mutate fixed-format object/ref data.", map[string]int{"slice": 5})
	add(kindStringTruncation, "internal/sybra/e2e_chaos_test.go", "Chaos fixture truncates a controlled ASCII test payload.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/sybra/e2e_workflow_test.go", "Workflow fixture slices a controlled ASCII command token.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/task/parser_test.go", "Parser fixture deliberately cuts serialized bytes to exercise malformed input.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/workflow/engine_test.go", "Engine fixture slices a controlled ASCII test token.", map[string]int{"slice": 1})

	// One deliberately weaker JSON fallback survives: the balanced shared
	// scanner can be confused by an unmatched quote in surrounding prose, so
	// this span is tried second and accepted only if json.Unmarshal succeeds.
	add(kindJSONExtraction, "internal/workflow/engine_steps_bestofn.go", "Fallback for unmatched quotes in judge prose; json.Unmarshal validates the candidate.", map[string]int{"outermostBraceSpan": 1})

	// Provider-name exceptions are external wire/vendor identifiers rather
	// than Sybra dispatch comparisons.
	add(kindProvider, "internal/agent/discovery.go", "Names the codex executable in an OS process probe.", map[string]int{"codex": 1})
	add(kindProvider, "internal/agent/manager_run.go", "Names OpenCode's vendor-owned state directories in sandbox policy.", map[string]int{"opencode": 2})
	add(kindProvider, "internal/agent/provider_codex.go", "Rejects Claude-family model IDs passed to the Codex adapter.", map[string]int{"claude": 1})
	add(kindProvider, "internal/config/file_config.go", "YAML migration paths must spell persisted provider keys on both alias and legacy sides.", map[string]int{"claude": 2, "codex": 2, "copilot": 2, "opencode": 2})
	add(kindProvider, "internal/github/check_filter.go", "Matches the external Copilot check-run product name, not a dispatch provider.", map[string]int{"copilot": 1})
	add(kindProvider, "internal/github/client.go", "Matches the external Copilot review actor/product name.", map[string]int{"copilot": 1})

	// Task-status spellings below belong to other persisted protocols or
	// deliberately simulate the CLI wire format. True task comparisons use
	// internal/taskstatus constants and are not allowlisted.
	add(kindTaskStatus, "cmd/fake-claude/main.go", "Fake-provider executable emits exact CLI/frontmatter wire values for E2E tests.", map[string]int{"todo": 1, "planning": 3, "done": 1, "in-review": 3, "human-required": 1, "plan-review": 1})
	add(kindTaskStatus, "cmd/fake-codex/main.go", "Fake-provider executable emits exact CLI/frontmatter wire values for E2E tests.", map[string]int{"todo": 1, "planning": 3, "done": 1, "in-review": 2, "human-required": 1, "ready-pr": 1})
	add(kindTaskStatus, "internal/autoupdate/autoupdate.go", "Auto-update Result has its own status vocabulary (blocked/new candidate), unrelated to tasks.", map[string]int{"blocked": 3, "new": 1})
	add(kindTaskStatus, "internal/bgop/model.go", "Background-operation Status is a separate persisted enum.", map[string]int{"done": 1})
	add(kindTaskStatus, "internal/config/config_migration.go", "Names the workflow.testing configuration key in alias/canonical YAML paths.", map[string]int{"testing": 4})
	add(kindTaskStatus, "internal/evaluation/phases.go", "Evaluation phases are a separate reporting taxonomy.", map[string]int{"planning": 1, "testing": 1})
	add(kindTaskStatus, "internal/github/automerge_backoff.go", "MergeErrorClass is a separate GitHub error taxonomy.", map[string]int{"blocked": 1})
	add(kindTaskStatus, "internal/monitor/detector.go", "Monitor evidence keys name board-count metrics in the serialized report.", map[string]int{"todo": 2})
	add(kindTaskStatus, "internal/monitor/service.go", "Structured log key names the todo-count metric.", map[string]int{"todo": 1})
	add(kindTaskStatus, "internal/sybra/app_init.go", "EvidenceDecision outcome is a separate verified/blocked result enum.", map[string]int{"blocked": 1})
	add(kindTaskStatus, "internal/sybra/config_registry.go", "Names the top-level testing configuration section.", map[string]int{"testing": 1})
	add(kindTaskStatus, "internal/triage/model.go", "Normalizes the English tag alias testing to the test tag.", map[string]int{"testing": 1})
	add(kindTaskStatus, "internal/umbrella/tags.go", "Umbrella expansion phases and control tags are separate vocabularies.", map[string]int{"planning": 1, "blocked": 1})
	add(kindTaskStatus, "internal/verdict/verdict.go", "todo is a placeholder word rejected from model-authored prose, not a task status.", map[string]int{"todo": 1})
	add(kindTaskStatus, "internal/workflow/engine_events_watchdog.go", "planning is prompt prose identifying the agent stage, not a status comparison.", map[string]int{"planning": 1})
	add(kindTaskStatus, "internal/workflow/engine_steps_admission.go", "AdmissionDecision outcome is a separate admitted/blocked result enum.", map[string]int{"blocked": 1})
	add(kindTaskStatus, "internal/workflow/engine_steps_evidence.go", "EvidenceDecision outcome is a separate verified/blocked result enum.", map[string]int{"blocked": 1})
	add(kindTaskStatus, "internal/workflow/engine_steps_prfix.go", "PR-fix agent sentinel protocol has its own human/continue/done verdict vocabulary.", map[string]int{"human-required": 2, "done": 1})
	add(kindTaskStatus, "internal/workflow/engine_steps_verify_checks.go", "StepOutput blocked is a workflow-step result, not the task status field.", map[string]int{"blocked": 1})

	return out
}

var taskStatuses = map[string]struct{}{
	"new": {}, "todo": {}, "in-progress": {}, "ready-review": {},
	"in-review": {}, "planning": {}, "plan-review": {}, "testing": {},
	"ready-pr": {}, "human-required": {}, "blocked": {}, "done": {},
	"cancelled": {},
}

var providerNames = map[string]struct{}{
	"claude": {}, "codex": {}, "copilot": {}, "opencode": {},
}

func main() {
	if err := validateAllowances(); err != nil {
		fmt.Fprintln(os.Stderr, "check-consolidated-primitives:", err)
		os.Exit(1)
	}
	fset := token.NewFileSet()
	var findings []finding
	failed := false
	for _, rawPath := range os.Args[1:] {
		path := filepath.ToSlash(rawPath)
		file, err := parser.ParseFile(fset, rawPath, nil, 0)
		if err != nil {
			fmt.Fprintf(os.Stderr, "check-consolidated-primitives: parse %s: %v\n", path, err)
			failed = true
			continue
		}
		findings = append(findings, inspectFile(fset, path, file)...)
	}

	for _, issue := range auditFindings(findings, allowances) {
		if issue.finding != nil {
			f := issue.finding
			fmt.Fprintf(os.Stderr, "::error file=%s,line=%d::%s %q is outside its canonical package; use %s or add a narrowly reasoned exception\n",
				f.path, f.line, f.kind, f.value, canonicalPackage(f.kind))
		} else {
			fmt.Fprintln(os.Stderr, "check-consolidated-primitives:", issue.message)
		}
		failed = true
	}
	if failed {
		os.Exit(1)
	}
	fmt.Println("check-consolidated-primitives: shared truncation, JSON extraction, task-status, and provider boundaries intact")
}

func auditFindings(findings []finding, ledger map[allowanceKey]allowance) []gateError {
	counts := make(map[allowanceKey]int)
	var issues []gateError
	for i := range findings {
		f := &findings[i]
		key := allowanceKey{kind: f.kind, path: f.path, value: f.value}
		counts[key]++
		allowed, ok := ledger[key]
		if !ok || counts[key] > allowed.count {
			issues = append(issues, gateError{finding: f})
		}
	}
	for key, allowed := range ledger {
		if counts[key] >= allowed.count {
			continue
		}
		issues = append(issues, gateError{message: fmt.Sprintf("stale exception %s %s %q: found %d, ledger requires %d (%s)",
			key.kind, key.path, key.value, counts[key], allowed.count, allowed.reason)})
	}
	return issues
}

func validateAllowances() error {
	for key, allowed := range allowances {
		if key.kind == "" || key.path == "" || key.value == "" || allowed.count < 1 || strings.TrimSpace(allowed.reason) == "" {
			return fmt.Errorf("invalid exception ledger entry: %#v => %#v", key, allowed)
		}
	}
	return nil
}

func canonicalPackage(kind findingKind) string {
	switch kind {
	case kindStringTruncation:
		return "internal/textutil"
	case kindJSONExtraction:
		return "internal/llmjob"
	case kindTaskStatus:
		return "internal/taskstatus"
	case kindProvider:
		return "internal/providerid"
	default:
		return "the shared package"
	}
}

func inspectFile(fset *token.FileSet, path string, file *ast.File) []finding {
	// The checker necessarily contains the literal vocabulary and synthetic
	// examples it searches for. It is its own bootstrap implementation, not a
	// product caller of any consolidated primitive.
	if strings.HasPrefix(path, "scripts/checkconsolidated/") {
		return nil
	}
	var out []finding
	isTest := strings.HasSuffix(path, "_test.go")
	stringNames, stringFields := collectStringNames(file)
	ast.Inspect(file, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.SliceExpr:
			if !strings.HasPrefix(path, "internal/textutil/") && isStringExpr(n.X, stringNames, stringFields) && isTruncatingSlice(n) {
				out = append(out, finding{kind: kindStringTruncation, path: path, value: "slice", line: fset.Position(n.Pos()).Line})
			}
		case *ast.BasicLit:
			// Tests intentionally spell persisted/wire values to prove parsing and
			// compatibility. They are excluded only from the literal gates; their
			// production helpers remain subject to truncation/extraction checks.
			if isTest || n.Kind != token.STRING || isStructTag(file, n) {
				return true
			}
			value, err := strconv.Unquote(n.Value)
			if err != nil {
				return true
			}
			if _, ok := taskStatuses[value]; ok && !strings.HasPrefix(path, "internal/taskstatus/") {
				out = append(out, finding{kind: kindTaskStatus, path: path, value: value, line: fset.Position(n.Pos()).Line})
			}
			if _, ok := providerNames[value]; ok && !strings.HasPrefix(path, "internal/providerid/") {
				out = append(out, finding{kind: kindProvider, path: path, value: value, line: fset.Position(n.Pos()).Line})
			}
		}
		return true
	})

	if !strings.HasPrefix(path, "internal/llmjob/") {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !extractsJSONByBraces(fn) {
				continue
			}
			out = append(out, finding{kind: kindJSONExtraction, path: path, value: fn.Name.Name, line: fset.Position(fn.Pos()).Line})
		}
	}
	return out
}

func isStructTag(file *ast.File, lit *ast.BasicLit) bool {
	found := false
	ast.Inspect(file, func(node ast.Node) bool {
		field, ok := node.(*ast.Field)
		if ok && field.Tag == lit {
			found = true
			return false
		}
		return !found
	})
	return found
}

func collectStringNames(file *ast.File) (map[string]struct{}, map[string]struct{}) {
	names := make(map[string]struct{})
	fields := make(map[string]struct{})
	ast.Inspect(file, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.Field:
			if id, ok := n.Type.(*ast.Ident); ok && id.Name == "string" {
				for _, name := range n.Names {
					names[name.Name] = struct{}{}
					fields[name.Name] = struct{}{}
				}
			}
		case *ast.ValueSpec:
			if id, ok := n.Type.(*ast.Ident); ok && id.Name == "string" {
				for _, name := range n.Names {
					names[name.Name] = struct{}{}
				}
			}
		}
		return true
	})
	for range 4 {
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.AssignStmt:
				if len(n.Lhs) != len(n.Rhs) {
					return true
				}
				for i, rhs := range n.Rhs {
					if !isStringExpr(rhs, names, fields) {
						continue
					}
					if id, ok := n.Lhs[i].(*ast.Ident); ok {
						names[id.Name] = struct{}{}
					}
				}
			case *ast.ValueSpec:
				if len(n.Names) != len(n.Values) {
					return true
				}
				for i, value := range n.Values {
					if isStringExpr(value, names, fields) {
						names[n.Names[i].Name] = struct{}{}
					}
				}
			}
			return true
		})
	}
	return names, fields
}

func isStringExpr(expr ast.Expr, names, fields map[string]struct{}) bool {
	switch e := expr.(type) {
	case *ast.BasicLit:
		return e.Kind == token.STRING
	case *ast.Ident:
		_, ok := names[e.Name]
		return ok
	case *ast.SelectorExpr:
		_, ok := fields[e.Sel.Name]
		return ok
	case *ast.ParenExpr:
		return isStringExpr(e.X, names, fields)
	case *ast.BinaryExpr:
		return e.Op == token.ADD && (isStringExpr(e.X, names, fields) || isStringExpr(e.Y, names, fields))
	case *ast.SliceExpr:
		return isStringExpr(e.X, names, fields)
	case *ast.CallExpr:
		if id, ok := e.Fun.(*ast.Ident); ok {
			return id.Name == "string"
		}
		sel, ok := e.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		if sel.Sel.Name == "String" || sel.Sel.Name == "Error" {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return false
		}
		if pkg.Name == "fmt" {
			return strings.HasPrefix(sel.Sel.Name, "Sprint")
		}
		if pkg.Name != "strings" {
			return false
		}
		switch sel.Sel.Name {
		case "Clone", "Join", "Map", "Repeat", "Replace", "ReplaceAll", "ToLower", "ToUpper", "ToTitle", "Trim", "TrimFunc", "TrimLeft", "TrimLeftFunc", "TrimPrefix", "TrimRight", "TrimRightFunc", "TrimSpace", "TrimSuffix":
			return true
		}
	}
	return false
}

func isTruncatingSlice(slice *ast.SliceExpr) bool {
	if slice.High != nil && looksLikeBound(slice.High) {
		return true
	}
	return isTailBound(slice.Low)
}

func looksLikeBound(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.BasicLit:
		return e.Kind == token.INT
	case *ast.Ident:
		name := strings.ToLower(e.Name)
		return name == "n" || name == "size" || name == "count" || strings.Contains(name, "max") || strings.Contains(name, "limit") || strings.Contains(name, "cap") || strings.Contains(name, "bytes")
	case *ast.ParenExpr:
		return looksLikeBound(e.X)
	case *ast.CallExpr:
		if id, ok := e.Fun.(*ast.Ident); ok && (id.Name == "min" || id.Name == "max") {
			return true
		}
	case *ast.BinaryExpr:
		// A constant inside parser arithmetic (end+1, len(s)-1) is a
		// delimiter adjustment, not a length budget. Binary bounds count only
		// when one side carries an explicit max/limit/size signal.
		return containsBoundSignal(e.X) || containsBoundSignal(e.Y)
	}
	return false
}

func containsBoundSignal(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.Ident:
		return looksLikeBound(e)
	case *ast.ParenExpr:
		return containsBoundSignal(e.X)
	case *ast.BinaryExpr:
		return containsBoundSignal(e.X) || containsBoundSignal(e.Y)
	case *ast.CallExpr:
		id, ok := e.Fun.(*ast.Ident)
		return ok && (id.Name == "min" || id.Name == "max")
	}
	return false
}

func isTailBound(expr ast.Expr) bool {
	bin, ok := expr.(*ast.BinaryExpr)
	if !ok || bin.Op != token.SUB || !looksLikeBound(bin.Y) {
		return false
	}
	call, ok := bin.X.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	id, ok := call.Fun.(*ast.Ident)
	return ok && id.Name == "len"
}

func extractsJSONByBraces(fn *ast.FuncDecl) bool {
	body := fn.Body
	hasFirst, hasLast := false, false
	braceDirections := make(map[string]uint8)
	ast.Inspect(body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.CallExpr:
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if !ok || len(n.Args) < 2 || !isBraceLiteral(n.Args[1]) {
				return true
			}
			switch sel.Sel.Name {
			case "Index", "IndexByte", "IndexRune":
				hasFirst = true
			case "LastIndex", "LastIndexByte":
				hasLast = true
			}
		case *ast.IncDecStmt:
			id, ok := n.X.(*ast.Ident)
			if !ok {
				return true
			}
			if n.Tok == token.INC {
				braceDirections[id.Name] |= 1
			} else if n.Tok == token.DEC {
				braceDirections[id.Name] |= 2
			}
		case *ast.AssignStmt:
			if len(n.Lhs) != 1 {
				return true
			}
			id, ok := n.Lhs[0].(*ast.Ident)
			if !ok {
				return true
			}
			if n.Tok == token.ADD_ASSIGN {
				braceDirections[id.Name] |= 1
			} else if n.Tok == token.SUB_ASSIGN {
				braceDirections[id.Name] |= 2
			}
		}
		return true
	})
	if hasFirst && hasLast {
		return true
	}
	name := strings.ToLower(fn.Name.Name)
	jsonishName := strings.Contains(name, "json") || strings.Contains(name, "object")
	for _, directions := range braceDirections {
		if directions == 3 && bodyHasBothBraces(body) && (jsonishName || returnsStringSlice(fn)) {
			return true
		}
	}
	return false
}

func returnsStringSlice(fn *ast.FuncDecl) bool {
	if fn.Type.Results == nil {
		return false
	}
	stringResult := false
	for _, field := range fn.Type.Results.List {
		if id, ok := field.Type.(*ast.Ident); ok && id.Name == "string" {
			stringResult = true
			break
		}
	}
	if !stringResult {
		return false
	}
	found := false
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		ret, ok := node.(*ast.ReturnStmt)
		if !ok {
			return !found
		}
		for _, result := range ret.Results {
			if _, ok := result.(*ast.SliceExpr); ok {
				found = true
				return false
			}
		}
		return !found
	})
	return found
}

func isBraceLiteral(expr ast.Expr) bool {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || (lit.Kind != token.STRING && lit.Kind != token.CHAR) {
		return false
	}
	value, err := strconv.Unquote(lit.Value)
	return err == nil && (value == "{" || value == "}")
}

func bodyHasBothBraces(body *ast.BlockStmt) bool {
	open, close := false, false
	ast.Inspect(body, func(node ast.Node) bool {
		lit, ok := node.(*ast.BasicLit)
		if !ok || (lit.Kind != token.STRING && lit.Kind != token.CHAR) {
			return true
		}
		value, err := strconv.Unquote(lit.Value)
		if err == nil {
			open = open || value == "{"
			close = close || value == "}"
		}
		return true
	})
	return open && close
}
