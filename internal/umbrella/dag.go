// Package umbrella implements the dependency-DAG primitive that gates the
// child tasks of an expanded ☂️ umbrella issue. A child stays `blocked` until
// every task it depends on has reached `done`, then is released in dependency
// order. The logic here is pure: it reads a snapshot of task state and reports
// which children are ready to release or are caught in a dependency cycle,
// leaving all task mutation to the caller (the orchestrator gate).
package umbrella

import (
	"sort"

	"github.com/Automaat/sybra/internal/issueref"
)

// GatedTag marks a child task held in `blocked` specifically by the umbrella
// dependency gate. It distinguishes umbrella gating from the unrelated
// `blocked` the human-review automation uses for a contained Sybra bug, so the
// gate only releases tasks it is responsible for. The expander sets it; the
// gate strips it on release.
const GatedTag = "umbrella-gated"

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
// "owner/repo#number" (lowercased). See issueref.Normalize; retained here as
// the umbrella package's ref-normalization entry point for its many callers.
func NormalizeIssueRef(s string) string { return issueref.Normalize(s) }

// ParseRef splits an issue ref into "owner/repo" and its number. See
// issueref.ParseRef.
func ParseRef(ref string) (repo string, number int, ok bool) { return issueref.ParseRef(ref) }

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

// HasCycle reports whether the graph contains any dependency cycle.
func (g *Graph) HasCycle() bool {
	return len(g.cycleMembers()) > 0
}

// UnresolvedRefs filters refs down to the ones that do not resolve to a known
// done node in the graph — either untracked entirely (the common case for a
// cross-program ref: no Sybra task carries that issue) or tracked but not yet
// Done. Lets a caller name exactly which dependency is still holding a node
// back instead of only reporting the aggregate satisfied/not-satisfied bool
// depsSatisfied computes.
func (g *Graph) UnresolvedRefs(refs []string) []string {
	var out []string
	for _, ref := range refs {
		j, ok := g.byIssue[NormalizeIssueRef(ref)]
		if !ok || !g.nodes[j].Done {
			out = append(out, ref)
		}
	}
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
// dependency cycle. A node is on a cycle iff its strongly-connected component
// has more than one node, or it depends on itself. Computed with Tarjan's SCC
// over the resolved dependency edges — a plain DFS back-edge walk misses cycle
// members reached through an already-finished node. Unresolvable dependency
// refs form no edge.
func (g *Graph) cycleMembers() map[int]bool {
	const unvisited = -1
	n := len(g.nodes)
	idx := make([]int, n)
	low := make([]int, n)
	onStack := make([]bool, n)
	for i := range idx {
		idx[i] = unvisited
	}
	var stack []int
	counter := 0
	onCycle := make(map[int]bool)

	var strongconnect func(v int)
	strongconnect = func(v int) {
		idx[v] = counter
		low[v] = counter
		counter++
		stack = append(stack, v)
		onStack[v] = true

		for _, dep := range g.nodes[v].DependsOn {
			w, ok := g.byIssue[NormalizeIssueRef(dep)]
			if !ok {
				continue
			}
			if w == v {
				onCycle[v] = true // self-loop
			}
			switch {
			case idx[w] == unvisited:
				strongconnect(w)
				low[v] = min(low[v], low[w])
			case onStack[w]:
				low[v] = min(low[v], idx[w])
			}
		}

		if low[v] != idx[v] {
			return
		}
		// v roots an SCC: pop it off the stack.
		var comp []int
		for {
			w := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			onStack[w] = false
			comp = append(comp, w)
			if w == v {
				break
			}
		}
		if len(comp) > 1 {
			for _, w := range comp {
				onCycle[w] = true
			}
		}
	}

	for v := range n {
		if idx[v] == unvisited {
			strongconnect(v)
		}
	}
	return onCycle
}
