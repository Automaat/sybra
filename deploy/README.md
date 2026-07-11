# Sybra server — systemd deployment with lossless redeploys + auto-deploy (Design A)

Runs `sybra-server` directly on the dedicated **sybra LXC** (CT 114,
`192.168.20.219`) as a systemd service instead of a Docker container, so that:

1. **Redeploys don't kill in-flight agents.** A `systemctl restart` signals only
   the main server process; the detached (`setsid`) claude/codex agent
   subprocesses keep running and are re-adopted by the next start's
   `ReattachAll` — no interrupted turn, no `--resume`, no redone tokens.
2. **New `main` auto-deploys.** Sybra's built-in `autoupdate` fast-forwards a
   git checkout to `origin/main`, rebuilds, and restarts itself — turning every
   merge to `main` into a lossless rolling deploy.

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
- **autoupdate** (`internal/autoupdate`): polls a git repo, `git merge
  --ff-only origin/main`, then calls `RequestRestart()` → the server exits with
  code **42** (`autoupdate.RestartExitCode`). It updates **source only** — it
  does **not** rebuild the binary. In the immutable prebuilt Docker image there
  was nothing to rebuild and no git checkout to advance, so autoupdate was inert
  and deploys were manual image-tag bumps. Under systemd, `ExecStartPre` rebuilds
  from the freshly-merged source and the supervisor honors exit 42 — the feature
  comes alive.

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
| `systemd/sybra.service` | `/etc/systemd/system/sybra.service` | The unit (KillMode=process, exit-42 restart, ExecStartPre build). |
| `systemd/sybra.env.example` | `/etc/sybra/sybra.env` | Runtime env (port, static dir, `PATH` with mise shims + npm globals). |
| `bin/sybra-build.sh` | `/opt/sybra/bin/sybra-build.sh` | `ExecStartPre`: build web + binary from `/opt/sybra/src`, atomic swap, keep last-good on failure. |
| `bin/sybra-run.sh` | `/opt/sybra/bin/sybra-run.sh` | `ExecStart`: activate mise toolchain, `exec` the built binary. |

Layout on the box:

```
/opt/sybra/
  src/     git checkout of Automaat/sybra on main   (autoupdate RepoDir)
  bin/     sybra-build.sh, sybra-run.sh
  build/   sybra-server (running binary) + web/ (static bundle)
/etc/sybra/sybra.env
/data/sybra/home  →  HOME=/home/sybra/.sybra  (config, tasks, worktrees, agent registry)
```

## Build-safety contract

`sybra-build.sh` builds to `*.new` and swaps in the new artifacts **only** on a
clean build; on failure it keeps the last-good build and exits 0 so the service
starts on the previous binary. Consequence to accept:

> autoupdate `ff-merge`s **before** requesting the restart, so a broken `main`
> advances `/opt/sybra/src` to a SHA that won't build. The service keeps running
> the previous binary (no downtime), but source and running-binary diverge until
> the **next green `main`** lands and rebuilds. Self-healing, but the mismatch is
> real — see caveats.

## One-time LXC provisioning

Run on the LXC as root (or fold into `setup-sybra-lxc.yml`). Assumes the
`sybra` user + `/data/sybra/*` already exist from the current deploy.

```bash
apt-get update && apt-get install -y --no-install-recommends \
  git openssh-client curl ca-certificates gpg ripgrep

# gh CLI (matches the Dockerfile's apt source)
curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
  | dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg
echo "deb [signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
  > /etc/apt/sources.list.d/github-cli.list
apt-get update && apt-get install -y gh

install -d -o sybra -g sybra /opt/sybra /opt/sybra/bin /opt/sybra/build /etc/sybra
```

As the `sybra` user:

```bash
# mise + toolchain (go/node pinned in mise.toml)
curl -fsSL https://mise.run | sh
export PATH="$HOME/.local/bin:$PATH"

git clone https://github.com/Automaat/sybra /opt/sybra/src
cd /opt/sybra/src && mise install

# claude + codex CLIs as npm globals, then reshim so mise exposes them on PATH
mise exec -- npm install -g @anthropic-ai/claude-code @openai/codex
mise reshim
```

Install the unit + scripts + env:

```bash
install -m 0755 /opt/sybra/src/deploy/bin/sybra-build.sh /opt/sybra/bin/
install -m 0755 /opt/sybra/src/deploy/bin/sybra-run.sh   /opt/sybra/bin/
install -m 0644 /opt/sybra/src/deploy/systemd/sybra.service /etc/systemd/system/
cp /opt/sybra/src/deploy/systemd/sybra.env.example /etc/sybra/sybra.env   # then edit
systemctl daemon-reload
systemctl enable --now sybra
```

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
    mode: notify               # start here; flip to auto once validated
    poll_seconds: 300
```

Also drop `human_review.sybra_repo_dir: /app/src` → `/opt/sybra/src` (the old
value was the in-image source copy).

## home-nas ansible changes (`deploy-sybra.yml`)

Rework the playbook from container to service:

- **Remove:** `docker.io` / `docker-compose-v2` install, the compose copy, the
  `docker_compose_v2` deploy task, and the `docker restart sybra` handler.
- **Add:** the provisioning above (apt deps, gh, mise, repo clone, `mise install`,
  npm globals + reshim), templated as tasks.
- **Install:** the unit, scripts, and a templated `/etc/sybra/sybra.env` via
  `ansible.builtin.copy` / `template`.
- **New handler:** `systemctl daemon-reload` + `systemctl restart sybra`
  (replaces `docker restart sybra`). Config/secret tasks keep `notify`-ing it.
- Keep all the existing secret + hook-config tasks (github-app.pem, klaudiush,
  claude/codex config) — those are unchanged; only the mount target moves from
  container paths to the host `sybra` user's `$HOME`.

The `deploy/` artifacts live in **this repo** on purpose: autoupdate rebuilds
from the sybra checkout, so the unit + build script are versioned alongside the
code they build. home-nas just installs them.

## How a deploy happens

- **Automatic:** merge to `main` → within `poll_seconds` autoupdate ff-merges +
  requests restart → exit 42 → systemd reruns `ExecStartPre` (rebuild) → new
  binary starts → agents reattach.
- **Manual:** `systemctl restart sybra` (rebuilds current `/opt/sybra/src` HEAD).
- **Pin/rollback:** `git -C /opt/sybra/src checkout <good-sha>` then
  `systemctl restart sybra`. (While pinned to a non-HEAD SHA, autoupdate's
  ff-only check blocks further auto-updates until you return to `main` — it
  refuses to update a diverged/ahead checkout.)

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
3. **Build fallback:** push a deliberately-broken commit to a test branch,
   point `repo_dir` at it, restart — the service must come up on the last-good
   binary (`journalctl -u sybra` shows `keeping last-good build`).
4. **Exit-42 loop:** with `mode: auto`, land a trivial `main` change and confirm
   `autoupdate.restart.requested` → rebuild → `Started sybra` in the journal.

## Caveats / decisions to make

- **Broken `main` auto-deploys.** autoupdate is ff-only and 5-min-batched, and
  the build-swap keeps the service up — but it does **not** check the SHA passed
  CI. If you want "only deploy green SHAs," gate autoupdate on a status check
  (small follow-up) or switch to Design B (CI builds the artifact, box pulls).
  Until then, run `mode: notify` and promote to `auto` deliberately.
- **On-box build time** (~1–2 min Go + frontend) is the restart window. Agents
  survive it; the HTTP endpoint is down for it. Fine for a home deployment.
- **No `restart: unless-stopped` auto-heal** — replaced by systemd
  `Restart=on-failure` + `WantedBy=multi-user.target` (survives host reboot).
- **codex sandbox:** `SYBRA_DISABLE_CODEX_SANDBOX=1` is still required (LXC
  kernels disable unprivileged userns; the LXC is the sandbox).
