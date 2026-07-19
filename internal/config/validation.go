package config

import (
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"strings"

	"github.com/Automaat/sybra/internal/providerid"
)

var modelNamePattern = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// ValidationError aggregates config validation failures into one startup-safe
// error so every entry point shares the same rules and messages.
type ValidationError struct {
	Messages []string
}

func ValidationMessages(err error) []string {
	if err == nil {
		return nil
	}
	var verr *ValidationError
	if errors.As(err, &verr) {
		return append([]string(nil), verr.Messages...)
	}
	return []string{err.Error()}
}

func (e *ValidationError) Error() string {
	if e == nil || len(e.Messages) == 0 {
		return ""
	}
	return strings.Join(e.Messages, "; ")
}

func ValidateResolvedConfig(cfg *ResolvedConfig) error {
	if cfg == nil {
		return nil
	}
	var msgs []string
	add := func(format string, a ...any) {
		msgs = append(msgs, fmt.Sprintf(format, a...))
	}

	if cfg.Agent.Provider != "" && !providerid.IsKnown(cfg.Agent.Provider) {
		add("agent.provider: invalid provider: %q (valid: %s)", cfg.Agent.Provider, providerid.List())
	}
	if cfg.Agent.Model != "" && !modelNamePattern.MatchString(cfg.Agent.Model) {
		add("agent.model: invalid model: %q", cfg.Agent.Model)
	}
	if cfg.Agent.FallbackModel != "" && !modelNamePattern.MatchString(cfg.Agent.FallbackModel) {
		add("agent.fallback_model: invalid fallback model: %q", cfg.Agent.FallbackModel)
	}
	if cfg.Agent.Mode != "" && cfg.Agent.Mode != "headless" && cfg.Agent.Mode != "interactive" {
		add("agent.mode: invalid mode: %q", cfg.Agent.Mode)
	}
	if cfg.Agent.MaxConcurrent < 1 || cfg.Agent.MaxConcurrent > 100 {
		add("agent.max_concurrent: maxConcurrent must be 1–100")
	}
	if cfg.Agent.LogRetentionDays < -1 {
		add("agent.log_retention_days: logRetentionDays must be -1 or greater")
	}
	if cfg.Agent.LogGzipAfterDays < -1 {
		add("agent.log_gzip_after_days: logGzipAfterDays must be -1 or greater")
	}
	if cfg.Agent.LogRetentionMaxSizeMB < -1 {
		add("agent.log_retention_max_size_mb: logRetentionMaxSizeMb must be -1 or greater")
	}
	if _, err := NormalizeHeadlessPermissionMode(cfg.Agent.HeadlessPermissionMode); err != nil {
		add("agent.headless_permission_mode: %v", err)
	}
	if mode, err := NormalizeSandboxMode(cfg.Agent.SandboxMode); err != nil {
		add("agent.sandbox_mode: %v", err)
	} else if mode == "enforce" && runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		add("agent.sandbox_mode=enforce requires darwin or linux; current host is %s", runtime.GOOS)
	}
	if cfg.Logging.Level != "debug" && cfg.Logging.Level != "info" && cfg.Logging.Level != "warn" && cfg.Logging.Level != "error" {
		add("logging.level: invalid log level: %q", cfg.Logging.Level)
	}
	if cfg.Logging.MaxSizeMB < 1 || cfg.Logging.MaxSizeMB > 500 {
		add("logging.max_size_mb: maxSizeMB must be 1–500")
	}
	if cfg.Logging.MaxFiles < 1 || cfg.Logging.MaxFiles > 50 {
		add("logging.max_files: maxFiles must be 1–50")
	}
	if cfg.Audit.RetentionDays < 1 || cfg.Audit.RetentionDays > 365 {
		add("audit.retention_days: retentionDays must be 1–365")
	}
	if cfg.Attachments.MaxSizeMB < 1 || cfg.Attachments.MaxSizeMB > 20 {
		add("attachments.max_size_mb: maxSizeMB must be 1–20")
	}
	if cfg.Todoist.PollSeconds < 30 || cfg.Todoist.PollSeconds > 3600 {
		add("todoist.poll_seconds: todoist poll interval must be 30–3600 seconds")
	}
	if cfg.Todoist.Enabled && cfg.Todoist.APIToken == "" {
		add("todoist.enabled: todoist API token required when enabled")
	}
	if cfg.GitHub.PollerRole != "" && cfg.GitHub.PollerRole != "primary" && cfg.GitHub.PollerRole != "secondary" {
		add("github.poller_role must be \"primary\", \"secondary\", or empty, got %q", cfg.GitHub.PollerRole)
	}
	if cfg.GitHub.App.Enabled && strings.TrimSpace(cfg.GitHub.App.PrivateKeyPath) == "" {
		add("github.app.enabled requires github.app.private_key_path")
	}
	if _, err := NormalizeClusterRole(cfg.Cluster.Role); err != nil {
		add("%v", err)
	}
	if _, err := NormalizeInstanceRole(cfg.Orchestrator.Role); err != nil {
		add("%v", err)
	}
	if err := cfg.ValidateCluster(); err != nil {
		add("%v", err)
	}
	switch action := strings.ToLower(strings.TrimSpace(cfg.HumanReview.SybraBugAction)); action {
	case "", HumanReviewSybraBugActionFileIssue, HumanReviewSybraBugActionLocalTask, HumanReviewSybraBugActionBlockOnly, HumanReviewSybraBugActionNoteOnly:
	default:
		add("human_review.sybra_bug_action must be one of file_issue, local_task, block_only, note_only, got %q", cfg.HumanReview.SybraBugAction)
	}
	if cfg.ReviewHold.Enabled && !validReviewHoldMode(cfg.ReviewHold.Mode) {
		add("review_hold.mode must be push, push_nits, or hold, got %q", cfg.ReviewHold.Mode)
	}
	if cfg.ReviewHold.Enabled && cfg.ReviewHold.NitMaxLines < 0 {
		add("review_hold.nit_max_lines must be 0 or greater, got %d", cfg.ReviewHold.NitMaxLines)
	}
	if cfg.Evaluation.Offline.UnavailablePolicy != "" &&
		cfg.Evaluation.Offline.UnavailablePolicy != "fail" &&
		cfg.Evaluation.Offline.UnavailablePolicy != "pass" {
		add("evaluation.offline.unavailable_policy must be \"fail\" or \"pass\", got %q", cfg.Evaluation.Offline.UnavailablePolicy)
	}
	if cfg.Monitor.DispatchLimit < 0 {
		add("monitor.dispatch_limit must be 0 or greater, got %d", cfg.Monitor.DispatchLimit)
	}
	validateK8sSecretEnv(cfg, add)

	if len(msgs) == 0 {
		return nil
	}
	return &ValidationError{Messages: msgs}
}

func validateK8sSecretEnv(cfg *ResolvedConfig, add func(format string, a ...any)) {
	if !cfg.Agent.K8sJobs.Enabled {
		return
	}
	for i, e := range cfg.Agent.K8sJobs.SecretEnv {
		var missing []string
		if strings.TrimSpace(e.Name) == "" {
			missing = append(missing, "name")
		}
		if strings.TrimSpace(e.SecretName) == "" {
			missing = append(missing, "secret_name")
		}
		if strings.TrimSpace(e.SecretKey) == "" {
			missing = append(missing, "secret_key")
		}
		if len(missing) > 0 {
			add("agent.k8s_jobs.secret_env[%d] is missing %s", i, strings.Join(missing, ", "))
		}
	}
}
