package prompteval

import "testing"

func TestDigest(t *testing.T) {
	t.Parallel()
	a := Digest([]byte("hello"))
	b := Digest([]byte("hello"))
	if a != b {
		t.Fatalf("Digest not stable: %q != %q", a, b)
	}
	c := Digest([]byte("hello!"))
	if a == c {
		t.Fatalf("Digest did not change for different input")
	}
	if len(a) != 64 {
		t.Fatalf("Digest length = %d, want 64 (hex sha256)", len(a))
	}
}
