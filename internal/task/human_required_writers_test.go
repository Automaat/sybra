package task

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestProductionHumanRequiredWriters is the static half of the eligibility
// guard. It inventories every production transition that names
// human-required literally and requires typed evidence at the call site.
// TestHumanRequiredGuardRejectsMissingAndMachineOwnedReasons is the runtime
// half: variable-status adapters cannot bypass Manager.Apply's validation.
func TestProductionHumanRequiredWriters(t *testing.T) {
	root := repositoryRoot(t)
	want := []string{
		"completion.hold_fix_review_for_human",
		"orchestrator.planning.blocked",
		"orchestrator.review_rate_limit.park",
		"orchestrator.review_task_limit.park",
		"review.pr-monitor.ci-rerun.permission",
		"review.pr-monitor.push-preflight",
		"umbrella.gate.condition.escalate",
		"umbrella.gate.scope_verdict.hold",
	}

	var got []string
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "vendor" || entry.Name() == "frontend" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			lit, ok := node.(*ast.CompositeLit)
			if !ok || typeName(lit.Type) != "TransitionIntent" {
				return true
			}
			fields := keyedFields(lit)
			if !isHumanRequiredExpr(fields["ToStatus"]) {
				return true
			}
			pos := fset.Position(lit.Pos())
			extra, ok := fields["Extra"].(*ast.CompositeLit)
			if !ok {
				t.Errorf("%s:%d: literal human-required writer must construct typed Extra inline", relative(root, path), pos.Line)
				return true
			}
			extraFields := keyedFields(extra)
			if extraFields["Escalation"] == nil || extraFields["AutonomyOutcome"] == nil {
				t.Errorf("%s:%d: human-required writer lacks Escalation or AutonomyOutcome", relative(root, path), pos.Line)
			}
			actor, ok := stringLiteral(fields["Actor"])
			if !ok {
				t.Errorf("%s:%d: literal human-required writer must have a stable actor", relative(root, path), pos.Line)
				return true
			}
			got = append(got, actor)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("production human-required writers changed\n got: %v\nwant: %v\nclassify the new writer and update this inventory", got, want)
	}
}

// Variable-status boundaries are intentionally few. Each marker is the local
// proof that the adapter supplies typed evidence before it calls Apply (or,
// for untrusted workflow adapters, downgrades an untyped request to blocked).
func TestDynamicHumanRequiredBoundariesAreTyped(t *testing.T) {
	root := repositoryRoot(t)
	markers := map[string][]string{
		// Proposal filing moved out of sybra-cli so the CLI and the server
		// share one dedupe and one scrub rule; the boundary moved with it.
		"internal/harnessevolution/filing.go": {
			`update.Escalation = task.PolicyRequired("harness.proposal_approval_required"`,
		},
		// The CLI no longer writes a status change itself: it calls the server,
		// whose svc_tasks.go entry below attaches the evidence for the same
		// transition. Handoff still mints a task here, so that marker stays.
		"cmd/sybra-cli/main.go": {
			`init.Escalation = task.OperatorDecisionEvidence("operator.handoff_status"`,
		},
		"internal/promptlab/filing.go": {
			`update.Escalation = task.PolicyRequired("promptlab.approval_required"`,
		},
		"internal/sybra/app_promptlab.go": {
			`update.Escalation = task.PolicyRequired("promptlab.approval_required"`,
		},
		"internal/sybra/app_umbrella_gate.go": {
			`extra.Escalation = task.SpecificationRequired("umbrella.rollup_requires_decision"`,
		},
		"internal/sybra/app_workflow.go": {
			"SetEscalationAndWorkflow(id, status, reason string, escalation autonomy.EscalationReason, outcome autonomy.Outcome",
			`MachineFailure("workflow.untyped_escalation"`,
		},
		"internal/sybra/review/inbound.go": {
			`u.Escalation = task.OperatorDecisionRequired("review.manual_action_required"`,
		},
		"internal/sybra/svc_tasks.go": {
			`extra.Escalation = task.OperatorDecisionEvidence("operator.manual_status_change"`,
		},
		"internal/triage/apply.go": {
			`extra.Escalation = task.OperatorDecisionRequired("triage.umbrella_expansion_decision"`,
		},
	}
	for name, required := range markers {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, marker := range required {
			if !strings.Contains(string(data), marker) {
				t.Errorf("%s: missing typed dynamic human-required boundary marker %q", name, marker)
			}
		}
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func keyedFields(lit *ast.CompositeLit) map[string]ast.Expr {
	out := make(map[string]ast.Expr, len(lit.Elts))
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		ident, ok := kv.Key.(*ast.Ident)
		if ok {
			out[ident.Name] = kv.Value
		}
	}
	return out
}

func typeName(expr ast.Expr) string {
	switch value := expr.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.SelectorExpr:
		return value.Sel.Name
	default:
		return ""
	}
}

func isHumanRequiredExpr(expr ast.Expr) bool {
	return typeName(expr) == "StatusHumanRequired"
}

func stringLiteral(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(lit.Value)
	return value, err == nil
}

func relative(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return rel
}
