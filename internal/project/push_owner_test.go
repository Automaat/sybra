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
		name         string
		remotes      map[string]string
		noRepo       bool
		missing      bool
		unreadble    bool
		emptyPath    bool
		relative     bool
		relativeRoot bool
		worktree     bool
		symlinked    bool
		nestedIn     map[string]string
		want         string
		wantErr      bool
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
		{name: "a relative path is refused", relative: true, wantErr: true},
		{name: "a relative path naming a real clone is still refused", relativeRoot: true, remotes: map[string]string{"fork": "https://github.com/sybrabot/app.git"}, wantErr: true},
		{name: "a plain directory inside another repo is refused", nestedIn: map[string]string{"origin": "https://github.com/enclosing/outer.git", "fork": "https://github.com/attacker/outer.git"}, wantErr: true},
		{
			name:     "a worktree root reads its own remotes",
			worktree: true,
			remotes:  map[string]string{"origin": "https://github.com/upstream/app.git"},
			want:     "upstream",
		},
		{
			name:      "a symlinked clone resolves to the same account",
			remotes:   map[string]string{"fork": "https://github.com/sybrabot/app.git"},
			symlinked: true,
			want:      "sybrabot",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Given a clone in the described state
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "app.git")
			switch {
			case tc.emptyPath:
				path = ""
			case tc.relative:
				path = filepath.Join("internal", "project")
			case tc.relativeRoot:
				dir := t.TempDir()
				if err := gitexec.Run(ctx, gitexec.Options{}, "init", "--bare", "-q", filepath.Join(dir, "app.git")); err != nil {
					t.Fatalf("git init: %v", err)
				}
				for remote, url := range tc.remotes {
					if err := gitexec.Run(ctx, gitexec.Options{Dir: filepath.Join(dir, "app.git")}, "remote", "add", remote, url); err != nil {
						t.Fatalf("git remote add %s: %v", remote, err)
					}
				}
				t.Chdir(dir)
				path = "app.git"
			case tc.missing:
			case tc.nestedIn != nil:
				outer := filepath.Dir(path)
				if err := gitexec.Run(ctx, gitexec.Options{}, "init", "-q", outer); err != nil {
					t.Fatalf("git init outer: %v", err)
				}
				for remote, url := range tc.nestedIn {
					if err := gitexec.Run(ctx, gitexec.Options{Dir: outer}, "remote", "add", remote, url); err != nil {
						t.Fatalf("git remote add %s: %v", remote, err)
					}
				}
				if err := os.MkdirAll(filepath.Join(outer, "nested", "clone"), 0o755); err != nil {
					t.Fatalf("mkdir nested: %v", err)
				}
				path = filepath.Join(outer, "nested", "clone")
			case tc.worktree:
				if err := gitexec.Run(ctx, gitexec.Options{}, "init", "-q", path); err != nil {
					t.Fatalf("git init: %v", err)
				}
				for remote, url := range tc.remotes {
					if err := gitexec.Run(ctx, gitexec.Options{Dir: path}, "remote", "add", remote, url); err != nil {
						t.Fatalf("git remote add %s: %v", remote, err)
					}
				}
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
				if tc.symlinked {
					link := filepath.Join(filepath.Dir(path), "link.git")
					if err := os.Symlink(path, link); err != nil {
						t.Fatalf("symlink: %v", err)
					}
					path = link
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
