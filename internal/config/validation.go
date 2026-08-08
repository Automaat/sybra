package config

import (
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"

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

// requireExplicitSandboxMode makes an unset agent.sandbox_mode a load-time
// error rather than a silent fall-through to "report". Off by default so the
// desktop app and CLI keep working on a config that omits the key; sybra-server
// turns it on at startup via RequireExplicitSandboxMode.
//
// It lives here, in the validator every load and every hot reload funnels
// through, rather than at the server's startup call site: a boot-time-only
// check is defeated by the config watcher re-applying an edited file, which
// is exactly the silent drift the requirement exists to stop.
var requireExplicitSandboxMode atomic.Bool

// RequireExplicitSandboxMode makes ValidateUnattendedPosture reject a config
// whose agent.sandbox_mode is unset. Intended for unattended processes, where
// an omitted key is indistinguishable from a deliberately unsandboxed one.
func RequireExplicitSandboxMode(require bool) {
	requireExplicitSandboxMode.Store(require)
}

// ValidateUnattendedPosture rejects operator config that leaves the OS-level
// sandbox posture unstated, once RequireExplicitSandboxMode is on. It is
// deliberately separate from ValidateResolvedConfig: the latter also resolves
// the built-in empty config (DefaultConfig), which legitimately has no
// sandbox_mode, so folding this in there would panic every default-config
// caller in an unattended process.
//
// Call it from every path that loads or replaces operator config — startup,
// preflight, and hot reload — since a boot-only check is defeated by the
// config watcher re-applying an edited file.
func ValidateUnattendedPosture(cfg *ResolvedConfig) error {
	if cfg == nil || !requireExplicitSandboxMode.Load() {
		return nil
	}
	if strings.TrimSpace(cfg.Agent.SandboxMode) != "" {
		return nil
	}
	return &ValidationError{Messages: []string{
		"agent.sandbox_mode is unset; set it explicitly to one of off, report, enforce " +
			"(enforce is the contained posture — off and report leave agent writes unrestricted)",
	}}
}

func ValidateResolvedConfig(cfg *ResolvedConfig) error {
	if cfg == nil {
		return nil
	}
	var msgs []string
	add := func(format string, a ...any) {
		msgs = append(msgs, fmt.Sprintf(format, a...))
	}

	validateAgentConfig(cfg, add)
	validateLoggingConfig(cfg, add)
	validateGitHubConfig(cfg, add)
	validateClusterConfig(cfg, add)
	validateHumanReviewConfig(cfg, add)
	validateReviewHoldConfig(cfg, add)
	validateEvaluationConfig(cfg, add)
	validateMonitorConfig(cfg, add)
	validateK8sSecretEnv(cfg, add)

	if len(msgs) == 0 {
		return nil
	}
	return &ValidationError{Messages: msgs}
}

func validateAgentConfig(cfg *ResolvedConfig, add func(format string, a ...any)) {
	if cfg.Agent.Provider != "" && !providerid.IsKnown(cfg.Agent.Provider) {
		add("agent.provider: invalid provider: %q (valid: %s)", cfg.Agent.Provider, providerid.List())
	}
	if cfg.Agent.Model != "" && !modelNamePattern.MatchString(cfg.Agent.Model) {
		add("agent.model: invalid model: %q", cfg.Agent.Model)
	}
	if cfg.Agent.FallbackModel != "" && !modelNamePattern.MatchString(cfg.Agent.FallbackModel) {
		add("agent.fallback_model: invalid fallback model: %q", cfg.Agent.FallbackModel)
	}
	if cfg.Agent.MaxConcurrent < 1 || cfg.Agent.MaxConcurrent > 100 {
		add("agent.max_concurrent: maxConcurrent must be 1–100")
	}
	if cfg.Agent.VerifyChecksMaxConcurrent < 0 || cfg.Agent.VerifyChecksMaxConcurrent > 100 {
		add("agent.verify_checks_max_concurrent: verifyChecksMaxConcurrent must be 0–100")
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
	if _, err := NormalizeCommitSigning(cfg.Agent.CommitSigning); err != nil {
		add("agent.commit_signing: %v", err)
	}
	if mode, err := NormalizeSandboxMode(cfg.Agent.SandboxMode); err != nil {
		add("agent.sandbox_mode: %v", err)
	} else if mode == "enforce" && runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		add("agent.sandbox_mode=enforce requires darwin or linux; current host is %s", runtime.GOOS)
	}
	validateClassReservations(cfg, add)
}

// validClassReservationKeys mirrors agent.Role.WorkloadClass's fixed output
// set (internal/agent.AllWorkloadClasses). Duplicated as string literals
// rather than imported: internal/agent already imports internal/config, so
// importing agent.WorkloadClass here would be a cycle.
var validClassReservationKeys = map[string]bool{
	"implementation": true,
	"completion":     true,
	"system":         true,
}

func validateClassReservations(cfg *ResolvedConfig, add func(format string, a ...any)) {
	if len(cfg.Agent.ClassReservations) == 0 {
		return
	}
	var sum int
	for class, floor := range cfg.Agent.ClassReservations {
		if !validClassReservationKeys[class] {
			add("agent.class_reservations: unknown class %q (valid: implementation, completion, system)", class)
			continue
		}
		if floor < 0 {
			add("agent.class_reservations.%s: reserved minimum must be >= 0, got %d", class, floor)
			continue
		}
		sum += floor
	}
	if cfg.Agent.MaxConcurrent > 0 && sum > cfg.Agent.MaxConcurrent {
		add("agent.class_reservations: sum of reserved minimums (%d) exceeds agent.max_concurrent (%d)", sum, cfg.Agent.MaxConcurrent)
	}
}

func validateLoggingConfig(cfg *ResolvedConfig, add func(format string, a ...any)) {
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
}

func validateGitHubConfig(cfg *ResolvedConfig, add func(format string, a ...any)) {
	if cfg.GitHub.PollerRole != "" && cfg.GitHub.PollerRole != "primary" && cfg.GitHub.PollerRole != "secondary" {
		add("github.poller_role must be \"primary\", \"secondary\", or empty, got %q", cfg.GitHub.PollerRole)
	}
	for path, value := range map[string]int{
		"github.polling.issues.interval_seconds":              cfg.GitHub.Polling.Issues.IntervalSeconds,
		"github.polling.sybra_prs.active_interval_seconds":    cfg.GitHub.Polling.SybraPRs.ActiveIntervalSeconds,
		"github.polling.sybra_prs.idle_interval_seconds":      cfg.GitHub.Polling.SybraPRs.IdleIntervalSeconds,
		"github.polling.assigned_prs.active_interval_seconds": cfg.GitHub.Polling.AssignedPRs.ActiveIntervalSeconds,
		"github.polling.assigned_prs.idle_interval_seconds":   cfg.GitHub.Polling.AssignedPRs.IdleIntervalSeconds,
	} {
		if value < 0 {
			add("%s must be 0 or greater, got %d", path, value)
		}
	}
	if cfg.GitHub.App.Enabled && strings.TrimSpace(cfg.GitHub.App.PrivateKeyPath) == "" {
		add("github.app.enabled requires github.app.private_key_path")
	}
	commandPrefix := strings.TrimSpace(cfg.GitHub.Webhook.CommandPrefix)
	if !strings.HasPrefix(commandPrefix, "/") || len(commandPrefix) < 2 || strings.ContainsAny(commandPrefix, " \t\r\n") {
		add("github.webhook.command_prefix must start with \"/\" and contain no whitespace, got %q", cfg.GitHub.Webhook.CommandPrefix)
	}
	if cfg.AutoUpdate.Enabled && cfg.AutoUpdate.Mode == "auto" && countNonEmpty(cfg.AutoUpdate.RequiredChecks) == 0 {
		add("auto_update.required_checks must be non-empty when auto_update.mode=auto")
	}
}

func countNonEmpty(items []string) int {
	n := 0
	for _, item := range items {
		if strings.TrimSpace(item) != "" {
			n++
		}
	}
	return n
}

func validateClusterConfig(cfg *ResolvedConfig, add func(format string, a ...any)) {
	if _, err := NormalizeClusterRole(cfg.Cluster.Role); err != nil {
		add("%v", err)
	}
	if _, err := NormalizeInstanceRole(cfg.Orchestrator.Role); err != nil {
		add("%v", err)
	}
	if err := cfg.ValidateCluster(); err != nil {
		add("%v", err)
	}
}

func validateHumanReviewConfig(cfg *ResolvedConfig, add func(format string, a ...any)) {
	switch action := strings.ToLower(strings.TrimSpace(cfg.HumanReview.SybraBugAction)); action {
	case "", HumanReviewSybraBugActionFileIssue, HumanReviewSybraBugActionLocalTask, HumanReviewSybraBugActionBlockOnly, HumanReviewSybraBugActionNoteOnly:
	default:
		add("human_review.sybra_bug_action must be one of note_only, local_task, block_only, or legacy file_issue, got %q", cfg.HumanReview.SybraBugAction)
	}
}

func validateReviewHoldConfig(cfg *ResolvedConfig, add func(format string, a ...any)) {
	if cfg.ReviewHold.Enabled && !validReviewHoldMode(cfg.ReviewHold.Mode) {
		add("review_hold.mode must be push, push_nits, or hold, got %q", cfg.ReviewHold.Mode)
	}
	if cfg.ReviewHold.Enabled && cfg.ReviewHold.NitMaxLines < 0 {
		add("review_hold.nit_max_lines must be 0 or greater, got %d", cfg.ReviewHold.NitMaxLines)
	}
}

func validateEvaluationConfig(cfg *ResolvedConfig, add func(format string, a ...any)) {
	if cfg.Evaluation.Offline.UnavailablePolicy != "" &&
		cfg.Evaluation.Offline.UnavailablePolicy != "fail" &&
		cfg.Evaluation.Offline.UnavailablePolicy != "pass" {
		add("evaluation.offline.unavailable_policy must be \"fail\" or \"pass\", got %q", cfg.Evaluation.Offline.UnavailablePolicy)
	}
	validateSLOTargets(&cfg.Evaluation.SLO, add)
}

// validateSLOTargets rejects out-of-range SLO targets at load time. A negative
// count-based target (max_identical_retry_cap, max_restarts_per_hour) can never
// be met by a real non-negative measurement, so it would permanently pin the
// SLO status to breached — and, with throttle_on_budget_exhausted enabled,
// silently and permanently halve dispatch concurrency. Fraction targets must
// stay within [0,1] to be meaningful ratios.
func validateSLOTargets(slo *SLOTargets, add func(format string, a ...any)) {
	if slo.MaxIdenticalRetryCap < 0 {
		add("evaluation.slo.max_identical_retry_cap must be 0 or greater, got %d", slo.MaxIdenticalRetryCap)
	}
	if slo.MaxRestartsPerHour < 0 {
		add("evaluation.slo.max_restarts_per_hour must be 0 or greater, got %g", slo.MaxRestartsPerHour)
	}
	for _, f := range []struct {
		key string
		val float64
	}{
		{"evaluation.slo.min_autonomy_rate", slo.MinAutonomyRate},
		{"evaluation.slo.min_ci_first_pass_rate", slo.MinCIFirstPassRate},
		{"evaluation.slo.max_rework_rate", slo.MaxReworkRate},
	} {
		if f.val < 0 || f.val > 1 {
			add("%s must be between 0 and 1, got %g", f.key, f.val)
		}
	}
}

func validateMonitorConfig(cfg *ResolvedConfig, add func(format string, a ...any)) {
	if cfg.Monitor.DispatchLimit < 0 {
		add("monitor.dispatch_limit must be 0 or greater, got %d", cfg.Monitor.DispatchLimit)
	}
	if cfg.Monitor.IncidentResolveGraceMinutes < 0 {
		add("monitor.incident_resolve_grace_minutes must be 0 or greater, got %d", cfg.Monitor.IncidentResolveGraceMinutes)
	}
	if cfg.Monitor.IncidentReopenGraceMinutes < 0 {
		add("monitor.incident_reopen_grace_minutes must be 0 or greater, got %d", cfg.Monitor.IncidentReopenGraceMinutes)
	}
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
