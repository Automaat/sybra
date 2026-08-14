#!/usr/bin/env bash
# Ban new code that spawns a provider CLI outside agent.newProviderCmd.
#
# newProviderCmd applies the OS sandbox, the per-run scratch home, and the
# write allowlist. A site that spawns claude/codex/copilot/opencode itself gets
# none of them: that is how a one-shot classifier ran a fully-permissioned CLI,
# in the serving process's own checkout, on prompts built from GitHub text
# (#3383). internal/agent enforced this for its own package by hand; this
# covers the tree.
#
# Matching the literal command name is not enough. The #3383 call passed its
# provider in a variable, and providerid constants are the usual spelling. So
# the rule is coarse and errs toward noise: a non-test Go file that both names
# a provider and calls exec.Command must be listed below with a reason. Naming
# means speaking in providerid constants, or spawning a literal provider name
# on the exec line; a quoted provider word anywhere else is not enough, since
# GitHub logins and prose use the same words. Files spawning something
# unrelated are unaffected, and a new provider spawn has to be argued for here
# rather than slipping in.
set -euo pipefail

cd "$(dirname "$0")/.."

# Allowed to spawn while naming a provider, each for a stated reason:
#   agent/runner_core.go        the sandboxed constructor itself
#   agent/procsandbox_linux.go  sandbox mechanism probe; no prompt, no roots
#   agent/oneshot.go            one-shot calls, built via that constructor
#   llmexec/command.go          runs what the registered factory returned, and
#                               falls back to a bare CLI only outside the app
#   agent/discovery.go          --version probes for install discovery
#   agent/skill_invoke.go       codex plugin list --json, a metadata query
#   provider/probes.go          auth status health probes
#   sybra/svc_info*.go          version strings for the About panel
#   limits/live.go              reads a keychain entry, not a provider
#   workflow/engine_steps_agent.go  runs a shell step, not a provider
#   cmd/sybra-perf, cmd/codex-appserver-spike  developer tools, not the server
ALLOWLIST=(
  "internal/agent/runner_core.go"
  "internal/agent/procsandbox_linux.go"
  "internal/agent/oneshot.go"
  "internal/llmexec/command.go"
  "internal/agent/discovery.go"
  "internal/agent/skill_invoke.go"
  "internal/provider/probes.go"
  "internal/sybra/svc_info.go"
  "internal/sybra/svc_info_runtimes.go"
  "internal/limits/live.go"
  "internal/workflow/engine_steps_agent.go"
  "cmd/sybra-perf/main.go"
  "cmd/codex-appserver-spike/main.go"
)

is_allowlisted() {
  local candidate="${1#./}"
  local entry
  for entry in "${ALLOWLIST[@]}"; do
    if [ "$candidate" = "$entry" ]; then
      return 0
    fi
  done
  return 1
}

violations=0
while IFS= read -r file; do
  [ -n "$file" ] || continue
  case "$file" in
    *_test.go) continue ;;
  esac
  if is_allowlisted "$file"; then
    continue
  fi
  # Either the file speaks in providerid constants, or it spawns a provider by
  # name on the exec line itself. A quoted provider word elsewhere in a file is
  # not enough: GitHub logins ("copilot") and prose both use these words.
  if ! grep -q "internal/providerid" "$file" &&
    ! grep -qE "exec\\.Command(Context)?\\([^)]*\"(claude|codex|copilot|opencode)\"" "$file"; then
    continue
  fi
  line=$(grep -nE "exec\\.Command(Context)?\\(" "$file" | head -1 | cut -d: -f1)
  echo "::error file=${file#./},line=${line}::names a provider CLI and spawns a process. Route it through agent.newProviderCmd so the sandbox applies (see #3383), or add it to the allowlist in $0 with a reason."
  violations=$((violations + 1))
done < <(grep -rlE "exec\\.Command(Context)?\\(" --include="*.go" . || true)

if [ "$violations" -gt 0 ]; then
  echo "provider-exec gate: ${violations} unsandboxed provider spawn site(s)" >&2
  exit 1
fi

echo "provider-exec gate: ok"
