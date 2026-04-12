package project

import "time"

type ProjectType string

const (
	ProjectTypePet  ProjectType = "pet"
	ProjectTypeWork ProjectType = "work"
)

type Project struct {
	ID            string      `yaml:"id" json:"id"`
	Name          string      `yaml:"name" json:"name"`
	Owner         string      `yaml:"owner" json:"owner"`
	Repo          string      `yaml:"repo" json:"repo"`
	URL           string      `yaml:"url" json:"url"`
	ClonePath     string      `yaml:"clone_path" json:"clonePath"`
	Type          ProjectType `yaml:"type" json:"type"`
	SetupCommands []string    `yaml:"setup_commands,omitempty" json:"setupCommands,omitempty"`
	CreatedAt     time.Time   `yaml:"created_at" json:"createdAt"`
	UpdatedAt     time.Time   `yaml:"updated_at" json:"updatedAt"`
}

type Worktree struct {
	Path   string `json:"path"`
	Branch string `json:"branch"`
	TaskID string `json:"taskId"`
	Head   string `json:"head"`
}
