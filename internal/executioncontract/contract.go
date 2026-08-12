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
	BaseSHA string        `json:"baseSha"`
	BaseRef string        `json:"baseRef"`
	Roots   []LogicalRoot `json:"roots"`
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
	if s.RunID == "" || s.EffectID == "" || s.IdempotencyKey == "" {
		return errors.New("execution contract: run, effect, and idempotency identities are required")
	}
	if s.Fence.TaskID == "" || s.Fence.WorkflowGeneration < 0 {
		return errors.New("execution contract: invalid task/workflow generation fence")
	}
	if s.Role == "" || s.Provider.Provider == "" || s.Provider.Model == "" || s.Prompt.Text == "" {
		return errors.New("execution contract: role, provider, model, and prompt are required")
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
	for _, binding := range s.Environment {
		if err := binding.Validate(); err != nil {
			return err
		}
	}
	for _, output := range s.ExpectedOutputs {
		if output.Name == "" || output.Kind == "" || !validRoot(output.Root) || !logicalPath(output.Path) {
			return fmt.Errorf("execution contract: invalid expected output %q", output.Name)
		}
		if !validSensitivity(output.Sensitivity) {
			return fmt.Errorf("execution contract: invalid output sensitivity %q", output.Sensitivity)
		}
	}
	return nil
}

func (b EnvironmentBinding) Validate() error {
	name := strings.ToUpper(strings.TrimSpace(b.Name))
	if name == "" || (b.Value == "") == (b.SecretRef == nil) {
		return fmt.Errorf("execution contract: environment %q must set exactly one of value or secretRef", b.Name)
	}
	if masterCredentialName(name) {
		return fmt.Errorf("execution contract: provider or node credential %q is forbidden", b.Name)
	}
	if b.Value != "" && sensitiveEnvironmentName(name) {
		return fmt.Errorf("execution contract: sensitive environment %q must use a secret reference", b.Name)
	}
	if b.SecretRef != nil && strings.TrimSpace(b.SecretRef.Name) == "" {
		return fmt.Errorf("execution contract: environment %q has an empty secret reference", b.Name)
	}
	return nil
}

func masterCredentialName(name string) bool {
	if slices.Contains([]string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GITHUB_TOKEN", "GH_TOKEN", "KUBECONFIG", "AWS_SECRET_ACCESS_KEY"}, name) {
		return true
	}
	return strings.Contains(name, "MASTER_KEY") || strings.Contains(name, "NODE_TOKEN") || strings.Contains(name, "SERVICE_ACCOUNT_TOKEN")
}

func sensitiveEnvironmentName(name string) bool {
	return strings.Contains(name, "TOKEN") || strings.Contains(name, "PASSWORD") ||
		strings.Contains(name, "SECRET") || strings.Contains(name, "API_KEY") ||
		strings.Contains(name, "PRIVATE_KEY") || strings.Contains(name, "CREDENTIAL")
}

func validateWorkspace(workspace Workspace) error {
	if !gitObjectID.MatchString(workspace.BaseSHA) || workspace.BaseRef == "" || len(workspace.Roots) == 0 {
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

func validRoot(root LogicalRoot) bool {
	return root == RootWorktree || root == RootSidecar || root == RootArtifact || root == RootWorkingMemory
}

func logicalPath(value string) bool {
	clean := path.Clean(strings.ReplaceAll(value, `\\`, "/"))
	return value != "" && clean != "." && clean != ".." && !strings.HasPrefix(clean, "../") &&
		!strings.HasPrefix(clean, "/") && !windowsAbsPath.MatchString(value)
}

func validSensitivity(value Sensitivity) bool {
	return value == SensitivityPublic || value == SensitivityInternal || value == SensitivitySecret
}

func DecodeRunSpec(data []byte) (RunSpec, error) {
	var spec RunSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return RunSpec{}, fmt.Errorf("decode run spec: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return RunSpec{}, err
	}
	return spec, nil
}
