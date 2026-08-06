package workflow

import "testing"

func TestDeclaresAlreadyFixedOnMain(t *testing.T) {
	for _, tc := range []struct {
		name   string
		signal string
		want   bool
	}{
		{
			name:   "structured already fixed",
			signal: "Checked the base branch.\n\n```sybra-recovery\n{\"decision\":\"already-fixed-on-main\",\"reason\":\"landed in an earlier PR\"}\n```",
			want:   true,
		},
		{
			name:   "structured none",
			signal: "```sybra-recovery\n{\"decision\":\"none\"}\n```",
			want:   false,
		},
		{
			name:   "structured none wins over prose",
			signal: "This is a duplicate task, already fixed on main.\n\n```sybra-recovery\n{\"decision\":\"none\"}\n```",
			want:   false,
		},
		{
			name:   "legacy prose already fixed plus base branch",
			signal: "Already fixed on main; nothing left to do.",
			want:   true,
		},
		{
			name:   "legacy prose duplicate plus close request",
			signal: "Duplicate task, safe to close.",
			want:   true,
		},
		{
			name:   "remaining does not corroborate",
			signal: "Already fixed the remaining lint errors in the helper.",
			want:   false,
		},
		{
			name:   "maintain does not corroborate",
			signal: "Already fixed the parser so we can maintain one code path.",
			want:   false,
		},
		{
			name:   "domain does not corroborate",
			signal: "Already fixed the domain model validation.",
			want:   false,
		},
		{
			name:   "base branch mention without an already-landed phrase",
			signal: "Rebased onto origin/main and pushed the branch.",
			want:   false,
		},
		{
			name:   "empty",
			signal: "",
			want:   false,
		},
		{
			name:   "whitespace",
			signal: " \n\t ",
			want:   false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := declaresAlreadyFixedOnMain(tc.signal)
			if err != nil {
				t.Fatalf("declaresAlreadyFixedOnMain(%q) returned error: %v", tc.signal, err)
			}
			if got != tc.want {
				t.Fatalf("declaresAlreadyFixedOnMain(%q) = %v, want %v", tc.signal, got, tc.want)
			}
		})
	}
}

func TestDeclaresAlreadyFixedOnMainUnreadableBlock(t *testing.T) {
	for _, tc := range []struct {
		name   string
		signal string
	}{
		{
			name:   "malformed json",
			signal: "```sybra-recovery\n{\"decision\": already-fixed-on-main}\n```",
		},
		{
			name:   "unknown decision",
			signal: "```sybra-recovery\n{\"decision\":\"close-it\"}\n```",
		},
		{
			name:   "missing decision",
			signal: "```sybra-recovery\n{\"reason\":\"it is a duplicate\"}\n```",
		},
		{
			name:   "unreadable block outranks corroborating prose",
			signal: "Already fixed on main, safe to close.\n\n```sybra-recovery\n{\"decision\":\n```",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := declaresAlreadyFixedOnMain(tc.signal)
			if err == nil {
				t.Fatalf("declaresAlreadyFixedOnMain(%q) = %v, want an error", tc.signal, got)
			}
			if got {
				t.Fatalf("declaresAlreadyFixedOnMain(%q) = true on error, want false", tc.signal)
			}
		})
	}
}
