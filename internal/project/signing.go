package project

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/gitexec"
)

// SigningPolicy is a deployment's declared posture on GPG-signing agent
// commits. It exists because inferring the posture from ambient host state
// alone is silent and reversible: dropping a key onto a host that must never
// sign would flip behavior with no code or config change.
type SigningPolicy string

const (
	// SigningAuto signs when the host resolves a signing key, and does not
	// otherwise. The historical (and default) behavior.
	SigningAuto SigningPolicy = "auto"
	// SigningNever never signs, whatever the host has configured. Used on
	// unattended hosts (e.g. the Linux server) that hold no key.
	SigningNever SigningPolicy = "never"
	// SigningRequire always signs, even where the host resolves no key: an
	// operator who asks for signatures gets a loud commit failure rather than
	// silently unsigned history. Config load cannot reject it on a keyless
	// host without making validation depend on host state.
	SigningRequire SigningPolicy = "require"
)

// NormalizeSigningPolicy maps a raw config value onto a known policy. Empty
// and unrecognized values resolve to SigningAuto so a typo degrades to the
// historical behavior instead of silently disabling signing.
func NormalizeSigningPolicy(raw string) SigningPolicy {
	switch SigningPolicy(raw) {
	case SigningNever:
		return SigningNever
	case SigningRequire:
		return SigningRequire
	default:
		return SigningAuto
	}
}

// SignsCommits reports whether commits under this policy should carry -S.
func (p SigningPolicy) SignsCommits(ctx context.Context) bool {
	switch p {
	case SigningNever:
		return false
	case SigningRequire:
		return true
	default:
		return GPGSigningAvailable(ctx)
	}
}

// CommitFlags returns the git commit flags an agent should use under this
// policy: "-s -S" when signing, otherwise "-s". The prepare-commit-msg hook
// enforces -s regardless; this only governs the optional GPG flag.
func (p SigningPolicy) CommitFlags(ctx context.Context) string {
	if p.SignsCommits(ctx) {
		return "-s -S"
	}
	return "-s"
}

// GPGSigningAvailable reports whether this machine can GPG-sign commits, i.e.
// whether git resolves a non-empty user.signingkey. Used to decide whether an
// agent commit instruction should carry -S: on a keyless host (e.g. the Linux
// server) `git commit -S` fails with "gpg failed to sign the data", so callers
// emit only -s there. DCO sign-off (-s) is guaranteed independently by the
// prepare-commit-msg hook (see InstallSignoffHook).
//
// This probes the host only. Prefer SigningPolicy.SignsCommits where a
// configured posture is available, so an explicit "never" is honored on a host
// that does happen to hold a key.
func GPGSigningAvailable(ctx context.Context) bool {
	out, err := gitexec.Output(ctx, gitexec.Options{}, "config", "--global", "--get", "user.signingkey")
	if err != nil {
		return false
	}
	return out != ""
}

// ProbeGPGSigning performs a bounded, non-persisting signature with the exact
// configured signing key, following git's own resolution: gpg.format selects
// openpgp (gpg.program) or ssh (gpg.ssh.program). A configured key name alone
// is not proof that its secret key or signing agent is usable by an unattended
// provider process.
func ProbeGPGSigning(ctx context.Context) error {
	key, err := gitexec.Output(ctx, gitexec.Options{}, "config", "--global", "--get", "user.signingkey")
	if err != nil {
		if code, ok := gitexec.ExitCode(err); ok && code == 1 {
			return errors.New("resolve signing key: key is not configured")
		}
		return fmt.Errorf("resolve signing key: %w", err)
	}
	if strings.TrimSpace(key) == "" {
		return errors.New("resolve signing key: key is not configured")
	}
	format, _ := gitexec.Output(ctx, gitexec.Options{}, "config", "--global", "--get", "gpg.format")
	switch format {
	case "", "openpgp":
		return probeOpenPGPSigning(ctx, key)
	case "ssh":
		return probeSSHSigning(ctx, key)
	default:
		return fmt.Errorf("unsupported signing format %q for non-persisting probe", format)
	}
}

func probeOpenPGPSigning(ctx context.Context, key string) error {
	program, _ := gitexec.Output(ctx, gitexec.Options{}, "config", "--global", "--get", "gpg.program")
	if strings.TrimSpace(program) == "" {
		program = "gpg"
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, program,
		"--batch", "--no-tty", "--pinentry-mode", "error",
		"--local-user", key, "--detach-sign", "--armor", "--output", "-")
	cmd.Stdin = strings.NewReader("sybra run environment signing probe\n")
	if output, runErr := cmd.CombinedOutput(); runErr != nil {
		return fmt.Errorf("signing probe: %w: %s", runErr, strings.TrimSpace(string(output)))
	}
	return nil
}

// probeSSHSigning signs with ssh-keygen -Y sign, mirroring what git runs for
// gpg.format=ssh. ssh-keygen resolves the private key from the file given to
// -f or, for OpenSSH >= 8.9, from the agent by public-key fingerprint, so a
// pub-file path or a literal pubkey blob both work with an agent-held key.
func probeSSHSigning(ctx context.Context, key string) error {
	program, _ := gitexec.Output(ctx, gitexec.Options{}, "config", "--global", "--get", "gpg.ssh.program")
	if strings.TrimSpace(program) == "" {
		program = "ssh-keygen"
	}
	keyFile, cleanup, err := resolveSSHKeyFile(key)
	if err != nil {
		return err
	}
	defer cleanup()
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, program, "-Y", "sign", "-n", "git", "-f", keyFile)
	// An unattended process can never answer a passphrase prompt, so force the
	// askpass helper to a failing stub: a passphrase-protected disk key fails
	// fast instead of hanging until the probe timeout. Agent-held keys never
	// consult askpass, so this leaves the common setup untouched.
	cmd.Env = append(os.Environ(), "SSH_ASKPASS="+filepath.Join("/bin", "false"), "SSH_ASKPASS_REQUIRE=force")
	cmd.Stdin = strings.NewReader("sybra run environment signing probe\n")
	if output, runErr := cmd.CombinedOutput(); runErr != nil {
		return fmt.Errorf("signing probe: %w: %s", runErr, strings.TrimSpace(string(output)))
	}
	return nil
}

// resolveSSHKeyFile maps git's user.signingkey onto the file argument
// ssh-keygen -f expects: an existing path (~/ expanded) passes through, and a
// literal key blob is staged into a temp file for the probe's lifetime.
func resolveSSHKeyFile(key string) (keyFile string, cleanup func(), err error) {
	key = strings.TrimSpace(key)
	if strings.HasPrefix(key, "ssh-") || strings.HasPrefix(key, "ecdsa-") || strings.HasPrefix(key, "sk-") {
		var dir string
		dir, err = os.MkdirTemp("", "sybra-ssh-probe-*")
		if err != nil {
			return "", nil, fmt.Errorf("resolve signing key: %w", err)
		}
		keyFile = filepath.Join(dir, "signing_key.pub")
		if err = os.WriteFile(keyFile, []byte(key+"\n"), 0o600); err != nil {
			_ = os.RemoveAll(dir)
			return "", nil, fmt.Errorf("resolve signing key: %w", err)
		}
		return keyFile, func() { _ = os.RemoveAll(dir) }, nil
	}
	if strings.HasPrefix(key, "~") {
		var home string
		home, err = os.UserHomeDir()
		if err != nil {
			return "", nil, fmt.Errorf("resolve signing key: %w", err)
		}
		key = filepath.Join(home, strings.TrimPrefix(key, "~"))
	}
	if _, err = os.Stat(key); err != nil {
		return "", nil, fmt.Errorf("resolve signing key: %s: %w", key, err)
	}
	return key, func() {}, nil
}

// CommitSignFlags returns the git commit flags an agent should use on this
// machine under the default SigningAuto policy.
func CommitSignFlags(ctx context.Context) string {
	return SigningAuto.CommitFlags(ctx)
}
