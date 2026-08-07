package runenv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/autonomy"
	"github.com/Automaat/sybra/internal/gitexec"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/providerid"
)

func TestCertifyLinkedWorktreeAndCachesStableEnvironment(t *testing.T) {
	bare, worktree := linkedWorktree(t)
	var providerProbes atomic.Int32
	service := New(Deps{
		ProbeSandbox: healthyProbe(false),
		ProbeProvider: func(context.Context, string) (ProbeResult, error) {
			providerProbes.Add(1)
			return ProbeResult{Available: true}, nil
		},
		ProbeTaskMutation: healthyProbe(false),
	})
	req := authorRequest(bare, worktree)
	first, err := service.Certify(context.Background(), req)
	if err != nil {
		t.Fatalf("Certify: %v", err)
		panic("unreachable")
	}
	second, err := service.Certify(context.Background(), req)
	if err != nil {
		t.Fatalf("Certify cached: %v", err)
		panic("unreachable")
	}
	if first.ID != second.ID {
		t.Fatalf("certificate IDs differ: %q != %q", first.ID, second.ID)
	}
	if providerProbes.Load() != 1 {
		t.Fatalf("provider probes = %d, want 1", providerProbes.Load())
	}
	if !first.Current(time.Now()) {
		t.Fatal("new certificate is not current")
	}
}

func TestCertifyEvictsExpiredFingerprintEntries(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	service := New(Deps{Now: func() time.Time { return now }, TTL: time.Second})
	req := Request{
		TaskID: "task", Action: "read.dispatch", WorkDir: t.TempDir(), ConfigVersion: "one",
		Requirements: []autonomy.CapabilityRequirement{{Capability: autonomy.CapabilitySourceRead, Action: "read.dispatch", Scope: "task"}},
	}
	if _, err := service.Certify(context.Background(), req); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	now = now.Add(2 * time.Second)
	req.ConfigVersion = "two"
	if _, err := service.Certify(context.Background(), req); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.cache) != 1 {
		t.Fatalf("cache entries = %d, want only current fingerprint", len(service.cache))
	}
}

func TestCachedCertificateInvalidatesWhenScratchRootBecomesReadOnly(t *testing.T) {
	bare, worktree := linkedWorktree(t)
	scratch := t.TempDir()
	service := New(Deps{ProbeSandbox: healthyProbe(false), ProbeProvider: healthyProbe(false), ProbeTaskMutation: healthyProbe(false)})
	req := authorRequest(bare, worktree)
	req.ScratchRoots = []string{scratch}
	if _, err := service.Certify(context.Background(), req); err != nil {
		t.Fatalf("Certify: %v", err)
		panic("unreachable")
	}
	if err := os.Chmod(scratch, 0o555); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	t.Cleanup(func() { _ = os.Chmod(scratch, 0o755) })
	_, err := service.Certify(context.Background(), req)
	var failure CertificationError
	if !errors.As(err, &failure) || failure.Code != "scratch_write_unavailable" || failure.Capability != autonomy.CapabilityScratchWrite {
		t.Fatalf("failure = %#v / %v", failure, err)
	}
}

func TestVerifierNeverReceivesSourceWriteProbe(t *testing.T) {
	bare, worktree := linkedWorktree(t)
	service := New(Deps{ProbeSandbox: healthyProbe(false), ProbeProvider: healthyProbe(false), ProbeNetwork: healthyProbe(false)})
	req := Request{TaskID: "judge", ProjectID: "owner/repo", Action: "review.dispatch", WorkDir: worktree, ScratchRoots: []string{t.TempDir()}, CloneDir: bare, Provider: providerid.Codex, SandboxMode: "report", SigningPolicy: project.SigningNever, Requirements: agent.RoleReview.CapabilityRequirements("review.dispatch")}
	cert, err := service.Certify(context.Background(), req)
	if err != nil {
		t.Fatalf("Certify: %v", err)
		panic("unreachable")
	}
	hasScratchWrite := false
	for _, observation := range cert.Observations {
		if observation.Capability == autonomy.CapabilitySourceWrite || observation.Capability == autonomy.CapabilityGitAdminWrite {
			t.Fatalf("verifier certificate includes mutation capability: %+v", observation)
		}
		if observation.Capability == autonomy.CapabilityScratchWrite {
			hasScratchWrite = true
		}
	}
	if !hasScratchWrite {
		t.Fatal("verifier certificate omitted scratch-write capability")
	}
}

func TestScratchWriteIsCertifiedWithoutSourceWrite(t *testing.T) {
	workDir := t.TempDir()
	req := Request{
		TaskID:       "judge",
		Action:       "review.dispatch",
		WorkDir:      workDir,
		ScratchRoots: []string{filepath.Join(t.TempDir(), "missing")},
		Requirements: []autonomy.CapabilityRequirement{{
			Capability: autonomy.CapabilityScratchWrite,
			Action:     "review.dispatch",
			Scope:      "task",
		}},
	}
	_, err := New(Deps{}).Certify(context.Background(), req)
	var failure CertificationError
	if !errors.As(err, &failure) || failure.Capability != autonomy.CapabilityScratchWrite || failure.Code != "scratch_write_unavailable" {
		t.Fatalf("failure = %#v / %v", failure, err)
	}
}

func TestScratchWriteRejectsMissingRoots(t *testing.T) {
	req := Request{
		TaskID: "judge", Action: "review.dispatch", WorkDir: t.TempDir(),
		Requirements: []autonomy.CapabilityRequirement{{
			Capability: autonomy.CapabilityScratchWrite,
			Action:     "review.dispatch",
			Scope:      "task",
		}},
	}
	_, err := New(Deps{}).Certify(context.Background(), req)
	var failure CertificationError
	if !errors.As(err, &failure) || failure.Capability != autonomy.CapabilityScratchWrite || failure.Code != "scratch_write_unavailable" {
		t.Fatalf("failure = %#v / %v", failure, err)
	}
}

func TestRepairIsSerializedAndProducesFreshCertificate(t *testing.T) {
	bare, worktree := linkedWorktree(t)
	var unhealthy atomic.Bool
	unhealthy.Store(true)
	var repairs atomic.Int32
	var repairAudits atomic.Int32
	var repairedAudit Certificate
	service := New(Deps{
		ProbeSandbox: healthyProbe(false), ProbeProvider: healthyProbe(false), ProbeTaskMutation: healthyProbe(false),
		Repair: func(context.Context, Request, []Observation) error {
			repairs.Add(1)
			unhealthy.Store(false)
			return nil
		},
		Audit: func(event string, cert Certificate, _ *CertificationError) {
			if event == "runenv.repair" {
				repairAudits.Add(1)
				repairedAudit = cert
			}
		},
	})
	// Replace the object-store requirement with a repairable callback-driven
	// failure by temporarily making the worktree unreadable through a missing
	// source path, then restoring it in Repair.
	missing := filepath.Join(filepath.Dir(worktree), "missing")
	req := authorRequest(bare, worktree)
	req.WorkDir = missing
	service.deps.Repair = func(context.Context, Request, []Observation) error {
		repairs.Add(1)
		if !unhealthy.Swap(false) {
			return errors.New("duplicate repair")
		}
		return os.Rename(worktree, missing)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			_, certifyErr := service.Certify(context.Background(), req)
			errs <- certifyErr
		})
	}
	wg.Wait()
	close(errs)
	for certifyErr := range errs {
		if certifyErr != nil {
			t.Fatalf("Certify: %v", certifyErr)
			panic("unreachable")
		}
	}
	if repairs.Load() != 1 {
		t.Fatalf("repairs = %d, want 1", repairs.Load())
	}
	if repairAudits.Load() != 1 || !repairedAudit.Repaired {
		t.Fatalf("repair audit = %+v (count %d), want repaired certificate", repairedAudit, repairAudits.Load())
	}
}

func TestDetectsReadOnlySourceAndGitAdminSeparately(t *testing.T) {
	bare, worktree := linkedWorktree(t)
	service := New(Deps{ProbeSandbox: healthyProbe(false), ProbeProvider: healthyProbe(false), ProbeTaskMutation: healthyProbe(false)})
	req := authorRequest(bare, worktree)
	if err := os.Chmod(worktree, 0o555); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	t.Cleanup(func() { _ = os.Chmod(worktree, 0o755) })
	_, err := service.Certify(context.Background(), req)
	var failure CertificationError
	if !errors.As(err, &failure) || failure.Code != "source_write_unavailable" {
		t.Fatalf("failure = %#v / %v", failure, err)
	}

	if err := os.Chmod(worktree, 0o755); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	gitDir := gitOutput(t, worktree, "rev-parse", "--path-format=absolute", "--git-dir")
	if err := os.Chmod(gitDir, 0o555); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	t.Cleanup(func() { _ = os.Chmod(gitDir, 0o755) })
	service = New(Deps{ProbeSandbox: healthyProbe(false), ProbeProvider: healthyProbe(false), ProbeTaskMutation: healthyProbe(false)})
	_, err = service.Certify(context.Background(), req)
	if !errors.As(err, &failure) || failure.Code != "git_admin_readonly" {
		t.Fatalf("failure = %#v / %v", failure, err)
	}
}

func TestSandboxReportIsAvailableButNeverContained(t *testing.T) {
	observation, err := agent.ProbeSandboxPosture("report")
	if err != nil {
		t.Fatalf("ProbeSandboxPosture(report): %v", err)
		panic("unreachable")
	}
	if !observation.Available || observation.Contained {
		t.Fatalf("report observation = %+v", observation)
	}
}

func TestMissingReferencedObjectFailsBeforeDispatch(t *testing.T) {
	bare, worktree := linkedWorktree(t)
	service := New(Deps{ProbeSandbox: healthyProbe(false), ProbeProvider: healthyProbe(false), ProbeTaskMutation: healthyProbe(false)})
	req := authorRequest(bare, worktree)
	if _, err := service.Certify(context.Background(), req); err != nil {
		t.Fatalf("initial Certify: %v", err)
		panic("unreachable")
	}
	tree := gitOutput(t, worktree, "rev-parse", "HEAD^{tree}")
	fanout := fmt.Sprintf("%.2s", tree)
	objectPath := filepath.Join(bare, "objects", fanout, strings.TrimPrefix(tree, fanout))
	if err := os.Remove(objectPath); err != nil {
		t.Fatalf("remove referenced tree: %v", err)
		panic("unreachable")
	}
	_, err := service.Certify(context.Background(), req)
	var failure CertificationError
	if !errors.As(err, &failure) || failure.Code != "object_store_unhealthy" || failure.Scope != "project" {
		t.Fatalf("failure = %#v / %v", failure, err)
	}
}

func TestIndexReferencedObjectFailsBeforeDispatch(t *testing.T) {
	bare, worktree := linkedWorktree(t)
	path := filepath.Join(worktree, "staged.txt")
	if err := os.WriteFile(path, []byte("staged only\n"), 0o644); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	runGit(t, worktree, "add", "staged.txt")
	blob := gitOutput(t, worktree, "rev-parse", ":staged.txt")
	fanout := fmt.Sprintf("%.2s", blob)
	objectPath := filepath.Join(bare, "objects", fanout, strings.TrimPrefix(blob, fanout))
	if err := os.Remove(objectPath); err != nil {
		t.Fatalf("remove staged blob: %v", err)
		panic("unreachable")
	}
	service := New(Deps{ProbeSandbox: healthyProbe(false), ProbeProvider: healthyProbe(false), ProbeTaskMutation: healthyProbe(false)})
	_, err := service.Certify(context.Background(), authorRequest(bare, worktree))
	var failure CertificationError
	if !errors.As(err, &failure) || failure.Code != "checkout_unhealthy" || failure.Scope != "task" {
		t.Fatalf("failure = %#v / %v", failure, err)
	}
}

func TestIndexReferencedObjectRepairsThenFreshlyCertifies(t *testing.T) {
	bare, worktree := linkedWorktree(t)
	path := filepath.Join(worktree, "staged.txt")
	if err := os.WriteFile(path, []byte("preserve me\n"), 0o644); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	runGit(t, worktree, "add", "staged.txt")
	blob := gitOutput(t, worktree, "rev-parse", ":staged.txt")
	fanout := fmt.Sprintf("%.2s", blob)
	if err := os.Remove(filepath.Join(bare, "objects", fanout, strings.TrimPrefix(blob, fanout))); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	service := New(Deps{
		ProbeSandbox: healthyProbe(false), ProbeProvider: healthyProbe(false), ProbeTaskMutation: healthyProbe(false),
		Repair: func(ctx context.Context, req Request, _ []Observation) error {
			_, err := project.EnsureBareCloneHealthy(ctx, req.CloneDir, req.TaskBranch)
			return err
		},
	})
	cert, err := service.Certify(context.Background(), authorRequest(bare, worktree))
	if err != nil {
		t.Fatalf("Certify after repair: %v", err)
		panic("unreachable")
	}
	if !cert.Repaired {
		t.Fatal("certificate did not record repair")
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "preserve me\n" {
		t.Fatalf("worktree file was not preserved: %q / %v", contents, err)
		panic("unreachable")
	}
	if staged := gitOutput(t, worktree, "ls-files", "--stage", "staged.txt"); staged != "" {
		t.Fatalf("rebuilt index retained invalid staged entry: %s", staged)
	}
}

func TestCachedCertificateInvalidatesWhenIndexChanges(t *testing.T) {
	bare, worktree := linkedWorktree(t)
	service := New(Deps{ProbeSandbox: healthyProbe(false), ProbeProvider: healthyProbe(false), ProbeTaskMutation: healthyProbe(false)})
	req := authorRequest(bare, worktree)
	first, err := service.Certify(context.Background(), req)
	if err != nil {
		t.Fatalf("initial Certify: %v", err)
		panic("unreachable")
	}
	missing := strings.Repeat("1", 40)
	runGit(t, worktree, "update-index", "--add", "--info-only", "--cacheinfo", "100644,"+missing+",missing.txt")

	second, err := service.Certify(context.Background(), req)
	var failure CertificationError
	if !errors.As(err, &failure) || failure.Code != "checkout_unhealthy" || failure.Scope != "task" {
		t.Fatalf("failure = %#v / %v", failure, err)
	}
	if second.ID == first.ID {
		t.Fatalf("reused stale certificate %q after index mutation", first.ID)
	}
}

func TestCachedCertificateInvalidatesWhenCloneRefChanges(t *testing.T) {
	bare, worktree := linkedWorktree(t)
	service := New(Deps{ProbeSandbox: healthyProbe(false), ProbeProvider: healthyProbe(false), ProbeTaskMutation: healthyProbe(false)})
	req := authorRequest(bare, worktree)
	first, err := service.Certify(context.Background(), req)
	if err != nil {
		t.Fatalf("initial Certify: %v", err)
		panic("unreachable")
	}
	poison := filepath.Join(bare, "refs", "heads", "poison")
	if err := os.WriteFile(poison, []byte(strings.Repeat("2", 40)+"\n"), 0o644); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}

	second, err := service.Certify(context.Background(), req)
	var failure CertificationError
	if !errors.As(err, &failure) || failure.Code != "object_store_unhealthy" {
		t.Fatalf("failure = %#v / %v", failure, err)
	}
	if second.ID == first.ID {
		t.Fatalf("reused stale certificate %q after clone ref mutation", first.ID)
	}
}

func TestWorktreeLessJudgeCertifiesCandidateGitRoots(t *testing.T) {
	bare, first := linkedWorktree(t)
	second := filepath.Join(t.TempDir(), "second")
	runGit(t, "", "--git-dir", bare, "worktree", "add", "-b", "second", second, "HEAD")
	service := New(Deps{ProbeSandbox: healthyProbe(false), ProbeProvider: healthyProbe(false)})
	req := Request{
		TaskID: "judge", ProjectID: "project", Action: "review.dispatch", WorkDir: t.TempDir(),
		ReadRoots: []string{first, second}, GitRoots: []string{first, second}, CloneDir: bare,
		Requirements: []autonomy.CapabilityRequirement{
			{Capability: autonomy.CapabilitySourceRead, Action: "review.dispatch", Scope: "task"},
			{Capability: autonomy.CapabilityObjectStore, Action: "review.dispatch", Scope: "project"},
			{Capability: autonomy.CapabilityCheckoutHealth, Action: "review.dispatch", Scope: "task"},
		},
	}
	if _, err := service.Certify(context.Background(), req); err != nil {
		t.Fatalf("Certify worktree-less judge: %v", err)
		panic("unreachable")
	}
}

func TestIsEnvironmentFailureRecognizesInvalidObject(t *testing.T) {
	if !IsEnvironmentFailure(errors.New("recovery commit: invalid object 111111 for 'ghost.txt'")) {
		t.Fatal("invalid object did not invalidate certificate")
	}
}

func TestTaskMutationIdentityInvalidatesCachedCertificate(t *testing.T) {
	var probes atomic.Int32
	service := New(Deps{ProbeTaskMutation: func(context.Context, string) (ProbeResult, error) {
		probes.Add(1)
		return ProbeResult{Available: true}, nil
	}})
	req := Request{
		TaskID: "task", Action: "triage.dispatch", WorkDir: t.TempDir(), TaskMutationIdentity: "writable",
		Requirements: []autonomy.CapabilityRequirement{{Capability: autonomy.CapabilityTaskMutation, Action: "triage.dispatch", Scope: "task"}},
	}
	if _, err := service.Certify(context.Background(), req); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	req.TaskMutationIdentity = "read-only"
	if _, err := service.Certify(context.Background(), req); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if probes.Load() != 2 {
		t.Fatalf("mutation probes = %d, want 2 after identity change", probes.Load())
	}
}

func TestFailedProviderCertificateSuppressesDuplicateProbeWithoutQuarantine(t *testing.T) {
	var probes, quarantines atomic.Int32
	service := New(Deps{
		ProbeProvider: func(context.Context, string) (ProbeResult, error) {
			probes.Add(1)
			return ProbeResult{Code: "provider_unavailable"}, errors.New("at capacity")
		},
		Quarantine: func(context.Context, CertificationError) { quarantines.Add(1) },
	})
	req := Request{TaskID: "task", Action: "dispatch", WorkDir: t.TempDir(), Provider: providerid.Codex, Requirements: []autonomy.CapabilityRequirement{{Capability: autonomy.CapabilityProviderCapacity, Action: "dispatch", Scope: "provider"}}}
	for range 2 {
		if _, err := service.Certify(context.Background(), req); err == nil {
			t.Fatal("Certify succeeded")
			panic("unreachable")
		}
	}
	if probes.Load() != 1 {
		t.Fatalf("probes = %d, want 1", probes.Load())
	}
	if quarantines.Load() != 0 {
		t.Fatalf("quarantines = %d, want 0 for external provider capacity", quarantines.Load())
	}
}

func TestFailedMachineCertificateCoalescesQuarantine(t *testing.T) {
	var quarantines atomic.Int32
	service := New(Deps{Quarantine: func(context.Context, CertificationError) { quarantines.Add(1) }})
	req := Request{TaskID: "task", Action: "dispatch", WorkDir: filepath.Join(t.TempDir(), "missing"), Requirements: []autonomy.CapabilityRequirement{{Capability: autonomy.CapabilitySourceRead, Action: "dispatch", Scope: "task"}}}
	for range 2 {
		if _, err := service.Certify(context.Background(), req); err == nil {
			t.Fatal("Certify succeeded")
			panic("unreachable")
		}
	}
	if quarantines.Load() != 1 {
		t.Fatalf("quarantines = %d, want 1", quarantines.Load())
	}
}

func TestProjectFailureCoalescesQuarantineAcrossTasks(t *testing.T) {
	var quarantines atomic.Int32
	var probes atomic.Int32
	service := New(Deps{
		ProbeSandbox: func(context.Context, string) (ProbeResult, error) {
			probes.Add(1)
			return ProbeResult{Code: "machine_project_failure"}, errors.New("project unavailable")
		},
		Quarantine: func(context.Context, CertificationError) { quarantines.Add(1) },
	})
	root := filepath.Join(t.TempDir(), "missing")
	for _, taskID := range []string{"task-one", "task-two"} {
		req := Request{TaskID: taskID, ProjectID: "owner/repo", Action: "dispatch", WorkDir: root, Requirements: []autonomy.CapabilityRequirement{{Capability: autonomy.CapabilitySandboxMechanism, Action: "dispatch", Scope: "project"}}}
		if _, err := service.Certify(context.Background(), req); err == nil {
			t.Fatalf("Certify(%s) succeeded", taskID)
			panic("unreachable")
		}
	}
	if quarantines.Load() != 1 {
		t.Fatalf("quarantines = %d, want 1 for one shared project failure", quarantines.Load())
	}
	if probes.Load() != 1 {
		t.Fatalf("probes = %d, want 1 while shared project quarantine is current", probes.Load())
	}
}

func TestRequestGitRootsSkipsGitForNonGitCapabilities(t *testing.T) {
	req := Request{
		WorkDir: t.TempDir(),
		Requirements: []autonomy.CapabilityRequirement{{
			Capability: autonomy.CapabilitySandboxMechanism,
			Action:     "startup.host",
			Scope:      "host",
		}},
	}
	if got := requestGitRoots(req); got != nil {
		t.Fatalf("requestGitRoots() = %v, want nil for non-Git requirements", got)
		panic("unreachable")
	}
	req.Requirements = append(req.Requirements, autonomy.CapabilityRequirement{
		Capability: autonomy.CapabilityCheckoutHealth,
		Action:     "startup.host",
		Scope:      "task",
	})
	if got := requestGitRoots(req); len(got) != 1 || got[0] != req.WorkDir {
		t.Fatalf("requestGitRoots() = %v, want [%q]", got, req.WorkDir)
	}
}

func TestObjectStoreEvidenceDoesNotClaimUndeclaredClone(t *testing.T) {
	cert, err := New(Deps{}).Certify(context.Background(), Request{
		TaskID: "projectless", Action: "dispatch", WorkDir: t.TempDir(),
		Requirements: []autonomy.CapabilityRequirement{{
			Capability: autonomy.CapabilityObjectStore,
			Action:     "dispatch",
			Scope:      "project",
		}},
	})
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if got := cert.Observations[0].Evidence; got != "no shared object store declared" {
		t.Fatalf("evidence = %q", got)
	}
}

func TestProjectlessCertificationsUseBoundedRepairLock(t *testing.T) {
	service := New(Deps{ProbeSandbox: healthyProbe(false)})
	for i := range 25 {
		req := Request{
			TaskID:  fmt.Sprintf("task-%d", i),
			Action:  "startup.host",
			WorkDir: filepath.Join(t.TempDir(), "work"),
			Requirements: []autonomy.CapabilityRequirement{{
				Capability: autonomy.CapabilitySandboxMechanism,
				Action:     "startup.host",
				Scope:      "host",
			}},
		}
		if _, err := service.Certify(context.Background(), req); err != nil {
			t.Fatalf("Certify(%d): %v", i, err)
			panic("unreachable")
		}
	}
	if got := len(service.locks); got != 1 {
		t.Fatalf("repair locks = %d, want one shared projectless lock", got)
	}
}

func healthyProbe(contained bool) func(context.Context, string) (ProbeResult, error) {
	return func(context.Context, string) (ProbeResult, error) {
		return ProbeResult{Available: true, Contained: contained}, nil
	}
}

func authorRequest(bare, worktree string) Request {
	return Request{TaskID: "task", ProjectID: "owner/repo", Action: "implementation.dispatch", WorkDir: worktree, ScratchRoots: []string{filepath.Dir(worktree)}, CloneDir: bare, Provider: providerid.Codex, SandboxMode: "report", SigningPolicy: project.SigningNever, Requirements: agent.RoleImplementation.CapabilityRequirements("implementation.dispatch")}
}

func linkedWorktree(t *testing.T) (bare, worktree string) {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	bare, worktree = filepath.Join(root, "repo.git"), filepath.Join(root, "worktree")
	runGit(t, root, "init", source)
	runGit(t, source, "config", "user.email", "test@example.com")
	runGit(t, source, "config", "user.name", "Test")
	runGit(t, source, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(source, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	runGit(t, source, "add", "seed.txt")
	runGit(t, source, "commit", "-m", "seed")
	runGit(t, root, "clone", "--bare", source, bare)
	runGit(t, bare, "worktree", "add", worktree, "HEAD")
	return bare, worktree
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitexec.WithoutRepoOverrides(nil)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
		panic("unreachable")
	}
}
func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitexec.WithoutRepoOverrides(nil)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
		panic("unreachable")
	}
	return string(bytesTrimSpace(out))
}
func bytesTrimSpace(value []byte) []byte {
	for len(value) > 0 && (value[len(value)-1] == '\n' || value[len(value)-1] == '\r') {
		value = value[:len(value)-1]
	}
	return value
}
