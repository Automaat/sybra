package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/providerid"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	k8stesting "k8s.io/client-go/testing"
)

func TestGitOutputUsesWorkspaceDirectoryAndDisablesPrompts(t *testing.T) {
	binDir := t.TempDir()
	fakeGit := filepath.Join(binDir, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\nprintf '%s\\n%s\\n' \"$PWD\" \"$GIT_TERMINAL_PROMPT\"\n"), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", binDir)

	workspace := t.TempDir()
	out, err := gitOutput(t.Context(), workspace, "status", "--short")
	if err != nil {
		t.Fatalf("gitOutput: %v", err)
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 2 || lines[1] != "0" {
		t.Fatalf("git environment = %q, want working directory and prompt=0", out)
	}
	actualInfo, err := os.Stat(lines[0])
	if err != nil {
		t.Fatalf("stat actual working directory: %v", err)
	}
	wantInfo, err := os.Stat(workspace)
	if err != nil {
		t.Fatalf("stat expected working directory: %v", err)
	}
	if !os.SameFile(actualInfo, wantInfo) {
		t.Fatalf("working directory = %q, want %q", lines[0], workspace)
	}
}

func TestK8sGitWorkspaceNeedsPushIgnoresLocalPRRef(t *testing.T) {
	repo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	runGit("init")
	runGit("config", "user.email", "test@test.com")
	runGit("config", "user.name", "Test")
	runGit("config", "commit.gpgsign", "false")
	runGit("commit", "--allow-empty", "-m", "base")
	runGit("update-ref", "refs/remotes/origin/main", "HEAD")
	runGit("commit", "--allow-empty", "-m", "pr head")
	runGit("update-ref", "refs/sybra/pr/42", "HEAD")

	if !k8sGitWorkspaceNeedsPush(t.Context(), repo) {
		t.Fatal("PR head absent from remote-tracking refs must still be pushed")
	}
}

func TestK8sName(t *testing.T) {
	got := k8sName("Sybra Agent_BAD.ID/With Stuff")
	if got != "sybra-agent-bad-id-with-stuff" {
		t.Fatalf("k8sName = %q, want sanitized DNS label", got)
	}
}

func TestSanitizeLabelValue(t *testing.T) {
	if got := sanitizeLabelValue("owner/repo"); got != "repo" {
		t.Fatalf("sanitizeLabelValue(owner/repo) = %q, want repo", got)
	}
	if got := sanitizeLabelValue(""); got != "none" {
		t.Fatalf("sanitizeLabelValue(empty) = %q, want none", got)
	}
}

func TestNormalizeK8sRunnerMode(t *testing.T) {
	if got := normalizeK8sRunnerMode(""); got != k8sRunnerModeFake {
		t.Fatalf("empty mode = %q, want fake", got)
	}
	if got := normalizeK8sRunnerMode("provider"); got != k8sRunnerModeProvider {
		t.Fatalf("provider mode = %q, want provider", got)
	}
	if got := normalizeK8sRunnerMode("bogus"); got != k8sRunnerModeFake {
		t.Fatalf("bogus mode = %q, want fake fallback", got)
	}
}

func TestK8sRunnerBaseEnvProjectsSecretRefs(t *testing.T) {
	r := newK8sJobRunner(nil, K8sJobRunnerConfig{
		Env: []K8sJobEnvVar{{Name: "PLAIN", Value: "value"}},
		SecretEnv: []K8sJobSecretEnvVar{{
			Name:       "ANTHROPIC_API_KEY",
			SecretName: "sybra-provider-api-keys",
			SecretKey:  "anthropic_api_key",
		}},
	})
	env := r.baseEnv(ExecutionSpec{ID: "agent-1", TaskID: "task-1"}, RunConfig{Prompt: "hello"})
	if len(env) != 5 {
		t.Fatalf("env len = %d, want 5: %#v", len(env), env)
	}
	ref := env[4].ValueFrom
	if ref == nil || ref.SecretKeyRef == nil {
		t.Fatalf("secret env missing secretKeyRef: %#v", env[4])
	}
	if ref.SecretKeyRef.Name != "sybra-provider-api-keys" || ref.SecretKeyRef.Key != "anthropic_api_key" {
		t.Fatalf("secretKeyRef = %#v", ref.SecretKeyRef)
	}
}

func TestK8sRunnerVolumeSpecProjectsPVCMounts(t *testing.T) {
	r := newK8sJobRunner(nil, K8sJobRunnerConfig{
		Volumes: []K8sJobVolume{{
			Name:      "sybra-home",
			ClaimName: "sybra-home",
			MountPath: "/data/sybra/home",
		}},
	})

	volumes, mounts := r.volumeSpec()
	if len(volumes) != 1 || len(mounts) != 1 {
		t.Fatalf("volume spec len = volumes:%d mounts:%d, want 1/1", len(volumes), len(mounts))
	}
	if volumes[0].PersistentVolumeClaim == nil {
		t.Fatalf("volume missing pvc spec: %#v", volumes[0])
	}
	if volumes[0].PersistentVolumeClaim.ClaimName != "sybra-home" {
		t.Fatalf("claimName = %#v, want sybra-home", volumes[0].PersistentVolumeClaim.ClaimName)
	}
	if mounts[0].MountPath != "/data/sybra/home" {
		t.Fatalf("mountPath = %#v, want /data/sybra/home", mounts[0].MountPath)
	}
}

func TestK8sCreateJobBuildsTypedManifest(t *testing.T) {
	client := fake.NewSimpleClientset()
	r := newFakeK8sJobRunner(client, nil, K8sJobRunnerConfig{
		Namespace: "sybra-poc",
		Image:     "busybox:1.36",
		Command:   []string{"sh", "-c", "echo ok"},
		TTL:       300,
		Env:       []K8sJobEnvVar{{Name: "PLAIN", Value: "value"}},
		SecretEnv: []K8sJobSecretEnvVar{{
			Name:       "SECRET",
			SecretName: "sybra-provider-api-keys",
			SecretKey:  "token",
		}},
		Volumes: []K8sJobVolume{{
			Name:      "sybra-home",
			ClaimName: "sybra-home",
			MountPath: "/data/sybra/home",
			ReadOnly:  true,
		}},
	})

	a := &Agent{ID: "agent-1", TaskID: "owner/repo"}
	if err := r.createJob(t.Context(), "sybra-agent-agent-1", executionSpecFromAgent(a), RunConfig{Prompt: "hello"}); err != nil {
		t.Fatalf("createJob: %v", err)
	}
	actions := client.Actions()
	if len(actions) == 0 {
		t.Fatal("expected create action")
	}
	createAction, ok := actions[0].(k8stesting.CreateAction)
	if !ok {
		t.Fatalf("first action = %T, want CreateAction", actions[0])
	}
	job, ok := createAction.GetObject().(*batchv1.Job)
	if !ok {
		t.Fatalf("created object = %T, want *batchv1.Job", createAction.GetObject())
	}
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 300 {
		t.Fatalf("ttlSecondsAfterFinished = %#v, want 300", job.Spec.TTLSecondsAfterFinished)
	}
	if got := job.Labels["sybra.task/id"]; got != "repo" {
		t.Fatalf("task label = %q, want repo", got)
	}
	if job.Spec.Template.Spec.SecurityContext == nil || job.Spec.Template.Spec.SecurityContext.RunAsNonRoot == nil || !*job.Spec.Template.Spec.SecurityContext.RunAsNonRoot {
		t.Fatalf("security context = %#v, want runAsNonRoot=true", job.Spec.Template.Spec.SecurityContext)
	}
	container := job.Spec.Template.Spec.Containers[0]
	if strings.Join(container.Command, " ") != "sh -c echo ok" {
		t.Fatalf("command = %#v, want injected command", container.Command)
	}
	if len(container.Env) != 5 {
		t.Fatalf("env len = %d, want 5", len(container.Env))
	}
	if container.Env[4].ValueFrom == nil || container.Env[4].ValueFrom.SecretKeyRef == nil || container.Env[4].ValueFrom.SecretKeyRef.Name != "sybra-provider-api-keys" {
		t.Fatalf("secret env = %#v", container.Env[4])
	}
	if len(job.Spec.Template.Spec.Volumes) != 1 || job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim == nil {
		t.Fatalf("volumes = %#v, want pvc volume", job.Spec.Template.Spec.Volumes)
	}
	if len(container.VolumeMounts) != 1 || !container.VolumeMounts[0].ReadOnly {
		t.Fatalf("volumeMounts = %#v, want readonly pvc mount", container.VolumeMounts)
	}
}

func TestK8sProviderInvocationRunsInPodWorkdir(t *testing.T) {
	cfg := RunConfig{Provider: "codex", Mode: "headless", Prompt: "hello", RequirePermissions: false}
	a := &Agent{ID: "agent-1", TaskID: "task-1", Provider: "codex", Mode: "headless", Model: "gpt-5.5"}

	inv, err := buildK8sProviderInvocation(executionSpecFromAgent(a), cfg, defaultK8sWorkdir)
	if err != nil {
		t.Fatalf("buildK8sProviderInvocation: %v", err)
	}
	args := strings.Join(inv.args, "\x00")
	if !strings.Contains(args, "-C\x00"+defaultK8sWorkdir) {
		t.Fatalf("codex args missing pod workdir: %#v", inv.args)
	}
}

func TestK8sProviderInvocationSupportsOpenCode(t *testing.T) {
	cfg := RunConfig{Provider: "opencode", Mode: "headless", Prompt: "hello", Dir: "/local/wt"}
	a := &Agent{
		ID:       "agent-1",
		TaskID:   "task-1",
		Provider: "opencode",
		Mode:     "headless",
		Model:    "openrouter/deepseek/deepseek-v4-flash",
	}

	inv, err := buildK8sProviderInvocation(executionSpecFromAgent(a), cfg, defaultK8sWorkdir)
	if err != nil {
		t.Fatalf("buildK8sProviderInvocation: %v", err)
	}
	if inv.name != "opencode" {
		t.Fatalf("invocation name = %q, want opencode", inv.name)
	}
	args := strings.Join(inv.args, "\x00")
	for _, want := range []string{
		"--model\x00openrouter/deepseek/deepseek-v4-flash",
		"--dir\x00" + defaultK8sWorkdir,
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("opencode args missing %q: %#v", want, inv.args)
		}
	}
}

func TestK8sProviderInvocationDoesNotResumeStatelessPodSession(t *testing.T) {
	cfg := RunConfig{Provider: "opencode", Mode: "headless", Prompt: "hello", Dir: "/local/wt"}
	a := &Agent{
		ID:       "agent-1",
		TaskID:   "task-1",
		Provider: "opencode",
		Mode:     "headless",
		Model:    "openrouter/deepseek/deepseek-v4-flash",
	}
	a.SetSessionID("ses_previous")

	inv, err := buildK8sProviderInvocation(executionSpecFromAgent(a), cfg, defaultK8sWorkdir)
	if err != nil {
		t.Fatalf("buildK8sProviderInvocation: %v", err)
	}
	args := strings.Join(inv.args, "\x00")
	if strings.Contains(args, "--session") || strings.Contains(args, "ses_previous") {
		t.Fatalf("k8s stateless pod invocation must not resume provider sessions: %#v", inv.args)
	}
}

func TestK8sProviderCommandWrapsGitWorkspace(t *testing.T) {
	cmd := k8sProviderCommand(headlessInvocation{name: "codex", args: []string{"exec", "--json", "hello"}})
	if len(cmd) < 6 || cmd[0] != "sh" || cmd[1] != "-ceu" || cmd[3] != "--" || cmd[4] != "codex" {
		t.Fatalf("unexpected command: %#v", cmd)
	}
	script := cmd[2]
	for _, want := range []string{"git clone", "GITHUB_TOKEN", ".git/info/exclude", "NOTES.md", "git push -u origin", "SYBRA_K8S_PROVIDER_BIN", `"$@"`} {
		if !strings.Contains(script, want) {
			t.Fatalf("wrapper script missing %q:\n%s", want, script)
		}
	}
}

func TestK8sProviderWrapperCommitsUntrackedFiles(t *testing.T) {
	script := k8sProviderWrapperScript()

	if !strings.Contains(script, "git status --porcelain") {
		t.Errorf("wrapper script must gate its commit on `git status --porcelain`:\n%s", script)
	}
	// `git diff` only reports tracked files, so gating on it skipped the commit
	// whenever the agent created a new file — and the push then reported
	// "Everything up-to-date" and silently dropped the work.
	if strings.Contains(script, "git diff --quiet") {
		t.Errorf("wrapper script gates its commit on `git diff`, which cannot see a newly created file:\n%s", script)
	}
}

func TestAppendK8sPRRepoEnv(t *testing.T) {
	tests := []struct {
		name    string
		remote  string
		want    string
		explain string
	}{
		{
			name:    "https remote",
			remote:  "https://github.com/Automaat/sybra-testbed.git",
			want:    "Automaat/sybra-testbed",
			explain: "the normal registered-project remote",
		},
		{
			name:    "https remote without .git",
			remote:  "https://github.com/Automaat/sybra-testbed",
			want:    "Automaat/sybra-testbed",
			explain: "gh and the API both accept this form",
		},
		{
			name:    "ssh remote",
			remote:  "git@github.com:Automaat/sybra-testbed.git",
			want:    "Automaat/sybra-testbed",
			explain: "the PR repo is derivable even though the Job cannot push over ssh",
		},
		{
			name:    "pvc bare clone",
			remote:  "/home/sybra/.sybra/clones/FakeOrg/k8s-testbed.git",
			want:    "",
			explain: "the fake-repo smoke has no GitHub remote and must not attempt a PR",
		},
		{
			name:    "empty remote",
			remote:  "",
			want:    "",
			explain: "no remote at all is not a misconfiguration worth failing over",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := appendK8sPRRepoEnv(nil, tt.remote, discardK8sLogger())

			var got string
			for _, e := range env {
				if e.Name == "SYBRA_K8S_PR_REPO" {
					got = e.Value
				}
			}
			if got != tt.want {
				t.Errorf("SYBRA_K8S_PR_REPO = %q, want %q — %s", got, tt.want, tt.explain)
			}
		})
	}
}

func discardK8sLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestK8sRunnerFailedTTLDefaultsWhenUnset(t *testing.T) {
	r := newK8sJobRunner(nil, K8sJobRunnerConfig{})
	if r.failedTTL != 86400 {
		t.Fatalf("failedTTL = %d, want 86400", r.failedTTL)
	}
}

func TestK8sRunnerFailedTTLHonorsConfiguredValue(t *testing.T) {
	r := newK8sJobRunner(nil, K8sJobRunnerConfig{FailedTTL: 3600})
	if r.failedTTL != 3600 {
		t.Fatalf("failedTTL = %d, want 3600", r.failedTTL)
	}
}

func TestPatchJobTTLSendsMergePatch(t *testing.T) {
	client := fake.NewSimpleClientset(&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "sybra-agent-abc", Namespace: "sybra-poc"}})
	r := newFakeK8sJobRunner(client, nil, K8sJobRunnerConfig{Namespace: "sybra-poc"})

	if err := r.patchJobTTL(context.Background(), "sybra-agent-abc", 86400); err != nil {
		t.Fatalf("patchJobTTL: %v", err)
	}
	actions := client.Actions()
	if len(actions) == 0 {
		t.Fatal("expected patch action")
	}
	patchAction, ok := actions[len(actions)-1].(k8stesting.PatchAction)
	if !ok {
		t.Fatalf("last action = %T, want PatchAction", actions[len(actions)-1])
	}
	if patchAction.GetPatchType() != types.MergePatchType {
		t.Fatalf("patch type = %s, want %s", patchAction.GetPatchType(), types.MergePatchType)
	}
	var gotBody map[string]any
	if err := json.Unmarshal(patchAction.GetPatch(), &gotBody); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	spec, _ := gotBody["spec"].(map[string]any)
	if got, want := spec["ttlSecondsAfterFinished"], float64(86400); got != want {
		t.Fatalf("ttlSecondsAfterFinished = %v, want %v", got, want)
	}
}

// TestK8sRunPatchesFailedJobTTL exercises Run() itself end-to-end against a
// mocked Kubernetes API, not just patchJobTTL in isolation — a regression
// that dropped the call, inverted the failedTTL!=ttl guard, or moved it into
// the success branch would pass every other test in this file.
func TestK8sRunPatchesFailedJobTTL(t *testing.T) {
	jobName := "sybra-agent-test-agent"
	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction, ok := action.(k8stesting.GetAction)
		if !ok || getAction.GetName() != jobName {
			return false, nil, nil
		}
		return true, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: "sybra-poc"},
			Status:     batchv1.JobStatus{Failed: 1},
		}, nil
	})
	r := newFakeK8sJobRunner(client, newFakePodClient(client.CoreV1().Pods("sybra-poc")), K8sJobRunnerConfig{Namespace: "sybra-poc", TTL: 300, FailedTTL: 86400})

	a := &Agent{ID: "test-agent", TaskID: "task-1", Provider: "claude"}
	sink := &recordingExecutionSink{}
	r.Run(t.Context(), "kubernetes:test-agent", ExecutionStart{Spec: executionSpecFromAgent(a), Config: RunConfig{}, Sink: sink})

	patchAction := lastPatchAction(client.Actions())
	if patchAction == nil {
		t.Fatal("expected a PATCH to extend TTL on the failed Job, saw none")
	}
	var gotBody map[string]any
	if err := json.Unmarshal(patchAction.GetPatch(), &gotBody); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	spec, _ := gotBody["spec"].(map[string]any)
	patchTTL, _ := spec["ttlSecondsAfterFinished"].(float64)
	if patchTTL != 86400 {
		t.Fatalf("patched ttlSecondsAfterFinished = %v, want 86400", patchTTL)
	}
	if err := sink.completedErr(); err == nil {
		t.Fatal("expected a failed completion event for a failed Job")
	}
}

// TestK8sRunSkipsPatchWhenFailedTTLMatchesTTL confirms the guard actually
// saves the extra API call when there's nothing to extend.
func TestK8sRunSkipsPatchWhenFailedTTLMatchesTTL(t *testing.T) {
	jobName := "sybra-agent-test-agent"
	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction, ok := action.(k8stesting.GetAction)
		if !ok || getAction.GetName() != jobName {
			return false, nil, nil
		}
		return true, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: "sybra-poc"},
			Status:     batchv1.JobStatus{Failed: 1},
		}, nil
	})
	r := newFakeK8sJobRunner(client, newFakePodClient(client.CoreV1().Pods("sybra-poc")), K8sJobRunnerConfig{Namespace: "sybra-poc", TTL: 300, FailedTTL: 300})

	a := &Agent{ID: "test-agent", TaskID: "task-1", Provider: "claude"}
	sink := &recordingExecutionSink{}
	r.Run(t.Context(), "kubernetes:test-agent", ExecutionStart{Spec: executionSpecFromAgent(a), Config: RunConfig{}, Sink: sink})

	if lastPatchAction(client.Actions()) != nil {
		t.Fatal("expected no PATCH when failedTTL equals ttl — the Job already has that TTL from creation")
	}
}

func TestK8sExecutionBackendStopDeletesJobAndRecoveryObservesCancellation(t *testing.T) {
	client := fake.NewSimpleClientset()
	runner := newFakeK8sJobRunner(client, nil, K8sJobRunnerConfig{Namespace: "sybra-poc"})
	backend := &k8sExecutionBackend{
		callbackExecutionBackend: newCallbackExecutionBackend("kubernetes"),
		runner:                   runner,
	}
	runCtx, cancel := context.WithCancel(t.Context())
	initial := &recordingExecutionSink{}
	handle, err := backend.Start(runCtx, ExecutionStart{
		Spec:   ExecutionSpec{ID: "stop-agent", TaskID: "task-1", Provider: providerid.Claude},
		Config: RunConfig{},
		Sink:   initial,
		stop: func(context.Context) error {
			cancel()
			return nil
		},
		inspect: func() ExecutionInspection {
			return ExecutionInspection{State: "running", Agent: View{ID: "stop-agent"}}
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !pollUntil(time.Second, time.Millisecond, func() bool {
		return hasK8sAction(client.Actions(), "create", "jobs")
	}) {
		t.Fatalf("Job was not created; actions = %+v", client.Actions())
	}
	recovered := &recordingExecutionSink{}
	if err := backend.Recover(t.Context(), handle, recovered); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if err := backend.Stop(t.Context(), handle); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := backend.Stop(t.Context(), handle); err != nil {
		t.Fatalf("repeated Stop: %v", err)
	}
	if !pollUntil(time.Second, time.Millisecond, func() bool {
		return hasK8sAction(client.Actions(), "delete", "jobs")
	}) {
		t.Fatalf("Stop did not delete Job; actions = %+v", client.Actions())
	}
	if !pollUntil(time.Second, time.Millisecond, func() bool {
		return errors.Is(recovered.completedErr(), context.Canceled)
	}) {
		t.Fatalf("recovered sink did not observe cancellation; events = %+v", recovered.events)
	}
}

func hasK8sAction(actions []k8stesting.Action, verb, resource string) bool {
	for _, action := range actions {
		if action.GetVerb() == verb && action.GetResource().Resource == resource {
			return true
		}
	}
	return false
}

type recordingExecutionSink struct {
	mu     sync.Mutex
	events []ExecutionEvent
}

func (s *recordingExecutionSink) EmitExecutionEvent(_ context.Context, _ ExecutionHandle, event ExecutionEvent) {
	s.mu.Lock()
	s.events = append(s.events, event)
	s.mu.Unlock()
}

func (s *recordingExecutionSink) completedErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, event := range slices.Backward(s.events) {
		if event.Kind == ExecutionCompleted {
			return event.Err
		}
	}
	return nil
}

func TestPatchJobTTLReturnsErrorOnNonSuccessStatus(t *testing.T) {
	client := fake.NewSimpleClientset(&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "sybra-agent-abc", Namespace: "sybra-poc"}})
	client.PrependReactor("patch", "jobs", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("boom")
	})
	r := newFakeK8sJobRunner(client, nil, K8sJobRunnerConfig{Namespace: "sybra-poc"})

	if err := r.patchJobTTL(context.Background(), "sybra-agent-abc", 86400); err == nil {
		t.Fatal("expected error for non-2xx response")
	}
}

func TestPatchJobTTLRejectsOutOfRangeValue(t *testing.T) {
	r := newFakeK8sJobRunner(fake.NewSimpleClientset(), nil, K8sJobRunnerConfig{Namespace: "sybra-poc"})

	if err := r.patchJobTTL(context.Background(), "sybra-agent-abc", -1); err == nil {
		t.Fatal("expected negative ttl to fail")
	}
}

func TestK8sTTLSecondsRejectsOutOfRangeValue(t *testing.T) {
	if _, err := k8sTTLSeconds(-1); err == nil {
		t.Fatal("expected negative ttl to fail")
	}
}

type fakePodClient struct {
	corev1client typedcorev1.PodInterface
	logs         map[string]string
	logErr       map[string]error
}

func newFakePodClient(corev1client typedcorev1.PodInterface) *fakePodClient {
	return &fakePodClient{corev1client: corev1client, logs: map[string]string{}, logErr: map[string]error{}}
}

func (p *fakePodClient) List(ctx context.Context, opts metav1.ListOptions) (*corev1.PodList, error) {
	return p.corev1client.List(ctx, opts)
}

func (p *fakePodClient) Logs(_ context.Context, podName, _ string) (string, error) {
	if err := p.logErr[podName]; err != nil {
		return "", err
	}
	return p.logs[podName], nil
}

func newFakeK8sJobRunner(clientset *fake.Clientset, pods k8sPodClient, cfg K8sJobRunnerConfig) *k8sJobRunner {
	r := newK8sJobRunner(discardK8sLogger(), cfg)
	r.jobs = clientset.BatchV1().Jobs(cfg.Namespace)
	if pods == nil {
		pods = newFakePodClient(clientset.CoreV1().Pods(cfg.Namespace))
	}
	r.pods = pods
	r.clientErr = nil
	return r
}

func lastPatchAction(actions []k8stesting.Action) k8stesting.PatchAction {
	for _, action := range slices.Backward(actions) {
		if patchAction, ok := action.(k8stesting.PatchAction); ok {
			return patchAction
		}
	}
	return nil
}
