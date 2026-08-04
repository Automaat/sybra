package llmexec

import "testing"

func TestOverloadedRecognizesOnlyStandalone529(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		text string
		want bool
	}{
		{"HTTP 529 provider overloaded", true},
		{"provider is overloaded", true},
		{"token count 1529 exceeded", false},
		{"stack frame at 5290", false},
	} {
		if got := overloaded(tt.text); got != tt.want {
			t.Errorf("overloaded(%q) = %v, want %v", tt.text, got, tt.want)
		}
	}
}
