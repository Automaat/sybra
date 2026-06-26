// Package umbrella implements the dependency-DAG primitive that gates the
// child tasks of an expanded ☂️ umbrella issue. A child stays `blocked` until
// every task it depends on has reached `done`, then is released in dependency
// order. The logic here is pure: it reads a snapshot of task state and reports
// which children are ready to release or are caught in a dependency cycle,
// leaving all task mutation to the caller (the orchestrator gate).
package umbrella

import (
	"slices"
	"sort"
	"strings"
)

// Node is the minimal projection of a task the dependency DAG reasons about.
type Node struct {
	ID        string   // task ID
	Issue     string   // issue ref this task implements (URL or shorthand); may be empty
	Umbrella  string   // umbrella issue ref this task belongs to; empty for non-children
	DependsOn []string // issue refs this task waits on
	Done      bool     // task has reached a satisfied (done) state
	Awaiting  bool     // task is currently held in `blocked`, awaiting release
}

// Graph is a resolved dependency graph over a snapshot of nodes. Build it with
// Build; it is read-only thereafter.
type Graph struct {
	nodes   []Node
	byIssue map[string]int // normalized issue ref -> index into nodes
}

// NormalizeIssueRef canonicalizes an issue URL or shorthand to
// "owner/repo#number" (lowercased) so a DependsOn entry matches a task's Issue
// field regardless of the spelling used to write it. A full GitHub issue/PR
// URL collapses to that form; anything else is lowercased and trimmed so
// shorthand like "Automaat/sybra#12" still matches a URL-form Issue. Empty in,
// empty out.
func NormalizeIssueRef(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if _, after, ok := strings.Cut(s, "github.com/"); ok {
		parts := strings.Split(after, "/")
		if len(parts) >= 4 && (parts[2] == "issues" || parts[2] == "pull") {
			if num := leadingDigits(parts[3]); num != "" {
				return strings.ToLower(parts[0]+"/"+parts[1]) + "#" + num
			}
		}
	}
	return strings.ToLower(s)
}

// leadingDigits returns the run of ASCII digits at the start of s (empty if
// none). Trims trailing URL noise like "#issuecomment-..." or "?foo" off an
// issue number.
func leadingDigits(s string) string {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return s[:i]
		}
	}
	return s
}

// Build indexes nodes by their normalized issue ref for dependency resolution.
// On duplicate issue refs the first node wins, so a stray duplicate cannot
// shadow the canonical task.
func Build(nodes []Node) *Graph {
	g := &Graph{nodes: nodes, byIssue: make(map[string]int, len(nodes))}
	for i := range nodes {
		key := NormalizeIssueRef(nodes[i].Issue)
		if key == "" {
			continue
		}
		if _, ok := g.byIssue[key]; !ok {
			g.byIssue[key] = i
		}
	}
	return g
}

// ReadyToRelease returns the IDs of awaiting (blocked) child nodes whose every
// resolvable dependency is done, excluding any node on a dependency cycle. The
// result is sorted for deterministic dispatch. A node with an unresolvable
// dependency stays held until that dependency appears.
func (g *Graph) ReadyToRelease() []string {
	onCycle := g.cycleMembers()
	var ready []string
	for i := range g.nodes {
		if !g.nodes[i].Awaiting || onCycle[i] {
			continue
		}
		if g.depsSatisfied(i) {
			ready = append(ready, g.nodes[i].ID)
		}
	}
	sort.Strings(ready)
	return ready
}

// CyclicUmbrellas returns the umbrella refs that contain at least one child on
// a dependency cycle. The caller flips those trackers to human-required since
// the chain can never make progress on its own. Result is sorted.
func (g *Graph) CyclicUmbrellas() []string {
	onCycle := g.cycleMembers()
	seen := map[string]bool{}
	var out []string
	for i := range g.nodes {
		if !onCycle[i] {
			continue
		}
		u := g.nodes[i].Umbrella
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

// depsSatisfied reports whether every dependency of node i resolves to a known
// node that is done. An unresolvable dependency is treated as unsatisfied so a
// child is never released ahead of a dependency that has not been materialized
// yet.
func (g *Graph) depsSatisfied(i int) bool {
	for _, dep := range g.nodes[i].DependsOn {
		j, ok := g.byIssue[NormalizeIssueRef(dep)]
		if !ok || !g.nodes[j].Done {
			return false
		}
	}
	return true
}

// cycleMembers returns the set of node indices that lie on at least one
// dependency cycle, via DFS three-coloring over the resolved dependency edges.
// Unresolvable dependency refs are skipped (they form no edge).
func (g *Graph) cycleMembers() map[int]bool {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make([]int8, len(g.nodes))
	onCycle := make(map[int]bool)
	var stack []int

	var dfs func(i int)
	dfs = func(i int) {
		color[i] = gray
		stack = append(stack, i)
		for _, dep := range g.nodes[i].DependsOn {
			j, ok := g.byIssue[NormalizeIssueRef(dep)]
			if !ok {
				continue
			}
			switch color[j] {
			case gray:
				// Back-edge: every node from j to the top of the stack is on a cycle.
				for _, n := range slices.Backward(stack) {
					onCycle[n] = true
					if n == j {
						break
					}
				}
			case white:
				dfs(j)
			}
		}
		stack = stack[:len(stack)-1]
		color[i] = black
	}

	for i := range g.nodes {
		if color[i] == white {
			dfs(i)
		}
	}
	return onCycle
}
