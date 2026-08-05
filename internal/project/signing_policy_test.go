package project

import (
	"context"
	"sync"
	"testing"

	"github.com/Automaat/sybra/internal/gitexec"
)

func TestNormalizeSigningPolicy(t *testing.T) {
	tests := []struct {
		raw  string
		want SigningPolicy
	}{
		{"", SigningAuto},
		{"auto", SigningAuto},
		{"never", SigningNever},
		{"require", SigningRequire},
		{"nonsense", SigningAuto},
	}
	for _, tc := range tests {
		if got := NormalizeSigningPolicy(tc.raw); got != tc.want {
			t.Errorf("NormalizeSigningPolicy(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// SigningNever and SigningRequire must not consult the host at all, so the
// posture a deployment declares cannot flip when a key appears or disappears.
func TestSigningPolicyCommitFlags_IgnoresHostForExplicitPolicies(t *testing.T) {
	ctx := context.Background()
	if got := SigningNever.CommitFlags(ctx); got != "-s" {
		t.Errorf("SigningNever.CommitFlags() = %q, want -s", got)
	}
	if got := SigningRequire.CommitFlags(ctx); got != "-s -S" {
		t.Errorf("SigningRequire.CommitFlags() = %q, want -s -S", got)
	}
	if SigningNever.SignsCommits(ctx) {
		t.Error("SigningNever.SignsCommits() = true, want false")
	}
	if !SigningRequire.SignsCommits(ctx) {
		t.Error("SigningRequire.SignsCommits() = false, want true")
	}
}

func TestConfigureCommitSigning_PinsGpgsignOffUnderNever(t *testing.T) {
	ctx := context.Background()
	bare := t.TempDir()
	if err := gitexec.Run(ctx, gitexec.Options{}, "init", "--bare", bare); err != nil {
		t.Fatalf("init bare: %v", err)
	}

	if err := ConfigureCommitSigning(ctx, bare, SigningNever); err != nil {
		t.Fatalf("ConfigureCommitSigning: %v", err)
	}
	for _, key := range []string{"commit.gpgsign", "tag.gpgsign"} {
		got, err := outputBare(ctx, bare, "config", "--get", key)
		if err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		if got != "false" {
			t.Errorf("%s = %q, want false", key, got)
		}
	}
}

// Under a signing policy the keys are unset, not forced true, so the host's
// own configuration stays authoritative.
func TestConfigureCommitSigning_UnsetsUnderSigningPolicy(t *testing.T) {
	ctx := context.Background()
	bare := t.TempDir()
	if err := gitexec.Run(ctx, gitexec.Options{}, "init", "--bare", bare); err != nil {
		t.Fatalf("init bare: %v", err)
	}
	if err := ConfigureCommitSigning(ctx, bare, SigningNever); err != nil {
		t.Fatalf("seed never: %v", err)
	}

	if err := ConfigureCommitSigning(ctx, bare, SigningRequire); err != nil {
		t.Fatalf("ConfigureCommitSigning(require): %v", err)
	}
	for _, key := range []string{"commit.gpgsign", "tag.gpgsign"} {
		if got, err := outputBare(ctx, bare, "config", "--get", key); err == nil && got != "" {
			t.Errorf("%s = %q, want unset", key, got)
		}
	}
}

// --unset on an absent key exits 5; that is the desired end state, so a
// repeated call must stay green rather than surfacing it as a failure.
func TestConfigureCommitSigning_IdempotentUnset(t *testing.T) {
	ctx := context.Background()
	bare := t.TempDir()
	if err := gitexec.Run(ctx, gitexec.Options{}, "init", "--bare", bare); err != nil {
		t.Fatalf("init bare: %v", err)
	}
	for range 2 {
		if err := ConfigureCommitSigning(ctx, bare, SigningRequire); err != nil {
			t.Fatalf("ConfigureCommitSigning(require): %v", err)
		}
	}
}

// A clone whose config already carries the key twice is the case the plain
// `git config <key> <value>` / `--unset` forms silently fail on: the former
// exits 5 with "cannot overwrite multiple values with a single value", the
// latter exits 5 and leaves both values in place.
func TestConfigureCommitSigning_HandlesMultiValuedKeys(t *testing.T) {
	ctx := context.Background()
	bare := t.TempDir()
	if err := gitexec.Run(ctx, gitexec.Options{}, "init", "--bare", bare); err != nil {
		t.Fatalf("init bare: %v", err)
	}
	for _, v := range []string{"true", "true"} {
		if err := gitexec.Run(ctx, gitexec.Options{Dir: bare}, "config", "--add", "commit.gpgsign", v); err != nil {
			t.Fatalf("seed duplicate commit.gpgsign: %v", err)
		}
	}

	if err := ConfigureCommitSigning(ctx, bare, SigningNever); err != nil {
		t.Fatalf("ConfigureCommitSigning(never): %v", err)
	}
	got, err := outputBare(ctx, bare, "config", "--get-all", "commit.gpgsign")
	if err != nil {
		t.Fatalf("read commit.gpgsign: %v", err)
	}
	if got != "false" {
		t.Errorf("commit.gpgsign = %q, want a single false", got)
	}

	if err := ConfigureCommitSigning(ctx, bare, SigningRequire); err != nil {
		t.Fatalf("ConfigureCommitSigning(require): %v", err)
	}
	if got, err := outputBare(ctx, bare, "config", "--get-all", "commit.gpgsign"); err == nil && got != "" {
		t.Errorf("commit.gpgsign = %q, want unset", got)
	}
}

// SetSigningPolicy runs on the config-reload goroutine while Create and the
// startup migration read the posture, so the field has to be race-safe. Run
// under -race; a plain field fails here.
func TestStoreSigningPolicy_ConcurrentReadWrite(t *testing.T) {
	store, err := NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	var wg sync.WaitGroup
	policies := []SigningPolicy{SigningAuto, SigningNever, SigningRequire}
	for i := range 8 {
		wg.Go(func() {
			for j := range 200 {
				store.SetSigningPolicy(policies[(i+j)%len(policies)])
			}
		})
	}
	for range 8 {
		wg.Go(func() {
			for range 200 {
				if got := store.SigningPolicy(); got == "" {
					t.Errorf("SigningPolicy() returned empty, want a resolved policy")
					return
				}
			}
		})
	}
	wg.Wait()
}
