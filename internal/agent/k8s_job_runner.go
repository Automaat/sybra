package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/events"
)

const (
	defaultK8sAPIHost     = "https://kubernetes.default.svc"
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
	Mode      string
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

type k8sJobRunner struct {
	logger    *slog.Logger
	namespace string
	image     string
	command   []string
	ttl       int
	mode      string
	env       []K8sJobEnvVar
	secretEnv []K8sJobSecretEnvVar
	volumes   []K8sJobVolume

	client *http.Client
	apiURL string
	token  string
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
	return &k8sJobRunner{
		logger:    logger,
		namespace: ns,
		image:     image,
		command:   append([]string(nil), cfg.Command...),
		ttl:       ttl,
		mode:      normalizeK8sRunnerMode(cfg.Mode),
		env:       append([]K8sJobEnvVar(nil), cfg.Env...),
		secretEnv: append([]K8sJobSecretEnvVar(nil), cfg.SecretEnv...),
		volumes:   append([]K8sJobVolume(nil), cfg.Volumes...),
		client:    inClusterHTTPClient(logger),
		apiURL:    strings.TrimRight(envOrDefault("KUBERNETES_SERVICE_URL", defaultK8sAPIHost), "/"),
		token:     readServiceAccountToken(),
	}
}

func (r *k8sJobRunner) Run(ctx context.Context, m *Manager, a *Agent, cfg RunConfig) {
	if r == nil {
		a.SetExitErr(fmt.Errorf("kubernetes runner is not configured"))
		m.finalizeRun(ctx, a, "agent.k8s.done")
		return
	}
	if r.token == "" {
		a.SetExitErr(fmt.Errorf("kubernetes service-account token is unavailable"))
		m.finalizeRun(ctx, a, "agent.k8s.done")
		return
	}

	jobName := k8sName("sybra-agent-" + a.ID)
	if err := r.createJob(ctx, jobName, a, cfg); err != nil {
		a.SetExitErr(err)
		m.finalizeRun(ctx, a, "agent.k8s.done")
		return
	}
	a.Command = "kubernetes job/" + jobName
	m.emit(events.AgentState(a.ID), a)

	prevLen := len(a.Output())
	var logOffset int
	var lastEmit time.Time
	var podName string
	logProvider := a.Provider
	if r.mode == k8sRunnerModeFake {
		logProvider = "claude"
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
				logOffset += processK8sLogChunk(ctx, m, a, logProvider, []byte(logs[logOffset:]), &lastEmit)
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
				processK8sLogChunk(ctx, m, a, logProvider, []byte(logs[logOffset:]), &lastEmit)
			}
		}
		if failed {
			a.SetExitErr(fmt.Errorf("kubernetes job %s failed", jobName))
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

func processK8sLogChunk(ctx context.Context, m *Manager, a *Agent, providerName string, chunk []byte, lastEmit *time.Time) int {
	scanner := bufio.NewScanner(bytes.NewReader(chunk))
	scanner.Buffer(make([]byte, 0, 64*1024), headlessScannerBuffer)
	prov := providerByName(providerName)
	processed := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		processed += len(line) + 1
		m.processHeadlessLine(ctx, a, line, lastEmit, prov)
	}
	return processed
}

func (r *k8sJobRunner) createJob(ctx context.Context, name string, a *Agent, cfg RunConfig) error {
	command, env, err := r.jobCommandAndEnv(ctx, a, cfg)
	if err != nil {
		return err
	}
	volumes, mounts := r.volumeSpec()
	container := map[string]any{
		"name":            "agent",
		"image":           r.image,
		"imagePullPolicy": "IfNotPresent",
		"command":         command,
		"env":             env,
	}
	if len(mounts) > 0 {
		container["volumeMounts"] = mounts
	}
	podSpec := map[string]any{
		"restartPolicy": "Never",
		"securityContext": map[string]any{
			"runAsNonRoot": true,
			"runAsUser":    1000,
			"runAsGroup":   1000,
			"fsGroup":      1000,
		},
		"containers": []map[string]any{container},
	}
	if len(volumes) > 0 {
		podSpec["volumes"] = volumes
	}
	body := map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name": name,
			"labels": map[string]string{
				"app.kubernetes.io/name": "sybra-agent",
				"sybra.agent/id":         a.ID,
				"sybra.task/id":          sanitizeLabelValue(a.TaskID),
			},
		},
		"spec": map[string]any{
			"backoffLimit":            0,
			"ttlSecondsAfterFinished": r.ttl,
			"template": map[string]any{
				"metadata": map[string]any{
					"labels": map[string]string{
						"app.kubernetes.io/name": "sybra-agent",
						"sybra.agent/id":         a.ID,
					},
				},
				"spec": podSpec,
			},
		},
	}
	var out map[string]any
	return r.doJSON(ctx, http.MethodPost, "/apis/batch/v1/namespaces/"+url.PathEscape(r.namespace)+"/jobs", body, &out)
}

func (r *k8sJobRunner) volumeSpec() (volumes, mounts []map[string]any) {
	for _, v := range r.volumes {
		if v.Name == "" || v.ClaimName == "" || v.MountPath == "" {
			continue
		}
		volumes = append(volumes, map[string]any{
			"name": v.Name,
			"persistentVolumeClaim": map[string]any{
				"claimName": v.ClaimName,
			},
		})
		mount := map[string]any{
			"name":      v.Name,
			"mountPath": v.MountPath,
		}
		if v.ReadOnly {
			mount["readOnly"] = true
		}
		mounts = append(mounts, mount)
	}
	return volumes, mounts
}

func (r *k8sJobRunner) jobCommandAndEnv(ctx context.Context, a *Agent, cfg RunConfig) (command []string, env []map[string]any, err error) {
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
		for _, pair := range inv.env {
			name, value, ok := strings.Cut(pair, "=")
			if ok && name != "" {
				env = append(env, map[string]any{"name": name, "value": value})
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

func (r *k8sJobRunner) baseEnv(a *Agent, cfg RunConfig) []map[string]any {
	env := []map[string]any{
		{"name": "SYBRA_AGENT_ID", "value": a.ID},
		{"name": "SYBRA_TASK_ID", "value": a.TaskID},
		{"name": "SYBRA_AGENT_PROMPT", "value": cfg.Prompt},
	}
	for _, e := range r.env {
		if e.Name != "" {
			env = append(env, map[string]any{"name": e.Name, "value": e.Value})
		}
	}
	for _, e := range r.secretEnv {
		if e.Name == "" || e.SecretName == "" || e.SecretKey == "" {
			continue
		}
		env = append(env, map[string]any{
			"name": e.Name,
			"valueFrom": map[string]any{
				"secretKeyRef": map[string]any{
					"name": e.SecretName,
					"key":  e.SecretKey,
				},
			},
		})
	}
	return env
}

func appendK8sWorkspaceEnv(env []map[string]any, workspace k8sGitWorkspace) []map[string]any {
	env = append(env, map[string]any{"name": "SYBRA_K8S_WORKDIR", "value": defaultK8sWorkdir})
	if workspace.Remote != "" {
		env = append(env, map[string]any{"name": "SYBRA_K8S_GIT_REMOTE", "value": workspace.Remote})
	}
	if workspace.Branch != "" {
		env = append(env, map[string]any{"name": "SYBRA_K8S_GIT_BRANCH", "value": workspace.Branch})
	}
	return env
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
	if ! git diff --quiet || ! git diff --cached --quiet; then
		git add -A
		git commit -m "chore: persist k8s agent changes" || true
	fi
	if [ -n "${SYBRA_K8S_GIT_BRANCH:-}" ]; then
		git push -u origin HEAD:"$SYBRA_K8S_GIT_BRANCH"
	else
		git push
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

func pushK8sGitWorkspace(ctx context.Context, dir string, workspace k8sGitWorkspace) error {
	if workspace.Remote == "" || workspace.Branch == "" {
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
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
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
	var pods struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	q := url.Values{"labelSelector": {"job-name=" + jobName}}
	err := r.doJSON(ctx, http.MethodGet, "/api/v1/namespaces/"+url.PathEscape(r.namespace)+"/pods?"+q.Encode(), nil, &pods)
	if err != nil || len(pods.Items) == 0 {
		return ""
	}
	return pods.Items[0].Metadata.Name
}

func (r *k8sJobRunner) podLogs(ctx context.Context, podName string) (string, error) {
	endpoint := "/api/v1/namespaces/" + url.PathEscape(r.namespace) + "/pods/" + url.PathEscape(podName) + "/log?container=agent"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.apiURL+endpoint, http.NoBody)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	resp, err := r.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusBadRequest {
		return "", nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("kubernetes pod log %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

func (r *k8sJobRunner) jobDone(ctx context.Context, jobName string) (done, failed bool, err error) {
	var job struct {
		Status struct {
			Succeeded int `json:"succeeded"`
			Failed    int `json:"failed"`
		} `json:"status"`
	}
	if err := r.doJSON(ctx, http.MethodGet, "/apis/batch/v1/namespaces/"+url.PathEscape(r.namespace)+"/jobs/"+url.PathEscape(jobName), nil, &job); err != nil {
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

func (r *k8sJobRunner) doJSON(ctx context.Context, method, endpoint string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.apiURL+endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("kubernetes %s %s: %s: %s", method, endpoint, resp.Status, strings.TrimSpace(string(b)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func inClusterHTTPClient(logger *slog.Logger) *http.Client {
	caPath := "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	ca, err := os.ReadFile(caPath)
	if err != nil {
		return http.DefaultClient
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		logger.Warn("agent.k8s.ca", "path", caPath, "err", "no certificates found")
		return http.DefaultClient
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
}

func readServiceAccountToken() string {
	data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func inClusterNamespace() string {
	data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
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
