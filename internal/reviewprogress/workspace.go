package reviewprogress

import (
	"context"
	"errors"

	"github.com/Automaat/sybra/internal/gitexec"
)

// PinBase materializes the leader-selected comparison input in a disposable
// checkout. It never resolves a mutable worker tracking ref as a substitute.
func PinBase(ctx context.Context, dir, ref, sha string) error {
	resolved, err := gitexec.Output(ctx, gitexec.Options{Dir: dir}, "rev-parse", "--verify", "--end-of-options", sha+"^{commit}")
	if err != nil || resolved != sha {
		return errors.New("review progress: exact comparison object unavailable")
	}
	return gitexec.Run(ctx, gitexec.Options{Dir: dir}, "update-ref", ref, sha)
}

// ValidateWorkspace is independent of final review evidence. Mutations in a
// disposable verifier clone remain supported, but disqualify its checkpoint.
func ValidateWorkspace(ctx context.Context, dir, head, baseRef, baseSHA string) error {
	for ref, want := range map[string]string{"HEAD": head, baseRef: baseSHA} {
		got, err := gitexec.Output(ctx, gitexec.Options{Dir: dir}, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
		if err != nil || got != want {
			return errors.New("review progress: inspected revision changed")
		}
	}
	status, err := gitexec.Output(ctx, gitexec.Options{Dir: dir}, "status", "--porcelain", "--untracked-files=all")
	if err != nil || status != "" {
		return errors.New("review progress: inspected workspace changed")
	}
	return nil
}
