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
| `systemd/sybra.env.example` | `/etc/sybra/sybra.env` | Runtime env (listen port, local CLI server target, static dir, `PATH` with mise shims + npm globals). |
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

A follower hard-disables every poller (Todoist/GitHub/Renovate) regardless of its
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
