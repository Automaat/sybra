package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/gitexec"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/providerid"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

const (
	defaultK8sNamespace   = "default"
	defaultK8sImage       = "busybox:1.36"
	defaultK8sWorkdir     = "/tmp/sybra-workspace/repo"
	k8sRunnerModeFake     = "fake"
	k8sRunnerModeProvider = "provider"
)

type k8sGitWorkspace struct {
	Remote string
	Branch string
}

// K8sJobRunnerConfig configures the PoC backend that runs headless agents as
// short-lived Kubernetes Jobs. Empty values are resolved from the pod's
// service-account environment/files and safe fake-agent defaults.
type K8sJobRunnerConfig struct {
	Namespace string
	Image     string
	Command   []string
	TTL       int
	FailedTTL int
	Mode      string
	CreatePR  bool
	Env       []K8sJobEnvVar
	SecretEnv []K8sJobSecretEnvVar
	Volumes   []K8sJobVolume
}

type K8sJobEnvVar struct {
	Name  string
	Value string
}

type K8sJobSecretEnvVar struct {
	Name       string
	SecretName string
	SecretKey  string
}

type K8sJobVolume struct {
	Name      string
	ClaimName string
	MountPath string
	ReadOnly  bool
}

type k8sJobClient interface {
	Create(ctx context.Context, job *batchv1.Job, opts metav1.CreateOptions) (*batchv1.Job, error)
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*batchv1.Job, error)
	Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (*batchv1.Job, error)
}

type k8sPodClient interface {
	List(ctx context.Context, opts metav1.ListOptions) (*corev1.PodList, error)
	Logs(ctx context.Context, podName, container string) (string, error)
}

type k8sTypedPodClient struct {
	typedcorev1.PodInterface
}

func (c k8sTypedPodClient) Logs(ctx context.Context, podName, container string) (string, error) {
	req := c.GetLogs(podName, &corev1.PodLogOptions{Container: container})
	stream, err := req.Stream(ctx)
	if err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsBadRequest(err) {
			return "", nil
		}
		return "", err
	}
	defer stream.Close()
	b, err := io.ReadAll(stream)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

type k8sJobRunner struct {
	logger    *slog.Logger
	namespace string
	image     string
	command   []string
	ttl       int
	failedTTL int
	mode      string
	createPR  bool
	env       []K8sJobEnvVar
	secretEnv []K8sJobSecretEnvVar
	volumes   []K8sJobVolume

	jobs      k8sJobClient
	pods      k8sPodClient
	clientErr error
}

func newK8sJobRunner(logger *slog.Logger, cfg K8sJobRunnerConfig) *k8sJobRunner {
	if logger == nil {
		logger = slog.Default()
	}
	ns := strings.TrimSpace(cfg.Namespace)
	if ns == "" {
		ns = inClusterNamespace()
	}
	if ns == "" {
		ns = defaultK8sNamespace
	}
	image := strings.TrimSpace(cfg.Image)
	if image == "" {
		image = defaultK8sImage
	}
	ttl := cfg.TTL
	if ttl == 0 {
		ttl = 300
	}
	failedTTL := cfg.FailedTTL
	if failedTTL == 0 {
		failedTTL = 86400
	}
	jobs, pods, clientErr := inClusterK8sClients(ns)
	return &k8sJobRunner{
		logger:    logger,
		namespace: ns,
		image:     image,
		command:   append([]string(nil), cfg.Command...),
		ttl:       ttl,
		failedTTL: failedTTL,
		mode:      normalizeK8sRunnerMode(cfg.Mode),
		createPR:  cfg.CreatePR,
		env:       append([]K8sJobEnvVar(nil), cfg.Env...),
		secretEnv: append([]K8sJobSecretEnvVar(nil), cfg.SecretEnv...),
		volumes:   append([]K8sJobVolume(nil), cfg.Volumes...),
		jobs:      jobs,
		pods:      pods,
		clientErr: clientErr,
	}
}

func (r *k8sJobRunner) Run(ctx context.Context, m *Manager, a *Agent, cfg RunConfig) {
	if r == nil {
		a.SetExitErr(fmt.Errorf("kubernetes runner is not configured"))
		m.finalizeRun(ctx, a, "agent.k8s.done")
		return
	}
	if r.clientErr != nil {
		a.SetExitErr(fmt.Errorf("kubernetes client: %w", r.clientErr))
		m.finalizeRun(ctx, a, "agent.k8s.done")
		return
	}
	if r.jobs == nil || r.pods == nil {
		a.SetExitErr(fmt.Errorf("kubernetes runner client is unavailable"))
		m.finalizeRun(ctx, a, "agent.k8s.done")
		return
	}

	jobName := k8sName("sybra-agent-" + a.ID)
	if err := r.createJob(ctx, jobName, a, cfg); err != nil {
		a.SetExitErr(err)
		m.finalizeRun(ctx, a, "agent.k8s.done")
		return
	}
	a.SetCommand("kubernetes job/" + jobName)
	m.emit(events.AgentState(a.ID), a)

	prevLen := len(a.Output())
	var logOffset int
	var lastEmit time.Time
	var podName string
	logProvider := a.Provider
	if r.mode == k8sRunnerModeFake {
		logProvider = providerid.Claude
	}

	ticker := time.NewTicker(750 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			a.SetExitErr(ctx.Err())
			m.finalizeRun(ctx, a, "agent.k8s.done")
			return
		case <-ticker.C:
		}

		if podName == "" {
			podName = r.podForJob(ctx, jobName)
		}
		if podName != "" {
			logs, err := r.podLogs(ctx, podName)
			if err == nil && logOffset < len(logs) {
				logOffset += processK8sLogChunk(ctx, m, a, logProvider, []byte(logs[logOffset:]), false, &lastEmit)
			}
		}

		done, failed, err := r.jobDone(ctx, jobName)
		if err != nil {
			a.SetExitErr(err)
			m.finalizeRun(ctx, a, "agent.k8s.done")
			return
		}
		if !done {
			continue
		}
		if podName != "" {
			if logs, err := r.podLogs(ctx, podName); err == nil && logOffset < len(logs) {
				processK8sLogChunk(ctx, m, a, logProvider, []byte(logs[logOffset:]), true, &lastEmit)
			}
		}
		if failed {
			a.SetExitErr(fmt.Errorf("kubernetes job %s failed", jobName))
			if r.failedTTL != r.ttl {
				if perr := r.patchJobTTL(ctx, jobName, r.failedTTL); perr != nil {
					r.logger.Warn("agent.k8s.failed_ttl_patch",
						"job", jobName, "err", perr,
						"hint", "failed-Job retention not extended past ttl_seconds_after_finished; check the patch verb on batch/jobs RBAC")
				}
			}
		} else {
			if err := syncK8sGitWorkspace(ctx, cfg.Dir); err != nil {
				a.SetExitErr(err)
			} else {
				m.finalizeFromResult(a, prevLen)
			}
		}
		m.finalizeRun(ctx, a, "agent.k8s.done")
		return
	}
}

func processK8sLogChunk(ctx context.Context, m *Manager, a *Agent, providerName string, chunk []byte, final bool, lastEmit *time.Time) int {
	prov := providerByName(providerName)
	processed := 0
	for len(chunk) > 0 {
		nl := bytes.IndexByte(chunk, '\n')
		if nl < 0 {
			if !final {
				return processed
			}
			line := chunk
			processed += len(line)
			if len(line) > 0 {
				m.processHeadlessLine(ctx, a, line, lastEmit, prov)
			}
			return processed
		}
		line := chunk[:nl]
		processed += nl + 1
		chunk = chunk[nl+1:]
		if len(line) == 0 {
			continue
		}
		m.processHeadlessLine(ctx, a, line, lastEmit, prov)
	}
	return processed
}

func (r *k8sJobRunner) createJob(ctx context.Context, name string, a *Agent, cfg RunConfig) error {
	command, env, err := r.jobCommandAndEnv(ctx, a, cfg)
	if err != nil {
		return err
	}
	ttlSeconds, err := k8sTTLSeconds(r.ttl)
	if err != nil {
		return err
	}
	volumes, mounts := r.volumeSpec()
	runAsUser := int64(1000)
	runAsGroup := int64(1000)
	fsGroup := int64(1000)
	runAsNonRoot := true
	noRetries := int32(0)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"app.kubernetes.io/name": "sybra-agent",
				"sybra.agent/id":         a.ID,
				"sybra.task/id":          sanitizeLabelValue(a.TaskID),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &noRetries,
			TTLSecondsAfterFinished: &ttlSeconds,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/name": "sybra-agent",
						"sybra.agent/id":         a.ID,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &runAsNonRoot,
						RunAsUser:    &runAsUser,
						RunAsGroup:   &runAsGroup,
						FSGroup:      &fsGroup,
					},
					Containers: []corev1.Container{{
						Name:            "agent",
						Image:           r.image,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Command:         command,
						Env:             env,
						VolumeMounts:    mounts,
					}},
					Volumes: volumes,
				},
			},
		},
	}
	_, err = r.jobs.Create(ctx, job, metav1.CreateOptions{})
	return err
}

func (r *k8sJobRunner) volumeSpec() (volumes []corev1.Volume, mounts []corev1.VolumeMount) {
	for _, v := range r.volumes {
		if v.Name == "" || v.ClaimName == "" || v.MountPath == "" {
			continue
		}
		volumes = append(volumes, corev1.Volume{
			Name: v.Name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: v.ClaimName,
				},
			},
		})
		mount := corev1.VolumeMount{
			Name:      v.Name,
			MountPath: v.MountPath,
		}
		if v.ReadOnly {
			mount.ReadOnly = true
		}
		mounts = append(mounts, mount)
	}
	return volumes, mounts
}

func (r *k8sJobRunner) jobCommandAndEnv(ctx context.Context, a *Agent, cfg RunConfig) (command []string, env []corev1.EnvVar, err error) {
	env = r.baseEnv(a, cfg)
	if len(r.command) > 0 {
		return append([]string(nil), r.command...), env, nil
	}
	if r.mode == k8sRunnerModeProvider {
		workspace, err := detectK8sGitWorkspace(ctx, cfg.Dir)
		if err != nil {
			return nil, nil, err
		}
		if err := pushK8sGitWorkspace(ctx, cfg.Dir, workspace); err != nil {
			return nil, nil, err
		}
		inv, err := buildK8sProviderInvocation(a, cfg, defaultK8sWorkdir)
		if err != nil {
			return nil, nil, err
		}
		env = appendK8sWorkspaceEnv(env, workspace)
		if r.createPR {
			env = appendK8sPRRepoEnv(env, workspace.Remote, r.logger)
		}
		for _, pair := range inv.env {
			name, value, ok := strings.Cut(pair, "=")
			if ok && name != "" {
				env = append(env, corev1.EnvVar{Name: name, Value: value})
			}
		}
		return k8sProviderCommand(inv), env, nil
	}
	script := fmt.Sprintf(`set -eu
printf '{"type":"system","subtype":"init","session_id":"k8s-%s"}\n'
printf '{"type":"assistant","message":{"content":[{"type":"text","text":"k8s poc agent started for task %s"}]}}\n'
sleep 2
printf '{"type":"result","subtype":"success","result":"k8s poc agent completed","session_id":"k8s-%s","total_cost_usd":0}\n'
`, a.ID, cfg.TaskID, a.ID)
	return []string{"sh", "-c", script}, env, nil
}

func (r *k8sJobRunner) baseEnv(a *Agent, cfg RunConfig) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: "SYBRA_AGENT_ID", Value: a.ID},
		{Name: "SYBRA_TASK_ID", Value: a.TaskID},
		{Name: "SYBRA_AGENT_PROMPT", Value: cfg.Prompt},
	}
	for _, e := range r.env {
		if e.Name != "" {
			env = append(env, corev1.EnvVar{Name: e.Name, Value: e.Value})
		}
	}
	for _, e := range r.secretEnv {
		if e.Name == "" || e.SecretName == "" || e.SecretKey == "" {
			continue
		}
		env = append(env, corev1.EnvVar{
			Name: e.Name,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: e.SecretName},
					Key:                  e.SecretKey,
				},
			},
		})
	}
	return env
}

func appendK8sWorkspaceEnv(env []corev1.EnvVar, workspace k8sGitWorkspace) []corev1.EnvVar {
	env = append(env, corev1.EnvVar{Name: "SYBRA_K8S_WORKDIR", Value: defaultK8sWorkdir})
	if workspace.Remote != "" {
		env = append(env, corev1.EnvVar{Name: "SYBRA_K8S_GIT_REMOTE", Value: workspace.Remote})
	}
	if workspace.Branch != "" {
		env = append(env, corev1.EnvVar{Name: "SYBRA_K8S_GIT_BRANCH", Value: workspace.Branch})
	}
	return env
}

// appendK8sPRRepoEnv tells the Job which repo to open its PR against. Silently
// skips a non-GitHub remote: the fake-repo smoke points origin at a bare clone
// on the PVC, which has no PR to open, and that is a valid setup rather than a
// misconfiguration worth failing the run over.
func appendK8sPRRepoEnv(env []corev1.EnvVar, remote string, logger *slog.Logger) []corev1.EnvVar {
	owner, repo, err := project.ParseGitHubURL(remote)
	if err != nil {
		logger.Info("agent.k8s.pr.skip", "reason", "remote is not a github url", "err", err)
		return env
	}
	return append(env, corev1.EnvVar{Name: "SYBRA_K8S_PR_REPO", Value: owner + "/" + repo})
}

func buildK8sProviderInvocation(a *Agent, cfg RunConfig, workdir string) (headlessInvocation, error) {
	cfg.Dir = workdir
	prov, err := providerForInvocation(a, cfg)
	if err != nil {
		return headlessInvocation{}, err
	}
	invocationAgent := &Agent{
		ID:              a.ID,
		TaskID:          a.TaskID,
		Mode:            a.Mode,
		Provider:        prov.Name(),
		Model:           prov.NormalizeModel(a.Model),
		ReasoningEffort: a.ReasoningEffort,
		sessionCWD:      workdir,
	}
	return prov.BuildHeadlessInvocation(invocationAgent, cfg)
}

func k8sProviderCommand(inv headlessInvocation) []string {
	cmd := []string{"sh", "-ceu", k8sProviderWrapperScript(), "--", inv.name}
	cmd = append(cmd, inv.args...)
	return cmd
}

//nolint:dupword // The shell wrapper has repeated shell keywords that dupword misreads.
func k8sProviderWrapperScript() string {
	return `workdir="${SYBRA_K8S_WORKDIR:-/workspace/repo}"
if [ -n "${GITHUB_TOKEN:-}" ]; then
	git config --global url."https://x-access-token:${GITHUB_TOKEN}@github.com/".insteadOf "https://github.com/"
fi
# Clone the prepared task branch when the server provided git workspace data.
if [ -n "${SYBRA_K8S_GIT_REMOTE:-}" ]; then
	rm -rf "$workdir"
	git clone "$SYBRA_K8S_GIT_REMOTE" "$workdir"
	cd "$workdir"
	if [ -n "${SYBRA_K8S_GIT_BRANCH:-}" ]; then
		if git ls-remote --exit-code --heads origin "$SYBRA_K8S_GIT_BRANCH" >/dev/null 2>&1; then
			git checkout "$SYBRA_K8S_GIT_BRANCH"
		else
			git checkout -b "$SYBRA_K8S_GIT_BRANCH"
		fi
	fi
	printf '%s\n' NOTES.md .sybra-context.md >> .git/info/exclude
	git config user.email "${GIT_AUTHOR_EMAIL:-sybra-agent@example.invalid}"
	git config user.name "${GIT_AUTHOR_NAME:-Sybra Agent}"
else
	mkdir -p "$workdir"
	cd "$workdir"
fi
set +e
cmd_name="$1"
shift
if [ -n "${SYBRA_K8S_PROVIDER_BIN:-}" ]; then
	set -- "$SYBRA_K8S_PROVIDER_BIN" "$@"
else
	set -- "$cmd_name" "$@"
fi
"$@"
status=$?
set -e
if [ "$status" -eq 0 ] && [ -n "${SYBRA_K8S_GIT_REMOTE:-}" ]; then
	# git status --porcelain, not git diff: diff only sees tracked files, so an
	# agent that created a new file (the common case) produced no commit and the
	# push below silently reported "Everything up-to-date", losing the work.
	if [ -n "$(git status --porcelain)" ]; then
		git add -A
		git commit -m "chore: persist k8s agent changes" || true
	fi
	if [ -n "${SYBRA_K8S_GIT_BRANCH:-}" ]; then
		git push -u origin HEAD:"$SYBRA_K8S_GIT_BRANCH"
	else
		git push
	fi
	# The Job opens its own PR. Creating it server-side would force the server
	# to hold a GitHub credential and own repo state, when its whole job in
	# Kubernetes is to dispatch Jobs and watch them. sybra-cli records the PR
	# number back through the HTTP API.
	if [ -n "${SYBRA_K8S_PR_REPO:-}" ] && [ -n "${SYBRA_K8S_GIT_BRANCH:-}" ]; then
		sybra-cli pr create "$SYBRA_TASK_ID" \
			--repo "$SYBRA_K8S_PR_REPO" \
			--head "$SYBRA_K8S_GIT_BRANCH" \
			--dir "$workdir" \
			${SYBRA_K8S_PR_TITLE:+--title "$SYBRA_K8S_PR_TITLE"} \
			${SYBRA_K8S_PR_BODY:+--body "$SYBRA_K8S_PR_BODY"}
	fi
fi
exit "$status"
`
}

func detectK8sGitWorkspace(ctx context.Context, dir string) (k8sGitWorkspace, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return k8sGitWorkspace{}, nil
	}
	inside, _ := gitOutput(ctx, dir, "rev-parse", "--is-inside-work-tree")
	if strings.TrimSpace(inside) != "true" {
		return k8sGitWorkspace{}, nil
	}
	remote, err := gitOutput(ctx, dir, "remote", "get-url", "origin")
	if err != nil {
		return k8sGitWorkspace{}, fmt.Errorf("detect k8s git workspace remote: %w", err)
	}
	branch, err := gitOutput(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return k8sGitWorkspace{}, fmt.Errorf("detect k8s git workspace branch: %w", err)
	}
	branch = strings.TrimSpace(branch)
	if branch == "HEAD" {
		branch = ""
	}
	return k8sGitWorkspace{Remote: strings.TrimSpace(remote), Branch: branch}, nil
}

// pushK8sGitWorkspace carries local commits to the remote so the Job's clone can
// see them. It skips entirely when the remote already has everything HEAD does,
// which is the normal k8s case: the agent works inside the Job, so a fresh task
// has nothing local, and the wrapper creates the branch itself when it is
// missing.
//
// The skip is what lets a Kubernetes server run without a GitHub credential.
// Pushing unconditionally meant a real GitHub remote failed here with "could not
// read Username", which aborted job creation — so the server could only dispatch
// at all if it held a token, exactly the coupling the k8s split removes. When
// there really are local commits, a failure here is still fatal: the Job would
// silently work from the wrong base.
func pushK8sGitWorkspace(ctx context.Context, dir string, workspace k8sGitWorkspace) error {
	if workspace.Remote == "" || workspace.Branch == "" {
		return nil
	}
	if !project.HasUnpushedCommits(ctx, dir) {
		return nil
	}
	if _, err := gitOutput(ctx, dir, "push", "-u", "origin", "HEAD:"+workspace.Branch); err != nil {
		return fmt.Errorf("push k8s workspace branch: %w", err)
	}
	return nil
}

func syncK8sGitWorkspace(ctx context.Context, dir string) error {
	workspace, err := detectK8sGitWorkspace(ctx, dir)
	if err != nil {
		return err
	}
	if workspace.Remote == "" || workspace.Branch == "" {
		return nil
	}
	if _, err := gitOutput(ctx, dir, "pull", "--ff-only", "origin", workspace.Branch); err != nil {
		return fmt.Errorf("sync k8s workspace branch: %w", err)
	}
	return nil
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := gitexec.CombinedOutput(ctx, gitexec.Options{Dir: dir}, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func normalizeK8sRunnerMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", k8sRunnerModeFake:
		return k8sRunnerModeFake
	case k8sRunnerModeProvider:
		return k8sRunnerModeProvider
	default:
		return k8sRunnerModeFake
	}
}

func (r *k8sJobRunner) podForJob(ctx context.Context, jobName string) string {
	pods, err := r.pods.List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + jobName})
	if err != nil || len(pods.Items) == 0 {
		return ""
	}
	return pods.Items[0].Name
}

func (r *k8sJobRunner) podLogs(ctx context.Context, podName string) (string, error) {
	return r.pods.Logs(ctx, podName, "agent")
}

func (r *k8sJobRunner) jobDone(ctx context.Context, jobName string) (done, failed bool, err error) {
	job, err := r.jobs.Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		return false, false, err
	}
	if job.Status.Succeeded > 0 {
		return true, false, nil
	}
	if job.Status.Failed > 0 {
		return true, true, nil
	}
	return false, false, nil
}

// patchJobTTL overrides a Job's ttlSecondsAfterFinished after the fact. The
// runner uses a merge patch so failed Jobs can retain logs longer than
// succeeded ones without changing the create-time manifest contract.
func (r *k8sJobRunner) patchJobTTL(ctx context.Context, jobName string, ttlSeconds int) error {
	boundedTTL, err := k8sTTLSeconds(ttlSeconds)
	if err != nil {
		return err
	}
	data, err := json.Marshal(map[string]any{"spec": map[string]any{"ttlSecondsAfterFinished": boundedTTL}})
	if err != nil {
		return err
	}
	_, err = r.jobs.Patch(ctx, jobName, types.MergePatchType, data, metav1.PatchOptions{})
	return err
}

func inClusterK8sClients(namespace string) (k8sJobClient, k8sPodClient, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, nil, err
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	return clientset.BatchV1().Jobs(namespace), k8sTypedPodClient{PodInterface: clientset.CoreV1().Pods(namespace)}, nil
}

func inClusterNamespace() string {
	data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func k8sTTLSeconds(v int) (int32, error) {
	if v < 0 || v > math.MaxInt32 {
		return 0, fmt.Errorf("kubernetes job ttl %d out of int32 range", v)
	}
	return int32(v), nil
}

func k8sName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "sybra-agent"
	}
	if len(out) > 63 {
		out = out[:63]
		out = strings.TrimRight(out, "-")
	}
	return out
}

func sanitizeLabelValue(s string) string {
	if s == "" {
		return "none"
	}
	return k8sName(path.Base(s))
}
