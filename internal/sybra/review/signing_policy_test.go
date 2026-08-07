package review

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
)

// keyBearingHost points git at a global config that resolves a signing key, so
// the SigningAuto fallback yields "-s -S". Without it a "never" assertion is
// vacuous on a keyless CI box: every policy would agree on "-s" and a
// regression that drops the policy entirely would still pass.
func keyBearingHost(t *testing.T) {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "gitconfig")
	contents := "[user]\n\tname = Test\n\temail = test@example.invalid\n\tsigningkey = DEADBEEFDEADBEEF\n"
	if err := os.WriteFile(cfgPath, []byte(contents), 0o600); err != nil {
		t.Fatalf("write git config: %v", err)
		panic("unreachable")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfgPath)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	if got := project.SigningAuto.CommitFlags(context.Background()); got != "-s -S" {
		t.Fatalf("precondition: SigningAuto.CommitFlags() = %q, want -s -S (GIT_CONFIG_GLOBAL not honored)", got)
	}
}

func handlerWithSigning(policy string) *Handler {
	cfg := &config.Config{}
	cfg.Agent.CommitSigning = policy
	return &Handler{cfg: cfg}
}

// The prompts this package builds must honor agent.commit_signing, not the
// host probe. A keyless deploy server configured "never" that is still told
// `git commit -s -S` parks the task on "gpg failed to sign the data" — the
// exact failure the policy exists to prevent.
func TestReviewPrompts_HonorNeverPolicyOnKeyBearingHost(t *testing.T) {
	keyBearingHost(t)
	ctx := context.Background()
	h := handlerWithSigning("never")
	signing := h.signingPolicy()

	if signing != project.SigningNever {
		t.Fatalf("signingPolicy() = %q, want never", signing)
	}

	pr := github.PullRequest{Number: 1178, HeadRefName: "fix/example", Repository: "owner/repo"}
	tk := task.Task{Branch: "fix/example", ProjectID: "owner/repo", PRNumber: 1178}

	prompts := map[string]string{
		"branchConflictPrompt":     branchConflictPrompt(ctx, tk, "main", signing),
		"sameBranchConflictPrompt": sameBranchConflictPrompt(ctx, tk, "origin", signing),
		"commentsPrompt":           commentsPrompt(ctx, pr, signing),
		"buildConflictPrompt":      buildConflictPrompt(ctx, pr, "", signing),
		"coalescedFixPrompt": coalescedFixPrompt(ctx,
			[]github.PRIssue{{Kind: github.PRIssueComments, PR: pr}}, "", signing),
	}
	for name, prompt := range prompts {
		// Match on "commit" rather than "git commit": these prompts also
		// spell the instruction as a bare "# commit -s to complete the
		// merge", which a "git commit" filter walks straight past.
		signedOff := false
		for line := range strings.Lines(prompt) {
			if !strings.Contains(line, "commit") {
				continue
			}
			if strings.Contains(line, "-S") {
				t.Errorf("%s instructs GPG signing under the never policy:\n%s", name, line)
			}
			if strings.Contains(line, "commit -s") {
				signedOff = true
			}
		}
		if !signedOff {
			t.Errorf("%s lost its sign-off instruction entirely:\n%s", name, prompt)
		}
	}
}

// The mirror case: "require" must reach the prompts even where the host probe
// would decline, so the assertions above cannot pass by simply never emitting
// -S anywhere.
func TestReviewPrompts_HonorRequirePolicyOnKeylessHost(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(cfgPath, []byte("[user]\n\tname = Test\n"), 0o600); err != nil {
		t.Fatalf("write git config: %v", err)
		panic("unreachable")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfgPath)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	ctx := context.Background()
	signing := handlerWithSigning("require").signingPolicy()
	if signing != project.SigningRequire {
		t.Fatalf("signingPolicy() = %q, want require", signing)
	}
	if got := project.SigningAuto.CommitFlags(ctx); got != "-s" {
		t.Fatalf("precondition: host resolves a key, wanted keyless; got %q", got)
	}

	prompt := commentsPrompt(ctx, github.PullRequest{Number: 1, HeadRefName: "fix/x"}, signing)
	if !strings.Contains(prompt, "git commit -s -S") {
		t.Errorf("commentsPrompt dropped -S under the require policy:\n%s", prompt)
	}
}

// A nil cfg must not panic and must keep the historical host-probing
// behavior — tests and any pre-config call path construct a bare Handler.
func TestSigningPolicy_NilConfigDefaultsToAuto(t *testing.T) {
	var nilHandler *Handler
	if got := nilHandler.signingPolicy(); got != project.SigningAuto {
		t.Errorf("nil Handler signingPolicy() = %q, want auto", got)
	}
	if got := (&Handler{}).signingPolicy(); got != project.SigningAuto {
		t.Errorf("nil-cfg Handler signingPolicy() = %q, want auto", got)
	}
}

// r.cfg is the construction-time snapshot and is never rewritten, so a hot
// reload has to reach the handler through SetSigningPolicy. Without it a
// reload to "never" keeps this package's prompts on the startup posture and a
// key-bearing host goes on being told to pass -S.
func TestSigningPolicy_LateBoundOverridesStartupSnapshot(t *testing.T) {
	keyBearingHost(t)
	ctx := context.Background()

	h := handlerWithSigning("auto")
	if got := h.signingPolicy().CommitFlags(ctx); got != "-s -S" {
		t.Fatalf("startup posture = %q, want -s -S", got)
	}

	h.SetSigningPolicy(project.SigningNever)

	if got := h.signingPolicy(); got != project.SigningNever {
		t.Errorf("after reload signingPolicy() = %q, want never", got)
	}
	prompt := commentsPrompt(ctx, github.PullRequest{Number: 1, HeadRefName: "fix/x"}, h.signingPolicy())
	if strings.Contains(prompt, "-S") {
		t.Errorf("commentsPrompt still instructs -S after reload to never:\n%s", prompt)
	}

	// And back, so the late binding is not a one-way downgrade.
	h.SetSigningPolicy(project.SigningAuto)
	if got := h.signingPolicy().CommitFlags(ctx); got != "-s -S" {
		t.Errorf("after reload back to auto = %q, want -s -S", got)
	}
}

// A handler that was never late-bound must keep reading its snapshot rather
// than silently resolving to auto.
func TestSigningPolicy_UnboundFallsBackToSnapshot(t *testing.T) {
	keyBearingHost(t)
	if got := handlerWithSigning("never").signingPolicy(); got != project.SigningNever {
		t.Errorf("unbound signingPolicy() = %q, want never from the snapshot", got)
	}
}
