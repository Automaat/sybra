package project

import (
	"context"
	"fmt"

	"github.com/Automaat/sybra/internal/gitexec"
)

func gitIdentityConfigured(ctx context.Context, wtPath string) bool {
	for _, key := range []string{"user.name", "user.email"} {
		out, err := gitexec.Output(ctx, gitexec.Options{Dir: wtPath}, "config", "--get", key)
		if err != nil || out == "" {
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
	err := gitCommit(ctx, wtPath, recoveryCommitArgs(message, hasIdentity, sign))
	if err != nil && sign {
		err = gitCommit(ctx, wtPath, recoveryCommitArgs(message, hasIdentity, false))
	}
	if err != nil {
		return fmt.Errorf("recovery commit: %w", err)
	}
	return nil
}

func gitCommit(ctx context.Context, wtPath string, args []string) error {
	return gitexec.Run(ctx, gitexec.Options{Dir: wtPath}, args...)
}
