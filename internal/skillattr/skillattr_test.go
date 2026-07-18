package skillattr

import "testing"

func TestNormalizeConformanceAcceptsNewStates(t *testing.T) {
	for _, state := range []string{
		ConformanceNone, ConformanceExact, ConformanceFallback,
		ConformanceUnavailable, ConformanceUnverified, ConformanceRecovered,
	} {
		if got := NormalizeConformance(state); got != state {
			t.Fatalf("NormalizeConformance(%q) = %q, want unchanged", state, got)
		}
	}
	if got := NormalizeConformance("bogus"); got != ConformanceUnknown {
		t.Fatalf("NormalizeConformance(bogus) = %q, want %q", got, ConformanceUnknown)
	}
}

func TestReceiptMarkerIncludesSourceHashOnlyWhenPresent(t *testing.T) {
	native := ReceiptMarker("simple-task-implement", "")
	if native != `<!-- sybra-skill-receipt skill="simple-task-implement" -->` {
		t.Fatalf("unexpected native marker: %q", native)
	}
	injected := ReceiptMarker("simple-task-implement", "abcd1234")
	if injected != `<!-- sybra-skill-receipt skill="simple-task-implement" source="abcd1234" -->` {
		t.Fatalf("unexpected injected marker: %q", injected)
	}
}

func TestFindReceipt(t *testing.T) {
	transcript := "some preamble\n" + ReceiptMarker("simple-task-implement", "abcd1234") + "\ntrailer"
	if !FindReceipt(transcript, "simple-task-implement", "abcd1234") {
		t.Fatal("expected receipt to be found")
	}
	if FindReceipt(transcript, "simple-task-implement", "") {
		t.Fatal("marker with a source hash must not match a bare-name lookup")
	}
	if FindReceipt(transcript, "other-skill", "abcd1234") {
		t.Fatal("receipt for a different skill must not match")
	}
	if FindReceipt("no marker here", "simple-task-implement", "abcd1234") {
		t.Fatal("expected no receipt in transcript without a marker")
	}
	if FindReceipt(transcript, "", "abcd1234") {
		t.Fatal("empty skill name must never match")
	}
}

func TestVerifyReceipt(t *testing.T) {
	present := ReceiptMarker("simple-task-implement", "abcd1234")

	cases := []struct {
		name         string
		conformance  string
		transcript   string
		wantVerified string
	}{
		{"exact with receipt stays exact", ConformanceExact, present, ConformanceExact},
		{"exact without receipt is downgraded", ConformanceExact, "no marker", ConformanceUnverified},
		{"fallback with receipt stays fallback", ConformanceFallback, present, ConformanceFallback},
		{"fallback without receipt is downgraded", ConformanceFallback, "no marker", ConformanceUnverified},
		{"unavailable passes through untouched", ConformanceUnavailable, "no marker", ConformanceUnavailable},
		{"none passes through untouched", ConformanceNone, "no marker", ConformanceNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := VerifyReceipt(tc.conformance, tc.transcript, "simple-task-implement", "abcd1234")
			if got != tc.wantVerified {
				t.Fatalf("VerifyReceipt() = %q, want %q", got, tc.wantVerified)
			}
		})
	}
}
