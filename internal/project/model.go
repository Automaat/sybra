package project

import (
	"strings"
	"time"
)

// ProjectType classifies a Project for per-machine automation routing (see
// Config.AllowsProjectType and the "Per-Machine Automations" section of
// CLAUDE.md) and for work-data confidentiality gating.
type ProjectType string

const (
	ProjectTypePet  ProjectType = "pet"
	ProjectTypeWork ProjectType = "work"
)

// ChecksConfig defines shell commands run as git hooks in agent worktrees.
// Commands execute in the worktree root; non-zero exit blocks the git operation.
type ChecksConfig struct {
	PreCommit []string `yaml:"pre_commit,omitempty" json:"preCommit,omitempty"`
	PrePush   []string `yaml:"pre_push,omitempty"   json:"prePush,omitempty"`
	// Codegen is the project's deterministic mutation pass (formatters,
	// goimports, go mod tidy, generated-file refresh) run by the codegen_gate
	// workflow step right before PR handoff. Each entry is a shell command run
	// in the worktree root, in order.
	Codegen []string `yaml:"codegen,omitempty" json:"codegen,omitempty"`
	// Verify is the project's deterministic verification suite (tests /
	// typecheck), run by the verify_checks workflow step on the agent's branch
	// before review so an implementation that does not pass its own tests
	// cannot reach a PR. Opt-in: unset means the check is skipped. Each entry
	// is a shell command run in the worktree root, in order.
	Verify []string `yaml:"verify,omitempty" json:"verify,omitempty"`
}

// RepoConfig is the subset of Sybra config that can be defined in a repo's
// .sybra.yaml file. Repo config takes priority over the app-level project config.
type RepoConfig struct {
	Checks *ChecksConfig `yaml:"checks,omitempty" json:"checks,omitempty"`
	// Setup is the shell commands run in every newly created worktree before
	// any agent starts. Declared in the repo so every machine that checks out
	// this project gets identical bootstrap (tool installs, dependency fetches)
	// without per-instance UI toil.
	Setup []string `yaml:"setup,omitempty" json:"setup,omitempty"`
	// ManualTest tells the testing workflow how this project can be exercised
	// through a user/operator-facing surface instead of only via unit tests.
	ManualTest *ManualTestConfig `yaml:"manual_test,omitempty" json:"manualTest,omitempty"`
}

// ManualTestKind identifies the runnable surface a test-runner should drive.
type ManualTestKind string

const (
	ManualTestKindWeb     ManualTestKind = "web"
	ManualTestKindCLI     ManualTestKind = "cli"
	ManualTestKindServer  ManualTestKind = "server"
	ManualTestKindDesktop ManualTestKind = "desktop"
	ManualTestKindK8s     ManualTestKind = "k8s"
	ManualTestKindLibrary ManualTestKind = "library"
)

// ManualTestConfig is repo-declared black-box testing guidance from .sybra.yaml.
// Commands execute in the worktree root when a test-runner chooses to use them.
type ManualTestConfig struct {
	Kind          ManualTestKind `yaml:"kind,omitempty" json:"kind,omitempty"`
	Command       string         `yaml:"command,omitempty" json:"command,omitempty"`
	HealthURL     string         `yaml:"health_url,omitempty" json:"healthUrl,omitempty"`
	ProbeCommands []string       `yaml:"probe_commands,omitempty" json:"probeCommands,omitempty"`
}

// MergeSetup combines repo-declared setup (.sybra.yaml) with app-level setup
// commands (~/.sybra/projects/<id>.yaml → SetupCommands). Repo commands run
// first so the canonical toolchain bootstrap happens before any per-machine
// additions (e.g. "also copy my .env.local"). Either side may be empty.
func MergeSetup(repo, app []string) []string {
	if len(repo) == 0 {
		return app
	}
	if len(app) == 0 {
		return repo
	}
	merged := make([]string, 0, len(repo)+len(app))
	merged = append(merged, repo...)
	merged = append(merged, app...)
	return merged
}

// MergeChecks returns a merged ChecksConfig where repo fields take priority over
// app fields on a per-slice basis. A non-nil, non-empty slice in repo wins.
func MergeChecks(repo, app *ChecksConfig) *ChecksConfig {
	if repo == nil && app == nil {
		return nil
	}
	out := &ChecksConfig{}
	if repo != nil && len(repo.PreCommit) > 0 {
		out.PreCommit = repo.PreCommit
	} else if app != nil {
		out.PreCommit = app.PreCommit
	}
	if repo != nil && len(repo.PrePush) > 0 {
		out.PrePush = repo.PrePush
	} else if app != nil {
		out.PrePush = app.PrePush
	}
	if repo != nil && len(repo.Codegen) > 0 {
		out.Codegen = repo.Codegen
	} else if app != nil {
		out.Codegen = app.Codegen
	}
	if repo != nil && len(repo.Verify) > 0 {
		out.Verify = repo.Verify
	} else if app != nil {
		out.Verify = app.Verify
	}
	if len(out.PreCommit) == 0 && len(out.PrePush) == 0 && len(out.Codegen) == 0 && len(out.Verify) == 0 {
		return nil
	}
	return out
}

// MergeManualTest returns repo-declared manual-test guidance when present,
// falling back to machine-local project config.
func MergeManualTest(repo, app *ManualTestConfig) *ManualTestConfig {
	if repo != nil {
		return repo
	}
	return app
}

// SandboxConfig describes how to spin up an isolated app environment for a task.
// Three modes are supported, detected by field presence:
//   - K8s mode:             Cluster != ""
//   - Docker existing file: ComposeFile != ""
//   - Docker generated:     Image != "" || Build != ""
type SandboxConfig struct {
	// Docker mode — generated compose
	Image string   `yaml:"image,omitempty" json:"image,omitempty"`
	Build string   `yaml:"build,omitempty" json:"build,omitempty"`
	With  []string `yaml:"with,omitempty"  json:"with,omitempty"`

	// Docker mode — existing compose file in the repo
	ComposeFile string `yaml:"compose_file,omitempty" json:"composeFile,omitempty"`
	Service     string `yaml:"service,omitempty"     json:"service,omitempty"`

	// Shared docker fields
	Port    int               `yaml:"port,omitempty"     json:"port,omitempty"`
	Env     map[string]string `yaml:"env,omitempty"      json:"env,omitempty"`
	EnvFile string            `yaml:"env_file,omitempty" json:"envFile,omitempty"`

	// K8s mode — presence of Cluster triggers k8s path
	Cluster string `yaml:"cluster,omitempty" json:"cluster,omitempty"`
	Deploy  string `yaml:"deploy,omitempty"  json:"deploy,omitempty"`
}

// IsK8s reports whether this config uses k8s mode.
func (s *SandboxConfig) IsK8s() bool { return s != nil && s.Cluster != "" }

// IsDocker reports whether this config uses docker mode.
func (s *SandboxConfig) IsDocker() bool { return s != nil && s.Cluster == "" }

// ProjectStatus tracks whether a project's bare clone is ready.
type ProjectStatus string

const (
	// ProjectStatusReady means the bare clone exists and is usable.
	ProjectStatusReady ProjectStatus = "ready"
	// ProjectStatusCloning means a bare-clone is in progress.
	ProjectStatusCloning ProjectStatus = "cloning"
	// ProjectStatusError means the bare-clone failed.
	ProjectStatusError ProjectStatus = "error"
)

// WorktreeBaseRefFresh branches new worktrees off origin/<default> — always
// starts from the latest pushed remote state.
const WorktreeBaseRefFresh = "fresh"

// WorktreeBaseRefHead branches new worktrees off the local HEAD of the
// default branch — picks up commits that exist locally but haven't been
// pushed yet.
const WorktreeBaseRefHead = "head"

// Project is the YAML-backed metadata record for a GitHub repo mirrored
// under ~/.sybra/clones/ as a bare clone. Store persists these under
// ~/.sybra/projects/; task-level checkouts are created on demand as
// Worktree entries under ~/.sybra/worktrees/ (see CreateWorktree).
type Project struct {
	ID        string      `yaml:"id" json:"id"`
	Name      string      `yaml:"name" json:"name"`
	Owner     string      `yaml:"owner" json:"owner"`
	Repo      string      `yaml:"repo" json:"repo"`
	URL       string      `yaml:"url" json:"url"`
	ClonePath string      `yaml:"clone_path" json:"clonePath"`
	Type      ProjectType `yaml:"type" json:"type"`
	// Status reflects the clone lifecycle. Empty value is treated as ready
	// so existing projects without this field continue to work.
	Status        ProjectStatus     `yaml:"status,omitempty" json:"status"`
	SetupCommands []string          `yaml:"setup_commands,omitempty" json:"setupCommands,omitempty"`
	Sandbox       *SandboxConfig    `yaml:"sandbox,omitempty" json:"sandbox,omitempty"`
	Checks        *ChecksConfig     `yaml:"checks,omitempty"  json:"checks,omitempty"`
	ManualTest    *ManualTestConfig `yaml:"manual_test,omitempty" json:"manualTest,omitempty"`
	// WorktreeBaseRef controls the starting point for new worktree branches.
	// "fresh" (default) branches off origin/<default>; "head" branches off the
	// local HEAD so unpushed commits are included. Empty value treated as "fresh".
	WorktreeBaseRef string    `yaml:"worktree_base_ref,omitempty" json:"worktreeBaseRef,omitempty"`
	CreatedAt       time.Time `yaml:"created_at" json:"createdAt"`
	UpdatedAt       time.Time `yaml:"updated_at" json:"updatedAt"`
}

// IsSybraProject reports whether p is the Sybra repo itself (owner
// "Automaat", repo "sybra", case-insensitive). Every task-scoped agent
// subprocess now gets an isolated SYBRA_HOME regardless of project (see
// agent.Manager.prepareRunConfig / ManagerConfig.SandboxHome), so this no
// longer gates that isolation; kept for callers that still need to detect
// Sybra-testing-Sybra specifically.
func (p Project) IsSybraProject() bool {
	return strings.EqualFold(p.Owner, "Automaat") && strings.EqualFold(p.Repo, "sybra")
}

func (p Project) WorkBlocklist() []string {
	if p.Type != ProjectTypeWork {
		return nil
	}
	bl := []string{p.ID, p.Owner, p.Repo}
	if p.URL != "" {
		bl = append(bl, p.URL)
	}
	return bl
}

// Worktree describes one `git worktree` checkout of a Project's bare clone,
// as reported by ListWorktrees.
type Worktree struct {
	Path   string `json:"path"`
	Branch string `json:"branch"`
	TaskID string `json:"taskId"`
	Head   string `json:"head"`
}
