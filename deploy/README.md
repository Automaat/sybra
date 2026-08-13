# Sybra server — systemd deployment with lossless redeploys + auto-deploy (Design A)

Runs `sybra-server` directly on the dedicated **sybra LXC** (CT 114,
`192.168.20.219`) as a systemd service instead of a Docker container, so that:

1. **Redeploys don't kill in-flight agents.** A `systemctl restart` signals only
   the main server process; the detached (`setsid`) claude/codex agent
   subprocesses keep running and are re-adopted by the next start's
   `ReattachAll` — no interrupted turn, no `--resume`, no redone tokens.
2. **New CI-green `main` auto-deploys.** Sybra's built-in `autoupdate`
   resolves the exact `origin/main` SHA, waits until the configured required
   checks for that SHA are green, then fast-forwards, rebuilds, and restarts
   itself — turning each safe merge to `main` into a lossless rolling deploy.

The LXC is single-tenant (only `deploy-sybra.yml` targets it), so dropping the
container removes a redundant isolation layer — the LXC itself remains the
blast-radius boundary.

## Why this works (and why Docker didn't)

Sybra already ships the whole mechanism; Docker just defeated it.

- **Survival + reattach** (`internal/agent`, `internal/recovery`): agents spawn
  detached with `Setsid` and stream to a log file; a registry under
  `~/.sybra/agents/` records PID + session id + log path. On boot,
  `recovery.RunStartupCleanup` → `Manager.ReattachAll` re-adopts live processes
  (tails their logs) or, for dead ones, bridges the session id for `--resume`.
  `Setsid` escapes the **parent process** but **not** a container's PID
  namespace — so a `docker` container recreate SIGKILLs every agent regardless.
  A `systemctl restart` under `KillMode=process` keeps them in the same host PID
  + mount namespace, so reattach actually reattaches.
- **autoupdate** (`internal/autoupdate`): polls a git repo, resolves the exact
  remote branch SHA without moving the local checkout, queries GitHub check-runs
  + commit statuses for that SHA, and only then `git merge --ff-only <sha>` +
  `RequestRestart()` when every configured required check is green. It updates
  **source only** — it does **not** rebuild the binary. In the immutable
  prebuilt Docker image there was nothing to rebuild and no git checkout to
  advance, so autoupdate was inert and deploys were manual image-tag bumps.
  Under systemd, `ExecStartPre` rebuilds from the freshly-merged source and the
  supervisor honors exit 42 — the feature comes alive.

## The one directive that matters: `KillMode=process`

systemd's default `KillMode=control-group` sends `SIGTERM` to **every process in
the unit's cgroup** on stop/restart — that would kill the detached agents on
every deploy and make this strictly worse than Docker. The unit uses
`KillMode=process` so only the main server is signaled; survivors reparent to
PID 1 (systemd), which reaps them cleanly. **This is the make-or-break setting —
validate it (below) before trusting the deploy.**

## Files

| File | Installed to | Purpose |
|------|--------------|---------|
| `systemd/sybra.service` | `/etc/systemd/system/sybra.service` | The unit (KillMode=process, exit-42 restart, ExecStartPre build, ExecStartPost health check, start-rate limit). |
| `systemd/sybra-agentd.service` | `/etc/systemd/system/sybra-agentd.service` | Optional thin local execution worker. It uses the active release, survives provider processes with KillMode=process, and is refreshed after leader restarts. |
| `systemd/sybra.env.example` | `/etc/sybra/sybra.env` | Runtime env (listen port, local CLI server target, deploy paths, `PATH` with mise shims + npm globals). |
| `bin/sybra-deploy-lib.sh` | `/opt/sybra/bin/sybra-deploy-lib.sh` | Shared helpers (logging, host lock, quarantine key/marker, atomic symlink swap) sourced by the two scripts below. |
| `bin/sybra-repair-src.sh` | `/opt/sybra/bin/sybra-repair-src.sh` | Privileged `ExecStartPre`: repairs `/opt/sybra/src` ownership drift before the unprivileged build/autoupdate path touches `.git/objects`. |
| `bin/sybra-build.sh` | `/opt/sybra/bin/sybra-build.sh` | `ExecStartPre`: build web + server + CLI + agentd from `/opt/sybra/src` into a versioned candidate, preflight it against the live config, atomically activate it, or quarantine + keep last-good. |
| `bin/sybra-healthcheck.sh` | `/opt/sybra/bin/sybra-healthcheck.sh` | `ExecStartPost`: poll the just-started release's `/health`; promote to last-good on success, or roll back + record a failure (quarantining after repeated failures) on timeout. |
| `bin/sybra-refresh-agentd.sh` | `/opt/sybra/bin/sybra-refresh-agentd.sh` | Final `ExecStartPost`: asynchronously restart an installed/enabled local agentd onto the newly activated release; no-op on server-only hosts. |
| `bin/sybra-run.sh` | `/opt/sybra/bin/sybra-run.sh` | `ExecStart`: activate mise toolchain, `exec` whichever release `current` points at. |

Layout on the box:

```
/opt/sybra/
  src/           git checkout of Automaat/sybra on main   (autoupdate RepoDir)
  review-src/    second, independent checkout for human-review's fallback dir
  bin/           sybra-deploy-lib.sh, sybra-repair-src.sh, sybra-build.sh, sybra-healthcheck.sh, sybra-run.sh
  releases/<id>/ versioned candidate builds: sybra-server, sybra-cli, sybra-agentd, web/
  current        symlink -> releases/<id>, the release ExecStart runs
  last-good      symlink -> releases/<id>, restored automatically on a failed health check
  quarantine/    <sha+config-fingerprint>.reason markers for rejected candidates
  deploy-state/  per-candidate health-failure counters + phase detail logs (host-local, never shipped)
  deploy.lock    flock'd across build + health-check so they never overlap
/etc/sybra/sybra.env
/data/sybra/home  →  HOME=/home/sybra/.sybra  (config, tasks, worktrees, agent registry)
```

`sybra-build.sh` builds `sybra-cli` from the same checkout as `sybra-server`
into the same candidate directory, activated in one atomic symlink swap — so
a candidate is always either the fully-matched pair or not activated at all,
never a half-applied mix (#2619: `sybra-cli` could hard-fail on a
config-schema key the running server already understood, because only
`sybra-server` was rebuilt). The CLI symlink at `$HOME/.local/bin/sybra-cli`
(`/home/sybra/.local/bin/sybra-cli`, already on `PATH` per
`sybra.env.example`) points at `$SYBRA_CURRENT_LINK/sybra-cli` rather than a
specific release, so it tracks whichever release is active — including after
a healthcheck-driven rollback — without being re-linked on every build.

`review-src/` exists so `human_review.sybra_repo_dir` never resolves to the
same directory `auto_update.repo_dir` builds and ff-merges from (#1925) —
see the config section below.

autoupdate also persists state under `~/.sybra/autoupdate-state.json`,
including the last restart time, so **restart coalescing** survives a process
restart: in `mode: auto`, a newly-approved candidate only triggers a merge +
restart once `auto_update.coalesce` (default 1h) has elapsed since the last
restart. Polls inside that window keep resolving the newest CI-green
candidate and hold it — a burst of merges to `main` produces at most one
restart per interval instead of one per merge. The manual emergency bypass is
a one-shot marker file at `~/.sybra/autoupdate-override`: create it to let the
next poll deploy the current remote SHA immediately, bypassing both the CI
gate and the coalescing window, and autoupdate deletes it after a successful
apply.

## Build-safety contract

`sybra-build.sh` builds into a fresh `releases/<sha>-<timestamp>` directory and
only ever repoints the `current` symlink — atomically, via `ln -sfn` + `mv -T`
so it's never briefly missing or half-written — after every phase has passed:
`mise install`, frontend build, `go build` for all three binaries, the linked-worktree
sandbox smoke test (when `bwrap` is present), and finally a **config
preflight**: it runs the freshly-built `sybra-server -check-config` against
the exact live `config.yaml` (same env, same `SYBRA_HOME`, no side effects —
see `cmd/sybra-server`'s `-check-config` flag) before that binary is ever
allowed to become `current`. Any phase failing keeps `current` untouched and
exits 0 so the service starts on the previous release; only a build failure
with **no** prior good release at all fails the unit (nothing safe to fall
back to — an expected failure mode on first-ever provisioning only).

Runtime health is a separate, later check: `sybra-healthcheck.sh` runs as
`ExecStartPost` once the newly-activated release has actually started, and
polls its `/health` endpoint. Success promotes that release to `last-good`.
Failure rolls `current` back to `last-good` immediately and records a
failure against that candidate; a candidate that keeps failing health checks
(`SYBRA_HEALTH_QUARANTINE_THRESHOLD` consecutive times, default 3) is
quarantined the same way a build/preflight failure is. `sybra-healthcheck.sh`
exiting non-zero
fails that systemd start, which is what drives the actual restart via
`Restart=on-failure` + `RestartSec=3` — neither script ever calls `systemctl`
itself.

The URL it polls comes from `SYBRA_SERVER_TARGET`, which takes either a bare
`host:port` or a full `http(s)://` origin — the same two forms `sybra-cli` and
the desktop app accept. An origin keeps its own scheme, so a board that
terminates TLS is polled over https rather than being rewritten to http; set
`SYBRA_SERVER_CA` alongside it when that board serves its own certificate, and
`SYBRA_HEALTH_URL` to name the URL outright (it wins over both).

`SYBRA_SERVER_CA` **pins** that certificate rather than verifying a chain,
matching what sybra's own client does with the same variable. `cluster
gen-cert` mints a self-signed leaf whose SANs are only the hosts the leader
dials, so demanding hostname verification would fail a certificate sybra
itself accepts — polling `https://127.0.0.1:8080`, the target this guide tells
you to set, against a certificate issued for the board's own name. The check
does not read `cluster.tls.cert_file` from the config: name the file here.

**Quarantine** ties a rejection (any phase — build, sandbox smoke, config
preflight, or repeated health-check failure) to a key derived from the
source SHA *and* a fingerprint of the live config file. A quarantined
candidate's next deploy attempt (the next autoupdate poll, or a manual
`systemctl restart`) is rejected up front — before `mise install` even runs —
so a candidate that is deterministically bad never repeats a full
dependency-install/build cycle, and a crash-looping service never rebuilds
and reactivates the same broken bits on every restart. The block clears the
moment either the source SHA or the live config changes (a new quarantine key).
Full phase diagnostics (build/test/preflight output, which may be verbose but
never intentionally secret-bearing) are written to a host-local file under
`deploy-state/` and only referenced by path in the journal — quarantine
markers and journal lines themselves stay short, static, and safe to paste
into an issue.

**Host lock:** both scripts wrap their work in a `flock` on `deploy.lock`
(`SYBRA_DEPLOY_LOCK`, `SYBRA_DEPLOY_LOCK_WAIT_SEC` default 300s), so a
manually-invoked `sybra-build.sh`/`sybra-healthcheck.sh` can never race an
in-flight systemd-driven one. Losing the race logs "lock contention" and
backs off (exit 0) rather than corrupting `current`/`last-good`.

**Start-rate limit:** `StartLimitIntervalSec=600` / `StartLimitBurst=6` in the
unit are a backstop behind the quarantine logic above — if something keeps
failing to start despite quarantine (e.g. `last-good` itself stops passing
health checks), systemd stops auto-restarting after 6 attempts in 10 minutes
and leaves the unit `failed` instead of retrying forever. See "Recovery
runbook" below for what to do when that fires.

Consequence to accept:

> autoupdate `ff-merge`s **before** requesting the restart, so a broken `main`
> advances `/opt/sybra/src` to a SHA that won't build or won't pass health
> checks. The service keeps running the previous release (no downtime), but
> source and running-release diverge until the **next green, healthy** `main`
> lands. Self-healing, but the mismatch is real — see caveats.

## Recovery runbook

- **Inspect why a candidate was rejected:** `ls /opt/sybra/quarantine` lists
  quarantined `<key>.reason` markers (timestamp, candidate sha, phase,
  short reason). Full output for a given release id lives at
  `/opt/sybra/deploy-state/<id>.<phase>.log` — check there before pasting
  anything from a quarantine marker into an issue, since the marker is
  intentionally the redacted summary. `journalctl -u sybra` also carries the
  same phase/candidate/rollback lines.
- **Force a retry of a quarantined candidate** once you believe source or
  config actually changed: `rm /opt/sybra/quarantine/<key>.reason` (or just
  land the fix — a genuinely different source SHA or config fingerprint
  clears it automatically) then `systemctl restart sybra`.
- **Manual rollback to last-good right now**, without waiting on a restart:
  `ln -sfn "$(readlink -f /opt/sybra/last-good)" /tmp/current.tmp && mv -T /tmp/current.tmp /opt/sybra/current && systemctl restart sybra`.
- **Intentional rollback to an older release:** every activated release is
  still on disk under `/opt/sybra/releases/` (the 3 most recent plus whatever
  `current`/`last-good` point at — `sybra-build.sh` prunes the rest after each
  successful activation). Repoint `current` at the desired `releases/<id>`
  the same way, then restart.
- **Unit stuck `failed` after hitting the start-rate limit:** `systemctl
  reset-failed sybra` clears systemd's counter — but first fix whatever's
  actually broken (check quarantine + `deploy-state/` as above), or you'll
  just burn through the limit again.
- **Stale lock after a killed script:** `deploy.lock` only holds a lock for
  the lifetime of the process that opened it (`flock` on an fd) — a killed
  script releases it immediately, nothing to clean up manually. If
  `sybra-build.sh`/`sybra-healthcheck.sh` reports contention against a
  process that's actually gone, that's a bug, not an operator fix.

## One-time LXC provisioning

Run on the LXC as root (or fold into `setup-sybra-lxc.yml`). Assumes the
`sybra` user + `/data/sybra/*` already exist from the current deploy.

```bash
apt-get update && apt-get install -y --no-install-recommends \
  git openssh-client curl ca-certificates gpg ripgrep bubblewrap apparmor

# gh CLI (matches the Dockerfile's apt source)
curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
  | dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg
echo "deb [signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
  > /etc/apt/sources.list.d/github-cli.list
apt-get update && apt-get install -y gh

install -d -o sybra -g sybra /opt/sybra /opt/sybra/bin /opt/sybra/review-src /etc/sybra
# releases/, current, last-good, quarantine/, deploy-state/, deploy.lock are
# created on demand by sybra-build.sh/sybra-healthcheck.sh — nothing to
# pre-create for them.
```

As the `sybra` user:

```bash
# mise + toolchain (go/node pinned in mise.toml)
curl -fsSL https://mise.run | sh
export PATH="$HOME/.local/bin:$PATH"

git clone https://github.com/Automaat/sybra /opt/sybra/src
cd /opt/sybra/src && mise install

# Second, independent checkout for human_review.sybra_repo_dir — never point
# this at /opt/sybra/src (see "Config changes" below). It only needs to stay
# roughly current for diagnosis, so a periodic `git -C /opt/sybra/review-src
# pull` (e.g. a daily cron) is enough; it does not need mise/toolchain install.
git clone https://github.com/Automaat/sybra /opt/sybra/review-src

# claude + codex CLIs as npm globals, then reshim so mise exposes them on PATH
mise exec -- npm install -g @anthropic-ai/claude-code @openai/codex
mise reshim
```

Install the unit + scripts + env:

```bash
install -m 0755 /opt/sybra/src/deploy/bin/sybra-deploy-lib.sh  /opt/sybra/bin/
install -m 0755 /opt/sybra/src/deploy/bin/sybra-repair-src.sh  /opt/sybra/bin/
install -m 0755 /opt/sybra/src/deploy/bin/sybra-build.sh       /opt/sybra/bin/
install -m 0755 /opt/sybra/src/deploy/bin/sybra-healthcheck.sh /opt/sybra/bin/
install -m 0755 /opt/sybra/src/deploy/bin/sybra-refresh-agentd.sh /opt/sybra/bin/
install -m 0755 /opt/sybra/src/deploy/bin/sybra-run.sh         /opt/sybra/bin/
install -m 0644 /opt/sybra/src/deploy/systemd/sybra.service /etc/systemd/system/
cp /opt/sybra/src/deploy/systemd/sybra.env.example /etc/sybra/sybra.env   # then edit
systemctl daemon-reload
systemctl enable --now sybra
```

### The bwrap AppArmor profile

Every host that runs agents needs this, or the process sandbox cannot start at
all. Ubuntu 24.04 sets `kernel.apparmor_restrict_unprivileged_userns=1`, which
denies unprivileged user namespaces to any binary without a profile permitting
them — `bwrap` is installed, on PATH, and still fails the moment it maps uids.
`agent.sandbox_mode: enforce` then refuses to certify the host, and `report`
leaves every agent unwrapped.

```bash
install -m 0644 /opt/sybra/src/deploy/apparmor/sybra-bwrap /etc/apparmor.d/sybra-bwrap
apparmor_parser -r -W /etc/apparmor.d/sybra-bwrap
```

Verify as the service account, which is who actually builds sandboxes — it
must print `ok`:

```bash
sudo -u sybra bwrap --unshare-pid --ro-bind / / --dev /dev --proc /proc \
  /bin/echo ok
```

The profile grants the namespace to the `bwrap` binary alone. Leave the sysctl
itself at `1`: clearing it host-wide would hand unprivileged user namespaces to
every other binary on the box, which is the containment this profile exists to
avoid giving up. Re-run both commands after a deploy that changes the profile —
it ships in the repo (`deploy/apparmor/sybra-bwrap`) with the code that depends
on it, exactly like the unit and the build scripts.

To run the optional local thin worker after its YAML and secret environment
from [the agentd runbook](../docs/agentd.md) are installed:

```bash
install -m 0644 /opt/sybra/src/deploy/systemd/sybra-agentd.service /etc/systemd/system/
install -d -m 0700 -o sybra -g sybra /var/lib/sybra-agentd
systemctl daemon-reload
systemctl enable --now sybra-agentd
```

`StateDirectory=sybra-agentd` also provisions `/var/lib/sybra-agentd` with the
service account ownership on a fresh host. The explicit `install -d` makes the
ownership visible before first start and is safe to repeat.

## Config changes (`config.yaml` / `sybra-config.yaml.j2`)

The template already carries both blocks — flip them:

```yaml
agent:
    survive_restart: null      # null == enabled; leave as-is (do NOT set false)

auto_update:
    enabled: true
    repo_dir: /opt/sybra/src
    remote: origin
    branch: main
    required_checks:
      - lint-go / lint
      - test-go / test
      - build / build
    mode: notify               # start here; flip to auto once validated
    poll_seconds: 300
    coalesce_seconds: 3600     # min gap between restarts in auto mode
```

`required_checks` must list the exact GitHub check/status names that gate a
deploy on this box. `mode: auto` fails closed when the list is empty, when a
required check is missing, or when GitHub App auth cannot fetch the SHA's
status cleanly.

Also drop `human_review.sybra_repo_dir: /app/src` → `/opt/sybra/review-src` —
**not** `/opt/sybra/src`. The old `/app/src` value was the in-image source
copy; the replacement must be the dedicated `review-src` checkout provisioned
above, never the live deploy checkout `auto_update.repo_dir` builds and
ff-merges from. The review agent is dispatched with a read-only process
sandbox whenever it falls back to `sybra_repo_dir` (no task worktree), so a
misconfigured value here can't actually be written to — but pointing both
keys at the same directory still means the diagnostic agent's `git`/`grep`
reads race the autoupdate loop's concurrent merges into that tree (#1925).

## home-nas ansible changes (`deploy-sybra.yml`)

Rework the playbook from container to service:

- **Remove:** `docker.io` / `docker-compose-v2` install, the compose copy, the
  `docker_compose_v2` deploy task, and the `docker restart sybra` handler.
- **Add:** the provisioning above (apt deps, gh, mise, repo clone, `mise install`,
  npm globals + reshim), templated as tasks.
- **Install:** the unit, scripts, and a templated `/etc/sybra/sybra.env` via
  `ansible.builtin.copy` / `template`.
- **Install:** `deploy/apparmor/sybra-bwrap` into `/etc/apparmor.d/`, from the
  checkout rather than a copy kept in the playbook, with a handler running
  `apparmor_parser -r -W` on it. Without it the process sandbox cannot start on
  any Ubuntu 24.04 host. Do **not** template the sysctl — the profile grants
  the namespace to `bwrap` alone.
- **New handler:** `systemctl daemon-reload` + `systemctl restart sybra`
  (replaces `docker restart sybra`). Config/secret tasks keep `notify`-ing it.
- Keep all the existing secret + hook-config tasks (github-app.pem, klaudiush,
  claude/codex config) — those are unchanged; only the mount target moves from
  container paths to the host `sybra` user's `$HOME`.

The `deploy/` artifacts live in **this repo** on purpose: autoupdate rebuilds
from the sybra checkout, so the unit + build script are versioned alongside the
code they build. home-nas just installs them.

## Deploying a follower (leader-follower mode)

A follower is the *same* unit and the *same* build — only its config and env
differ. See `docs/leader-follower.md` for the transport tiers and the security
model; this is the deployment checklist.

**1. Provision the host exactly as above** (mise + toolchain + provider CLIs).
A follower runs agents, so it needs the full toolchain, not a thin proxy.

**2. Give the unit a token and explicit local API target.** In `/etc/sybra/sybra.env`:

```
SYBRA_AUTH_TOKEN=<32-byte hex, unique per node>
SYBRA_SERVER_TARGET=127.0.0.1:8080
```

The same value goes on the leader, named — never inlined:

```yaml
followers:
  - name: gpu-box
    auth_token_env: GPU_BOX_TOKEN     # leader's unit reads it from its own env
```

**3. Set the role and the bind** in the follower's `~/.sybra/config.yaml`:

```yaml
cluster:
  role: follower
  bind_addr: ":8080"          # or bind_addrs: [...] to lock down interfaces
```

A follower hard-disables every poller (GitHub/Renovate) regardless of its
own feature flags, so no `project_types` juggling is needed — the leader is the
only ingress.

**4. Pick a transport.** On a tailnet, nothing more is required. For the LAN +
pinned-cert tier, generate the keypair **on the follower**:

```bash
sudo -u sybra sybra-cli cluster gen-cert --host gpu-box.lan --host 192.168.20.9 \
  --out /data/sybra/tls
```

It prints both config blocks, including the `tls_pin` for the leader. Point the
follower at the keypair:

```yaml
cluster:
  tls:
    cert_file: /data/sybra/tls/follower.crt
    key_file: /data/sybra/tls/follower.key
```

The private key is written `0600` and owned by the `sybra` user; it must stay off
the leader and out of git. `gen-cert` **overwrites** `follower.crt`/`follower.key`
in the target directory, so running it against a live follower's dir rotates that
node's identity — the leader will refuse the connection until you update
`tls_pin`. The pin is the whole trust anchor: regenerating the cert always means
updating the leader.

**5. Home some projects on it** (leader side) and restart both:

```yaml
followers:
  - name: gpu-box
    endpoints: ["https://gpu-box.lan:8080"]
    tls_pin: <printed by gen-cert>
    trusted: true                      # required for work-typed projects
    homes: ["Automaat/sybra"]
```

**Verify:** each node logs one `server.listen` line per listener with its `tls=`
and `role=`; the leader logs `cluster.leader.enabled followers=[...]`. From the
leader, `sybra-cli cluster nodes` lists the roster with its resolved
`trusted`/`encrypted` posture — if a node you expect to be encrypted shows
`encrypted=false`, the confidentiality guard will refuse work-typed tasks on it.

## How a deploy happens

- **Automatic:** merge to `main` → within `poll_seconds` autoupdate resolves the
  exact remote SHA and waits for the configured required checks on that SHA to
  turn green. Once approved, the merge + restart itself is deferred until at
  least `coalesce_seconds` has passed since the last restart — so a burst of
  merges within that window still produces exactly one ff-merge + restart, for
  the newest approved SHA at the time the window elapses → exit 42 → systemd
  reruns `ExecStartPre` (build + preflight + atomic activate) → `ExecStart`
  (run the activated release) → `ExecStartPost` (health check → promote to
  last-good, or roll back + record a failure) → agents reattach.
- **Manual:** `systemctl restart sybra` (rebuilds current `/opt/sybra/src` HEAD).
- **Pin/rollback:** `git -C /opt/sybra/src checkout <good-sha>` then
  `systemctl restart sybra`. (While pinned to a non-HEAD SHA, autoupdate's
  ff-only check blocks further auto-updates until you return to `main` — it
  refuses to update a diverged/ahead checkout.)
- **Emergency bypass:** `sudo -u sybra touch /data/sybra/home/autoupdate-override`
  to let the next poll apply the current remote SHA once even if CI is still
  red/pending, skipping the coalescing window too. The marker is deleted after
  a successful apply; this is for operator intervention only, never normal
  automation.

## Validation checklist (do this before trusting it)

1. **Toolchain reachable by the service:** `systemctl show sybra -p ExecStart`
   env resolves `git`, `gh`, `claude`, `codex`, `node`, `mise` on `PATH`
   (`sudo -u sybra env -i bash -lc 'mise exec -- which claude codex git gh node'`).
2. **The KillMode test (make-or-break):** start a real headless agent, note its
   PID (`pgrep -f 'claude|codex'`), `systemctl restart sybra`, then confirm the
   **same** agent PID is still alive and reparented to PID 1
   (`ps -o pid,ppid,cmd -p <pid>`), and the sybra log shows `agent.reattach`
   for it (not `agent.reattach.dead`). If the PID is gone, `KillMode=process`
   isn't taking effect — fix before enabling auto-update.
3. **CI gate:** with `mode: notify`, land a trivial `main` change and confirm
   the log reports `autoupdate.check status=waiting|approved` for the exact
   candidate SHA instead of merging immediately; a missing/failed required check
   must stay unapplied.
4. **Build fallback:** push a deliberately-broken commit to a test branch,
   point `repo_dir` at it, restart — the service must come up on the last-good
   binary (`journalctl -u sybra` shows `keeping last-good build`).
5. **Exit-42 loop:** with `mode: auto`, land a trivial green `main` change and
   confirm `autoupdate.restart.requested` → rebuild → `Started sybra` in the
   journal.
6. **Coalescing:** with `mode: auto`, land two green `main` changes a few
   minutes apart, both inside `coalesce_seconds`. The log should show
   `autoupdate.check status=coalesced ... coalesced_count=1` for the second one
   instead of a second restart; only one `autoupdate.restart.requested` fires,
   for the newer SHA, once the window elapses.
7. **Config preflight:** add an unknown/incompatible key to
   `~/.sybra/config.yaml`, restart — the journal must show a `config-preflight`
   rejection and a quarantine marker under `/opt/sybra/quarantine`, and
   `current` must not move. Revert the key and restart again — the same source
   SHA now activates.
8. **Health-check rollback:** temporarily break something the server needs at
   startup that CI can't catch (e.g. point `SYBRA_SERVER_TARGET`'s port at one
   already bound by another process), restart — `sybra-healthcheck.sh` must
   time out, roll `current` back to `last-good`, and the service must come
   back up on the previous release without operator intervention.
9. **Deploy lock:** run `sybra-build.sh` by hand while a real deploy (or a
   second manual invocation) holds `/opt/sybra/deploy.lock`; the second one
   must log "lock contention" and exit 0 without touching `current`.
10. **Fixture coverage:** `bash deploy/tests/deploy_integration_test.sh` runs
    all five of the above (config incompatibility, build failure,
    health-check failure + quarantine, lock contention, successful
    activation) hermetically against a trimmed fixture module — run it after
    touching any `deploy/bin/*.sh` script, before validating on the real box.

## Caveats / decisions to make

- **Required-check names are configuration, not discovery.** If branch
  protection changes, update `auto_update.required_checks` to match or
  autoupdate will fail closed on "missing required checks".
- **Broken `main` no longer auto-deploys by default.** A red/pending/missing
  required check leaves the candidate unapplied. The emergency bypass marker is
  the explicit operator escape hatch when you intentionally want to deploy a red
  SHA.
- **A green merge can wait up to `coalesce_seconds` (default 1h) for its
  restart.** This is the point of coalescing — restart churn was the reported
  incident — but it means a legitimate hotfix landed just after a restart sits
  approved-but-unapplied until the window elapses, unless you use the
  emergency bypass marker.
- **On-box build time** (~1–2 min Go + frontend) is the restart window. Agents
  survive it; the HTTP endpoint is down for it. Fine for a home deployment.
- **No `restart: unless-stopped` auto-heal** — replaced by systemd
  `Restart=on-failure` + `WantedBy=multi-user.target` (survives host reboot).
- **codex sandbox:** `SYBRA_DISABLE_CODEX_SANDBOX=1` is still required (LXC
  kernels disable unprivileged userns; the LXC is the sandbox).
- **Health-check timeout is a startup budget, not a liveness probe.**
  `SYBRA_HEALTH_TIMEOUT_SEC` (default 60s) only bounds how long
  `sybra-healthcheck.sh` waits for `/health` to answer once at startup; it
  never re-checks a release that was already promoted. A regression that only
  shows up after minutes/hours of runtime is not caught by this mechanism —
  that's what alerting on the running service is for.
- **Quarantine threshold trades blast radius for tolerance of transient
  flakiness.** Config-preflight and build/sandbox failures quarantine
  immediately (deterministic against an unchanged sha+config, so a retry
  can't succeed differently). Health-check failures get
  `SYBRA_HEALTH_QUARANTINE_THRESHOLD` (default 3) attempts first, since a slow
  dependency or transient startup hiccup is plausible at that layer — each
  attempt still costs a full restart cycle, bounded by `RestartSec=3` and the
  unit's `StartLimitBurst=6`/`StartLimitIntervalSec=600` backstop.
- **`releases/` retention is best-effort.** `sybra-build.sh` prunes to the 3
  most recently built releases plus whatever `current`/`last-good` point at,
  after every successful activation; a run that never reaches activation
  (quarantined) leaves nothing extra behind (its `.building` candidate
  directory is removed on rejection).
