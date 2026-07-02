package config

import "github.com/Automaat/sybra/internal/abtest"

type Config struct {
	Logging       LoggingConfig       `yaml:"logging" json:"logging"`
	Audit         AuditConfig         `yaml:"audit" json:"audit"`
	Agent         AgentDefaults       `yaml:"agent" json:"agent"`
	Testing       TestingConfig       `yaml:"testing" json:"testing"`
	Notification  NotificationConfig  `yaml:"notification" json:"notification"`
	Orchestrator  OrchestratorConfig  `yaml:"orchestrator" json:"orchestrator"`
	Todoist       TodoistConfig       `yaml:"todoist" json:"todoist"`
	Renovate      RenovateConfig      `yaml:"renovate" json:"renovate"`
	GitHub        GitHubConfig        `yaml:"github" json:"github"`
	Umbrella      UmbrellaConfig      `yaml:"umbrella" json:"umbrella"`
	Triage        TriageConfig        `yaml:"triage" json:"triage"`
	HumanReview   HumanReviewConfig   `yaml:"human_review" json:"humanReview"`
	Monitor       MonitorConfig       `yaml:"monitor" json:"monitor"`
	Watchdog      WatchdogConfig      `yaml:"watchdog" json:"watchdog"`
	SelfMonitor   SelfMonitorConfig   `yaml:"self_monitor" json:"selfMonitor"`
	Evaluation    EvaluationConfig    `yaml:"evaluation" json:"evaluation"`
	HarnessEvolve HarnessEvolveConfig `yaml:"harness_evolution" json:"harnessEvolution"`
	PromptLab     PromptLabConfig     `yaml:"prompt_lab" json:"promptLab"`
	Experience    ExperienceConfig    `yaml:"experience" json:"experience"`
	ABTesting     abtest.Config       `yaml:"ab_testing" json:"abTesting"`
	Providers     ProvidersConfig     `yaml:"providers" json:"providers"`
	Metrics       MetricsConfig       `yaml:"metrics" json:"metrics"`
	AutoUpdate    AutoUpdateConfig    `yaml:"auto_update" json:"autoUpdate"`
	ProjectTypes  []string            `yaml:"project_types" json:"projectTypes"`
	TasksDir      string              `yaml:"tasks_dir" json:"tasksDir"`
	SkillsDir     string              `yaml:"skills_dir" json:"skillsDir"`
	RepoDir       string              `yaml:"repo_dir" json:"repoDir"`
	ProjectsDir   string              `yaml:"projects_dir" json:"projectsDir"`
	ClonesDir     string              `yaml:"clones_dir" json:"clonesDir"`
	WorktreesDir  string              `yaml:"worktrees_dir" json:"worktreesDir"`
	LoopAgentsDir string              `yaml:"loop_agents_dir" json:"loopAgentsDir"`
}
