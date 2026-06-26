package umbrella

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// DefaultMaxParallel bounds how many children of one umbrella run at once when
// the planner does not specify a value.
const DefaultMaxParallel = 5

// plannerAttempts is how many times Generate re-asks the model when its output
// fails to parse or validate. The planner is stochastic, so a fresh sample
// often fixes a malformed or incomplete DAG.
const plannerAttempts = 3

// SubIssue is the minimal projection of a GitHub sub-issue the planner needs.
type SubIssue struct {
	Ref    string // issue ref (owner/repo#n or full URL)
	Title  string
	Body   string
	Closed bool // already completed — no child task, satisfies dependents
}

// PlannedChild is one child task the planner proposes: the sub-issue it maps
// to, the issue refs it depends on, and an advisory parallel-track label.
type PlannedChild struct {
	Ref       string   `json:"issue"`
	DependsOn []string `json:"dependsOn"`
	Track     string   `json:"track,omitempty"`
}

// Plan is the dependency DAG the planner extracts from an umbrella body.
type Plan struct {
	Children    []PlannedChild `json:"children"`
	MaxParallel int            `json:"maxParallel"`
}

// Runner executes one planner prompt and returns the model's raw stdout.
// Injected so the planner logic is unit-testable without spawning a CLI.
type Runner func(ctx context.Context, prompt string) (string, error)

// Generate runs the planner end to end: build the prompt, invoke the model,
// then resolve and validate the result against the sub-issues. A malformed or
// invalid plan is retried (the model is stochastic); a runner error is fatal.
// All child and dependency refs in the returned plan are canonical.
func Generate(ctx context.Context, run Runner, umbrellaRef, umbrellaBody string, subs []SubIssue) (Plan, error) {
	if len(subs) == 0 {
		return Plan{}, fmt.Errorf("umbrella %s has no sub-issues to expand", umbrellaRef)
	}
	idx := buildRefIndex(subs)
	prompt := BuildPrompt(umbrellaRef, umbrellaBody, subs)

	var lastErr error
	for range plannerAttempts {
		raw, err := run(ctx, prompt)
		if err != nil {
			return Plan{}, fmt.Errorf("run planner: %w", err)
		}
		plan, err := ParsePlan(raw)
		if err != nil {
			lastErr = err
			continue
		}
		if err := plan.resolve(idx); err != nil {
			lastErr = err
			continue
		}
		if err := plan.validate(subs); err != nil {
			lastErr = err
			continue
		}
		if plan.MaxParallel <= 0 {
			plan.MaxParallel = DefaultMaxParallel
		}
		return plan, nil
	}
	return Plan{}, fmt.Errorf("planner produced no valid plan in %d attempts: %w", plannerAttempts, lastErr)
}

// BuildPrompt renders the planner instruction. The model is asked to emit ONLY
// a JSON object covering every sub-issue, reading the umbrella body for
// dependency edges and parallel tracks.
func BuildPrompt(umbrellaRef, umbrellaBody string, subs []SubIssue) string {
	var b strings.Builder
	b.WriteString("You are decomposing a GitHub umbrella issue into a dependency DAG of its sub-issues.\n")
	b.WriteString("Umbrella: " + umbrellaRef + "\n\n")
	b.WriteString("Umbrella body:\n")
	b.WriteString(umbrellaBody)
	b.WriteString("\n\nSub-issues (reference each by its owner/repo#number):\n")
	for _, s := range subs {
		line := "- " + NormalizeIssueRef(s.Ref) + " — " + strings.TrimSpace(s.Title)
		if s.Closed {
			line += " (already done)"
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\nFor each sub-issue, infer which other sub-issues it depends on (must finish first)")
	b.WriteString(" and an optional parallel-track label. Read dependency markers like \"← #N\"")
	b.WriteString(" (depends on N) and \"⛔ blocks all\", plus prose describing serial vs parallel work.\n\n")
	b.WriteString("Output ONLY a JSON object, no prose, no code fence:\n")
	b.WriteString(`{"children":[{"issue":"<ref>","dependsOn":["<ref>"],"track":"<label>"}],"maxParallel":<int>}` + "\n")
	b.WriteString("Rules: include EVERY sub-issue exactly once (including done ones); dependsOn must")
	b.WriteString(" reference only the sub-issues above; never create a cycle; maxParallel is the max")
	b.WriteString(" children to run at once (default 5).\n")
	return b.String()
}

// ParsePlan extracts a Plan from a model's raw stdout. It tolerates the claude
// `--output-format json` envelope ({"result":"..."}) and a surrounding code
// fence or prose by extracting the first balanced JSON object.
func ParsePlan(raw string) (Plan, error) {
	text := raw
	var env struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err == nil && env.Result != "" {
		text = env.Result
	}

	obj, ok := firstJSONObject(text)
	if !ok {
		return Plan{}, fmt.Errorf("planner output contains no JSON object")
	}
	var plan Plan
	if err := json.Unmarshal([]byte(obj), &plan); err != nil {
		return Plan{}, fmt.Errorf("parse planner JSON: %w", err)
	}
	return plan, nil
}

// firstJSONObject returns the first brace-balanced JSON object substring in s,
// ignoring braces inside string literals. ok is false when none is found.
func firstJSONObject(s string) (string, bool) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return "", false
	}
	depth := 0
	inStr := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\' && inStr:
			escaped = true
		case c == '"':
			inStr = !inStr
		case inStr:
			// skip
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}

// resolve rewrites every child and dependency ref to its canonical sub-issue
// ref, accepting shorthand (`#N`, `N`) and URL forms. It errors loudly on a
// ref that matches no sub-issue rather than silently dropping the edge, so a
// planner mistake becomes a retry instead of a wrong DAG. Self-dependencies
// are dropped (they are noise, not ordering).
func (p *Plan) resolve(idx refIndex) error {
	for i := range p.Children {
		c := &p.Children[i]
		canon, ok := idx.lookup(c.Ref)
		if !ok {
			return fmt.Errorf("planner referenced unknown sub-issue %q", c.Ref)
		}
		c.Ref = canon
		deps := make([]string, 0, len(c.DependsOn))
		for _, d := range c.DependsOn {
			dc, ok := idx.lookup(d)
			if !ok {
				return fmt.Errorf("child %s has unresolved dependency %q", canon, d)
			}
			if dc != canon {
				deps = append(deps, dc)
			}
		}
		c.DependsOn = deps
	}
	return nil
}

// validate confirms the plan covers every sub-issue exactly once and is
// acyclic. resolve must run first so all refs are canonical.
func (p *Plan) validate(subs []SubIssue) error {
	if len(p.Children) != len(subs) {
		return fmt.Errorf("planner covered %d of %d sub-issues", len(p.Children), len(subs))
	}
	seen := make(map[string]bool, len(p.Children))
	nodes := make([]Node, 0, len(p.Children))
	for _, c := range p.Children {
		if seen[c.Ref] {
			return fmt.Errorf("planner listed %s more than once", c.Ref)
		}
		seen[c.Ref] = true
		nodes = append(nodes, Node{ID: c.Ref, Issue: c.Ref, DependsOn: c.DependsOn})
	}
	if Build(nodes).HasCycle() {
		return fmt.Errorf("planner produced a dependency cycle")
	}
	return nil
}

// refIndex resolves an issue ref in any form (URL, owner/repo#n, or bare #n/n)
// to the canonical ref of the sub-issue it names.
type refIndex struct {
	byNorm map[string]string // normalized ref -> canonical
	byNum  map[string]string // issue number -> canonical (only when unambiguous)
}

func buildRefIndex(subs []SubIssue) refIndex {
	idx := refIndex{byNorm: make(map[string]string, len(subs)), byNum: make(map[string]string, len(subs))}
	ambiguous := map[string]bool{}
	for _, s := range subs {
		canon := NormalizeIssueRef(s.Ref)
		idx.byNorm[canon] = canon
		if n := numberOf(canon); n != "" {
			if _, seen := idx.byNum[n]; seen {
				ambiguous[n] = true
			} else {
				idx.byNum[n] = canon
			}
		}
	}
	// A number shared by sub-issues in different repos cannot be resolved from
	// a bare "#N", so drop it from the number index.
	for n := range ambiguous {
		delete(idx.byNum, n)
	}
	return idx
}

func (idx refIndex) lookup(ref string) (string, bool) {
	key := NormalizeIssueRef(ref)
	if c, ok := idx.byNorm[key]; ok {
		return c, true
	}
	if n := numberOf(key); n != "" {
		if c, ok := idx.byNum[n]; ok {
			return c, true
		}
	}
	return "", false
}

// numberOf returns the trailing issue number of a ref ("o/r#12" -> "12",
// "#12" -> "12", "12" -> "12"), or "" when the tail is not all digits.
func numberOf(ref string) string {
	r := ref
	if i := strings.LastIndexByte(r, '#'); i >= 0 {
		r = r[i+1:]
	}
	r = strings.TrimSpace(r)
	if r == "" {
		return ""
	}
	for i := range len(r) {
		if r[i] < '0' || r[i] > '9' {
			return ""
		}
	}
	return r
}
