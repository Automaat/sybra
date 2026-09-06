package agent

import "testing"

func TestReviewProgressRequiresPortableTerminalProof(t *testing.T) {
	a := &Agent{}
	if !a.CanCaptureReviewProgress() {
		t.Fatal("local workspace verification disabled")
	}
	a.setBackendOwnsCompletion(true)
	a.AppendOutput(StreamEvent{Type: "assistant", Content: "reviewProgressVerified=true"})
	if a.CanCaptureReviewProgress() {
		t.Fatal("provider output substituted for host proof")
	}
	a.mu.Lock()
	a.reviewProgressVerified = true
	a.mu.Unlock()
	if !a.CanCaptureReviewProgress() {
		t.Fatal("verified terminal proof ignored")
	}
}
