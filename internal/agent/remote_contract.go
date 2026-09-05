package agent

import (
	"errors"
	"fmt"
	"time"

	"github.com/Automaat/sybra/internal/executioncontract"
)

// RemoteSidecarPathToken is the host-independent prompt path substituted by
// agentd after it prepares the run's private workspace. A leader must never
// serialize its own absolute sidecar path into a remote prompt.
const RemoteSidecarPathToken = "sybra://sidecar" //nolint:gosec // Logical path marker, not a credential.

// RemoteRunMetadata supplies durable leader-owned identities and logical
// workspace facts that are deliberately absent from RunConfig.
type RemoteRunMetadata struct {
	BuildVersion          string
	RunID                 string
	EffectID              string
	WorkflowID            string
	WorkflowGeneration    int64
	WorkflowStepID        string
	Deadline              time.Time
	WorkspaceRepositoryID string
	WorkspaceBaseSHA      string
	WorkspaceBaseRef      string
	WorkspaceRoots        []executioncontract.LogicalRoot
	Environment           []executioncontract.EnvironmentBinding
	ExpectedOutputs       []executioncontract.ExpectedOutput
	Resources             executioncontract.ResourceLimits
}

// RemoteRunSpec converts an already-admitted execution into the explicit wire
// contract. It never serializes RunConfig itself, callbacks, host paths, or raw
// process environment. Callers must translate approved non-secret inputs or
// scoped secret capabilities into metadata.Environment.
func RemoteRunSpec(start ExecutionStart, metadata RemoteRunMetadata) (executioncontract.RunSpec, error) {
	if start.Spec.ID == "" {
		return executioncontract.RunSpec{}, errors.New("remote run spec: execution identity is required")
	}
	if len(start.Config.ExtraEnv) > 0 {
		return executioncontract.RunSpec{}, errors.New("remote run spec: raw process environment cannot cross the execution boundary")
	}
	if start.Config.BeforeStart != nil {
		return executioncontract.RunSpec{}, errors.New("remote run spec: process-local callback cannot cross the execution boundary")
	}
	providerName := start.Spec.Provider
	if providerName == "" && start.Provider != nil {
		providerName = start.Provider.Name()
	}
	model := start.Spec.Model
	if model == "" {
		model = start.Config.Model
	}
	resources := metadata.Resources
	if resources.MaxTurns == 0 {
		resources.MaxTurns = start.Config.MaxTurns
	}
	if resources.BashTimeoutMillis == 0 {
		resources.BashTimeoutMillis = start.Config.BashTimeoutMs
	}
	spec := executioncontract.RunSpec{
		Version:        executioncontract.CurrentVersion(),
		BuildVersion:   metadata.BuildVersion,
		RunID:          metadata.RunID,
		EffectID:       metadata.EffectID,
		IdempotencyKey: firstNonEmpty(start.Config.IntentID, metadata.EffectID),
		Fence: executioncontract.GenerationFence{
			TaskID:         firstNonEmpty(start.Config.AdmissionTaskKey, start.Spec.TaskID, start.Config.TaskID),
			TaskGeneration: start.Config.TaskGeneration,
			WorkflowID:     metadata.WorkflowID, WorkflowGeneration: metadata.WorkflowGeneration,
			StepID: metadata.WorkflowStepID,
		},
		Role: string(start.Config.Role),
		Provider: executioncontract.ProviderIntent{
			Provider: providerName, Model: model, ReasoningEffort: start.Spec.ReasoningEffort,
		},
		Prompt: executioncontract.Prompt{Text: start.Config.Prompt, OutputSchema: start.Config.OutputSchema},
		Tools: executioncontract.ToolPolicy{
			AllowedTools:       append([]string(nil), start.Config.AllowedTools...),
			RequirePermissions: start.Config.RequirePermissions,
			PermissionMode:     start.Config.HeadlessPermissionMode,
		},
		Deadline:  metadata.Deadline,
		Resources: resources,
		Options: executioncontract.ExecutionOptions{
			Steerable: start.Config.HeadlessSteerable, ForkSubagent: start.Config.ForkSubagent,
			RetryWatchdog: start.Config.RetryWatchdog, FallbackModel: start.Config.FallbackModel,
			RequestedSkill: start.Config.RequestedSkill, SkillExecutionMode: start.Config.SkillExecutionMode,
			SeedWorkingMemory: start.Config.SeedWorkingMemory, ResumeSessionID: start.Config.ResumeSessionID,
			DiscardWorkspaceChanges: start.Config.RemoteDiscardWorkspaceChanges,
		},
		Workspace: executioncontract.Workspace{
			RepositoryID: metadata.WorkspaceRepositoryID, BaseSHA: metadata.WorkspaceBaseSHA, BaseRef: metadata.WorkspaceBaseRef,
			Roots: append([]executioncontract.LogicalRoot(nil), metadata.WorkspaceRoots...),
		},
		Environment:     append([]executioncontract.EnvironmentBinding(nil), metadata.Environment...),
		ExpectedOutputs: append([]executioncontract.ExpectedOutput(nil), metadata.ExpectedOutputs...),
	}
	if spec.Role == "" {
		spec.Role = string(RoleImplementation)
	}
	if err := spec.Validate(); err != nil {
		return executioncontract.RunSpec{}, fmt.Errorf("remote run spec: %w", err)
	}
	return spec, nil
}
