#!/usr/bin/env bash
# Integration tests for deploy/bin/sybra-build.sh and
# deploy/bin/sybra-healthcheck.sh, exercised end to end (real processes, real
# flock, real symlinks) against the trimmed fixture module in
# deploy/tests/fixtures/fake-src — see that dir's file comments for why a
# stand-in module exists instead of building the real repo. No real
# mise/npm/network required (deploy/tests/fixtures/stubs shadows them).
#
# Run: bash deploy/tests/deploy_integration_test.sh
# Debug a failure without cleanup: KEEP_TMP=1 bash deploy/tests/deploy_integration_test.sh
#
# Covers the acceptance bar from issue #2729: config incompatibility, build
# failure, health-check failure + quarantine, lock contention, and
# successful activation.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_BIN="$HERE/../bin"
FIXTURE_SRC="$HERE/fixtures/fake-src"
STUBS="$HERE/fixtures/stubs"
ORIG_PATH="$PATH"

PASS=0
FAIL=0

pass() { echo "PASS: $*"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $*" >&2; FAIL=$((FAIL + 1)); }

assert_eq() {
  local desc="$1" expected="$2" actual="$3"
  if [[ "$expected" == "$actual" ]]; then pass "$desc"; else fail "$desc (expected '$expected', got '$actual')"; fi
}

assert_ne() {
  local desc="$1" unexpected="$2" actual="$3"
  if [[ "$unexpected" != "$actual" ]]; then pass "$desc"; else fail "$desc (expected a value other than '$unexpected')"; fi
}

assert_contains() {
  local desc="$1" haystack="$2" needle="$3"
  if [[ "$haystack" == *"$needle"* ]]; then pass "$desc"; else fail "$desc (missing '$needle' in: $haystack)"; fi
}

assert_true() {
  local desc="$1"; shift
  if "$@" >/dev/null 2>&1; then pass "$desc"; else fail "$desc"; fi
}

TMPBASE=""
cleanup() {
  # EXIT traps are inherited by every $(...) command-substitution subshell
  # this script spawns (e.g. every helper called as `x="$(fn ...)"`); without
  # this guard, each of those subshells finishing its own work would also
  # fire this trap, killing whatever background job it just started and
  # rm -rf'ing TMPBASE out from under the still-running top-level script.
  [[ "$BASH_SUBSHELL" -eq 0 ]] || return 0
  jobs -p | xargs -r kill 2>/dev/null || true
  if [[ -n "$TMPBASE" && -z "${KEEP_TMP:-}" ]]; then
    rm -rf "$TMPBASE"
  fi
}
trap cleanup EXIT

resolved_target() { [[ -e "$1" || -L "$1" ]] && readlink -f "$1"; }
current_target() { resolved_target "$SYBRA_CURRENT_LINK"; }
last_good_target() { resolved_target "$SYBRA_LAST_GOOD_LINK"; }
quarantine_count() { find "$SYBRA_QUARANTINE_DIR" -name '*.reason' 2>/dev/null | wc -l | tr -d ' '; }

# new_env <root> gives each scenario a fully isolated deploy root: a fresh
# git checkout of the fixture module plus empty releases/current/last-good/
# quarantine/deploy-state/lock paths, with the env vars sybra-deploy-lib.sh
# reads pointed entirely inside $root — nothing touches the real host.
new_env() {
  local root="$1"
  mkdir -p "$root/home/.local/bin"
  cp -a "$FIXTURE_SRC" "$root/src"
  ( cd "$root/src" && git init -q && git config user.email t@t.com && git config user.name t && git add -A && git commit -q -m init )

  unset FAKE_CHECK_CONFIG FAKE_CHECK_CONFIG_CLI FAKE_NPM_FAIL FAKE_SANDBOX_SMOKE_FAIL FAKE_HEALTH_MODE FAKE_LISTEN_ADDR
  unset FAKE_TLS_CERT FAKE_TLS_KEY
  unset SYBRA_HEALTH_URL SYBRA_HEALTH_QUARANTINE_THRESHOLD SYBRA_DEPLOY_LOCK_WAIT_SEC
  unset SYBRA_SERVER_TARGET SYBRA_SERVER_CA
  unset SYBRA_HEALTH_TIMEOUT_SEC SYBRA_HEALTH_INTERVAL_SEC
  unset SYBRA_APPARMOR_PROFILE

  export SYBRA_SRC_DIR="$root/src"
  export SYBRA_RELEASES_DIR="$root/opt/releases"
  export SYBRA_CURRENT_LINK="$root/opt/current"
  export SYBRA_LAST_GOOD_LINK="$root/opt/last-good"
  export SYBRA_QUARANTINE_DIR="$root/opt/quarantine"
  export SYBRA_DEPLOY_STATE_DIR="$root/opt/deploy-state"
  export SYBRA_DEPLOY_LOCK="$root/opt/deploy.lock"
  export SYBRA_HOME="$root/home"
  export HOME="$root/home"
  export PATH="$STUBS:$ORIG_PATH"
}

bump_commit() {
  local root="$1" msg="$2"
  ( cd "$root/src" && echo "// $msg $RANDOM" >>internal/agent/sandbox.go && git add -A && git commit -q -m "$msg" )
}

run_build() { timeout 90 bash "$DEPLOY_BIN/sybra-build.sh"; }
run_healthcheck() { timeout 30 bash "$DEPLOY_BIN/sybra-healthcheck.sh"; }
run_repair_src() { timeout 30 bash "$DEPLOY_BIN/sybra-repair-src.sh"; }

# start_fake_server backgrounds the just-built candidate's server binary on a
# scenario-local port and waits for it to accept connections. Prints the pid.
start_fake_server() {
  local bin="$1" port="$2"
  export FAKE_LISTEN_ADDR="127.0.0.1:$port"
  # Redirected, not inherited: this is invoked as `x="$(start_fake_server
  # ...)"` in every caller, and a background job that inherits the
  # subshell's stdout keeps that command-substitution pipe's write end open
  # for as long as the job runs — bash's $(...) then blocks forever waiting
  # for EOF that never comes, even though this function returned ages ago.
  "$bin" >/dev/null 2>&1 &
  local pid=$!
  local i=0 rc
  while (( i < 50 )); do
    curl -fsS -m 1 "http://127.0.0.1:$port/health" >/dev/null 2>&1
    rc=$?
    # 0 = healthy response, 22 = curl -f saw an HTTP error status (still
    # means the port accepted the connection) — either way the process is up
    # and ready for the caller's own healthcheck poll.
    [[ "$rc" -eq 0 || "$rc" -eq 22 ]] && break
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.1
    i=$((i + 1))
  done
  echo "$pid"
}

stop_fake_server() {
  local pid="$1"
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
}

next_port() {
  echo $((20000 + RANDOM % 20000))
}

# start_fake_tls_server backgrounds the candidate server with a certificate, so
# the health check has a real TLS board to authenticate. Prints the pid.
start_fake_tls_server() {
  local bin="$1" port="$2" cert="$3" key="$4"
  export FAKE_LISTEN_ADDR="127.0.0.1:$port" FAKE_TLS_CERT="$cert" FAKE_TLS_KEY="$key"
  "$bin" >/dev/null 2>&1 &
  local pid=$!
  local i=0 rc
  while (( i < 50 )); do
    curl -k -fsS -m 1 "https://127.0.0.1:$port/health" >/dev/null 2>&1
    rc=$?
    [[ "$rc" -eq 0 || "$rc" -eq 22 ]] && break
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.1
    i=$((i + 1))
  done
  echo "$pid"
}

# gen_selfsigned mints a certificate deliberately shaped like `sybra-cli
# cluster gen-cert`: self-signed, and carrying a name the health check does NOT
# dial. A check that verified a chain and hostname would reject it; sybra's own
# client pins the certificate instead, and the health check has to agree.
gen_selfsigned() {
  local dir="$1"
  mkdir -p "$dir"
  openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
    -keyout "$dir/key.pem" -out "$dir/cert.pem" \
    -subj "/CN=sybra-board" -addext "subjectAltName=DNS:sybra-board.invalid" >/dev/null 2>&1
}

# scenario_health_target_scheme covers the health URL built from
# SYBRA_SERVER_TARGET rather than the SYBRA_HEALTH_URL override every other
# scenario sets. The target takes a bare host and port or a full origin, and
# prefixing http:// to an origin produced http://https://host:port/health,
# which connects to nothing and failed the start.
scenario_health_target_scheme() {
  local root="$1"
  new_env "$root"
  export FAKE_CHECK_CONFIG=ok
  run_build >"$root/build.log" 2>&1
  local cur; cur="$(current_target)"

  export FAKE_HEALTH_MODE=ok
  export SYBRA_HEALTH_TIMEOUT_SEC=5
  export SYBRA_HEALTH_INTERVAL_SEC=1
  local port; port="$(next_port)"
  unset SYBRA_HEALTH_URL
  export SYBRA_SERVER_TARGET="http://127.0.0.1:$port"
  local srv; srv="$(start_fake_server "$cur/sybra-server" "$port")"
  run_healthcheck >"$root/health-origin.log" 2>&1
  local rc=$?
  stop_fake_server "$srv"

  assert_eq "health-scheme: an http origin target passes the health check" "0" "$rc"
  assert_eq "health-scheme: release promoted to last-good" "$cur" "$(last_good_target)"
  assert_contains "health-scheme: the polled URL carries one scheme" "$(cat "$root/health-origin.log")" "http://127.0.0.1:$port/health"
  if grep -q 'http://http' "$root/health-origin.log"; then
    fail "health-scheme: the polled URL carries two schemes"
  else
    pass "health-scheme: the polled URL never doubles the scheme"
  fi

  # An https origin is never rewritten to http: the check has to fail against a
  # plain-http server rather than silently connect on the wrong scheme. The
  # whole log line is asserted, not the URL alone — the pre-fix URL
  # http://https://127.0.0.1:PORT/health *contains* the https one, so a
  # substring check here would pass against the exact bug it pins.
  port="$(next_port)"
  export SYBRA_SERVER_TARGET="https://127.0.0.1:$port"
  srv="$(start_fake_server "$cur/sybra-server" "$port")"
  run_healthcheck >"$root/health-https.log" 2>&1
  rc=$?
  stop_fake_server "$srv"
  assert_contains "health-scheme: an https origin is polled over https" "$(cat "$root/health-https.log")" "for https://127.0.0.1:$port/health to answer"
  assert_ne "health-scheme: an https origin is never downgraded to http" "0" "$rc"

  # A whitespace-padded target is what an env file produces by accident, and
  # the Go consumers of this variable trim it before parsing.
  port="$(next_port)"
  export SYBRA_SERVER_TARGET=" https://127.0.0.1:$port "
  srv="$(start_fake_server "$cur/sybra-server" "$port")"
  run_healthcheck >"$root/health-padded.log" 2>&1
  stop_fake_server "$srv"
  assert_contains "health-scheme: a padded target still resolves to one origin" "$(cat "$root/health-padded.log")" "for https://127.0.0.1:$port/health to answer"

  # A bare host and port keeps its implied http scheme.
  port="$(next_port)"
  export SYBRA_SERVER_TARGET="127.0.0.1:$port"
  srv="$(start_fake_server "$cur/sybra-server" "$port")"
  run_healthcheck >"$root/health-bare.log" 2>&1
  rc=$?
  stop_fake_server "$srv"
  assert_eq "health-scheme: a bare host:port target still passes" "0" "$rc"
}

# scenario_health_tls_board drives the whole point of honouring the scheme: a
# board that actually terminates TLS has to reach a healthy release. Its
# certificate is self-signed and names a host the check never dials, exactly
# like one `sybra-cli cluster gen-cert` mints, so a check that verified a chain
# and hostname would fail it while sybra's own pinning client accepts it.
scenario_health_tls_board() {
  local root="$1"
  new_env "$root"
  if ! command -v openssl >/dev/null 2>&1; then
    echo "SKIP: health-tls (openssl unavailable)"
    return 0
  fi
  export FAKE_CHECK_CONFIG=ok
  run_build >"$root/build.log" 2>&1
  local cur; cur="$(current_target)"

  gen_selfsigned "$root/tls"
  export FAKE_HEALTH_MODE=ok
  export SYBRA_HEALTH_TIMEOUT_SEC=5
  export SYBRA_HEALTH_INTERVAL_SEC=1
  unset SYBRA_HEALTH_URL

  local port; port="$(next_port)"
  export SYBRA_SERVER_TARGET="https://127.0.0.1:$port"
  export SYBRA_SERVER_CA="$root/tls/cert.pem"
  local srv; srv="$(start_fake_tls_server "$cur/sybra-server" "$port" "$root/tls/cert.pem" "$root/tls/key.pem")"
  run_healthcheck >"$root/health-tls.log" 2>&1
  local rc=$?
  stop_fake_server "$srv"

  assert_eq "health-tls: a TLS board with a pinned certificate passes" "0" "$rc"
  assert_eq "health-tls: the TLS release is promoted to last-good" "$cur" "$(last_good_target)"
  assert_contains "health-tls: the resolved TLS posture is logged" "$(cat "$root/health-tls.log")" "tls: pinned to $root/tls/cert.pem"

  # Without the certificate there is nothing to trust, so the start fails
  # rather than skipping verification.
  port="$(next_port)"
  export SYBRA_SERVER_TARGET="https://127.0.0.1:$port"
  unset SYBRA_SERVER_CA
  srv="$(start_fake_tls_server "$cur/sybra-server" "$port" "$root/tls/cert.pem" "$root/tls/key.pem")"
  run_healthcheck >"$root/health-tls-nocert.log" 2>&1
  rc=$?
  stop_fake_server "$srv"
  assert_ne "health-tls: an unpinned self-signed board is not trusted" "0" "$rc"
  assert_contains "health-tls: the unpinned run reports no TLS material" "$(cat "$root/health-tls-nocert.log")" "tls: none"
}

# scenario_apparmor_profile checks the artifact every Ubuntu 24.04 agent host
# installs. A profile that stops compiling, or stops granting the namespace,
# breaks the process sandbox on every such host at once — and the failure would
# otherwise only surface as agents refusing to certify, long after the deploy.
scenario_apparmor_profile() {
  local profile="$HERE/../apparmor/sybra-bwrap"
  local runbook="$HERE/../README.md"
  assert_true "apparmor: the profile ships in the repo" test -f "$profile"

  # Rules only, never comments: the header explains the grant at length, so a
  # whole-file grep passes just as happily against `# userns,` — a profile that
  # compiles cleanly, loads, and grants nothing.
  local rules; rules="$(grep -v '^[[:space:]]*#' "$profile")"
  if grep -qE '^[[:space:]]*userns,[[:space:]]*$' <<<"$rules"; then
    pass "apparmor: it grants the user namespace as a rule, not a comment"
  else
    fail "apparmor: it grants the user namespace as a rule, not a comment"
  fi
  assert_contains "apparmor: it attaches to the bwrap binary" "$rules" "profile sybra-bwrap /usr/bin/bwrap"

  # The runbook is where a host-wide relaxation could plausibly creep in; the
  # profile has no syntax that could express one.
  if grep -qE 'apparmor_restrict_unprivileged_userns[[:space:]]*=[[:space:]]*0' "$runbook"; then
    fail "apparmor: the runbook clears the host-wide sysctl"
  else
    pass "apparmor: the runbook leaves the host-wide sysctl alone"
  fi

  # abi <abi/4.0> resolves against /etc/apparmor.d/abi/4.0, which only apparmor
  # 4.x (24.04+) ships. Gating on the parser alone turns a 22.04 runner into a
  # red suite that says nothing about the artifact.
  if [[ ! -f /etc/apparmor.d/abi/4.0 ]] || ! command -v apparmor_parser >/dev/null 2>&1; then
    echo "SKIP: apparmor: profile compilation (needs apparmor 4.x tooling)"
    return 0
  fi
  local out
  if out="$(apparmor_parser -Q -T "$profile" 2>&1)"; then
    pass "apparmor: the profile compiles"
  else
    fail "apparmor: the profile compiles ($out)"
  fi
}

# scenario_apparmor_drift covers the unattended case: auto_update ff-merges a
# corrected profile into the checkout and restarts the unit, but nothing
# reloads host policy, so the host can keep enforcing the previously parsed
# one. The build says so rather than leaving the operator to infer it from
# agents that stop certifying.
scenario_apparmor_drift() {
  local root="$1"
  new_env "$root"
  export FAKE_CHECK_CONFIG=ok
  mkdir -p "$root/src/deploy/apparmor"
  cp "$HERE/../apparmor/sybra-bwrap" "$root/src/deploy/apparmor/sybra-bwrap"
  ( cd "$root/src" && git add -A && git commit -q -m "carry the profile" )

  export SYBRA_APPARMOR_PROFILE="$root/installed-profile"
  cp "$HERE/../apparmor/sybra-bwrap" "$SYBRA_APPARMOR_PROFILE"
  run_build >"$root/build-same.log" 2>&1
  if grep -q "apparmor profile drift" "$root/build-same.log"; then
    fail "apparmor-drift: an identical profile reported drift"
  else
    pass "apparmor-drift: an identical profile is silent"
  fi

  printf '# stale copy\n' >>"$SYBRA_APPARMOR_PROFILE"
  bump_commit "$root" "rebuild against the stale profile"
  run_build >"$root/build-drift.log" 2>&1
  assert_contains "apparmor-drift: a stale host profile is reported" "$(cat "$root/build-drift.log")" "apparmor profile drift"
  unset SYBRA_APPARMOR_PROFILE
}

# seed_last_good runs one full build+activate+healthy-startup cycle so
# last-good is actually populated — sybra-build.sh's own fallback
# (keep_last_good_or_fail) only ever consults last-good, never current, so
# scenarios that expect a rejected candidate to "fall back" need this exact
# real sequence (build, then a passing health check) rather than just a bare
# run_build. Prints the seeded release dir.
seed_last_good() {
  local root="$1"
  export FAKE_CHECK_CONFIG=ok
  run_build >"$root/seed-build.log" 2>&1
  local rel; rel="$(current_target)"
  local port; port="$(next_port)"
  export FAKE_HEALTH_MODE=ok
  export SYBRA_HEALTH_URL="http://127.0.0.1:$port/health"
  export SYBRA_HEALTH_TIMEOUT_SEC=5
  export SYBRA_HEALTH_INTERVAL_SEC=1
  local srv; srv="$(start_fake_server "$rel/sybra-server" "$port")"
  run_healthcheck >"$root/seed-health.log" 2>&1
  stop_fake_server "$srv"
  echo "$rel"
}

# The CLI can disagree with the server about the same config: it tolerates a
# config it cannot parse by falling back to a direct task store, so a schema
# drift is a per-invocation warning rather than a failure. Agents run sybra-cli
# from inside their worktrees to read and update task state, so a CLI silently
# dropping the resolved config is a state-drift hazard in the middle of every
# workflow — the preflight must reject that build, not ship it.
scenario_cli_config_incompatibility() {
  local root="$1"
  new_env "$root"

  local seeded; seeded="$(seed_last_good "$root")"
  assert_eq "cli-config-incompat: seed promoted to last-good" "$seeded" "$(last_good_target)"

  bump_commit "$root" "cli-rejects-config"
  export FAKE_CHECK_CONFIG=ok
  export FAKE_CHECK_CONFIG_CLI=fail
  run_build >"$root/build-cli.log" 2>&1
  assert_eq "cli-config-incompat: rejected candidate still exits 0" "0" "$?"
  assert_eq "cli-config-incompat: current unchanged when only the CLI rejects" "$seeded" "$(current_target)"
  assert_contains "cli-config-incompat: logs the CLI preflight rejection" "$(cat "$root/build-cli.log")" "config-preflight-cli"

  # Deterministic, so the next attempt short-circuits rather than rebuilding.
  run_build >"$root/build-cli2.log" 2>&1
  assert_eq "cli-config-incompat: quarantined retry exits 0" "0" "$?"
  assert_contains "cli-config-incompat: quarantined retry skips rebuild" "$(cat "$root/build-cli2.log")" "Skipping rebuild"

  export FAKE_CHECK_CONFIG_CLI=ok
  mkdir -p "$SYBRA_HOME"
  echo "marker: cli-fixed" >"$SYBRA_HOME/config.yaml"
  run_build >"$root/build-cli3.log" 2>&1
  assert_eq "cli-config-incompat: activates once the CLI accepts" "0" "$?"
  if [[ "$(current_target)" == "$seeded" ]]; then
    fail "cli-config-incompat: current should advance after the CLI accepts the config"
  fi
}

scenario_config_incompatibility() {
  local root="$1"
  new_env "$root"

  local seeded; seeded="$(seed_last_good "$root")"
  assert_eq "config-incompat: seed promoted to last-good" "$seeded" "$(last_good_target)"

  bump_commit "$root" "bad-config-candidate"
  export FAKE_CHECK_CONFIG=fail
  run_build >"$root/build2.log" 2>&1
  assert_eq "config-incompat: rejected candidate still exits 0" "0" "$?"
  assert_eq "config-incompat: current unchanged after preflight rejection" "$seeded" "$(current_target)"
  assert_contains "config-incompat: logs the config-preflight rejection" "$(cat "$root/build2.log")" "config-preflight"

  run_build >"$root/build3.log" 2>&1
  assert_eq "config-incompat: quarantined retry exits 0" "0" "$?"
  assert_contains "config-incompat: quarantined retry skips rebuild" "$(cat "$root/build3.log")" "Skipping rebuild"
  assert_eq "config-incompat: current still unchanged after quarantined retry" "$seeded" "$(current_target)"

  # Fixing the live config (source unchanged) clears the block.
  export FAKE_CHECK_CONFIG=ok
  mkdir -p "$SYBRA_HOME"
  echo "marker: fixed" >"$SYBRA_HOME/config.yaml"
  run_build >"$root/build4.log" 2>&1
  assert_eq "config-incompat: fixed config activates" "0" "$?"
  assert_ne "config-incompat: current advanced once config is fixed" "$seeded" "$(current_target)"
}

scenario_build_failure() {
  local root="$1"
  new_env "$root"
  local seeded; seeded="$(seed_last_good "$root")"

  bump_commit "$root" "broken-frontend"
  export FAKE_NPM_FAIL=1
  export SYBRA_BUILD_RETRY_THRESHOLD=3
  run_build >"$root/build.log" 2>&1
  assert_eq "build-failure: exits 0 (keeps last-good)" "0" "$?"
  assert_eq "build-failure: current unchanged" "$seeded" "$(current_target)"
  assert_contains "build-failure: logs the frontend-build rejection" "$(cat "$root/build.log")" "frontend-build"
  assert_eq "build-failure: transient failure not quarantined yet" "0" "$(quarantine_count)"

  # A transient build failure must NOT permanently block the same sha+config:
  # the next attempt rebuilds rather than short-circuiting on a quarantine.
  run_build >"$root/build2.log" 2>&1
  assert_true "build-failure: retry rebuilds rather than skipping" \
    bash -c '! grep -q "Skipping rebuild" "'"$root"'/build2.log"'
  assert_eq "build-failure: still not quarantined after second transient failure" "0" "$(quarantine_count)"

  # Once the flake clears, the same sha+config builds and activates.
  unset FAKE_NPM_FAIL
  run_build >"$root/build3.log" 2>&1
  assert_eq "build-failure: recovers and activates once the flake clears" "0" "$?"
  assert_ne "build-failure: current advances after recovery" "$seeded" "$(current_target)"
}

scenario_build_failure_persistent() {
  local root="$1"
  new_env "$root"
  local seeded; seeded="$(seed_last_good "$root")"

  bump_commit "$root" "persistently-broken-frontend"
  export FAKE_NPM_FAIL=1
  export SYBRA_BUILD_RETRY_THRESHOLD=2

  run_build >"$root/build1.log" 2>&1
  assert_eq "build-persistent: first failure keeps last-good, no quarantine" "0" "$(quarantine_count)"
  assert_true "build-persistent: first failure stays retriable" \
    bash -c '! grep -q "Skipping rebuild" "'"$root"'/build1.log"'

  run_build >"$root/build2.log" 2>&1
  assert_eq "build-persistent: quarantined after crossing retry threshold" "1" "$(quarantine_count)"
  assert_eq "build-persistent: current still on last-good" "$seeded" "$(current_target)"

  run_build >"$root/build3.log" 2>&1
  assert_contains "build-persistent: subsequent build skips rebuild once quarantined" "$(cat "$root/build3.log")" "Skipping rebuild"
  assert_eq "build-persistent: current unchanged after quarantine" "$seeded" "$(current_target)"
}

scenario_lock_contention() {
  local root="$1"
  new_env "$root"
  export FAKE_CHECK_CONFIG=ok
  run_build >/dev/null 2>&1
  local seeded; seeded="$(current_target)"

  export SYBRA_DEPLOY_LOCK_WAIT_SEC=1
  mkdir -p "$(dirname "$SYBRA_DEPLOY_LOCK")"
  ( flock "$SYBRA_DEPLOY_LOCK" sleep 3 ) &
  local holder=$!
  sleep 0.3

  bump_commit "$root" "would-be-next-release"
  run_build >"$root/build.log" 2>&1
  local rc=$?
  wait "$holder" 2>/dev/null || true

  assert_eq "lock-contention: exits 0 rather than failing the unit" "0" "$rc"
  assert_contains "lock-contention: logs contention" "$(cat "$root/build.log")" "lock contention"
  assert_eq "lock-contention: current untouched by the blocked attempt" "$seeded" "$(current_target)"
}

scenario_successful_activation() {
  local root="$1"
  new_env "$root"
  export FAKE_CHECK_CONFIG=ok

  run_build >"$root/build.log" 2>&1
  assert_eq "success: build exits 0" "0" "$?"

  local cur; cur="$(current_target)"
  [[ -n "$cur" ]] && pass "success: current points to a release" || fail "success: current points to a release"
  assert_true "success: candidate server binary is executable" test -x "$cur/sybra-server"
  assert_true "success: candidate agentd binary is executable" test -x "$cur/sybra-agentd"
  assert_true "success: candidate web bundle staged" test -f "$cur/web/index.html"
  assert_eq "success: nothing quarantined" "0" "$(quarantine_count)"
  assert_true "success: sybra-cli symlink resolves through current" test -x "$SYBRA_HOME/.local/bin/sybra-cli"

  export FAKE_HEALTH_MODE=ok
  local port; port="$(next_port)"
  export SYBRA_HEALTH_URL="http://127.0.0.1:$port/health"
  export SYBRA_HEALTH_TIMEOUT_SEC=5
  export SYBRA_HEALTH_INTERVAL_SEC=1
  local srv; srv="$(start_fake_server "$cur/sybra-server" "$port")"

  run_healthcheck >"$root/health.log" 2>&1
  local rc=$?
  stop_fake_server "$srv"

  assert_eq "success: healthcheck exits 0" "0" "$rc"
  assert_eq "success: release promoted to last-good" "$cur" "$(last_good_target)"
}

scenario_agentd_refresh() {
  local root="$1"
  new_env "$root"
  export SYBRA_AGENTD_CONFIG="$root/sybra-agentd.yaml"
  export SYBRA_AGENTD_BINARY="$root/sybra-agentd"
  export FAKE_SYSTEMCTL_LOG="$root/systemctl.log"

  # Server-only hosts must remain a silent no-op even if a stale unit happens
  # to be enabled: the daemon config is the operator's opt-in boundary.
  export FAKE_AGENTD_ENABLED=1
  bash "$DEPLOY_BIN/sybra-refresh-agentd.sh"
  assert_true "agentd-refresh: absent config does not invoke systemctl" test ! -e "$FAKE_SYSTEMCTL_LOG"

  : >"$SYBRA_AGENTD_CONFIG"
  export FAKE_AGENTD_ENABLED=0
  bash "$DEPLOY_BIN/sybra-refresh-agentd.sh"
  assert_true "agentd-refresh: disabled unit is not started" test ! -e "$FAKE_SYSTEMCTL_LOG"

  export FAKE_AGENTD_ENABLED=1
  bash "$DEPLOY_BIN/sybra-refresh-agentd.sh"
  assert_true "agentd-refresh: rollback without binary is not restarted" test ! -e "$FAKE_SYSTEMCTL_LOG"

  : >"$SYBRA_AGENTD_BINARY"
  chmod +x "$SYBRA_AGENTD_BINARY"
  export FAKE_AGENTD_ENABLED=1
  bash "$DEPLOY_BIN/sybra-refresh-agentd.sh"
  assert_contains "agentd-refresh: enabled daemon restarts asynchronously" "$(cat "$FAKE_SYSTEMCTL_LOG")" "--no-block restart sybra-agentd.service"
}

scenario_repair_src_preflight() {
  local root="$1"
  new_env "$root"
  export SYBRA_REPAIR_ALLOW_ANY_SRC=1
  export SYBRA_SERVICE_USER="$(id -un)"
  export SYBRA_SERVICE_GROUP="$(id -gn)"

  run_repair_src >"$root/repair.log" 2>&1
  assert_eq "repair-src: exits 0 for isolated checkout" "0" "$?"
  assert_contains "repair-src: logs ownership status" "$(cat "$root/repair.log")" "source ownership"
}

scenario_health_check_failure() {
  local root="$1"
  new_env "$root"

  local good; good="$(seed_last_good "$root")"
  assert_eq "health-failure: seed promoted to last-good" "$good" "$(last_good_target)"

  # seed_last_good runs (and exports SYBRA_HEALTH_TIMEOUT_SEC/INTERVAL_SEC)
  # inside its own "$(...)" subshell — exports there never reach back into
  # this function's environment, so re-set them here for the loop below.
  bump_commit "$root" "unhealthy-candidate"
  export SYBRA_HEALTH_QUARANTINE_THRESHOLD=2
  export SYBRA_HEALTH_TIMEOUT_SEC=3
  export SYBRA_HEALTH_INTERVAL_SEC=1
  export FAKE_HEALTH_MODE=fail

  local bad="" i port srv
  for i in 1 2; do
    run_build >"$root/build-$i.log" 2>&1
    bad="$(current_target)"
    port="$(next_port)"
    export SYBRA_HEALTH_URL="http://127.0.0.1:$port/health"
    srv="$(start_fake_server "$bad/sybra-server" "$port")"
    run_healthcheck >"$root/health-$i.log" 2>&1
    local hc_rc=$?
    stop_fake_server "$srv"
    assert_eq "health-failure: attempt $i healthcheck fails" "1" "$hc_rc"
    assert_eq "health-failure: attempt $i rolls current back to last-good" "$good" "$(current_target)"
  done

  assert_contains "health-failure: quarantined once the threshold is crossed" "$(cat "$root/health-2.log")" "quarantin"

  run_build >"$root/build-3.log" 2>&1
  assert_contains "health-failure: subsequent build skips rebuild once quarantined" "$(cat "$root/build-3.log")" "Skipping rebuild"
  assert_eq "health-failure: current still on last-good after quarantine" "$good" "$(current_target)"
}

# scenario_rollback_preserves_quarantine reproduces the bug where a healthy
# restart on a rolled-back last-good release cleared the quarantine of the bad
# candidate it rolled back from — because the healthcheck keyed off the source
# HEAD (still the bad sha) instead of the release actually running behind
# "current". A cleared quarantine re-arms a rebuild/reactivation of the
# known-bad sha on the next restart.
scenario_rollback_preserves_quarantine() {
  local root="$1"
  new_env "$root"

  local good; good="$(seed_last_good "$root")"
  assert_eq "rollback-quarantine: seed promoted to last-good" "$good" "$(last_good_target)"

  # Build a candidate that fails its startup health check and quarantines
  # immediately (threshold 1), then rolls "current" back to the good release.
  bump_commit "$root" "unhealthy-candidate"
  export SYBRA_HEALTH_QUARANTINE_THRESHOLD=1
  export SYBRA_HEALTH_TIMEOUT_SEC=3
  export SYBRA_HEALTH_INTERVAL_SEC=1
  export FAKE_HEALTH_MODE=fail

  run_build >"$root/build-bad.log" 2>&1
  local bad; bad="$(current_target)"
  local port; port="$(next_port)"
  export SYBRA_HEALTH_URL="http://127.0.0.1:$port/health"
  local srv; srv="$(start_fake_server "$bad/sybra-server" "$port")"
  run_healthcheck >"$root/health-bad.log" 2>&1
  stop_fake_server "$srv"

  assert_eq "rollback-quarantine: bad candidate quarantined" "1" "$(quarantine_count)"
  assert_eq "rollback-quarantine: current rolled back to last-good" "$good" "$(current_target)"

  # The next restart boots the rolled-back good release (current -> good) and
  # its health check passes. That must promote the good release WITHOUT
  # clearing the bad candidate's quarantine: the bad sha is still HEAD and
  # unchanged, so re-arming it would rebuild/reactivate a known-bad release.
  export FAKE_HEALTH_MODE=ok
  port="$(next_port)"
  export SYBRA_HEALTH_URL="http://127.0.0.1:$port/health"
  srv="$(start_fake_server "$good/sybra-server" "$port")"
  run_healthcheck >"$root/health-good.log" 2>&1
  local rc=$?
  stop_fake_server "$srv"

  assert_eq "rollback-quarantine: healthy rollback release promotes" "0" "$rc"
  assert_eq "rollback-quarantine: good release stays last-good" "$good" "$(last_good_target)"
  assert_eq "rollback-quarantine: bad candidate stays quarantined" "1" "$(quarantine_count)"
}

main() {
  command -v go >/dev/null 2>&1 || { echo "go toolchain required to build the fixture module" >&2; exit 1; }

  TMPBASE="$(mktemp -d)"
  echo "test root: $TMPBASE"

  scenario_config_incompatibility "$TMPBASE/config-incompat"
  scenario_cli_config_incompatibility "$TMPBASE/cli-config-incompat"
  scenario_build_failure "$TMPBASE/build-failure"
  scenario_build_failure_persistent "$TMPBASE/build-failure-persistent"
  scenario_lock_contention "$TMPBASE/lock-contention"
  scenario_repair_src_preflight "$TMPBASE/repair-src"
  scenario_successful_activation "$TMPBASE/success"
  scenario_agentd_refresh "$TMPBASE/agentd-refresh"
  scenario_health_check_failure "$TMPBASE/health-failure"
  scenario_rollback_preserves_quarantine "$TMPBASE/rollback-quarantine"
  scenario_health_target_scheme "$TMPBASE/health-scheme"
  scenario_health_tls_board "$TMPBASE/health-tls"
  scenario_apparmor_profile
  scenario_apparmor_drift "$TMPBASE/apparmor-drift"

  echo
  echo "== $PASS passed, $FAIL failed =="
  [[ "$FAIL" -eq 0 ]]
}

main
