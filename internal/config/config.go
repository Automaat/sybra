package config

//go:generate go run ../../cmd/gen-config-docs

import "github.com/Automaat/sybra/internal/abtest"

// Config is Sybra's top-level configuration, loaded from
// ~/.sybra/config.yaml by Load with env-var and default-value fallbacks
// applied per field. See docs/CONFIG.md (generated from this file's
// struct tags and doc comments) for the full reference, or run
// `sybra-cli config dump` to see the resolved, redacted config for this
// machine.
type Config struct {
	Logging        LoggingConfig        `yaml:"logging" json:"logging"`
	Audit          AuditConfig          `yaml:"audit" json:"audit"`
	Trash          TrashConfig          `yaml:"trash" json:"trash"`
	TaskSnapshot   TaskSnapshotConfig   `yaml:"task_snapshot" json:"taskSnapshot"`
	Agent          AgentDefaults        `yaml:"agent" json:"agent"`
	Testing        TestingConfig        `yaml:"testing" json:"testing"`
	Notification   NotificationConfig   `yaml:"notification" json:"notification"`
	Orchestrator   OrchestratorConfig   `yaml:"orchestrator" json:"orchestrator"`
	Todoist        TodoistConfig        `yaml:"todoist" json:"todoist"`
	Renovate       RenovateConfig       `yaml:"renovate" json:"renovate"`
	GitHub         GitHubConfig         `yaml:"github" json:"github"`
	Umbrella       UmbrellaConfig       `yaml:"umbrella" json:"umbrella"`
	Triage         TriageConfig         `yaml:"triage" json:"triage"`
	HumanReview    HumanReviewConfig    `yaml:"human_review" json:"humanReview"`
	ReviewHold     ReviewHoldConfig     `yaml:"review_hold" json:"reviewHold"`
	Monitor        MonitorConfig        `yaml:"monitor" json:"monitor"`
	Watchdog       WatchdogConfig       `yaml:"watchdog" json:"watchdog"`
	SelfMonitor    SelfMonitorConfig    `yaml:"self_monitor" json:"selfMonitor"`
	Evaluation     EvaluationConfig     `yaml:"evaluation" json:"evaluation"`
	LearningDigest LearningDigestConfig `yaml:"learning_digest" json:"learningDigest"`
	HarnessEvolve  HarnessEvolveConfig  `yaml:"harness_evolution" json:"harnessEvolution"`
	PromptLab      PromptLabConfig      `yaml:"prompt_lab" json:"promptLab"`
	Experience     ExperienceConfig     `yaml:"experience" json:"experience"`
	ABTesting      abtest.Config        `yaml:"ab_testing" json:"abTesting"`
	Providers      ProvidersConfig      `yaml:"providers" json:"providers"`
	Metrics        MetricsConfig        `yaml:"metrics" json:"metrics"`
	Server         ServerConfig         `yaml:"server" json:"server"`
	Cluster        ClusterConfig        `yaml:"cluster" json:"cluster"`
	AutoUpdate     AutoUpdateConfig     `yaml:"auto_update" json:"autoUpdate"`
	Browser        BrowserConfig        `yaml:"browser" json:"browser"`
	ProjectTypes   []string             `yaml:"project_types" json:"projectTypes"`
	TasksDir       string               `yaml:"tasks_dir" json:"tasksDir"`
	SkillsDir      string               `yaml:"skills_dir" json:"skillsDir"`
	RepoDir        string               `yaml:"repo_dir" json:"repoDir"`
	ProjectsDir    string               `yaml:"projects_dir" json:"projectsDir"`
	ClonesDir      string               `yaml:"clones_dir" json:"clonesDir"`
	WorktreesDir   string               `yaml:"worktrees_dir" json:"worktreesDir"`
	LoopAgentsDir  string               `yaml:"loop_agents_dir" json:"loopAgentsDir"`
}
