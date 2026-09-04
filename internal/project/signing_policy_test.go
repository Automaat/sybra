package project

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Automaat/sybra/internal/gitexec"
)

func TestProbeGPGSigningExecutesConfiguredSigner(t *testing.T) {
	for _, tc := range []struct {
		name     string
		exitCode int
		wantErr  bool
	}{{name: "usable", exitCode: 0}, {name: "unusable", exitCode: 7, wantErr: true}} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			signer := filepath.Join(root, "fake-gpg")
			if err := os.WriteFile(signer, fmt.Appendf(nil, "#!/bin/sh\ncat >/dev/null\nexit %d\n", tc.exitCode), 0o755); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(root, "gitconfig")
			configBody := fmt.Sprintf("[user]\n\tsigningkey = test-key\n[gpg]\n\tprogram = %s\n", signer)
			if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GIT_CONFIG_GLOBAL", configPath)
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			err := ProbeGPGSigning(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("ProbeGPGSigning() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestProbeGPGSigningReportsMissingKey(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(configPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", configPath)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	err := ProbeGPGSigning(context.Background())
	if err == nil || !strings.Contains(err.Error(), "key is not configured") {
		t.Fatalf("ProbeGPGSigning() error = %v", err)
	}
}

// gpg.format=ssh must follow git's own resolution: gpg.ssh.program signs with
// -Y sign -n git, and the -f argument names a real key file for both a pub
// path and a literal key blob.
func TestProbeGPGSigningSSHFormat(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "probe.pub")
	if err := os.WriteFile(keyPath, []byte("ssh-ed25519 AAAA path\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		key     string
		wantKey string
		staged  bool
		wantErr bool
	}{{name: "pub path", key: keyPath, wantKey: keyPath},
		{name: "tilde path", key: "~/probe.pub", wantKey: keyPath},
		{name: "literal key", key: "ssh-ed25519 AAAA literal", wantKey: "signing_key.pub", staged: true},
		{name: "missing path", key: filepath.Join(dir, "absent.pub"), wantErr: true},
		{name: "unusable signer", key: keyPath, wantKey: keyPath, wantErr: true}} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			argsFile := filepath.Join(root, "args")
			exitCode := map[bool]int{true: 7, false: 0}[tc.name == "unusable signer"]
			script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" >> %s\ncat > /dev/null\nexit %d\n", argsFile, exitCode)
			signer := filepath.Join(root, "fake-ssh-keygen")
			if err := os.WriteFile(signer, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(root, "gitconfig")
			configBody := fmt.Sprintf("[user]\n\tsigningkey = %s\n[gpg]\n\tformat = ssh\n[gpg \"ssh\"]\n\tprogram = %s\n", tc.key, signer)
			if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GIT_CONFIG_GLOBAL", configPath)
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			t.Setenv("HOME", dir)
			err := ProbeGPGSigning(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("ProbeGPGSigning() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			args, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatalf("read args: %v", err)
			}
			argv := strings.Split(strings.TrimSpace(string(args)), "\n")
			if len(argv) != 6 {
				t.Fatalf("fake signer argv = %v, want 6 args", argv)
			}
			if argv[0] != "-Y" || argv[1] != "sign" || argv[2] != "-n" || argv[3] != "git" || argv[4] != "-f" {
				t.Fatalf("fake signer argv = %v", argv)
			}
			if !filepath.IsAbs(argv[5]) {
				t.Fatalf("signing key arg = %q, want an absolute path", argv[5])
			}
			if tc.staged {
				if filepath.Base(argv[5]) != tc.wantKey {
					t.Fatalf("literal key staged at %q, want %s", argv[5], tc.wantKey)
				}
				if _, err := os.Stat(argv[5]); !os.IsNotExist(err) {
					t.Fatalf("staged key file %q still exists after probe", argv[5])
				}
			} else if argv[5] != tc.wantKey {
				t.Fatalf("signing key arg = %q, want %q", argv[5], tc.wantKey)
			}
		})
	}
}

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
