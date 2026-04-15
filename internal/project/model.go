package project

import "time"

type ProjectType string

const (
	ProjectTypePet  ProjectType = "pet"
	ProjectTypeWork ProjectType = "work"
)

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
	Status        ProjectStatus  `yaml:"status,omitempty" json:"status"`
	SetupCommands []string       `yaml:"setup_commands,omitempty" json:"setupCommands,omitempty"`
	Sandbox       *SandboxConfig `yaml:"sandbox,omitempty" json:"sandbox,omitempty"`
	CreatedAt     time.Time      `yaml:"created_at" json:"createdAt"`
	UpdatedAt     time.Time      `yaml:"updated_at" json:"updatedAt"`
}

type Worktree struct {
	Path   string `json:"path"`
	Branch string `json:"branch"`
	TaskID string `json:"taskId"`
	Head   string `json:"head"`
}
