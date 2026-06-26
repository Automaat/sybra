package umbrella

import (
	"reflect"
	"testing"
)

func TestNormalizeIssueRef(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"  ", ""},
		{"https://github.com/Automaat/sybra/issues/1129", "automaat/sybra#1129"},
		{"https://github.com/Automaat/sybra/pull/1129", "automaat/sybra#1129"},
		{"https://github.com/Automaat/sybra/issues/1129#issuecomment-99", "automaat/sybra#1129"},
		{"Automaat/sybra#1129", "automaat/sybra#1129"},
		{"automaat/sybra#1129", "automaat/sybra#1129"},
		{"#1129", "#1129"},
		// URL and shorthand for the same issue must normalize identically so a
		// DependsOn written either way matches the dependency's Issue field.
	}
	for _, c := range cases {
		if got := NormalizeIssueRef(c.in); got != c.want {
			t.Errorf("NormalizeIssueRef(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Cross-form equality: the canonical key is spelling-independent.
	if NormalizeIssueRef("https://github.com/Automaat/sybra/issues/7") != NormalizeIssueRef("Automaat/sybra#7") {
		t.Error("URL and shorthand for the same issue normalized differently")
	}
}

func TestParseRef(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in     string
		repo   string
		number int
		ok     bool
	}{
		{"https://github.com/Automaat/sybra/issues/100", "Automaat/sybra", 100, true},
		{"https://github.com/Automaat/sybra/pull/7", "Automaat/sybra", 7, true},
		{"Automaat/sybra#42", "Automaat/sybra", 42, true},
		{"#42", "", 0, false},
		{"not a ref", "", 0, false},
		{"https://notgithub.com/o/r/issues/5", "", 0, false},
	}
	for _, c := range cases {
		repo, n, ok := ParseRef(c.in)
		if repo != c.repo || n != c.number || ok != c.ok {
			t.Errorf("ParseRef(%q) = (%q,%d,%v), want (%q,%d,%v)", c.in, repo, n, ok, c.repo, c.number, c.ok)
		}
	}
}

// node builds a Node; status flags are set by the caller per case.
func node(id, issue, umb string, deps []string, done, awaiting bool) Node {
	return Node{ID: id, Issue: issue, Umbrella: umb, DependsOn: deps, Done: done, Awaiting: awaiting}
}

func TestReadyToRelease(t *testing.T) {
	t.Parallel()
	const umb = "Automaat/sybra#100"
	tests := []struct {
		name  string
		nodes []Node
		want  []string
	}{
		{
			name: "no deps releases immediately",
			nodes: []Node{
				node("a", "o/r#1", umb, nil, false, true),
			},
			want: []string{"a"},
		},
		{
			name: "blocked child held while dep not done",
			nodes: []Node{
				node("root", "o/r#1", umb, nil, false, false), // in progress, not done
				node("dep", "o/r#2", umb, []string{"o/r#1"}, false, true),
			},
			want: nil,
		},
		{
			name: "blocked child released once dep done",
			nodes: []Node{
				node("root", "o/r#1", umb, nil, true, false), // done
				node("dep", "o/r#2", umb, []string{"o/r#1"}, false, true),
			},
			want: []string{"dep"},
		},
		{
			name: "parallel tracks both release after shared root done",
			nodes: []Node{
				node("root", "o/r#1", umb, nil, true, false),
				node("b", "o/r#2", umb, []string{"o/r#1"}, false, true),
				node("c", "o/r#3", umb, []string{"o/r#1"}, false, true),
			},
			want: []string{"b", "c"},
		},
		{
			name: "chain only releases the next link",
			nodes: []Node{
				node("a", "o/r#1", umb, nil, true, false),               // done
				node("b", "o/r#2", umb, []string{"o/r#1"}, false, true), // ready
				node("c", "o/r#3", umb, []string{"o/r#2"}, false, true), // still blocked on b
			},
			want: []string{"b"},
		},
		{
			name: "multi-dep child waits for all",
			nodes: []Node{
				node("a", "o/r#1", umb, nil, true, false),
				node("b", "o/r#2", umb, nil, false, true), // b not done yet
				node("c", "o/r#3", umb, []string{"o/r#1", "o/r#2"}, false, true),
			},
			want: []string{"b"}, // c held until b done; b itself has no deps
		},
		{
			name: "url and shorthand dep forms both resolve",
			nodes: []Node{
				node("a", "https://github.com/o/r/issues/1", umb, nil, true, false),
				node("b", "o/r#2", umb, []string{"o/r#1"}, false, true),
			},
			want: []string{"b"},
		},
		{
			name: "unresolvable dep keeps child blocked",
			nodes: []Node{
				node("b", "o/r#2", umb, []string{"o/r#999"}, false, true),
			},
			want: nil,
		},
		{
			name: "non-awaiting node never released",
			nodes: []Node{
				node("a", "o/r#1", umb, nil, false, false), // not awaiting (already running)
			},
			want: nil,
		},
		{
			name: "cyclic nodes excluded from release",
			nodes: []Node{
				node("x", "o/r#1", umb, []string{"o/r#2"}, false, true),
				node("y", "o/r#2", umb, []string{"o/r#1"}, false, true),
			},
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Build(tc.nodes).ReadyToRelease()
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ReadyToRelease() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCyclicUmbrellas(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		nodes []Node
		want  []string
	}{
		{
			name: "acyclic graph has no cyclic umbrellas",
			nodes: []Node{
				node("a", "o/r#1", "o/r#100", nil, true, false),
				node("b", "o/r#2", "o/r#100", []string{"o/r#1"}, false, true),
			},
			want: nil,
		},
		{
			name: "direct two-node cycle detected",
			nodes: []Node{
				node("x", "o/r#1", "o/r#100", []string{"o/r#2"}, false, true),
				node("y", "o/r#2", "o/r#100", []string{"o/r#1"}, false, true),
			},
			want: []string{"o/r#100"},
		},
		{
			name: "three-node cycle detected",
			nodes: []Node{
				node("x", "o/r#1", "o/r#100", []string{"o/r#2"}, false, true),
				node("y", "o/r#2", "o/r#100", []string{"o/r#3"}, false, true),
				node("z", "o/r#3", "o/r#100", []string{"o/r#1"}, false, true),
			},
			want: []string{"o/r#100"},
		},
		{
			name: "self-dependency is a cycle",
			nodes: []Node{
				node("x", "o/r#1", "o/r#100", []string{"o/r#1"}, false, true),
			},
			want: []string{"o/r#100"},
		},
		{
			// Regression: a cycle member reached through an already-finished
			// node must still be flagged. D (umbrella 200) joins the A→B→C→A
			// cycle via B→D, D→C; a plain DFS back-edge walk drops D and never
			// flags umbrella 200. Tarjan SCC catches it.
			name: "cross-component cycle flags both umbrellas",
			nodes: []Node{
				node("a", "o/r#1", "o/r#100", []string{"o/r#2"}, false, true),
				node("b", "o/r#2", "o/r#100", []string{"o/r#3", "o/r#4"}, false, true),
				node("c", "o/r#3", "o/r#100", []string{"o/r#1"}, false, true),
				node("d", "o/r#4", "o/r#200", []string{"o/r#3"}, false, true),
			},
			want: []string{"o/r#100", "o/r#200"},
		},
		{
			name: "diamond is acyclic",
			nodes: []Node{
				node("a", "o/r#1", "o/r#100", nil, true, false),
				node("b", "o/r#2", "o/r#100", []string{"o/r#1"}, false, true),
				node("c", "o/r#3", "o/r#100", []string{"o/r#1"}, false, true),
				node("d", "o/r#4", "o/r#100", []string{"o/r#2", "o/r#3"}, false, true),
			},
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Build(tc.nodes).CyclicUmbrellas()
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("CyclicUmbrellas() = %v, want %v", got, tc.want)
			}
		})
	}
}
