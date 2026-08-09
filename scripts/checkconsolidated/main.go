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
	"go/types"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Automaat/sybra/internal/providerid"
	"github.com/Automaat/sybra/internal/taskstatus"
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

func buildAllowances() map[allowanceKey]allowance { //nolint:funlen // The explicit exception ledger is intentionally centralized for review.
	out := make(map[allowanceKey]allowance)
	add := func(kind findingKind, path, reason string, counts map[string]int) {
		for value, count := range counts {
			key := allowanceKey{kind: kind, path: path, value: value}
			current := out[key]
			current.count += count
			if current.reason == "" {
				current.reason = reason
			} else if current.reason != reason {
				current.reason += "; " + reason
			}
			out[key] = current
		}
	}

	// String slicing exceptions. These operate on an ASCII-only token, slice
	// at parser-discovered boundaries, or are test fixtures for byte-oriented
	// protocols; none truncates arbitrary human/provider text.
	add(kindStringTruncation, "cmd/gen-config-docs/main.go", "Generated Go/YAML identifiers are normalized ASCII; the slice changes first-letter case.", map[string]int{"slice": 1})
	add(kindStringTruncation, "cmd/gen-events/main.go", "Generated event function identifiers are ASCII; the slice changes first-letter case.", map[string]int{"slice": 1})
	add(kindStringTruncation, "cmd/sybra-cli/main.go", "Git revisions are hexadecimal ASCII and this produces a display-only short SHA.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/monitor/issueoutbox.go", "The value is a slice of outbox records capped to a flush batch, not text; the store returns it through an interface so its element type is unresolved here.", map[string]int{"slice": 1})
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
	add(kindStringTruncation, "internal/agent/manager_run.go", "UUID strings are canonical ASCII tokens; this slice selects the fixed-width short agent identifier.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/loopagent/store.go", "UUID strings are canonical ASCII tokens; this slice selects the fixed-width short loop-agent identifier.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/sybra/app.go", "UUID strings are canonical ASCII tokens; this slice selects the fixed-width Kubernetes smoke-test identifier.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/task/comment.go", "UUID strings are canonical ASCII tokens; this slice selects the fixed-width short comment identifier.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/task/store.go", "UUID strings are canonical ASCII tokens; this slice selects the fixed-width short task identifier.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/db/db.go", "libpq keyword/value DSNs are ASCII; these slices cut at parser-discovered key, value, and quote boundaries while redacting a password.", map[string]int{"slice": 5})

	add(kindStringTruncation, "internal/agent/procsandbox_darwin_integration_test.go", "Integration fixtures split fixed-format sandbox profile text and byte buffers.", map[string]int{"slice": 3})
	add(kindStringTruncation, "internal/project/repair_test.go", "Git-repair fixtures deliberately mutate fixed-format object/ref data.", map[string]int{"slice": 5})
	add(kindStringTruncation, "internal/sybra/e2e_chaos_test.go", "Chaos fixture truncates a controlled ASCII test payload.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/sybra/e2e_workflow_test.go", "Workflow fixture slices a controlled ASCII command token.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/task/parser_test.go", "Parser fixture deliberately cuts serialized bytes to exercise malformed input.", map[string]int{"slice": 1})

	// One deliberately weaker JSON fallback survives: the balanced shared
	// scanner can be confused by an unmatched quote in surrounding prose, so
	// this span is tried second and accepted only if json.Unmarshal succeeds.
	add(kindJSONExtraction, "internal/workflow/engine_steps_bestofn.go", "Fallback for unmatched quotes in judge prose; json.Unmarshal validates the candidate.", map[string]int{"outermostBraceSpan": 1})
	add(kindJSONExtraction, "cmd/gen-api-shim/shim.go", "TypeScript signature parser balances every delimiter kind to split top-level parameters; it does not extract JSON.", map[string]int{"splitTopLevel": 1})
	add(kindJSONExtraction, "internal/workflow/engine_steps_tamper.go", "Go-source tamper analysis counts lexical brace balance after excluding strings and comments; it does not extract JSON.", map[string]int{"codeBraceDelta": 1})

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
	add(kindTaskStatus, "internal/workflow/engine_steps_parallel_gates.go", "coordinatorTerminalOutput maps a gate's own StepOutput onto the coordinator's, not the task status field.", map[string]int{"blocked": 1})
	add(kindTaskStatus, "internal/workflow/engine_steps_prfix.go", "PR-fix agent sentinel protocol has its own human/continue/done verdict vocabulary.", map[string]int{"human-required": 2, "done": 1})
	add(kindTaskStatus, "internal/workflow/engine_steps_verify_checks.go", "StepOutput blocked is a workflow-step result, not the task status field.", map[string]int{"blocked": 1})

	// The type-aware truncation rule intentionally treats every string slice as
	// suspicious. These pre-existing parser/protocol/test slices are exact
	// baselines; any added or removed occurrence requires a ledger review.
	add(kindStringTruncation, "cmd/gen-api-shim/shim.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 6})
	add(kindStringTruncation, "cmd/gen-config-docs/main.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 1})
	add(kindStringTruncation, "cmd/gen-events/main.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 1})
	add(kindStringTruncation, "cmd/sybra-cli/selfmonitor.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 1})
	add(kindStringTruncation, "cmd/sybra-perf/main.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 5})
	add(kindStringTruncation, "internal/agent/discovery.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/agent/k8s_job_runner.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/agent/malformed_tool_call.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/agent/orphan_sweep_other.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/agent/procsandbox_darwin_integration_test.go", "Existing test fixture slices controlled parser/protocol text at a deliberate byte boundary.", map[string]int{"slice": 3})
	add(kindStringTruncation, "internal/agent/procsandbox_linux_integration_test.go", "Existing test fixture slices controlled parser/protocol text at a deliberate byte boundary.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/agent/reattach_linux.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/agent/skill_invoke.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/agent/tool_loop_semantic.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 3})
	add(kindStringTruncation, "internal/cluster/tls_test.go", "Existing test fixture slices controlled parser/protocol text at a deliberate byte boundary.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/config/config_migration.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/fsutil/fsutil.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/fsutil/projectkey.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/github/client.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/harnessevolution/cluster.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/harnessevolution/collector.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/harnessevolution/propose.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/health/docker.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/issueref/issueref.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/limits/live.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/llmjob/llmjob.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/monitor/issueoutbox.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/monitor/issuesink.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 8})
	add(kindStringTruncation, "internal/notes/notes_test.go", "Existing test fixture slices controlled parser/protocol text at a deliberate byte boundary.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/project/git.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 3})
	add(kindStringTruncation, "internal/project/repair_test.go", "Existing test fixture slices controlled parser/protocol text at a deliberate byte boundary.", map[string]int{"slice": 5})
	add(kindStringTruncation, "internal/project/store_test.go", "Existing test fixture slices controlled parser/protocol text at a deliberate byte boundary.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/prompteval/store.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/promptlab/model.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/sandbox/docker.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/sandbox/envfile.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/sandbox/k8s_integration_test.go", "Existing test fixture slices controlled parser/protocol text at a deliberate byte boundary.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/scrub/scrub.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 6})
	add(kindStringTruncation, "internal/skillinvoke/skillinvoke.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 9})
	add(kindStringTruncation, "internal/stats/pricing.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/sybra/app_automation_routing_test.go", "Existing test fixture slices controlled parser/protocol text at a deliberate byte boundary.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/sybra/app_umbrella_gate.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 4})
	add(kindStringTruncation, "internal/sybra/config_sparse.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 4})
	add(kindStringTruncation, "internal/sybra/config_subscribers.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/sybra/review/agent_manager_test_helpers_test.go", "Existing test fixture slices controlled parser/protocol text at a deliberate byte boundary.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/sybra/svc_tasks.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/task/plan_draft.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 3})
	add(kindStringTruncation, "internal/task/slug.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/umbrella/expand.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 9})
	add(kindStringTruncation, "internal/umbrella/planner.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/workflow/builtin_plan_prompt_test.go", "Existing test fixture slices controlled parser/protocol text at a deliberate byte boundary.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/workflow/engine_events_agents.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/workflow/engine_skill_receipt_summary.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/workflow/engine_steps_bestofn.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 3})
	add(kindStringTruncation, "internal/workflow/engine_steps_clear_test.go", "Existing test fixture slices controlled parser/protocol text at a deliberate byte boundary.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/workflow/engine_steps_prfix.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/workflow/engine_steps_tamper.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 12})
	add(kindStringTruncation, "internal/workflow/engine_steps_testroute.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 8})
	add(kindStringTruncation, "internal/workflow/engine_steps_triage.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/workflow/engine_validate_plan_contract.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/workflow/engine_verifycommits_test.go", "Existing test fixture slices controlled parser/protocol text at a deliberate byte boundary.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/workflow/envtest_assets.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/workflow/template.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/worktree/branch.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/worktree/cleanup.go", "Pre-existing parser/protocol string slice retained as an exact audited baseline; count changes require migration review.", map[string]int{"slice": 3})
	add(kindStringTruncation, "internal/worktree/manager_test.go", "Existing test fixture slices controlled parser/protocol text at a deliberate byte boundary.", map[string]int{"slice": 1})

	// Calls whose imported result types are unavailable to the per-file type
	// pass are tracked through assignments and treated as possible strings.
	// These exact pre-existing non-string/parser slices are the fail-closed baseline.
	add(kindStringTruncation, "cmd/fake-claude/main.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "cmd/fake-codex/main.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "cmd/fake-copilot/main.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "cmd/sybra-cli/main.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 2})
	add(kindStringTruncation, "cmd/sybra-perf/main.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/agent/logfile.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/agent/loop_detector.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/agent/manager_control.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/agent/model.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/agent/reattach.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/agent/runner_headless_test.go", "Conservative unresolved-call provenance reaches a controlled non-string test-fixture slice.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/agent/runner_headless.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/agent/skill_invoke.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/agent/tool_loop_semantic.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 4})
	add(kindStringTruncation, "internal/agent/tool_result_bound.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 7})
	add(kindStringTruncation, "internal/cleanup/protected.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/experience/store.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/gitexec/gitexec.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/github/client.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/github/runtime.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/httpapi/handler.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/intervention/store.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/monitor/issuesink.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/notification/emitter.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/procstat/sample_unix.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/project/repair.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/promptlab/collect.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/selfmonitor/loganalyzer.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 6})
	add(kindStringTruncation, "internal/skillattr/skillattr.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/sybra/app_human_review.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/sybra/app_umbrella_gate.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/sybra/review/pr_poll_sched.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/sybra/svc_agents.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/sybra/svc_config.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/task/parser.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/task/status_effect.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/task/store_cache.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/task/store.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/task/transition.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/umbrella/expand.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/umbrella/ground.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/workflow/effect_claim.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 3})
	add(kindStringTruncation, "internal/workflow/engine_skill_receipt_summary.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/workflow/engine_steps_tamper.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 4})
	add(kindStringTruncation, "internal/workflow/engine_steps_testroute.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 9})
	add(kindStringTruncation, "internal/workflow/envtest_assets.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/workflow/execution.go", "Conservative unresolved-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 4})

	// Multi-result unresolved calls conservatively taint every assignment target.
	// These exact existing tuple-result slices keep that fail-closed rule reviewable.
	add(kindStringTruncation, "cmd/gen-api-shim/shim.go", "Conservative unresolved tuple-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/agent/orphan_sweep_linux.go", "Conservative unresolved tuple-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/agent/procsandbox_linux_test.go", "Conservative unresolved tuple-call provenance reaches a controlled non-string test-fixture slice.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/agent/reattach.go", "Conservative unresolved tuple-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/pressure/sample_darwin.go", "Conservative unresolved tuple-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/procstat/procstat.go", "Conservative unresolved tuple-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/project/git.go", "Conservative unresolved tuple-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 5})
	add(kindStringTruncation, "internal/stats/store.go", "Conservative unresolved tuple-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 3})
	add(kindStringTruncation, "internal/sybra/clusterlead/mirror_oob_integration_test.go", "Conservative unresolved tuple-call provenance reaches a controlled non-string test-fixture slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/sybra/svc_agents.go", "Conservative unresolved tuple-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/sybra/svc_tasks.go", "Conservative unresolved tuple-call provenance reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 4})

	// A slice whose base type remains unresolved is conservatively treated as a
	// possible imported named string. These exact existing unknown-base slices
	// keep that fail-closed policy reviewable without weakening production scans.
	add(kindStringTruncation, "cmd/fake-claude/main.go", "Unresolved imported/base type reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "cmd/fake-codex/main.go", "Unresolved imported/base type reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "cmd/fake-copilot/main.go", "Unresolved imported/base type reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "cmd/sybra-cli/evaluation.go", "Unresolved imported/base type reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "cmd/sybra-cli/main.go", "Unresolved imported/base type reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/agent/orphan_sweep_other.go", "Unresolved imported/base type reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/experience/record_test.go", "Unresolved imported/base type reaches a controlled non-string test-fixture slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/learning/store.go", "Unresolved imported/base type reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/monitor/detector.go", "Unresolved imported/base type reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/project/conflict_recovery.go", "Unresolved imported/base type reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/provider/probes.go", "Unresolved imported/base type reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/sybra/app_human_review.go", "Unresolved imported/base type reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/sybra/e2e_bootstrap_test.go", "Unresolved imported/base type reaches a controlled non-string test-fixture slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/sybra/svc_tasks_test.go", "Unresolved imported/base type reaches a controlled non-string test-fixture slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/sybra/svc_tasks.go", "Unresolved imported/base type reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/workflow/engine_parallel_test.go", "Unresolved imported/base type reaches a controlled non-string test-fixture slice.", map[string]int{"slice": 2})
	add(kindStringTruncation, "internal/workflow/engine_steps_prfix.go", "Unresolved imported/base type reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "internal/workflow/engine_steps_verify_checks_test.go", "Unresolved imported/base type reaches a controlled non-string test-fixture slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "scripts/checkatomicwrite/main.go", "Unresolved imported/base type reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})
	add(kindStringTruncation, "scripts/checkgitexec/main.go", "Unresolved imported/base type reaches a pre-existing non-string or parser-boundary slice.", map[string]int{"slice": 1})

	// Test fixtures intentionally spell persisted wire values. They remain
	// exact path/value/count entries so adding or removing a literal forces an
	// explicit review of this ledger instead of receiving a blanket test exemption.
	add(kindProvider, "cmd/fake-claude/main_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 5})
	add(kindProvider, "cmd/sybra-cli/evaluation_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 3})
	add(kindProvider, "cmd/sybra-cli/main_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 11, "codex": 7, "copilot": 4})
	add(kindProvider, "cmd/sybra-server/main_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 2, "codex": 1, "copilot": 1})
	add(kindProvider, "internal/abtest/selector_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 53, "codex": 12, "copilot": 2, "opencode": 1})
	add(kindProvider, "internal/agent/abvariant_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 9, "codex": 11, "copilot": 2})
	add(kindProvider, "internal/agent/allowed_tools_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 2, "codex": 4, "copilot": 3, "opencode": 1})
	add(kindProvider, "internal/agent/background_task_adversarial_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 2})
	add(kindProvider, "internal/agent/copilot_stream_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"copilot": 6})
	add(kindProvider, "internal/agent/discovery_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"codex": 4})
	add(kindProvider, "internal/agent/effort_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 2})
	add(kindProvider, "internal/agent/foreground_command_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 2})
	add(kindProvider, "internal/agent/inspector_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 1})
	add(kindProvider, "internal/agent/k8s_job_runner_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 2, "codex": 4, "opencode": 5})
	add(kindProvider, "internal/agent/logfile_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"codex": 1})
	add(kindProvider, "internal/agent/malformed_tool_call_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 6, "codex": 1})
	add(kindProvider, "internal/agent/manager_gate_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 87, "codex": 58, "copilot": 8, "opencode": 5})
	add(kindProvider, "internal/agent/manager_run_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 41, "codex": 18, "copilot": 3, "opencode": 1})
	add(kindProvider, "internal/agent/manager_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 16, "codex": 25, "copilot": 5})
	add(kindProvider, "internal/agent/model_json_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 2})
	add(kindProvider, "internal/agent/opencode_stream_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"opencode": 5})
	add(kindProvider, "internal/agent/orphan_sweep_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 2})
	add(kindProvider, "internal/agent/procsandbox_linux_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 5, "codex": 4})
	add(kindProvider, "internal/agent/procsandbox_read_darwin_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 2, "codex": 1})
	add(kindProvider, "internal/agent/procsandbox_read_linux_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 2})
	add(kindProvider, "internal/agent/procsandbox_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 5})
	add(kindProvider, "internal/agent/provider_lookup_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 2})
	add(kindProvider, "internal/agent/reattach_effort_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 1})
	add(kindProvider, "internal/agent/reattach_escalation_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 2})
	add(kindProvider, "internal/agent/reattach_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 13})
	add(kindProvider, "internal/agent/registry_roundtrip_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 1, "codex": 1})
	add(kindProvider, "internal/agent/runner_headless_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 99, "codex": 35, "copilot": 9})
	add(kindProvider, "internal/agent/session_filter_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 7, "codex": 8})
	add(kindProvider, "internal/agent/skill_invoke_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 1, "codex": 6, "copilot": 2, "opencode": 1})
	add(kindProvider, "internal/agent/survive_restart_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 19, "codex": 2})
	add(kindProvider, "internal/agent/tool_failure_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 7})
	add(kindProvider, "internal/agent/tool_result_bound_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 2})
	add(kindProvider, "internal/agent/zz_adversarial_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 4, "copilot": 6})
	add(kindProvider, "internal/config/config_providers_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 2, "codex": 1, "copilot": 1, "opencode": 2})
	add(kindProvider, "internal/config/config_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 3, "codex": 4})
	add(kindProvider, "internal/evaluation/scorecard_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 30, "codex": 24, "copilot": 4})
	add(kindProvider, "internal/evaluation/service_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 6})
	add(kindProvider, "internal/evaluation/weakness_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 1, "codex": 3})
	add(kindProvider, "internal/experience/record_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 1, "codex": 2})
	add(kindProvider, "internal/github/check_filter_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"copilot": 1})
	add(kindProvider, "internal/learning/digest_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 4, "codex": 2, "copilot": 1})
	add(kindProvider, "internal/learning/input_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 3, "codex": 1})
	add(kindProvider, "internal/learning/model_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 1})
	add(kindProvider, "internal/llmexec/llmexec_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 12, "codex": 20, "copilot": 9, "opencode": 6})
	add(kindProvider, "internal/llmjob/llmjob_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 31, "codex": 8, "copilot": 4, "opencode": 4})
	add(kindProvider, "internal/loopagent/scheduler_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 1})
	add(kindProvider, "internal/loopagent/store_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 2, "codex": 1, "copilot": 1})
	add(kindProvider, "internal/metrics/metrics_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 15, "codex": 7})
	add(kindProvider, "internal/modeltier/tier_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 2, "codex": 2, "opencode": 1})
	add(kindProvider, "internal/monitor/capacity_dispatch_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"codex": 2})
	add(kindProvider, "internal/monitor/no_capacity_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 3, "codex": 2, "copilot": 2})
	add(kindProvider, "internal/prompteval/promptfoo_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 2})
	add(kindProvider, "internal/provider/health_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 68, "codex": 36, "copilot": 6, "opencode": 4})
	add(kindProvider, "internal/provider/reset_hint_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"codex": 1})
	add(kindProvider, "internal/recovery/recovery_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 2})
	add(kindProvider, "internal/routing/service_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 6, "codex": 5})
	add(kindProvider, "internal/selfmonitor/provider_feedback_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 1, "codex": 2})
	add(kindProvider, "internal/stats/pricing_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 3, "codex": 5, "copilot": 1})
	add(kindProvider, "internal/stats/store_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"codex": 3})
	add(kindProvider, "internal/sybra/agentorch/agentorch_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 18, "codex": 4})
	add(kindProvider, "internal/sybra/agentorch/prompt_render_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"codex": 1})
	add(kindProvider, "internal/sybra/agentorch/scratch_manual_probe_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 1})
	add(kindProvider, "internal/sybra/app_agent_release_lifecycle_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 3})
	add(kindProvider, "internal/sybra/app_human_review_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 3, "codex": 5})
	add(kindProvider, "internal/sybra/app_provider_capacity_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 4, "codex": 2})
	add(kindProvider, "internal/sybra/app_queue_order_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 1})
	add(kindProvider, "internal/sybra/app_startup_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 2})
	add(kindProvider, "internal/sybra/app_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 4})
	add(kindProvider, "internal/sybra/app_tool_ledger_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 1})
	add(kindProvider, "internal/sybra/app_workflow_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 14})
	add(kindProvider, "internal/sybra/completion/completion_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 2, "codex": 4, "copilot": 2})
	add(kindProvider, "internal/sybra/completion/malformed_tool_call_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 2})
	add(kindProvider, "internal/sybra/completion/parked_adoption_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 2})
	add(kindProvider, "internal/sybra/completion/permissions_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"codex": 3})
	add(kindProvider, "internal/sybra/config_diff_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"codex": 1})
	add(kindProvider, "internal/sybra/e2e_autonomy_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 11, "codex": 2})
	add(kindProvider, "internal/sybra/e2e_bestofn_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 1})
	add(kindProvider, "internal/sybra/e2e_bootstrap_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 1})
	add(kindProvider, "internal/sybra/e2e_crossprovider_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 4, "codex": 1})
	add(kindProvider, "internal/sybra/e2e_evaluation_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"codex": 1})
	add(kindProvider, "internal/sybra/e2e_stats_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 5, "codex": 3})
	add(kindProvider, "internal/sybra/e2e_workflow_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 76, "codex": 33, "copilot": 2})
	add(kindProvider, "internal/sybra/lifecycle_metrics_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 9, "codex": 9})
	add(kindProvider, "internal/sybra/monitor_cluster_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 2})
	add(kindProvider, "internal/sybra/orchestrator_resume_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 2})
	add(kindProvider, "internal/sybra/review/fix_dispatch_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 2})
	add(kindProvider, "internal/sybra/review/rebase_recover_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 2, "codex": 3})
	add(kindProvider, "internal/sybra/svc_config_reload_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 3, "codex": 5})
	add(kindProvider, "internal/sybra/svc_info_runtimes_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 5, "codex": 5, "opencode": 2})
	add(kindProvider, "internal/sybra/svc_orchestrator_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 1})
	add(kindProvider, "internal/sybra/svc_reviews_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 1})
	add(kindProvider, "internal/sybra/svc_tasks_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 2, "codex": 2, "copilot": 3})
	add(kindProvider, "internal/sybra/sybra_home_sentinel_e2e_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 1})
	add(kindProvider, "internal/task/model_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 1, "codex": 1, "copilot": 1})
	add(kindProvider, "internal/task/persistence_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 2, "codex": 2})
	add(kindProvider, "internal/task/store_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 2, "codex": 2})
	add(kindProvider, "internal/triage/classifier_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 2})
	add(kindProvider, "internal/umbrella/planner_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 1})
	add(kindProvider, "internal/watchdog/agent_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 10})
	add(kindProvider, "internal/workflow/engine_agent_route_race_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 1})
	add(kindProvider, "internal/workflow/engine_bestofn_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 2})
	add(kindProvider, "internal/workflow/engine_parallel_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 9, "codex": 5})
	add(kindProvider, "internal/workflow/engine_resume_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 18, "codex": 5})
	add(kindProvider, "internal/workflow/engine_runagent_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 13, "codex": 14, "copilot": 3})
	add(kindProvider, "internal/workflow/engine_skill_receipt_zero_output_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 1})
	add(kindProvider, "internal/workflow/engine_steps_core_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 1, "codex": 4})
	add(kindProvider, "internal/workflow/engine_steps_testroute_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"codex": 4})
	add(kindProvider, "internal/workflow/engine_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 1})
	add(kindProvider, "internal/workflow/engine_verifycommits_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 7})
	add(kindProvider, "internal/workflow/engine_workflow_dispatch_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 1})
	add(kindProvider, "internal/workflow/permutation_contract_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 2, "codex": 2})
	add(kindProvider, "internal/workflow/provider_resolution_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 6, "codex": 15, "copilot": 4, "opencode": 2})
	add(kindProvider, "internal/workflow/start_error_test.go", "Test fixture intentionally spells provider wire values to verify parsing, routing, or compatibility.", map[string]int{"claude": 1, "codex": 2})
	add(kindTaskStatus, "cmd/sybra-cli/cli_home_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{})
	add(kindTaskStatus, "cmd/sybra-cli/doctor_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 2})
	add(kindTaskStatus, "cmd/sybra-cli/handoff_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 1, "in-progress": 1, "in-review": 1, "ready-pr": 3, "ready-review": 1, "testing": 2})
	add(kindTaskStatus, "cmd/sybra-cli/httpclient_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"todo": 4})
	add(kindTaskStatus, "cmd/sybra-cli/main_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"blocked": 4, "done": 5, "human-required": 2, "in-progress": 6, "in-review": 1, "ready-pr": 1, "todo": 3})
	add(kindTaskStatus, "cmd/sybra-cli/progress_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"human-required": 2, "todo": 1})
	add(kindTaskStatus, "cmd/sybra-cli/unknown_key_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 2})
	add(kindTaskStatus, "internal/agent/convo_io_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"blocked": 2})
	add(kindTaskStatus, "internal/agent/logfile_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 1})
	add(kindTaskStatus, "internal/agent/manager_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"cancelled": 1})
	add(kindTaskStatus, "internal/agent/model_json_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 2})
	add(kindTaskStatus, "internal/agent/opencode_stream_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 1})
	add(kindTaskStatus, "internal/agent/reattach_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"blocked": 1, "done": 3, "human-required": 4, "in-progress": 2, "todo": 3})
	add(kindTaskStatus, "internal/agent/runner_headless_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 9})
	add(kindTaskStatus, "internal/agent/stream_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 2})
	add(kindTaskStatus, "internal/agent/tool_calls_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 1})
	add(kindTaskStatus, "internal/agentqueue/queue_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"cancelled": 3, "done": 3, "in-progress": 3, "new": 4})
	add(kindTaskStatus, "internal/audit/summary_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 1, "in-progress": 2, "new": 1, "todo": 2})
	add(kindTaskStatus, "internal/autoupdate/autoupdate_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"blocked": 4, "new": 1})
	add(kindTaskStatus, "internal/blocker/blocker_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"blocked": 4, "cancelled": 1, "done": 1, "human-required": 10, "in-progress": 2, "new": 1, "todo": 1})
	add(kindTaskStatus, "internal/evaluation/autonomy_trend_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"human-required": 1, "in-review": 1})
	add(kindTaskStatus, "internal/evaluation/phases_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"blocked": 1, "cancelled": 1, "done": 5, "human-required": 1, "in-progress": 10, "in-review": 9, "new": 1, "plan-review": 1, "planning": 5, "ready-pr": 1, "ready-review": 1, "testing": 3, "todo": 5})
	add(kindTaskStatus, "internal/evaluation/scorecard_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"human-required": 6, "in-progress": 6, "in-review": 14, "testing": 3})
	add(kindTaskStatus, "internal/evaluation/slo_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"human-required": 5, "in-progress": 6, "in-review": 2, "todo": 3})
	add(kindTaskStatus, "internal/experience/store_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"new": 4})
	add(kindTaskStatus, "internal/fsutil/fsutil_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"new": 9})
	add(kindTaskStatus, "internal/github/issue_close_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 2})
	add(kindTaskStatus, "internal/github/rest_monitor_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"blocked": 2})
	add(kindTaskStatus, "internal/github/review_state_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"new": 1})
	add(kindTaskStatus, "internal/harnessevolution/cluster_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"testing": 1})
	add(kindTaskStatus, "internal/health/checks_extra_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 2, "human-required": 4, "in-progress": 7, "in-review": 1, "new": 1, "plan-review": 5, "planning": 2, "todo": 1})
	add(kindTaskStatus, "internal/health/checks_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"human-required": 2, "in-progress": 6, "in-review": 1, "todo": 7})
	add(kindTaskStatus, "internal/health/e2e_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 1, "human-required": 1, "in-progress": 3, "plan-review": 2, "planning": 1})
	add(kindTaskStatus, "internal/health/fingerprint_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"in-review": 2, "todo": 1})
	add(kindTaskStatus, "internal/intervention/fingerprint_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"human-required": 3, "in-progress": 3, "ready-pr": 1})
	add(kindTaskStatus, "internal/intervention/record_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"in-progress": 3, "todo": 2})
	add(kindTaskStatus, "internal/intervention/store_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"human-required": 5, "in-progress": 5})
	add(kindTaskStatus, "internal/limits/collect_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"new": 5})
	add(kindTaskStatus, "internal/limits/store_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"blocked": 1, "new": 2})
	add(kindTaskStatus, "internal/metrics/metrics_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 1, "todo": 1})
	add(kindTaskStatus, "internal/monitor/detector_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"human-required": 1, "in-progress": 2, "new": 1, "plan-review": 3})
	add(kindTaskStatus, "internal/monitor/prompts_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"human-required": 12})
	add(kindTaskStatus, "internal/monitor/remediator_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"human-required": 9, "in-progress": 1, "plan-review": 2})
	add(kindTaskStatus, "internal/project/git_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"new": 1})
	add(kindTaskStatus, "internal/routing/plan_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"new": 5})
	add(kindTaskStatus, "internal/selfmonitor/ledger_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"new": 2})
	add(kindTaskStatus, "internal/sybra/app_dispatch_gate_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"planning": 2})
	add(kindTaskStatus, "internal/sybra/app_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 3})
	add(kindTaskStatus, "internal/sybra/app_watcher_status_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"planning": 1})
	add(kindTaskStatus, "internal/sybra/app_workflow_reason_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"blocked": 1})
	add(kindTaskStatus, "internal/sybra/completion/evidence_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"new": 1})
	add(kindTaskStatus, "internal/sybra/completion/parked_adoption_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 1, "human-required": 2, "todo": 2})
	add(kindTaskStatus, "internal/sybra/e2e_autonomy_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"in-progress": 2})
	add(kindTaskStatus, "internal/sybra/e2e_bestofn_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"human-required": 5})
	add(kindTaskStatus, "internal/sybra/e2e_workflow_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"in-progress": 9, "in-review": 5, "plan-review": 8, "testing": 1, "todo": 1})
	add(kindTaskStatus, "internal/sybra/review/automerge_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"blocked": 2})
	add(kindTaskStatus, "internal/sybra/review/autoresolve_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 1, "in-progress": 1})
	add(kindTaskStatus, "internal/sybra/review/pr_poll_sched_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 1, "new": 2})
	add(kindTaskStatus, "internal/sybra/review/rebase_recover_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 2, "in-progress": 2, "in-review": 1})
	add(kindTaskStatus, "internal/sybra/review/review_hold_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"human-required": 1})
	add(kindTaskStatus, "internal/sybra/review/unit_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 1})
	add(kindTaskStatus, "internal/sybra/svc_orchestrator_replaceable_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"new": 4})
	add(kindTaskStatus, "internal/sybra/svc_planning_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 2, "plan-review": 1, "planning": 1})
	add(kindTaskStatus, "internal/sybra/svc_stats_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 5, "in-review": 3, "todo": 2})
	add(kindTaskStatus, "internal/sybra/svc_tasks_cluster_push_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 1})
	add(kindTaskStatus, "internal/sybra/svc_tasks_dispatch_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"cancelled": 1, "done": 1, "in-progress": 9, "in-review": 6, "ready-pr": 6, "ready-review": 2, "testing": 2})
	add(kindTaskStatus, "internal/sybra/svc_tasks_listfornode_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"in-progress": 3})
	add(kindTaskStatus, "internal/sybra/svc_tasks_lock_timeout_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"blocked": 1})
	add(kindTaskStatus, "internal/sybra/svc_tasks_restart_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"in-progress": 2})
	add(kindTaskStatus, "internal/task/manager_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"in-progress": 1})
	add(kindTaskStatus, "internal/task/parser_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 2})
	add(kindTaskStatus, "internal/task/persistence_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 2, "testing": 2})
	add(kindTaskStatus, "internal/task/store_lock_timeout_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"blocked": 1})
	add(kindTaskStatus, "internal/task/store_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 12})
	add(kindTaskStatus, "internal/umbrella/tags_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"blocked": 1})
	add(kindTaskStatus, "internal/verdict/verdict_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"ready-pr": 1})
	add(kindTaskStatus, "internal/watchdog/agent_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 3})
	add(kindTaskStatus, "internal/workflow/atomic_status_workflow_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 3, "in-progress": 2})
	add(kindTaskStatus, "internal/workflow/bounded_retry_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"human-required": 2, "in-progress": 2})
	add(kindTaskStatus, "internal/workflow/builtin_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"blocked": 2, "done": 2, "human-required": 7, "in-progress": 12, "in-review": 1, "planning": 3, "ready-pr": 3, "ready-review": 2, "todo": 1})
	add(kindTaskStatus, "internal/workflow/condition_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 6, "in-progress": 3, "planning": 2, "todo": 4})
	add(kindTaskStatus, "internal/workflow/effect_id_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"in-review": 1, "plan-review": 1, "todo": 1})
	add(kindTaskStatus, "internal/workflow/effect_reclaim_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"planning": 1})
	add(kindTaskStatus, "internal/workflow/engine_agent_route_race_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"in-progress": 1, "todo": 2})
	add(kindTaskStatus, "internal/workflow/engine_autodispatch_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"in-progress": 1, "todo": 1})
	add(kindTaskStatus, "internal/workflow/engine_bench_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"todo": 5})
	add(kindTaskStatus, "internal/workflow/engine_bestofn_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 2, "human-required": 10, "ready-review": 2, "todo": 3})
	add(kindTaskStatus, "internal/workflow/engine_cascade_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 6, "in-progress": 7, "new": 4, "todo": 3})
	add(kindTaskStatus, "internal/workflow/engine_dispatch_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"human-required": 2, "ready-pr": 2})
	add(kindTaskStatus, "internal/workflow/engine_drain_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"new": 1})
	add(kindTaskStatus, "internal/workflow/engine_events_watchdog_reask_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"blocked": 1, "human-required": 10, "in-progress": 20, "in-review": 2, "planning": 1, "ready-pr": 1, "testing": 5})
	add(kindTaskStatus, "internal/workflow/engine_import_sidecar_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"human-required": 8, "in-progress": 8, "planning": 4})
	add(kindTaskStatus, "internal/workflow/engine_missing_step_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"cancelled": 3, "done": 3, "human-required": 2, "planning": 6})
	add(kindTaskStatus, "internal/workflow/engine_parallel_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 1, "human-required": 1, "todo": 11})
	add(kindTaskStatus, "internal/workflow/engine_prompt_undelivered_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"human-required": 2, "in-progress": 1})
	add(kindTaskStatus, "internal/workflow/engine_resume_pr_steps_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"human-required": 1, "in-progress": 1, "ready-pr": 2, "ready-review": 1})
	add(kindTaskStatus, "internal/workflow/engine_skill_receipt_zero_output_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"blocked": 1, "human-required": 1, "in-progress": 1})
	add(kindTaskStatus, "internal/workflow/engine_stale_route_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 1, "in-progress": 2})
	add(kindTaskStatus, "internal/workflow/engine_steps_admission_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"blocked": 1, "human-required": 9, "in-progress": 13})
	add(kindTaskStatus, "internal/workflow/engine_steps_classify_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"human-required": 5, "new": 5})
	add(kindTaskStatus, "internal/workflow/engine_steps_clear_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"human-required": 7})
	add(kindTaskStatus, "internal/workflow/engine_steps_codegen_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"human-required": 4, "in-progress": 12})
	add(kindTaskStatus, "internal/workflow/engine_steps_evidence_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"blocked": 1, "human-required": 9})
	add(kindTaskStatus, "internal/workflow/engine_steps_focused_checks_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"human-required": 2, "in-progress": 24})
	add(kindTaskStatus, "internal/workflow/engine_steps_parallel_gates_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"blocked": 2, "human-required": 6, "in-progress": 22, "ready-review": 2})
	add(kindTaskStatus, "internal/workflow/engine_steps_pr_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 2, "human-required": 14, "in-progress": 3, "ready-pr": 47})
	add(kindTaskStatus, "internal/workflow/engine_steps_prfix_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"human-required": 14, "in-progress": 29, "in-review": 2})
	add(kindTaskStatus, "internal/workflow/engine_steps_resume_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 2, "human-required": 6, "in-progress": 10, "testing": 3})
	add(kindTaskStatus, "internal/workflow/engine_steps_reviewroute_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"human-required": 2, "planning": 1, "ready-review": 1})
	add(kindTaskStatus, "internal/workflow/engine_steps_sync_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"ready-pr": 10})
	add(kindTaskStatus, "internal/workflow/engine_steps_tamper_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"blocked": 1, "human-required": 13, "in-progress": 41, "ready-review": 5})
	add(kindTaskStatus, "internal/workflow/engine_steps_testroute_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"human-required": 13, "in-progress": 14, "ready-pr": 6, "testing": 29})
	add(kindTaskStatus, "internal/workflow/engine_steps_verify_checks_autofix_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"human-required": 4, "in-progress": 25, "ready-review": 1, "todo": 1})
	add(kindTaskStatus, "internal/workflow/engine_steps_verify_checks_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"blocked": 5, "human-required": 5, "in-progress": 39})
	add(kindTaskStatus, "internal/workflow/engine_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"blocked": 3, "done": 7, "human-required": 18, "in-progress": 18, "in-review": 2, "plan-review": 8, "planning": 3, "ready-pr": 2, "todo": 10})
	add(kindTaskStatus, "internal/workflow/engine_plan_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"human-required": 8, "in-progress": 5, "plan-review": 3, "planning": 20, "todo": 4})
	add(kindTaskStatus, "internal/workflow/engine_pr_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"blocked": 6, "human-required": 1, "in-progress": 18, "in-review": 15, "ready-pr": 12})
	add(kindTaskStatus, "internal/workflow/engine_resume_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"blocked": 8, "cancelled": 6, "done": 8, "human-required": 22, "in-progress": 63, "in-review": 3, "plan-review": 5, "planning": 9, "ready-pr": 1, "testing": 3, "todo": 4})
	add(kindTaskStatus, "internal/workflow/engine_runagent_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 1, "human-required": 1, "in-progress": 5, "plan-review": 1, "planning": 1, "todo": 17})
	add(kindTaskStatus, "internal/workflow/engine_steps_core_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"blocked": 1, "done": 2, "human-required": 6, "in-progress": 10, "plan-review": 8, "planning": 2, "todo": 6})
	add(kindTaskStatus, "internal/workflow/engine_verifycommits_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"human-required": 11, "in-progress": 33})
	add(kindTaskStatus, "internal/workflow/engine_workflow_dispatch_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"blocked": 1, "done": 2, "human-required": 4, "in-progress": 5, "in-review": 6, "new": 2, "plan-review": 1, "planning": 1, "ready-pr": 1, "ready-review": 1, "testing": 1, "todo": 7})
	add(kindTaskStatus, "internal/workflow/engine_validate_plan_contract_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"human-required": 3, "planning": 8})
	add(kindTaskStatus, "internal/workflow/engine_validate_plan_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"human-required": 5, "planning": 5})
	add(kindTaskStatus, "internal/workflow/evidence_attempts_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"in-progress": 4})
	add(kindTaskStatus, "internal/workflow/execution_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"new": 2})
	add(kindTaskStatus, "internal/workflow/handoff_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"in-progress": 1, "ready-pr": 2, "ready-review": 2, "testing": 2})
	add(kindTaskStatus, "internal/workflow/model_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 2})
	add(kindTaskStatus, "internal/workflow/permutation_contract_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"plan-review": 3, "todo": 1})
	add(kindTaskStatus, "internal/workflow/rewind_retry_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"in-progress": 3})
	add(kindTaskStatus, "internal/workflow/sidecar_seed_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"in-review": 1})
	add(kindTaskStatus, "internal/workflow/start_error_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"blocked": 5, "human-required": 8, "in-progress": 35, "todo": 9})
	add(kindTaskStatus, "internal/workflow/state_reducer_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"done": 5, "human-required": 1, "in-review": 2, "plan-review": 5, "testing": 1, "todo": 2})
	add(kindTaskStatus, "internal/workflow/status_reason_utf8_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"human-required": 1, "planning": 1, "todo": 1})
	add(kindTaskStatus, "internal/workflow/store_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"todo": 2})
	add(kindTaskStatus, "internal/workflow/triage_review_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"blocked": 2, "human-required": 5, "testing": 10})
	add(kindTaskStatus, "internal/workflow/zz_adversarial_verify_checks_test.go", "Test fixture intentionally spells persisted task-status wire values to verify workflow or compatibility.", map[string]int{"human-required": 1, "in-progress": 1})

	return out
}

var taskStatuses = statusVocabulary()

var providerNames = providerVocabulary()

func statusVocabulary() map[string]struct{} {
	out := make(map[string]struct{}, len(taskstatus.All()))
	for _, status := range taskstatus.All() {
		out[string(status)] = struct{}{}
	}
	return out
}

func providerVocabulary() map[string]struct{} {
	out := make(map[string]struct{}, len(providerid.All()))
	for _, provider := range providerid.All() {
		out[provider] = struct{}{}
	}
	return out
}

type parsedFile struct {
	path string
	file *ast.File
}

func main() {
	if err := validateAllowances(); err != nil {
		fmt.Fprintln(os.Stderr, "check-consolidated-primitives:", err)
		os.Exit(1)
	}
	fset := token.NewFileSet()
	groups := make(map[string][]parsedFile)
	failed := false
	for _, rawPath := range os.Args[1:] {
		path := filepath.ToSlash(rawPath)
		file, err := parser.ParseFile(fset, rawPath, nil, 0)
		if err != nil {
			fmt.Fprintf(os.Stderr, "check-consolidated-primitives: parse %s: %v\n", path, err)
			failed = true
			continue
		}
		key := filepath.Dir(path) + "\x00" + file.Name.Name
		groups[key] = append(groups[key], parsedFile{path: path, file: file})
	}

	var findings []finding
	for _, files := range groups {
		for _, file := range files {
			var info *types.Info
			if fileNeedsTypeInfo(file) {
				info = collectTypeInfo(file.path, fset, []*ast.File{file.file}, nil)
			}
			findings = append(findings, inspectFile(fset, file.path, file.file, info)...)
		}
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

func fileNeedsTypeInfo(file parsedFile) bool {
	if strings.HasPrefix(file.path, "internal/textutil/") {
		return false
	}
	found := false
	ast.Inspect(file.file, func(node ast.Node) bool {
		if _, ok := node.(*ast.SliceExpr); ok {
			found = true
			return false
		}
		return !found
	})
	return found
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

func collectTypeInfo(pkgPath string, fset *token.FileSet, files []*ast.File, imports types.Importer) *types.Info {
	info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue)}
	config := types.Config{
		Importer: imports,
		// Module-local and generated imports may be unavailable to the source
		// importer. Retain partial local type facts instead of failing open.
		Error: func(error) {},
	}
	_, _ = config.Check(pkgPath, fset, files, info)
	return info
}

func inspectFile(fset *token.FileSet, path string, file *ast.File, info *types.Info) []finding {
	// The checker necessarily contains the literal vocabulary and synthetic
	// examples it searches for. It is its own bootstrap implementation, not a
	// product caller of any consolidated primitive.
	if strings.HasPrefix(path, "scripts/checkconsolidated/") {
		return nil
	}
	var out []finding
	stringNames, stringFields := collectStringNames(file, info)
	ast.Inspect(file, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.SliceExpr:
			if !strings.HasPrefix(path, "internal/textutil/") && (n.Low != nil || n.High != nil) &&
				(isStringExpr(n.X, info, stringNames, stringFields) || isUnresolvedCall(n.X, info) || typeUnknown(n.X, info)) {
				out = append(out, finding{kind: kindStringTruncation, path: path, value: "slice", line: fset.Position(n.Pos()).Line})
			}
		case *ast.BasicLit:
			if n.Kind != token.STRING || isStructTag(file, n) || isImportPath(file, n) {
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

func typeUnknown(expr ast.Expr, info *types.Info) bool {
	return info == nil || info.TypeOf(expr) == nil
}

func isUnresolvedCall(expr ast.Expr, info *types.Info) bool {
	for {
		paren, ok := expr.(*ast.ParenExpr)
		if !ok {
			break
		}
		expr = paren.X
	}
	_, ok := expr.(*ast.CallExpr)
	return ok && (info == nil || info.TypeOf(expr) == nil)
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

func isImportPath(file *ast.File, lit *ast.BasicLit) bool {
	for _, spec := range file.Imports {
		if spec.Path == lit {
			return true
		}
	}
	return false
}

func collectStringNames(file *ast.File, info *types.Info) (names, fields map[string]struct{}) {
	names = make(map[string]struct{})
	fields = make(map[string]struct{})
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
					if len(n.Rhs) == 1 && isUnresolvedCall(n.Rhs[0], info) {
						for _, target := range n.Lhs {
							rememberStringTarget(target, names, fields)
						}
					}
					return true
				}
				for i, rhs := range n.Rhs {
					if !isStringExpr(rhs, info, names, fields) && !isUnresolvedCall(rhs, info) {
						continue
					}
					rememberStringTarget(n.Lhs[i], names, fields)
				}
			case *ast.ValueSpec:
				if len(n.Names) != len(n.Values) {
					if len(n.Values) == 1 && isUnresolvedCall(n.Values[0], info) {
						for _, name := range n.Names {
							names[name.Name] = struct{}{}
						}
					}
					return true
				}
				for i, value := range n.Values {
					if isStringExpr(value, info, names, fields) || isUnresolvedCall(value, info) {
						names[n.Names[i].Name] = struct{}{}
					}
				}
			}
			return true
		})
	}
	return names, fields
}

func rememberStringTarget(expr ast.Expr, names, fields map[string]struct{}) {
	switch target := expr.(type) {
	case *ast.Ident:
		names[target.Name] = struct{}{}
	case *ast.SelectorExpr:
		fields[target.Sel.Name] = struct{}{}
	}
}

func isStringExpr(expr ast.Expr, info *types.Info, names, fields map[string]struct{}) bool {
	if info != nil {
		if typ := info.TypeOf(expr); typ != nil {
			if basic, ok := typ.Underlying().(*types.Basic); ok && basic.Kind() == types.String {
				return true
			}
		}
	}
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
		return isStringExpr(e.X, info, names, fields)
	case *ast.BinaryExpr:
		return e.Op == token.ADD && (isStringExpr(e.X, info, names, fields) || isStringExpr(e.Y, info, names, fields))
	case *ast.SliceExpr:
		return isStringExpr(e.X, info, names, fields)
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
			switch n.Tok {
			case token.INC:
				braceDirections[id.Name] |= 1
			case token.DEC:
				braceDirections[id.Name] |= 2
			default:
				// No other token is valid for ast.IncDecStmt.
			}
		case *ast.AssignStmt:
			if len(n.Lhs) != 1 || len(n.Rhs) != 1 {
				return true
			}
			id, ok := n.Lhs[0].(*ast.Ident)
			if !ok {
				return true
			}
			switch n.Tok {
			case token.ADD_ASSIGN:
				braceDirections[id.Name] |= compoundDirection(false, n.Rhs[0])
			case token.SUB_ASSIGN:
				braceDirections[id.Name] |= compoundDirection(true, n.Rhs[0])
			case token.ASSIGN:
				braceDirections[id.Name] |= assignmentDirection(id.Name, n.Rhs[0])
			default:
				// Other assignment operators cannot encode a signed brace delta.
			}
		}
		return true
	})
	if hasFirst && hasLast {
		return true
	}
	for _, directions := range braceDirections {
		if directions == 3 && bodyHasBothBraces(body) {
			return true
		}
	}
	return false
}

func assignmentDirection(name string, expr ast.Expr) uint8 {
	bin, ok := expr.(*ast.BinaryExpr)
	if !ok {
		return 0
	}
	isName := func(expr ast.Expr) bool {
		id, ok := expr.(*ast.Ident)
		return ok && id.Name == name
	}
	if bin.Op == token.ADD {
		if isName(bin.X) {
			return compoundDirection(false, bin.Y)
		}
		if isName(bin.Y) {
			return compoundDirection(false, bin.X)
		}
	}
	if bin.Op == token.SUB && isName(bin.X) {
		return compoundDirection(true, bin.Y)
	}
	return 0
}

func compoundDirection(subtract bool, delta ast.Expr) uint8 {
	sign := integerSign(delta)
	if sign == 0 {
		// A dynamic delta can move in either direction. Treat it as both so a
		// lookup-table or helper-computed brace delta cannot evade the gate.
		return 3
	}
	if subtract {
		sign = -sign
	}
	if sign > 0 {
		return 1
	}
	return 2
}

func integerSign(expr ast.Expr) int {
	switch value := expr.(type) {
	case *ast.BasicLit:
		if value.Kind != token.INT {
			return 0
		}
		n, err := strconv.ParseInt(value.Value, 0, 64)
		if err != nil || n == 0 {
			return 0
		}
		if n > 0 {
			return 1
		}
		return -1
	case *ast.UnaryExpr:
		if value.Op == token.SUB {
			return -integerSign(value.X)
		}
		if value.Op == token.ADD {
			return integerSign(value.X)
		}
	case *ast.ParenExpr:
		return integerSign(value.X)
	}
	return 0
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
	open, closeBrace := false, false
	ast.Inspect(body, func(node ast.Node) bool {
		lit, ok := node.(*ast.BasicLit)
		if !ok || (lit.Kind != token.STRING && lit.Kind != token.CHAR) {
			return true
		}
		value, err := strconv.Unquote(lit.Value)
		if err == nil {
			open = open || value == "{"
			closeBrace = closeBrace || value == "}"
		}
		return true
	})
	return open && closeBrace
}
