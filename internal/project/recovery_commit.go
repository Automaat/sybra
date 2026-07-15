package project

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

func gitIdentityConfigured(ctx context.Context, wtPath string) bool {
	for _, key := range []string{"user.name", "user.email"} {
		cmd := exec.CommandContext(ctx, "git", "config", "--get", key)
		cmd.Dir = wtPath
		out, err := cmd.Output()
		if err != nil || strings.TrimSpace(string(out)) == "" {
			return false
		}
	}
	return true
}

func recoveryCommitArgs(message string, hasIdentity, sign bool) []string {
	var args []string
	if !hasIdentity {
		args = append(args, "-c", "user.name=Sybra", "-c", "user.email=sybra@localhost")
	}
	args = append(args, "commit", "--no-verify", "--signoff")
	if sign {
		args = append(args, "-S")
	} else {
		args = append(args, "--no-gpg-sign")
	}
	return append(args, "-m", message)
}

func runRecoveryCommit(ctx context.Context, wtPath, message string) error {
	hasIdentity := gitIdentityConfigured(ctx, wtPath)
	sign := GPGSigningAvailable(ctx)
	out, err := gitCommitOutput(ctx, wtPath, recoveryCommitArgs(message, hasIdentity, sign))
	if err != nil && sign {
		out, err = gitCommitOutput(ctx, wtPath, recoveryCommitArgs(message, hasIdentity, false))
	}
	if err != nil {
		return fmt.Errorf("recovery commit: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func gitCommitOutput(ctx context.Context, wtPath string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = wtPath
	return cmd.CombinedOutput()
}
