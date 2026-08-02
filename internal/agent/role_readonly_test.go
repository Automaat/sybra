package agent

import "testing"

// TestJudgesWithoutWritingCoversVerifierRoles pins which roles run against a
// read-only worktree. The split matters: these roles reuse the *same* worktree
// as the implementer, so a writable tree lets a reviewer quietly fix what it
// was asked to judge, and no tool allowlist can prevent it because every role
// has Bash.
func TestJudgesWithoutWritingCoversVerifierRoles(t *testing.T) {
	t.Parallel()
	cases := []struct {
		role Role
		want bool
		why  string
	}{
		{role: RoleReview, want: true, why: "judges the implementer's diff in its worktree"},
		{role: RolePlan, want: true, why: "writes only its plan sidecars"},
		{role: RolePlanCritic, want: true, why: "writes only its critique sidecars"},
		{role: RoleEval, want: true, why: "inspects, never authors"},

		{role: RoleTestRunner, want: false, why: "building and running tests writes into the tree"},
		{role: RoleImplementation, want: false, why: "authors the code"},
		{role: RoleFixReview, want: false, why: "authors fixes"},
		{role: RolePRFix, want: false, why: "authors fixes"},
		{role: RoleTestFix, want: false, why: "authors fixes"},
		{role: RoleHumanReview, want: false, why: "authors code, commits and pushes despite the name"},
		{role: RoleTriage, want: false, why: "no worktree of its own"},
		{role: RoleOrchestrator, want: false, why: "already read-only by its own RunConfig"},
	}
	for _, tc := range cases {
		t.Run(string(tc.role), func(t *testing.T) {
			t.Parallel()
			if got := tc.role.JudgesWithoutWriting(); got != tc.want {
				t.Errorf("%s.JudgesWithoutWriting() = %v, want %v (%s)", tc.role, got, tc.want, tc.why)
			}
		})
	}
}

// TestJudgesWithoutWritingAndAuthorsCodeAreDisjoint guards the invariant tying
// the two role predicates together: a role that commits code can never be one
// whose worktree is read-only, or dispatch would hand it a tree it cannot
// write and every run would fail.
func TestJudgesWithoutWritingAndAuthorsCodeAreDisjoint(t *testing.T) {
	t.Parallel()
	for _, r := range allRoles {
		if r.JudgesWithoutWriting() && r.AuthorsCode() {
			t.Errorf("role %q both authors code and judges without writing", r)
		}
	}
}
