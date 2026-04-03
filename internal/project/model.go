package project

import "time"

type Project struct {
	ID        string    `yaml:"id" json:"id"`
	Name      string    `yaml:"name" json:"name"`
	Owner     string    `yaml:"owner" json:"owner"`
	Repo      string    `yaml:"repo" json:"repo"`
	URL       string    `yaml:"url" json:"url"`
	ClonePath string    `yaml:"clone_path" json:"clonePath"`
	CreatedAt time.Time `yaml:"created_at" json:"createdAt"`
	UpdatedAt time.Time `yaml:"updated_at" json:"updatedAt"`
}

type Worktree struct {
	Path   string `json:"path"`
	Branch string `json:"branch"`
	TaskID string `json:"taskId"`
	Head   string `json:"head"`
}
