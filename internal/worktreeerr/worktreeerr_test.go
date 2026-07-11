package worktreeerr

import (
	"fmt"
	"testing"
)

func TestIsDiskSpaceError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil is false", err: nil, want: false},
		{
			name: "raw ENOSPC message",
			err:  fmt.Errorf("fetch origin: cannot open 'FETCH_HEAD': No space left on device"),
			want: true,
		},
		{
			name: "wrapped in ErrRebaseFailed",
			err:  fmt.Errorf("%w: reconcile branch with remote: %w", ErrRebaseFailed, fmt.Errorf("write task file: no space left on device")),
			want: true,
		},
		{
			name: "disk quota exceeded",
			err:  fmt.Errorf("write task file: disk quota exceeded"),
			want: true,
		},
		{
			name: "case insensitive",
			err:  fmt.Errorf("git fetch: No Space Left On Device"),
			want: true,
		},
		{
			name: "unrelated rebase failure is false",
			err:  fmt.Errorf("%w: rebase branch onto main: %w", ErrRebaseFailed, fmt.Errorf("local branch diverged from remote head")),
			want: false,
		},
		{
			name: "unrelated network failure is false",
			err:  fmt.Errorf("connection refused"),
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsDiskSpaceError(tc.err); got != tc.want {
				t.Errorf("IsDiskSpaceError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
