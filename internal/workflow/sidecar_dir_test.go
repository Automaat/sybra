package workflow

import "testing"

// TestSidecarDirVarFallsBackToWorktree pins the property that makes the
// template safe to use unconditionally: with no sidecar dir seeded, every
// workflow keeps writing exactly where it did before #2791.
func TestSidecarDirVarFallsBackToWorktree(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		vars map[string]string
		want string
	}{
		{
			name: "sidecar dir wins when set",
			vars: map[string]string{WorkflowVarDir: "/wt", WorkflowVarSidecarDir: "/sandbox"},
			want: "/sandbox",
		},
		{
			name: "falls back to the worktree when unset",
			vars: map[string]string{WorkflowVarDir: "/wt"},
			want: "/wt",
		},
		{
			name: "blank sidecar dir is treated as unset",
			vars: map[string]string{WorkflowVarDir: "/wt", WorkflowVarSidecarDir: "   "},
			want: "/wt",
		},
		{
			name: "neither set yields empty",
			vars: map[string]string{},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := sidecarDirVar(tc.vars); got != tc.want {
				t.Errorf("sidecarDirVar(%v) = %q, want %q", tc.vars, got, tc.want)
			}
		})
	}
}

// TestSidecarDirRendersInTemplates covers the helper through the actual
// template path the builtin workflows use, so a rename or FuncMap slip is
// caught here rather than as a silently-empty path at dispatch.
func TestSidecarDirRendersInTemplates(t *testing.T) {
	t.Parallel()
	got, err := RenderTemplate(`{{sidecardir .Vars}}/.sybra-review-abc.md`, TemplateContext{
		Vars: map[string]string{WorkflowVarDir: "/wt", WorkflowVarSidecarDir: "/sandbox"},
	})
	if err != nil {
		t.Fatalf("RenderTemplate: %v", err)
		panic("unreachable")
	}
	if want := "/sandbox/.sybra-review-abc.md"; got != want {
		t.Fatalf("rendered %q, want %q", got, want)
	}
}
