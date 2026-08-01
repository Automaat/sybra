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
