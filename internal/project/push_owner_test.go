package project

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Automaat/sybra/internal/gitexec"
)

func TestPushOwner(t *testing.T) {
	tests := []struct {
		name      string
		remotes   map[string]string
		noRepo    bool
		missing   bool
		unreadble bool
		emptyPath bool
		want      string
		wantErr   bool
	}{
		{
			name:    "a fork remote names the account we push to",
			remotes: map[string]string{"origin": "https://github.com/upstream/app.git", "fork": "https://github.com/sybrabot/app.git"},
			want:    "sybrabot",
		},
		{
			name:    "an ssh fork url resolves the same account",
			remotes: map[string]string{"origin": "https://github.com/upstream/app.git", "fork": "git@github.com:sybrabot/app.git"},
			want:    "sybrabot",
		},
		{
			name:    "without a fork the origin account is used",
			remotes: map[string]string{"origin": "https://github.com/upstream/app.git"},
			want:    "upstream",
		},
		{
			name:    "a fork on another host is refused",
			remotes: map[string]string{"origin": "https://github.com/upstream/app.git", "fork": "https://gitlab.com/sybrabot/app.git"},
			wantErr: true,
		},
		{
			name:    "a repo with no remotes is refused",
			remotes: map[string]string{},
			wantErr: true,
		},
		{name: "a plain directory is refused", noRepo: true, wantErr: true},
		{name: "a missing directory is refused", missing: true, wantErr: true},
		{name: "an unreadable repo is refused", remotes: map[string]string{"fork": "https://github.com/sybrabot/app.git"}, unreadble: true, wantErr: true},
		{name: "an empty path is refused", emptyPath: true, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Given a clone in the described state
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "app.git")
			switch {
			case tc.emptyPath:
				path = ""
			case tc.missing:
			case tc.noRepo:
				if err := os.MkdirAll(path, 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
			default:
				if err := gitexec.Run(ctx, gitexec.Options{}, "init", "--bare", "-q", path); err != nil {
					t.Fatalf("git init: %v", err)
				}
				for remote, url := range tc.remotes {
					if err := gitexec.Run(ctx, gitexec.Options{Dir: path}, "remote", "add", remote, url); err != nil {
						t.Fatalf("git remote add %s: %v", remote, err)
					}
				}
				if tc.unreadble {
					if err := os.Chmod(path, 0o000); err != nil {
						t.Fatalf("chmod: %v", err)
					}
					t.Cleanup(func() { _ = os.Chmod(path, 0o755) })
				}
			}

			// When the push owner is resolved
			got, err := PushOwner(ctx, path)

			// Then an account only comes back when the clone really names one
			if tc.wantErr {
				if err == nil {
					t.Fatalf("owner = %q, want an error", got)
				}
				if got != "" {
					t.Errorf("owner = %q, want empty alongside the error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("PushOwner: %v", err)
			}
			if got != tc.want {
				t.Errorf("owner = %q, want %q", got, tc.want)
			}
		})
	}
}
