package attribution

import "testing"

func TestAppend(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "empty body untouched",
			in:   "",
			want: "",
		},
		{
			name: "whitespace-only body untouched",
			in:   "  \n\t",
			want: "  \n\t",
		},
		{
			name: "plain body gets footer after blank line",
			in:   "Fixed the off-by-one.",
			want: "Fixed the off-by-one.\n\n" + Footer,
		},
		{
			name: "trailing whitespace trimmed before footer",
			in:   "Fixed the off-by-one.\n\n",
			want: "Fixed the off-by-one.\n\n" + Footer,
		},
		{
			name: "idempotent when footer already present",
			in:   "Fixed the off-by-one.\n\n" + Footer,
			want: "Fixed the off-by-one.\n\n" + Footer,
		},
		{
			name: "idempotent with trailing whitespace after footer",
			in:   "Fixed the off-by-one.\n\n" + Footer + "\n",
			want: "Fixed the off-by-one.\n\n" + Footer + "\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Append(tt.in); got != tt.want {
				t.Errorf("Append(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
