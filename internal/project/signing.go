package project

import (
	"context"

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

// CommitSignFlags returns the git commit flags an agent should use on this
// machine under the default SigningAuto policy.
func CommitSignFlags(ctx context.Context) string {
	return SigningAuto.CommitFlags(ctx)
}
