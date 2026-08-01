package task

import "testing"

// TestUpdateFromMapAcceptsSandboxOffReason pins that disabling the sandbox and
// stating why travel on the same call. UpdateFromMap rejects the whole map on
// an unknown key, so a missing case here would not merely drop the reason — it
// would reject the `sandbox: false` flip alongside it and break the escape
// hatch for anyone who followed the field's documentation.
func TestUpdateFromMapAcceptsSandboxOffReason(t *testing.T) {
	t.Parallel()
	const reason = "docker-in-docker e2e needs host mounts"

	u, err := UpdateFromMap(map[string]any{
		"sandbox":            false,
		"sandbox_off_reason": reason,
	})
	if err != nil {
		t.Fatalf("UpdateFromMap: %v", err)
	}
	if u.Sandbox == nil || *u.Sandbox {
		t.Fatalf("Sandbox = %v, want false", u.Sandbox)
	}
	if u.SandboxOffReason == nil || *u.SandboxOffReason != reason {
		t.Fatalf("SandboxOffReason = %v, want %q", u.SandboxOffReason, reason)
	}
}

func TestUpdateFromMapRejectsNonStringSandboxOffReason(t *testing.T) {
	t.Parallel()
	if _, err := UpdateFromMap(map[string]any{"sandbox_off_reason": 42}); err == nil {
		t.Fatal("want type error for a non-string reason, got nil")
	}
}

// TestSandboxEscapeHatchRequiresReason pins #2778's verification criterion:
// disabling the sandbox without saying why is refused at the boundary an
// operator actually calls, rather than recorded and discovered later. The
// reason is dropped when the hatch is not off, so a task never carries a
// stale justification for a sandbox that is enabled.
func TestSandboxEscapeHatchRequiresReason(t *testing.T) {
	t.Parallel()
	off, on := false, true
	const reason = "docker-in-docker e2e needs host mounts"

	cases := []struct {
		name       string
		task       Task
		wantErr    bool
		wantReason string
	}{
		{
			name:    "disabling without a reason is refused",
			task:    Task{Sandbox: &off},
			wantErr: true,
		},
		{
			name:    "whitespace is not a reason",
			task:    Task{Sandbox: &off, SandboxOffReason: "   "},
			wantErr: true,
		},
		{
			name:       "disabling with a reason is kept, trimmed",
			task:       Task{Sandbox: &off, SandboxOffReason: "  " + reason + " "},
			wantReason: reason,
		},
		{
			name:       "reason is dropped when the sandbox stays on",
			task:       Task{Sandbox: &on, SandboxOffReason: reason},
			wantReason: "",
		},
		{
			name:       "reason is dropped when the hatch is unset",
			task:       Task{SandboxOffReason: reason},
			wantReason: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.task
			err := normalizeSandboxEscapeHatch(&got)
			if tc.wantErr {
				if err == nil {
					t.Fatal("want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("want nil, got %v", err)
			}
			if got.SandboxOffReason != tc.wantReason {
				t.Errorf("SandboxOffReason = %q, want %q", got.SandboxOffReason, tc.wantReason)
			}
		})
	}
}
