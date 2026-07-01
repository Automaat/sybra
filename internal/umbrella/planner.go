package umbrella

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
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
// to, its change-surface metadata, and an advisory parallel-track label.
// DependsOn may still carry explicit edges the model emits; deriveEdges unions
// in edges derived from Touches/Produces/Requires on top of them.
type PlannedChild struct {
	Ref       string   `json:"issue"`
	DependsOn []string `json:"dependsOn"`
	Track     string   `json:"track,omitempty"`
	Touches   []string `json:"touches,omitempty"`
	Produces  []string `json:"produces,omitempty"`
	Requires  []string `json:"requires,omitempty"`
}

// Plan is the dependency DAG the planner extracts from an umbrella body.
type Plan struct {
	Children    []PlannedChild `json:"children"`
	MaxParallel int            `json:"maxParallel"`
}

// Runner executes one planner prompt and returns the model's raw stdout.
// Injected so the planner logic is unit-testable without spawning a CLI.
type Runner func(ctx context.Context, prompt string) (string, error)

// criticSuffix is appended to the prompt for the single re-ask Generate issues
// when the first valid plan looks suspiciously flat (see flatPlanSuspicious).
const criticSuffix = "You produced a fully-parallel plan — re-examine `touches`/`requires` for overlaps you missed. " +
	"Sub-issues that edit the same files or need each other's symbols must reflect that in their metadata."

// Generate runs the planner end to end: build the prompt, invoke the model,
// then resolve and validate the result against the sub-issues. A malformed or
// invalid plan is retried (the model is stochastic); a runner error is fatal.
// All child and dependency refs in the returned plan are canonical. If the
// first valid plan is suspiciously flat, Generate re-asks the model once with
// a critic nudge; any failure of that re-ask (parse, validate, or run error)
// falls back to the original plan rather than failing the whole expansion.
func Generate(ctx context.Context, run Runner, umbrellaRef, umbrellaBody string, subs []SubIssue) (Plan, error) {
	if len(subs) == 0 {
		return Plan{}, fmt.Errorf("umbrella %s has no sub-issues to expand", umbrellaRef)
	}
	idx := buildRefIndex(subs)
	prompt := BuildPrompt(umbrellaRef, umbrellaBody, subs)

	plan, err := attemptPlan(ctx, run, idx, prompt, subs)
	if err != nil {
		return Plan{}, err
	}
	if flatPlanSuspicious(plan, subs) {
		reasked, err := attemptPlan(ctx, run, idx, prompt+"\n\n"+criticSuffix, subs)
		if err == nil {
			return reasked, nil
		}
	}
	return plan, nil
}

// attemptPlan runs up to plannerAttempts model invocations with the given
// prompt, returning the first plan that parses, resolves, and validates
// against subs, with MaxParallel defaulted. A runner error is fatal and
// returned immediately; a parse or validate failure retries with the same
// prompt.
func attemptPlan(ctx context.Context, run Runner, idx refIndex, prompt string, subs []SubIssue) (Plan, error) {
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
		plan.deriveEdges(subs)
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

// flatPlanSuspicious reports whether a validated plan looks like it skipped
// dependency derivation: 3 or more non-done sub-issues but zero total derived
// edges across all children. Below that threshold small plans are legitimately
// edge-free and are not flagged.
func flatPlanSuspicious(p Plan, subs []SubIssue) bool {
	nonDone := 0
	for _, s := range subs {
		if !s.Closed {
			nonDone++
		}
	}
	if nonDone < 3 {
		return false
	}
	for i := range p.Children {
		if len(p.Children[i].DependsOn) > 0 {
			return false
		}
	}
	return true
}

// BuildPrompt renders the planner instruction. The model is asked to emit ONLY
// a JSON object covering every sub-issue with its change-surface metadata
// (touches/produces/requires); dependency edges are derived from that
// metadata afterward (see deriveEdges), not read from the model.
func BuildPrompt(umbrellaRef, umbrellaBody string, subs []SubIssue) string {
	var b strings.Builder
	b.WriteString("You are decomposing a GitHub umbrella issue into per-sub-issue change-surface metadata.\n")
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
	b.WriteString("\nFor each sub-issue, report its change surface, not a dependency graph — dependency\n")
	b.WriteString("edges are derived automatically from this metadata:\n")
	b.WriteString("  - touches: EVERY file, directory, or package this sub-issue will actually edit")
	b.WriteString(" (e.g. \"internal/foo\", \"internal/bar/x.go\"). List all of them, including paths")
	b.WriteString(" shared with other sub-issues — sub-issues whose touches overlap on the SAME files")
	b.WriteString(" or code area get serialized automatically so they merge one at a time instead of")
	b.WriteString(" colliding. Omitting a shared path to avoid ordering just causes silent merge")
	b.WriteString(" conflicts later. Keep paths as narrow as the real edit (a single package or file),")
	b.WriteString(" not a broad directory that creates false overlap with unrelated siblings.\n")
	b.WriteString("  - produces: symbols, APIs, schemas, or flags it creates or changes (e.g. \"Foo.Run\",")
	b.WriteString(" \"Tier type\").\n")
	b.WriteString("  - requires: symbols, APIs, schemas, or flags it needs another sub-issue to have")
	b.WriteString(" produced first. A sub-issue that requires something another produces will")
	b.WriteString(" automatically depend on it.\n")
	b.WriteString("  - dependsOn (optional): explicit hard-ordering edges the metadata above can't")
	b.WriteString(" express, e.g. markers like \"← #N\" or \"⛔ blocks all\" in the umbrella body. Kept in")
	b.WriteString(" addition to the derived edges, not instead of them.\n\n")
	b.WriteString("Output ONLY a JSON object, no prose, no code fence:\n")
	b.WriteString(`{"children":[{"issue":"<ref>","touches":["<path>"],"produces":["<symbol>"],"requires":["<symbol>"],"dependsOn":["<ref>"],"track":"<label>"}],"maxParallel":<int>}` + "\n")
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

// deriveEdges rewrites each child's DependsOn to union in edges derived from
// change-surface metadata, on top of whatever explicit edges the model already
// emitted: a child requiring a symbol depends on every child that produces it,
// and a child touching a file/package another (earlier-ordered) child also
// touches depends on that earlier child. subs establishes the canonical
// ordering used both to pick the "earlier" side of a touch overlap and to make
// edge derivation deterministic regardless of plan.Children's slice order.
// resolve must run first so every ref here is canonical. Legacy input with no
// touches/produces/requires derives no new edges, but DependsOn is still
// rewritten: it is de-duped and self/empty entries are dropped.
func (p *Plan) deriveEdges(subs []SubIssue) {
	order := make(map[string]int, len(subs))
	for i, s := range subs {
		order[NormalizeIssueRef(s.Ref)] = i
	}

	producers := make(map[string][]string, len(p.Children)) // normalized symbol -> producing refs
	touchesByRef := make(map[string][]string, len(p.Children))
	refs := make([]string, 0, len(p.Children))
	for i := range p.Children {
		c := &p.Children[i]
		refs = append(refs, c.Ref)
		for _, sym := range c.Produces {
			if key := normalizeSymbol(sym); key != "" {
				producers[key] = append(producers[key], c.Ref)
			}
		}
		touchesByRef[c.Ref] = c.Touches
	}
	for key, list := range producers {
		sort.Slice(list, func(a, b int) bool { return order[list[a]] < order[list[b]] })
		producers[key] = list
	}
	// Sorted by subs order (not plan.Children order) so touch-overlap edges are
	// derived the same way regardless of how the model ordered its children.
	sort.Slice(refs, func(a, b int) bool { return order[refs[a]] < order[refs[b]] })

	for i := range p.Children {
		c := &p.Children[i]
		seen := make(map[string]bool, len(c.DependsOn))
		merged := make([]string, 0, len(c.DependsOn))
		add := func(ref string) {
			if ref == "" || ref == c.Ref || seen[ref] {
				return
			}
			seen[ref] = true
			merged = append(merged, ref)
		}
		for _, d := range c.DependsOn {
			add(d)
		}
		for _, req := range c.Requires {
			key := normalizeSymbol(req)
			if key == "" {
				continue
			}
			for _, producer := range producers[key] {
				add(producer)
			}
		}
		if len(c.Touches) > 0 {
			for _, other := range refs {
				if order[other] >= order[c.Ref] {
					continue
				}
				if touchesOverlap(c.Touches, touchesByRef[other]) {
					add(other)
				}
			}
		}
		c.DependsOn = merged
	}
}

// normalizeSymbol canonicalizes a produces/requires entry for comparison.
func normalizeSymbol(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// normalizePath canonicalizes a touches entry for comparison: trims
// whitespace, strips a leading "./", trims a trailing "/", and lowercases
// (matching normalizeSymbol) so casing differences in the LLM's output don't
// hide a real overlap.
func normalizePath(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "./")
	s = strings.TrimSuffix(s, "/")
	return strings.ToLower(s)
}

// pathsOverlap reports whether two touches entries name the same file/package
// or one is a slash-delimited ancestor directory of the other, compared
// segment by segment — never by string prefix, so "internal/foo" does not
// overlap "internal/foobar".
func pathsOverlap(a, b string) bool {
	na, nb := normalizePath(a), normalizePath(b)
	if na == "" || nb == "" {
		return false
	}
	if na == nb {
		return true
	}
	sa := strings.Split(na, "/")
	sb := strings.Split(nb, "/")
	shorter, longer := sa, sb
	if len(sb) < len(sa) {
		shorter, longer = sb, sa
	}
	for i, seg := range shorter {
		if longer[i] != seg {
			return false
		}
	}
	return true
}

// touchesOverlap reports whether any path in a overlaps any path in b.
func touchesOverlap(a, b []string) bool {
	for _, pa := range a {
		for _, pb := range b {
			if pathsOverlap(pa, pb) {
				return true
			}
		}
	}
	return false
}

// validate confirms the plan covers every sub-issue exactly once and is
// acyclic. resolve must run first so all refs are canonical.
func (p *Plan) validate(subs []SubIssue) error {
	if len(p.Children) != len(subs) {
		return fmt.Errorf("planner covered %d of %d sub-issues", len(p.Children), len(subs))
	}
	seen := make(map[string]bool, len(p.Children))
	nodes := make([]Node, 0, len(p.Children))
	for i := range p.Children {
		c := &p.Children[i]
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
