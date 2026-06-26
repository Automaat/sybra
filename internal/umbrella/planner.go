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

// SubIssue is the minimal projection of a GitHub sub-issue the planner needs.
type SubIssue struct {
	Ref   string // issue ref (owner/repo#n or full URL)
	Title string
	Body  string
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
// parse and normalize the result, and validate it against the sub-issues.
func Generate(ctx context.Context, run Runner, umbrellaRef, umbrellaBody string, subs []SubIssue) (Plan, error) {
	if len(subs) == 0 {
		return Plan{}, fmt.Errorf("umbrella %s has no sub-issues to expand", umbrellaRef)
	}
	raw, err := run(ctx, BuildPrompt(umbrellaRef, umbrellaBody, subs))
	if err != nil {
		return Plan{}, fmt.Errorf("run planner: %w", err)
	}
	plan, err := ParsePlan(raw)
	if err != nil {
		return Plan{}, err
	}
	plan.normalize(subs)
	if err := plan.validate(subs); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

// BuildPrompt renders the planner instruction. The model is asked to emit ONLY
// a JSON object using the exact sub-issue refs provided, reading the umbrella
// body for dependency edges and parallel tracks.
func BuildPrompt(umbrellaRef, umbrellaBody string, subs []SubIssue) string {
	var b strings.Builder
	b.WriteString("You are decomposing a GitHub umbrella issue into a dependency DAG of its sub-issues.\n")
	b.WriteString("Umbrella: " + umbrellaRef + "\n\n")
	b.WriteString("Umbrella body:\n")
	b.WriteString(umbrellaBody)
	b.WriteString("\n\nSub-issues (use these exact refs):\n")
	for _, s := range subs {
		b.WriteString("- " + s.Ref + " — " + strings.TrimSpace(s.Title) + "\n")
	}
	b.WriteString("\nFrom the umbrella body, infer for each sub-issue which other sub-issues it depends on")
	b.WriteString(" (must finish first) and an optional parallel-track label. Read dependency markers")
	b.WriteString(" like \"← #N\" (depends on N) and \"⛔ blocks all\", plus any prose describing")
	b.WriteString(" serial vs parallel work.\n\n")
	b.WriteString("Output ONLY a JSON object, no prose, no code fence:\n")
	b.WriteString(`{"children":[{"issue":"<ref>","dependsOn":["<ref>"],"track":"<label>"}],"maxParallel":<int>}` + "\n")
	b.WriteString("Rules: include every sub-issue exactly once; dependsOn must reference only the refs above;")
	b.WriteString(" never create a cycle; maxParallel is the max children to run at once (default 5).\n")
	return b.String()
}

// ParsePlan extracts a Plan from a model's raw stdout. It tolerates the
// claude `--output-format json` envelope ({"result":"..."}) and a surrounding
// code fence or prose by extracting the first balanced JSON object.
func ParsePlan(raw string) (Plan, error) {
	text := raw
	// Unwrap the claude json envelope if present.
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

// normalize fills defaults and drops anything that does not map onto a real
// sub-issue, so a planner that hallucinates a ref or omits maxParallel still
// yields a usable plan rather than failing validation outright.
func (p *Plan) normalize(subs []SubIssue) {
	if p.MaxParallel <= 0 {
		p.MaxParallel = DefaultMaxParallel
	}
	known := make(map[string]bool, len(subs))
	for _, s := range subs {
		known[NormalizeIssueRef(s.Ref)] = true
	}
	children := p.Children[:0]
	for _, c := range p.Children {
		if !known[NormalizeIssueRef(c.Ref)] {
			continue
		}
		deps := c.DependsOn[:0]
		for _, d := range c.DependsOn {
			if known[NormalizeIssueRef(d)] && NormalizeIssueRef(d) != NormalizeIssueRef(c.Ref) {
				deps = append(deps, d)
			}
		}
		c.DependsOn = deps
		children = append(children, c)
	}
	p.Children = children
}

// validate confirms the plan covers every sub-issue exactly once and is
// acyclic. normalize must run first so refs are already constrained to the
// sub-issue set.
func (p *Plan) validate(subs []SubIssue) error {
	if len(p.Children) != len(subs) {
		return fmt.Errorf("planner covered %d of %d sub-issues", len(p.Children), len(subs))
	}
	seen := make(map[string]bool, len(p.Children))
	nodes := make([]Node, 0, len(p.Children))
	for _, c := range p.Children {
		key := NormalizeIssueRef(c.Ref)
		if seen[key] {
			return fmt.Errorf("planner listed %s more than once", c.Ref)
		}
		seen[key] = true
		nodes = append(nodes, Node{ID: c.Ref, Issue: c.Ref, DependsOn: c.DependsOn})
	}
	if Build(nodes).HasCycle() {
		return fmt.Errorf("planner produced a dependency cycle")
	}
	return nil
}
