package agent

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/fsutil"
	"gopkg.in/yaml.v3"
)

// Record is the on-disk snapshot of a live agent, written so the next app
// instance can rediscover and reattach to a subprocess that survived a
// restart. One YAML file per agent under the registry dir
// (~/.sybra/agents/<id>.yaml). Written when the PID is known and whenever
// the session ID is first captured; deleted when the agent terminates.
type Record struct {
	ID              string        `yaml:"id"`
	TaskID          string        `yaml:"task_id,omitempty"`
	Name            string        `yaml:"name,omitempty"`
	Role            Role          `yaml:"role,omitempty"`
	Mode            string        `yaml:"mode"`
	Provider        string        `yaml:"provider"`
	Model           string        `yaml:"model,omitempty"`
	RequestedModel  string        `yaml:"requested_model,omitempty"`
	ExperimentID    string        `yaml:"experiment_id,omitempty"`
	VariantID       string        `yaml:"variant_id,omitempty"`
	RoutingReason   string        `yaml:"routing_reason,omitempty"`
	AssignmentUnit  string        `yaml:"assignment_unit,omitempty"`
	AssignmentKey   string        `yaml:"assignment_key,omitempty"`
	DecisionVersion int           `yaml:"decision_version,omitempty"`
	AttemptIntentID string        `yaml:"attempt_intent_id,omitempty"`
	AttemptTaskKey  string        `yaml:"attempt_task_key,omitempty"`
	AttemptTaskGen  uint64        `yaml:"attempt_task_generation,omitempty"`
	AttemptWorktree string        `yaml:"attempt_worktree,omitempty"`
	AttemptWorkGen  uint64        `yaml:"attempt_worktree_generation,omitempty"`
	AttemptAccess   AttemptAccess `yaml:"attempt_access,omitempty"`
	AttemptLeaseID  string        `yaml:"attempt_lease_id,omitempty"`
	AttemptVersion  uint64        `yaml:"attempt_version,omitempty"`
	PID             int           `yaml:"pid"`
	SessionID       string        `yaml:"session_id,omitempty"`
	LogPath         string        `yaml:"log_path,omitempty"`
	CWD             string        `yaml:"cwd,omitempty"`
	SandboxHomeDir  string        `yaml:"sandbox_home_dir,omitempty"`
	StartedAt       time.Time     `yaml:"started_at"`
	ProcStartedAt   string        `yaml:"proc_started_at,omitempty"` // ps lstart, guards PID reuse
	StdinPath       string        `yaml:"stdin_path,omitempty"`      // FIFO for interactive survival
	PendingPrompts  []string      `yaml:"pending_prompts,omitempty"` // queued follow-up turns
	OneShot         bool          `yaml:"one_shot,omitempty"`
	MaxTurns        int           `yaml:"max_turns,omitempty"`
	// RequirePermissions preserves a codex chat's sandbox/approval choice
	// across a restart (codex respawns per turn and would otherwise default
	// to permissive).
	RequirePermissions bool `yaml:"require_permissions,omitempty"`
	// SandboxMode preserves the resolved OS process-sandbox posture across a
	// restart so per-turn conversational agents keep enforce/report/off
	// behavior on the next spawned provider process.
	SandboxMode string `yaml:"sandbox_mode,omitempty"`
	// ReasoningEffort preserves the codex model_reasoning_effort across restarts.
	// Codex convo respawns a fresh process per turn — without this the effort
	// would revert to model default after a restart.
	ReasoningEffort string `yaml:"reasoning_effort,omitempty"`
	// RequestedSkill preserves the workflow-owned skill name across restart so
	// completion persists the same attribution it started with.
	RequestedSkill string `yaml:"requested_skill,omitempty"`
	// SkillExecutionMode preserves how a workflow-owned skill actually ran
	// (native, injected, fallback) so a reattached completion can persist the
	// same attribution it started with.
	SkillExecutionMode string `yaml:"skill_execution_mode,omitempty"`
	// ResolvedSkillSourceHash preserves the privacy-safe source identifier hash
	// across restart so completion stats remain stable.
	ResolvedSkillSourceHash string `yaml:"resolved_skill_source_hash,omitempty"`
	// SkillConformance preserves whether the resolved source exactly matched,
	// fell back, or was unavailable.
	SkillConformance string `yaml:"skill_conformance,omitempty"`
	// OutputSchema preserves whether this run enforces structured output, so a
	// reattached completion still knows to skip the skill-receipt requirement.
	OutputSchema string `yaml:"output_schema,omitempty"`
	// SkillRecoveryAttempt preserves whether this live run is the workflow
	// engine's automatic second-chance retry after a missing conformance
	// receipt.
	SkillRecoveryAttempt bool `yaml:"skill_recovery_attempt,omitempty"`
	// HasOutputSchema preserves whether this run enforced a provider output
	// schema across restart, so a reattached completion still skips the
	// unsatisfiable skill-conformance receipt check for schema-enforced runs
	// instead of downgrading them to unverified and parking on human-required.
	HasOutputSchema bool `yaml:"has_output_schema,omitempty"`
	// PostResultWait* preserve the runner's post-terminal-result teardown
	// decision so reattach can continue the same fast-close/grace path instead
	// of starting a fresh wait window from restart time.
	PostResultWaitReason string    `yaml:"post_result_wait_reason,omitempty"`
	PostResultWaitSince  time.Time `yaml:"post_result_wait_since,omitempty"`
	ForkSubagent         bool      `yaml:"fork_subagent,omitempty"`
	// PromptHash and the RenderedSyntax/RenderedSkills/UnrenderedSkills triple
	// preserve the dispatch-time prompt hash and provider render summary across
	// restart so a reattached completion still emits agent.prompt_rendered
	// instead of dropping it on an empty hash.
	PromptHash       string   `yaml:"prompt_hash,omitempty"`
	RenderedSyntax   string   `yaml:"rendered_syntax,omitempty"`
	RenderedSkills   []string `yaml:"rendered_skills,omitempty"`
	UnrenderedSkills []string `yaml:"unrendered_skills,omitempty"`
}

// survivalRegistry implementations must be safe for concurrent use.
type survivalRegistry interface {
	Save(Record) error
	List() ([]Record, error)
	Delete(string) error
}

// registryStore persists Records as one YAML file per agent under dir.
// It owns registry persistence serialization. Manager.mu owns the live
// in-memory agent map and runtime config, not registry file I/O; keeping the
// registry lock here lets reattach/lifecycle code depend on the narrow
// survivalRegistry interface without carrying Manager's lifecycle mutex.
// Unlike internal/loopagent.Store, this store owns its own serialization lock
// because runner goroutines can save and delete agent records concurrently.
type registryStore struct {
	mu  sync.Mutex
	dir string
}

func newRegistryStore(dir string) (*registryStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create agents dir %s: %w", dir, err)
	}
	return &registryStore{dir: dir}, nil
}

// Save atomically writes the record. A blank ID is rejected so a malformed
// record can never overwrite the directory listing logic.
func (s *registryStore) Save(r Record) error {
	if r.ID == "" {
		return fmt.Errorf("registry: empty agent id")
	}
	// Record is a value type; this marshal captures a complete snapshot before
	// registry file serialization is acquired.
	data, err := yaml.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal record: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return fsutil.AtomicWrite(s.path(r.ID), data)
}

// List returns every persisted record. Unreadable or malformed files are
// skipped rather than failing the whole sweep — a corrupt record must not
// block reattachment of healthy ones.
func (s *registryStore) List() ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	paths, err := fsutil.ListFiles(s.dir, ".yaml")
	if err != nil {
		return nil, fmt.Errorf("read agents dir: %w", err)
	}
	out := make([]Record, 0, len(paths))
	for _, p := range paths {
		data, readErr := os.ReadFile(p)
		if readErr != nil {
			continue
		}
		var r Record
		if yaml.Unmarshal(data, &r) != nil || r.ID == "" {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// Delete removes the record file and the agent's stdin FIFO (if any). A
// missing file is not an error.
func (s *registryStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeFIFO(s.stdinPathForDelete(id), id)
	if err := os.Remove(s.path(id)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete record: %w", err)
	}
	return nil
}

func (s *registryStore) stdinPathForDelete(id string) string {
	data, err := os.ReadFile(s.path(id))
	if err != nil {
		return agentFIFOPath(s.dir, id)
	}
	var r Record
	if yaml.Unmarshal(data, &r) != nil || r.StdinPath == "" {
		return agentFIFOPath(s.dir, id)
	}
	return r.StdinPath
}

func (s *registryStore) removeFIFO(path, id string) {
	// Best-effort FIFO cleanup so detached conversational agents don't leak
	// a named pipe per run under the agents dir.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		slog.Warn("agent.registry.fifo.remove", "id", id, "path", path, "err", err)
	}
}

func (s *registryStore) path(id string) string {
	return filepath.Join(s.dir, id+".yaml")
}

// agentFIFOPath returns the stdin FIFO path for a detached conversational
// agent, alongside its registry record under the agents dir.
func agentFIFOPath(registryDir, id string) string {
	return filepath.Join(registryDir, id+".stdin")
}
