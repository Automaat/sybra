package umbrella

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// DefaultMaxParallel bounds how many children of one umbrella run at once when
// the planner does not specify a value.
const DefaultMaxParallel = 5

// plannerAttempts is how many times Generate re-asks the model when its output
// fails to parse or validate. The planner is stochastic, so a fresh sample
// often fixes a malformed or incomplete DAG.
const plannerAttempts = 3

// errPlannerExhausted marks attemptPlan's exhaustion return (plannerAttempts
// samples, none parsed/resolved/validated) so Generate can distinguish it from
// a fatal runner error via errors.Is and fall back to a linear chain instead
// of aborting the whole expansion.
var errPlannerExhausted = errors.New("planner exhausted retries")

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
	// ParallelJustification maps a sibling ref to why it is safe to run in
	// parallel with this child (disjoint change surfaces). A non-empty entry
	// on either side of a pair exempts that pair from deriveEdges's
	// serial-default layer; resolve trims and drops blank values so a
	// whitespace-only entry cannot count. The text itself is advisory only
	// beyond that presence check (never further validated or enforced).
	ParallelJustification map[string]string `json:"parallelJustification,omitempty"`
}

// Plan is the dependency DAG the planner extracts from an umbrella body.
type Plan struct {
	Children    []PlannedChild `json:"children"`
	MaxParallel int            `json:"maxParallel"`
	// Fallback marks a plan built by linearChainFallback instead of the model,
	// so callers can surface degradation loudly. Never persisted on the wire.
	Fallback bool `json:"-"`
}

// Runner executes one planner prompt and returns the model's raw stdout.
// Injected so the planner logic is unit-testable without spawning a CLI.
type Runner func(ctx context.Context, prompt string) (string, error)

// generateConfig holds the optional settings GenerateOption values apply.
type generateConfig struct {
	lister  TrackedFilesFunc
	minSubs int
}

// GenerateOption configures an optional Generate behavior.
type GenerateOption func(*generateConfig)

// WithGrounder enables the grounding step: after a plan resolves and before
// dependency edges are derived, each child's touches are confirmed against
// lister's real tracked-file listing for its repo and unioned in. minSubs
// gates grounding on umbrella size (<=0 means always ground once a lister is
// set). Omitting this option leaves Generate's behavior unchanged.
func WithGrounder(lister TrackedFilesFunc, minSubs int) GenerateOption {
	return func(c *generateConfig) {
		c.lister = lister
		c.minSubs = minSubs
	}
}

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
// If the first attempt exhausts plannerAttempts without ever producing a
// valid plan, or the planner deadline expires, Generate falls back to a
// fully-serial linear chain (linearChainFallback) instead of aborting the
// expansion; a fatal runner error (couldn't launch the model) is not
// exhaustion and still aborts.
func Generate(ctx context.Context, run Runner, umbrellaRef, umbrellaBody string, subs []SubIssue, opts ...GenerateOption) (Plan, error) {
	if len(subs) == 0 {
		return Plan{}, fmt.Errorf("umbrella %s has no sub-issues to expand", umbrellaRef)
	}
	var cfg generateConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	idx := buildRefIndex(subs)
	prompt := BuildPrompt(umbrellaRef, umbrellaBody, subs)

	plan, err := attemptPlan(ctx, run, idx, prompt, subs, cfg)
	if err != nil {
		if plannerExhausted(err) {
			return linearChainFallback(subs)
		}
		return Plan{}, err
	}
	if flatPlanSuspicious(plan, subs) {
		reasked, err := attemptPlan(ctx, run, idx, prompt+"\n\n"+criticSuffix, subs, cfg)
		if err == nil {
			return reasked, nil
		}
	}
	return plan, nil
}

func plannerExhausted(err error) bool {
	return errors.Is(err, errPlannerExhausted) || errors.Is(err, context.DeadlineExceeded)
}

// attemptPlan runs up to plannerAttempts model invocations with the given
// prompt, returning the first plan that parses, resolves, and validates
// against subs, with MaxParallel defaulted. A runner error is fatal and
// returned immediately; a parse or validate failure retries with the same
// prompt.
func attemptPlan(ctx context.Context, run Runner, idx refIndex, prompt string, subs []SubIssue, cfg generateConfig) (Plan, error) {
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
		if cfg.lister != nil {
			plan.ground(ctx, cfg.lister, subs, cfg.minSubs)
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
	return Plan{}, fmt.Errorf("planner produced no valid plan in %d attempts: %w: %w", plannerAttempts, errPlannerExhausted, lastErr)
}

// linearChainFallback builds a fully-serial fallback plan for when the
// planner exhausts its retries without producing a valid DAG: sub-issues are
// ordered by numeric issue number (ties and non-numeric refs keep fetch
// order, via a stable sort), and each open child depends on the previous open
// child. A closed sub-issue carries no edges — ChildSpecs already drops any
// dependency on a closed sub-issue as satisfied — so a closed sub-issue in
// the middle of the chain cannot split the remaining open children into
// parallel islands; the open child that follows it still chains to the last
// open child before it. MaxParallel is pinned to 1 and Fallback is set so
// callers can surface the degradation. The constructed plan still runs
// through validate as a real guard: a validation failure is returned rather
// than silently committing a malformed chain.
func linearChainFallback(subs []SubIssue) (Plan, error) {
	type numberedSub struct {
		sub    SubIssue
		num    int
		hasNum bool
	}
	keyed := make([]numberedSub, len(subs))
	for i, s := range subs {
		ns := numberedSub{sub: s}
		if n, err := strconv.Atoi(numberOf(NormalizeIssueRef(s.Ref))); err == nil {
			ns.num, ns.hasNum = n, true
		}
		keyed[i] = ns
	}
	// Sort only the numbered entries by number, then drop them back into the
	// slots they originally occupied. Non-numbered entries never move, so
	// they keep fetch order and numeric entries reorder around them instead
	// of being blocked by a non-numeric ref sitting between them.
	var numberedIdx []int
	for i, ns := range keyed {
		if ns.hasNum {
			numberedIdx = append(numberedIdx, i)
		}
	}
	numbered := make([]numberedSub, len(numberedIdx))
	for i, idx := range numberedIdx {
		numbered[i] = keyed[idx]
	}
	sort.SliceStable(numbered, func(i, j int) bool {
		return numbered[i].num < numbered[j].num
	})
	for i, idx := range numberedIdx {
		keyed[idx] = numbered[i]
	}

	p := Plan{MaxParallel: 1, Fallback: true}
	prevOpen := ""
	for _, ks := range keyed {
		canon := NormalizeIssueRef(ks.sub.Ref)
		child := PlannedChild{Ref: canon}
		if !ks.sub.Closed {
			if prevOpen != "" {
				child.DependsOn = []string{prevOpen}
			}
			prevOpen = canon
		}
		p.Children = append(p.Children, child)
	}
	if err := p.validate(subs); err != nil {
		return Plan{}, fmt.Errorf("linear-chain fallback failed validation: %w", err)
	}
	return p, nil
}

// flatPlanSuspicious reports whether a validated plan looks like it skipped
// dependency derivation: 3 or more non-done sub-issues but zero derived edges
// that will actually survive into the materialized plan. Below that threshold
// small plans are legitimately edge-free and are not flagged. Edges on a
// closed child, or pointing at a closed sub-issue, are excluded from the
// count — ChildSpecs drops both (the closed child gets no task, and a
// dependency on a closed sub-issue is already satisfied), so counting them
// here would suppress the critic re-ask while the materialized plan for the
// remaining non-done children is still fully parallel.
func flatPlanSuspicious(p Plan, subs []SubIssue) bool {
	closed := make(map[string]bool, len(subs))
	nonDone := 0
	for _, s := range subs {
		if s.Closed {
			closed[NormalizeIssueRef(s.Ref)] = true
		} else {
			nonDone++
		}
	}
	if nonDone < 3 {
		return false
	}
	for i := range p.Children {
		c := &p.Children[i]
		if closed[NormalizeIssueRef(c.Ref)] {
			continue
		}
		for _, d := range c.DependsOn {
			if !closed[NormalizeIssueRef(d)] {
				return false
			}
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
	b.WriteString("\nSub-issues of one umbrella in this codebase almost always share code. Assume a\n")
	b.WriteString("dependency exists between any two sub-issues by default — serial execution is the\n")
	b.WriteString("safe, zero-cost default. Running a pair in parallel is the expensive claim: it is\n")
	b.WriteString("only allowed when you back it with a parallelJustification (see below).\n\n")
	b.WriteString("For each sub-issue, report its change surface, not a dependency graph — dependency\n")
	b.WriteString("edges are derived automatically from this metadata:\n")
	b.WriteString("  - touches: EVERY file, directory, or package this sub-issue will actually edit")
	b.WriteString(" (e.g. \"internal/foo\", \"internal/bar/x.go\"). List all of them, including paths")
	b.WriteString(" shared with other sub-issues — sub-issues whose touches overlap on the SAME files")
	b.WriteString(" or code area get serialized automatically so they merge one at a time instead of")
	b.WriteString(" colliding, regardless of any parallelJustification. Omitting a shared path to avoid")
	b.WriteString(" ordering just causes silent merge conflicts later. Keep paths as narrow as the real")
	b.WriteString(" edit (a single package or file), not a broad directory that creates false overlap")
	b.WriteString(" with unrelated siblings.\n")
	b.WriteString("  - produces: symbols, APIs, schemas, or flags it creates or changes (e.g. \"Foo.Run\",")
	b.WriteString(" \"Tier type\").\n")
	b.WriteString("  - requires: symbols, APIs, schemas, or flags it needs another sub-issue to have")
	b.WriteString(" produced first. A sub-issue that requires something another produces will")
	b.WriteString(" automatically depend on it.\n")
	b.WriteString("  - dependsOn (optional): explicit hard-ordering edges the metadata above can't")
	b.WriteString(" express, e.g. markers like \"← #N\" or \"⛔ blocks all\" in the umbrella body. Kept in")
	b.WriteString(" addition to the derived edges, not instead of them.\n")
	b.WriteString("  - parallelJustification (optional): to run this sub-issue in parallel with another,")
	b.WriteString(" add an entry keyed by that sub-issue's ref explaining concretely why their change")
	b.WriteString(" surfaces are disjoint. Without an entry for a pair, that pair defaults to serial")
	b.WriteString(" (the later sub-issue depends on the earlier one). Overlapping touches always")
	b.WriteString(" re-serializes the pair even with a justification present.\n\n")
	b.WriteString("Output ONLY a JSON object, no prose, no code fence:\n")
	b.WriteString(`{"children":[{"issue":"<ref>","touches":["<path>"],"produces":["<symbol>"],"requires":["<symbol>"],"dependsOn":["<ref>"],"parallelJustification":{"<ref>":"<why disjoint>"},"track":"<label>"}],"maxParallel":<int>}` + "\n")
	b.WriteString("Rules: include EVERY sub-issue exactly once (including done ones); dependsOn and")
	b.WriteString(" parallelJustification keys must reference only the sub-issues above; never create a")
	b.WriteString(" cycle; maxParallel is the max children to run at once (default 5).\n")
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
	// Canonicalize parallelJustification keys in a second pass (after every
	// child's Ref is already canonical above). Unlike DependsOn, an unresolved
	// or self-referencing key is dropped silently instead of erroring: a
	// justification only relaxes the serial-default safety net in
	// deriveEdges, it never introduces an ordering constraint, so a malformed
	// key should fail closed to serial rather than abort an otherwise-valid
	// plan. Do not "fix" this into matching DependsOn's loud failure — the two
	// fields have opposite risk profiles by design. Values are trimmed and
	// dropped when blank so a whitespace-only justification can't exempt a
	// pair from the safety net, and so two raw keys canonicalizing to the same
	// ref don't produce a nondeterministic blank/non-blank result.
	for i := range p.Children {
		c := &p.Children[i]
		if len(c.ParallelJustification) == 0 {
			continue
		}
		canon := make(map[string]string, len(c.ParallelJustification))
		for k, v := range c.ParallelJustification {
			ck, ok := idx.lookup(k)
			if !ok || ck == c.Ref {
				continue
			}
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			canon[ck] = v
		}
		c.ParallelJustification = canon
		if len(c.ParallelJustification) == 0 {
			c.ParallelJustification = nil
		}
	}
	return nil
}

// deriveEdges rewrites each child's DependsOn in two passes. Pass 1 unions in
// edges derived from change-surface metadata, on top of whatever explicit
// edges the model already emitted: a child requiring a symbol depends on
// every child that produces it, and a child touching a file/package another
// (earlier-ordered) child also touches depends on that earlier child. Pass 2
// applies a serial-default layer on top of pass 1's fully-merged result: every
// child depends on every earlier canonical sibling unless that pair carries a
// ParallelJustification on either side, or the sibling already transitively
// depends on it (reachability guard, not a direct 2-cycle check) — so a plan
// with no metadata at all is serialized
// end to end rather than treated as all-parallel. subs establishes the
// canonical ordering used to pick the "earlier" side of both the touch
// overlap and the serial-default layer, and to make edge derivation
// deterministic regardless of plan.Children's slice order. resolve must run
// first so every ref (including ParallelJustification keys) here is
// canonical. DependsOn is always rewritten: it is de-duped and self/empty
// entries are dropped even for legacy input with no touches/produces/requires.
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

	p.applySerialDefault(refs, order)
}

// applySerialDefault is pass 2 of deriveEdges: every child depends on every
// earlier canonical sibling unless the pair is justified parallel or that
// would create a cycle. This is a DISTINCT second pass, not grafted into
// pass 1: the cycle guard must read each sibling's DependsOn only after
// pass 1 has fully merged it, otherwise which pairs get serialized would
// depend on iteration order instead of being deterministic.
func (p *Plan) applySerialDefault(refs []string, order map[string]int) {
	byRef := make(map[string]*PlannedChild, len(p.Children))
	for i := range p.Children {
		byRef[p.Children[i].Ref] = &p.Children[i]
	}
	dependsOn := func(c *PlannedChild, ref string) bool {
		return slices.Contains(c.DependsOn, ref)
	}
	justifiedParallel := func(a, b *PlannedChild) bool {
		return a.ParallelJustification[b.Ref] != "" || b.ParallelJustification[a.Ref] != ""
	}
	for _, ref := range refs {
		c := byRef[ref]
		if c == nil {
			continue
		}
		for _, otherRef := range refs {
			if order[otherRef] >= order[ref] {
				continue
			}
			other := byRef[otherRef]
			if other == nil {
				continue
			}
			if justifiedParallel(c, other) {
				continue
			}
			if reachable(byRef, otherRef, ref, map[string]bool{}) {
				continue // other already transitively depends on ref; adding the reverse edge would close a cycle
			}
			if !dependsOn(c, otherRef) {
				c.DependsOn = append(c.DependsOn, otherRef)
			}
		}
	}
}

// reachable reports whether fromRef transitively depends on target by
// walking the (partially built) DependsOn graph. Used instead of a direct
// membership check so a serial-default edge is never added if it would close
// a longer cycle through edges pass 1 (or an earlier pass-2 iteration)
// already created.
func reachable(byRef map[string]*PlannedChild, fromRef, target string, visiting map[string]bool) bool {
	if fromRef == target {
		return true
	}
	if visiting[fromRef] {
		return false
	}
	visiting[fromRef] = true
	from := byRef[fromRef]
	if from == nil {
		return false
	}
	for _, dep := range from.DependsOn {
		if reachable(byRef, dep, target, visiting) {
			return true
		}
	}
	return false
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
