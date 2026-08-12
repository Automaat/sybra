package executioncontract

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
)

const CurrentMajor = 1

var (
	ErrUnsupportedMajor = errors.New("execution contract: unsupported protocol major")
	windowsAbsPath      = regexp.MustCompile(`^[A-Za-z]:[\\/]`)
	gitObjectID         = regexp.MustCompile(`^[0-9a-fA-F]{40}([0-9a-fA-F]{24})?$`)
	invalidGitRefChar   = regexp.MustCompile(`[\x00-\x20\x7f~^:?*\[\\]`)
)

type Version struct {
	Major int `json:"major"`
	Minor int `json:"minor"`
}

// CurrentVersion returns the protocol version emitted by this build.
func CurrentVersion() Version {
	return Version{Major: CurrentMajor}
}

func (v Version) Validate() error {
	if v.Major != CurrentMajor {
		return fmt.Errorf("%w: got %d, support %d", ErrUnsupportedMajor, v.Major, CurrentMajor)
	}
	if v.Minor < 0 {
		return errors.New("execution contract: protocol minor must be non-negative")
	}
	return nil
}

// Negotiation is exchanged before commands. BuildVersion is diagnostic only;
// compatibility is determined by the overlapping protocol major/minor range.
type Negotiation struct {
	ProtocolMin  Version `json:"protocolMin"`
	ProtocolMax  Version `json:"protocolMax"`
	BuildVersion string  `json:"buildVersion"`
}

func Negotiate(local, remote Negotiation) (Version, error) {
	if local.ProtocolMin.Major != CurrentMajor || local.ProtocolMax.Major != CurrentMajor ||
		remote.ProtocolMin.Major != CurrentMajor || remote.ProtocolMax.Major != CurrentMajor {
		return Version{}, ErrUnsupportedMajor
	}
	if local.ProtocolMin.Minor < 0 || remote.ProtocolMin.Minor < 0 ||
		local.ProtocolMax.Minor < local.ProtocolMin.Minor || remote.ProtocolMax.Minor < remote.ProtocolMin.Minor {
		return Version{}, errors.New("execution contract: invalid protocol version range")
	}
	minMinor := max(local.ProtocolMin.Minor, remote.ProtocolMin.Minor)
	maxMinor := min(local.ProtocolMax.Minor, remote.ProtocolMax.Minor)
	if minMinor > maxMinor {
		return Version{}, errors.New("execution contract: no compatible protocol version")
	}
	return Version{Major: CurrentMajor, Minor: maxMinor}, nil
}

type Sensitivity string

const (
	SensitivityPublic   Sensitivity = "public"
	SensitivityInternal Sensitivity = "internal"
	SensitivitySecret   Sensitivity = "secret"
)

type LogicalRoot string

const (
	RootWorktree      LogicalRoot = "worktree"
	RootSidecar       LogicalRoot = "sidecar"
	RootArtifact      LogicalRoot = "artifact"
	RootWorkingMemory LogicalRoot = "working_memory"
)

type GenerationFence struct {
	TaskID             string `json:"taskId"`
	TaskGeneration     uint64 `json:"taskGeneration"`
	WorkflowID         string `json:"workflowId"`
	WorkflowGeneration int64  `json:"workflowGeneration"`
	StepID             string `json:"stepId"`
}

type ProviderIntent struct {
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
}

type Prompt struct {
	// Text may contain repository/task content and injected working memory. It
	// is sensitive even when it contains no credential and must not be logged.
	Text string `json:"text"`
	// OutputSchema can contain user-authored names/descriptions; treat it with
	// the same confidentiality as Text.
	OutputSchema string `json:"outputSchema,omitempty"`
}

type ToolPolicy struct {
	AllowedTools       []string `json:"allowedTools,omitempty"`
	RequirePermissions bool     `json:"requirePermissions"`
	PermissionMode     string   `json:"permissionMode,omitempty"`
}

type ResourceLimits struct {
	CPUMillis             int64 `json:"cpuMillis,omitempty"`
	MemoryBytes           int64 `json:"memoryBytes,omitempty"`
	EphemeralStorageBytes int64 `json:"ephemeralStorageBytes,omitempty"`
	MaxTurns              int   `json:"maxTurns,omitempty"`
	BashTimeoutMillis     int   `json:"bashTimeoutMillis,omitempty"`
}

type ExecutionOptions struct {
	Steerable          bool   `json:"steerable,omitempty"`
	ForkSubagent       bool   `json:"forkSubagent,omitempty"`
	RetryWatchdog      int    `json:"retryWatchdog,omitempty"`
	FallbackModel      string `json:"fallbackModel,omitempty"`
	RequestedSkill     string `json:"requestedSkill,omitempty"`
	SkillExecutionMode string `json:"skillExecutionMode,omitempty"`
	SeedWorkingMemory  bool   `json:"seedWorkingMemory,omitempty"`
	// ResumeSessionID is provider conversation state and may expose prior task
	// context. Encrypt it in transit/at rest and never include it in logs.
	ResumeSessionID string `json:"resumeSessionId,omitempty"`
}

type Workspace struct {
	// RepositoryID is an opaque, leader-owned repository identity. Daemons
	// resolve it through local configuration; repository URLs and host paths do
	// not cross the execution boundary.
	RepositoryID string        `json:"repositoryId"`
	BaseSHA      string        `json:"baseSha"`
	BaseRef      string        `json:"baseRef"`
	Roots        []LogicalRoot `json:"roots"`
}

// SecretRef is a worker-scoped capability name, never a credential value.
// Provider API keys and node master credentials are forbidden here because a
// compromised run must not gain reusable infrastructure authority.
type SecretRef struct {
	Name string `json:"name"`
}

type EnvironmentBinding struct {
	Name string `json:"name"`
	// Value is allowed only for public, non-secret configuration. Validation
	// rejects credential-shaped names; sensitive inputs use SecretRef.
	Value     string     `json:"value,omitempty"`
	SecretRef *SecretRef `json:"secretRef,omitempty"`
}

type ExpectedOutput struct {
	Name        string      `json:"name"`
	Kind        string      `json:"kind"`
	Root        LogicalRoot `json:"root"`
	Path        string      `json:"path"`
	Required    bool        `json:"required,omitempty"`
	Sensitivity Sensitivity `json:"sensitivity"`
	MaxBytes    int64       `json:"maxBytes,omitempty"`
	MediaTypes  []string    `json:"mediaTypes,omitempty"`
}

type RunSpec struct {
	Version         Version              `json:"version"`
	BuildVersion    string               `json:"buildVersion"`
	RunID           string               `json:"runId"`
	EffectID        string               `json:"effectId"`
	IdempotencyKey  string               `json:"idempotencyKey"`
	Fence           GenerationFence      `json:"fence"`
	Role            string               `json:"role"`
	Provider        ProviderIntent       `json:"provider"`
	Prompt          Prompt               `json:"prompt"`
	Tools           ToolPolicy           `json:"tools"`
	Deadline        time.Time            `json:"deadline"`
	Resources       ResourceLimits       `json:"resources"`
	Options         ExecutionOptions     `json:"options"`
	Workspace       Workspace            `json:"workspace"`
	Environment     []EnvironmentBinding `json:"environment,omitempty"`
	ExpectedOutputs []ExpectedOutput     `json:"expectedOutputs,omitempty"`
}

func (s RunSpec) Validate() error {
	if err := s.Version.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(s.BuildVersion) == "" {
		return errors.New("execution contract: build version is required")
	}
	if !contractID.MatchString(s.RunID) || !contractID.MatchString(s.EffectID) || !contractID.MatchString(s.IdempotencyKey) {
		return errors.New("execution contract: run, effect, and idempotency identities are required")
	}
	if s.Fence.TaskID == "" || s.Fence.WorkflowID == "" || s.Fence.StepID == "" || s.Fence.WorkflowGeneration < 0 {
		return errors.New("execution contract: invalid task/workflow generation fence")
	}
	if s.Role == "" || s.Provider.Provider == "" || s.Provider.Model == "" || s.Prompt.Text == "" {
		return errors.New("execution contract: role, provider, model, and prompt are required")
	}
	if s.Options.SeedWorkingMemory && !roleAuthorsCode(s.Role) {
		return errors.New("execution contract: verifier roles cannot inherit private working memory")
	}
	if s.Deadline.IsZero() {
		return errors.New("execution contract: deadline is required")
	}
	if s.Resources.CPUMillis < 0 || s.Resources.MemoryBytes < 0 || s.Resources.EphemeralStorageBytes < 0 ||
		s.Resources.MaxTurns < 0 || s.Resources.BashTimeoutMillis < 0 {
		return errors.New("execution contract: resource limits must be non-negative")
	}
	if err := validateWorkspace(s.Workspace); err != nil {
		return err
	}
	declaredRoots := make(map[LogicalRoot]struct{}, len(s.Workspace.Roots))
	for _, root := range s.Workspace.Roots {
		declaredRoots[root] = struct{}{}
	}
	seenEnvironment := map[string]bool{}
	for _, binding := range s.Environment {
		if err := binding.Validate(); err != nil {
			return err
		}
		name := strings.ToUpper(strings.TrimSpace(binding.Name))
		if seenEnvironment[name] {
			return fmt.Errorf("execution contract: duplicate environment binding %q", binding.Name)
		}
		seenEnvironment[name] = true
		if binding.SecretRef != nil && !strings.HasPrefix(binding.SecretRef.Name, "run/"+s.RunID+"/") {
			return fmt.Errorf("execution contract: environment %q secret reference belongs to another run", binding.Name)
		}
	}
	seenOutputNames, seenOutputPaths := map[string]bool{}, map[string]bool{}
	for _, output := range s.ExpectedOutputs {
		_, rootDeclared := declaredRoots[output.Root]
		if output.Name == "" || output.Kind == "" || !rootDeclared || !logicalPath(output.Path) {
			return fmt.Errorf("execution contract: invalid expected output %q", output.Name)
		}
		if !validSensitivity(output.Sensitivity) {
			return fmt.Errorf("execution contract: invalid output sensitivity %q", output.Sensitivity)
		}
		if output.MaxBytes < 0 || output.MaxBytes > MaxArtifactEntrySize {
			return fmt.Errorf("execution contract: invalid size limit for output %q", output.Name)
		}
		for _, mediaType := range output.MediaTypes {
			if strings.TrimSpace(mediaType) == "" || strings.ContainsAny(mediaType, " \t\r\n") {
				return fmt.Errorf("execution contract: invalid media type for output %q", output.Name)
			}
		}
		key := string(output.Root) + ":" + output.Path
		if seenOutputNames[output.Name] || seenOutputPaths[key] {
			return fmt.Errorf("execution contract: duplicate expected output %q", output.Name)
		}
		seenOutputNames[output.Name], seenOutputPaths[key] = true, true
		if output.Root == RootWorkingMemory && !s.Options.SeedWorkingMemory {
			return errors.New("execution contract: working-memory output requires author memory seeding")
		}
		if output.Root == RootWorkingMemory && output.Sensitivity != SensitivitySecret {
			return errors.New("execution contract: working-memory output must remain secret")
		}
	}
	return nil
}

func roleAuthorsCode(role string) bool {
	switch role {
	case "implementation", "fix-review", "pr-fix", "test-fix", "human-review":
		return true
	default:
		return false
	}
}

func (b EnvironmentBinding) Validate() error {
	name := strings.ToUpper(strings.TrimSpace(b.Name))
	hasValue, hasSecretRef := strings.TrimSpace(b.Value) != "", b.SecretRef != nil
	if name == "" || hasValue == hasSecretRef {
		return fmt.Errorf("execution contract: environment %q must set exactly one of value or secretRef", b.Name)
	}
	if slices.Contains([]string{"SYBRA_WORKTREE_ROOT", "SYBRA_SIDECAR_ROOT", "SYBRA_ARTIFACT_ROOT", "SYBRA_WORKING_MEMORY_ROOT"}, name) {
		return fmt.Errorf("execution contract: daemon-owned environment %q is forbidden", b.Name)
	}
	if masterCredentialName(name) {
		return fmt.Errorf("execution contract: provider or node credential %q is forbidden", b.Name)
	}
	if b.Value != "" && sensitiveEnvironmentName(name) {
		return fmt.Errorf("execution contract: sensitive environment %q must use a secret reference", b.Name)
	}
	if b.SecretRef != nil {
		refName := strings.TrimSpace(b.SecretRef.Name)
		segments := strings.Split(refName, "/")
		if len(segments) < 3 || segments[0] != "run" || slices.Contains(segments, "") || !logicalPath(refName) {
			return fmt.Errorf("execution contract: environment %q requires a run-scoped secret reference", b.Name)
		}
		if masterCredentialName(strings.ToUpper(segments[len(segments)-1])) {
			return fmt.Errorf("execution contract: provider or node credential reference %q is forbidden", b.SecretRef.Name)
		}
	}
	return nil
}

func masterCredentialName(name string) bool {
	if slices.Contains([]string{
		"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "GITHUB_TOKEN", "GH_TOKEN", "KUBECONFIG",
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"SYBRA_AUTH_TOKEN", "SYBRA_SERVER_TOKEN", "SYBRA_FOLLOWER_TOKEN",
	}, name) {
		return true
	}
	if strings.Contains(name, "MASTER_KEY") || strings.Contains(name, "NODE_TOKEN") || strings.Contains(name, "SERVICE_ACCOUNT_TOKEN") {
		return true
	}
	for _, provider := range []string{"ANTHROPIC", "OPENAI", "CLAUDE", "CODEX", "COPILOT", "GITHUB", "AZURE", "GOOGLE", "GCP", "KUBE"} {
		if strings.Contains(name, provider) && sensitiveEnvironmentName(name) {
			return true
		}
	}
	return false
}

func sensitiveEnvironmentName(name string) bool {
	return strings.Contains(name, "TOKEN") || strings.Contains(name, "PASSWORD") ||
		strings.Contains(name, "SECRET") || strings.Contains(name, "API_KEY") ||
		strings.Contains(name, "PRIVATE_KEY") || strings.Contains(name, "CREDENTIAL")
}

func validateWorkspace(workspace Workspace) error {
	if !contractID.MatchString(workspace.RepositoryID) || !gitObjectID.MatchString(workspace.BaseSHA) || !validFullGitRef(workspace.BaseRef) || len(workspace.Roots) == 0 {
		return errors.New("execution contract: workspace base SHA/ref and logical roots are required")
	}
	seen := map[LogicalRoot]bool{}
	for _, root := range workspace.Roots {
		if !validRoot(root) || seen[root] {
			return fmt.Errorf("execution contract: invalid or duplicate logical root %q", root)
		}
		seen[root] = true
	}
	return nil
}

func validFullGitRef(ref string) bool {
	if !strings.HasPrefix(ref, "refs/") || strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".") ||
		strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.Contains(ref, "//") ||
		invalidGitRefChar.MatchString(ref) {
		return false
	}
	for component := range strings.SplitSeq(ref, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
	}
	return true
}

func validRoot(root LogicalRoot) bool {
	return root == RootWorktree || root == RootSidecar || root == RootArtifact || root == RootWorkingMemory
}

func logicalPath(value string) bool {
	normalized := strings.ReplaceAll(value, `\`, "/")
	if normalized != value || strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return false
	}
	if slices.Contains(strings.Split(normalized, "/"), "..") {
		return false
	}
	clean := path.Clean(normalized)
	return value != "" && clean == value && clean != "." && clean != ".." && !strings.HasPrefix(clean, "../") &&
		!strings.HasPrefix(clean, "/") && !windowsAbsPath.MatchString(value)
}

func validSensitivity(value Sensitivity) bool {
	return value == SensitivityPublic || value == SensitivityInternal || value == SensitivitySecret
}

func DecodeRunSpec(data []byte) (RunSpec, error) {
	if err := rejectProcessLocalFields(data); err != nil {
		return RunSpec{}, err
	}
	var spec RunSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return RunSpec{}, fmt.Errorf("decode run spec: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return RunSpec{}, err
	}
	return spec, nil
}

func rejectProcessLocalFields(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return fmt.Errorf("decode execution contract object: %w", err)
	}
	for name := range fields {
		if processLocalField(name) {
			return fmt.Errorf("execution contract: process-local field %q is forbidden", name)
		}
	}
	return nil
}

func processLocalField(name string) bool {
	normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(name))
	switch normalized {
	case "dir", "sidecardir", "extraenv", "beforestart", "process", "processobject", "manager", "agent", "runconfig":
		return true
	default:
		return false
	}
}
