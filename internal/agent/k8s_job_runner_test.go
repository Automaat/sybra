package agent

import (
	"strings"
	"testing"
)

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
	env := r.baseEnv(&Agent{ID: "agent-1", TaskID: "task-1"}, RunConfig{Prompt: "hello"})
	if len(env) != 5 {
		t.Fatalf("env len = %d, want 5: %#v", len(env), env)
	}
	secret, ok := env[4]["valueFrom"].(map[string]any)
	if !ok {
		t.Fatalf("secret env missing valueFrom: %#v", env[4])
	}
	ref, ok := secret["secretKeyRef"].(map[string]any)
	if !ok {
		t.Fatalf("secret env missing secretKeyRef: %#v", secret)
	}
	if ref["name"] != "sybra-provider-api-keys" || ref["key"] != "anthropic_api_key" {
		t.Fatalf("secretKeyRef = %#v", ref)
	}
}

func TestK8sRunnerVolumeSpecProjectsPVCMounts(t *testing.T) {
	r := newK8sJobRunner(nil, K8sJobRunnerConfig{
		Volumes: []K8sJobVolume{{
			Name:      "sybra-home",
			ClaimName: "sybra-home",
			MountPath: "/home/sybra/.sybra",
		}},
	})

	volumes, mounts := r.volumeSpec()
	if len(volumes) != 1 || len(mounts) != 1 {
		t.Fatalf("volume spec len = volumes:%d mounts:%d, want 1/1", len(volumes), len(mounts))
	}
	pvc, ok := volumes[0]["persistentVolumeClaim"].(map[string]any)
	if !ok {
		t.Fatalf("volume missing pvc spec: %#v", volumes[0])
	}
	if pvc["claimName"] != "sybra-home" {
		t.Fatalf("claimName = %#v, want sybra-home", pvc["claimName"])
	}
	if mounts[0]["mountPath"] != "/home/sybra/.sybra" {
		t.Fatalf("mountPath = %#v, want /home/sybra/.sybra", mounts[0]["mountPath"])
	}
}

func TestK8sProviderInvocationRunsInPodWorkdir(t *testing.T) {
	cfg := RunConfig{Provider: "codex", Mode: "headless", Prompt: "hello", RequirePermissions: false}
	a := &Agent{ID: "agent-1", TaskID: "task-1", Provider: "codex", Mode: "headless", Model: "gpt-5.5"}

	inv, err := buildK8sProviderInvocation(a, cfg, defaultK8sWorkdir)
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

	inv, err := buildK8sProviderInvocation(a, cfg, defaultK8sWorkdir)
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

	inv, err := buildK8sProviderInvocation(a, cfg, defaultK8sWorkdir)
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
