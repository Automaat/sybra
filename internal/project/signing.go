package project

import (
	"context"
	"os/exec"
	"strings"
)

// GPGSigningAvailable reports whether this machine can GPG-sign commits, i.e.
// whether git resolves a non-empty user.signingkey. Used to decide whether an
// agent commit instruction should carry -S: on a keyless host (e.g. the Linux
// server) `git commit -S` fails with "gpg failed to sign the data", so callers
// emit only -s there. DCO sign-off (-s) is guaranteed independently by the
// prepare-commit-msg hook (see InstallSignoffHook).
func GPGSigningAvailable(ctx context.Context) bool {
	out, err := exec.CommandContext(ctx, "git", "config", "--global", "--get", "user.signingkey").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}

// CommitSignFlags returns the git commit flags an agent should use on this
// machine: "-s -S" when a signing key is available, otherwise "-s". The
// prepare-commit-msg hook still enforces -s regardless; this only governs the
// optional, key-dependent GPG flag.
func CommitSignFlags(ctx context.Context) string {
	if GPGSigningAvailable(ctx) {
		return "-s -S"
	}
	return "-s"
}
