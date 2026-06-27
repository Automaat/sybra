package agent

import (
	"fmt"
	"os"
	"path/filepath"
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
	ID             string    `yaml:"id"`
	TaskID         string    `yaml:"task_id,omitempty"`
	Name           string    `yaml:"name,omitempty"`
	Mode           string    `yaml:"mode"`
	Provider       string    `yaml:"provider"`
	Model          string    `yaml:"model,omitempty"`
	ExperimentID   string    `yaml:"experiment_id,omitempty"`
	VariantID      string    `yaml:"variant_id,omitempty"`
	AssignmentUnit string    `yaml:"assignment_unit,omitempty"`
	AssignmentKey  string    `yaml:"assignment_key,omitempty"`
	PID            int       `yaml:"pid"`
	SessionID      string    `yaml:"session_id,omitempty"`
	LogPath        string    `yaml:"log_path,omitempty"`
	CWD            string    `yaml:"cwd,omitempty"`
	StartedAt      time.Time `yaml:"started_at"`
	ProcStartedAt  string    `yaml:"proc_started_at,omitempty"` // ps lstart, guards PID reuse
	StdinPath      string    `yaml:"stdin_path,omitempty"`      // FIFO for interactive survival
	MaxTurns       int       `yaml:"max_turns,omitempty"`
	// RequirePermissions preserves a codex chat's sandbox/approval choice
	// across a restart (codex respawns per turn and would otherwise default
	// to permissive).
	RequirePermissions bool `yaml:"require_permissions,omitempty"`
	OneShot            bool `yaml:"one_shot,omitempty"`
	// ReasoningEffort preserves the codex model_reasoning_effort across restarts.
	// Codex convo respawns a fresh process per turn — without this the effort
	// would revert to model default after a restart.
	ReasoningEffort string `yaml:"reasoning_effort,omitempty"`
}

// registryStore persists Records as one YAML file per agent under dir.
// Mirrors internal/loopagent.Store.
type registryStore struct {
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
	data, err := yaml.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal record: %w", err)
	}
	return fsutil.AtomicWrite(s.path(r.ID), data)
}

// List returns every persisted record. Unreadable or malformed files are
// skipped rather than failing the whole sweep — a corrupt record must not
// block reattachment of healthy ones.
func (s *registryStore) List() ([]Record, error) {
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
	// Best-effort FIFO cleanup so detached conversational agents don't leak
	// a named pipe per run under the agents dir.
	_ = os.Remove(s.fifoPath(id))
	if err := os.Remove(s.path(id)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete record: %w", err)
	}
	return nil
}

func (s *registryStore) path(id string) string {
	return filepath.Join(s.dir, id+".yaml")
}

// fifoPath returns the stdin FIFO path for a detached conversational agent,
// alongside its registry record under the agents dir.
func (s *registryStore) fifoPath(id string) string {
	return filepath.Join(s.dir, id+".stdin")
}
