# Leader-follower mode

Sybra can run as a single node (the default, `standalone`) or as a small cluster:
one **leader** that owns the canonical task store and does all the polling, plus
one or more **followers** that execute tasks assigned to them.

The unit of assignment is a **project**, not a task. Each follower declares the
project ids it `homes:`, and the leader routes every task for those projects to
that node. An operator can override the home for a single task (see
[Reassignment](#reassignment)).

```yaml
# leader
cluster:
  role: leader
  followers:
    - name: gpu-box
      endpoints: ["https://gpu-box.tailnet-1234.ts.net:8080"]
      auth_token_env: GPU_BOX_TOKEN
      trusted: true
      homes: ["Automaat/sybra", "Automaat/klaudiush"]
```

```yaml
# follower
cluster:
  role: follower
  bind_addr: ":8080"
```

A follower hard-disables every inbound poller (Todoist, GitHub issues, Renovate)
regardless of its own feature flags: the leader is the only ingress, so two nodes
can never race to claim the same upstream item.

Monitor ownership follows the same rule: the leader is the only node allowed to
remediate, dispatch monitor agents, or file issues. Followers may still run the
detector locally for observation, but that pass is read-only. Lost-agent recovery
for follower-homed tasks is decided by the leader and then routed back to the
assigned follower for execution.

## Transport tiers

The leader talks to a follower over the same HTTP control plane the web UI uses
(`POST /api/{service}/{method}`, bearer token). Pick **one** of these three
tiers. They are listed best-first; the leader's confidentiality guard treats the
first two as encrypted and the third as cleartext.

### 1. Tailscale (preferred)

```yaml
endpoints: ["https://gpu-box.tailnet-1234.ts.net:8080"]
```

WireGuard does the encryption and the identity; Sybra adds the bearer token on
top. Nothing is exposed to the LAN, there are no certificates to rotate, and
MagicDNS names survive an address change. A `.ts.net` endpoint counts as
encrypted even over plain `http://`, because the tailnet itself is the tunnel.

**Use this unless you have a specific reason not to.**

### 2. In-process TLS with a pinned certificate (LAN, no CA)

For a LAN with no Tailscale and no certificate authority. The follower terminates
TLS itself, and the leader trusts **exactly one certificate fingerprint** — not a
CA chain, not the system trust store.

Generate the keypair on the follower:

```bash
sybra-cli cluster gen-cert --host gpu-box.lan --host 192.168.20.9
```

It writes `follower.crt` / `follower.key` (key mode `0600`) and prints the config
for both sides, including the `tls_pin` the leader must use. The pin is the
SHA-256 of the certificate that was actually written, so it cannot drift from the
file on disk.

```yaml
# follower
cluster:
  role: follower
  bind_addr: ":8080"
  tls:
    cert_file: /data/sybra/tls/follower.crt
    key_file: /data/sybra/tls/follower.key
```

```yaml
# leader
followers:
  - name: gpu-box
    endpoints: ["https://gpu-box.lan:8080"]
    tls_pin: 8c98022c027068bb0a67f1781f0cd40d82436d80791da230944cebcca4b3c1d5
```

`gen-cert` overwrites an existing `follower.crt`/`follower.key` in the target
directory. Because the pin *is* the trust anchor, **regenerating the certificate
means updating `tls_pin` on the leader** — otherwise the leader will (correctly)
refuse to talk to the follower.

The generated leaf is not a CA and cannot sign other certificates: the pinned
path never builds a chain, so the certificate needs no authority beyond proving
it is the one the leader pinned.

### 3. Plain HTTP with a bearer token (pet projects only, trusted LAN)

```yaml
endpoints: ["http://192.168.20.9:8080"]
trusted: false
```

The token is protected only by the network. The confidentiality guard refuses to
route a **work-typed** project over this tier — see below. Do not use it for
anything you would not shout across the room.

> A raw CGNAT address (`100.64.0.0/10`) is **not** treated as encrypted. Tailscale
> assigns addresses from that range, but so can other things; only a `.ts.net`
> hostname or `https://` counts.

## Confidentiality

A project typed `work` is never routed to a follower unless that node is **both**
`trusted: true` **and** reachable only over an encrypted tier. This is enforced at
dispatch and again at manual reassignment — moving a task by hand is not a way
around it. A refusal blocks the task, records the reason, and emits a
`cluster.assign_blocked` audit event.

The classifier fails safe: a project whose type cannot be determined is treated as
work. See the Work-Data Confidentiality section of `CLAUDE.md`.

## Tokens

Each follower's bearer token is named, not inlined:

```yaml
followers:
  - name: gpu-box
    auth_token_env: GPU_BOX_TOKEN   # leader reads the token from this env var
```

On the follower, the same secret is `server.auth_token`, overridable with
`SYBRA_AUTH_TOKEN`. Keep it in the unit file's `EnvironmentFile=`, not in
`config.yaml`.

**Known limitation (tracked, not fixed):** the RPC allowlist is coarse — a leaked
follower token grants any allowlisted method on that follower, not a read-only
subset. Treat a follower token as equivalent to control of that node.

## Binding

```yaml
cluster:
  bind_addr: ":8080"                # all interfaces (tailnet + LAN)
  # or, to lock down which interfaces answer:
  bind_addrs:
    - "100.64.0.2:8080"             # tailnet only
    - "127.0.0.1:8080"              # local tooling
```

`bind_addrs` opens one listener per entry, all sharing a single handler. If any
address fails to bind the server does **not** start: a follower that came up on
only some of its interfaces would be silently unreachable from the leader on the
others.

Precedence is **`SYBRA_BIND_ADDR` > `bind_addrs` > `bind_addr` > `SYBRA_HOST`/
`SYBRA_PORT` > all interfaces on 8080.** A configured bind deliberately beats
`SYBRA_PORT`, because the shipped unit file and the Dockerfile both set it — if
env won, `bind_addrs` would be silently discarded on every supported deploy and
a control plane you locked to the tailnet would come up on the LAN as well.
`SYBRA_BIND_ADDR` is the escape hatch for rescuing a bad bind from the unit file
without editing `config.yaml`.

If `cluster.tls` names a `cert_file` but no `key_file` (or vice versa) the server
**refuses to start**. A half-configured block would otherwise come up in
cleartext while the leader still believed the `https://` endpoint was encrypted —
and the confidentiality guard would go on treating the node as safe for
work-typed projects.

When a configured bind discards a `SYBRA_PORT` from the unit file, startup logs
`server.bind.env_ignored` at WARN — so a surprising bind is visible without
turning on debug logging.

> If you lock the bind to a single interface, add `127.0.0.1:<port>` as a second
> entry when anything probes the node locally. A liveness check against
> `localhost` (the container `HEALTHCHECK`, a local `curl`) cannot reach a
> control plane bound only to a tailnet address. The systemd deploy is the
> supported path and does not health-probe over HTTP; the Dockerfile is CI/legacy.

## Reassignment

Per-project homing is static config. When a follower dies or degrades, move a task
off it:

```bash
sybra-cli cluster nodes
sybra-cli cluster reassign <task-id> --node gpu-box
sybra-cli cluster reassign <task-id> --node local    # bring it back to the leader
```

or use the **Node** dropdown on the task detail panel.

The new node re-provisions its own worktree from the branch — a worktree is not
transferable between machines, so the branch is the handoff artifact. If the old
node is still reachable its agents for that task are stopped first, so two agents
never drive one branch. A node that is genuinely dead is skipped and the move
proceeds anyway; if it later comes back, it cannot clobber the task, because the
mirror ignores any follower that is no longer the task's assigned node.

Reassignment records a `node_override` on the task, which beats the configured
home. Without it, the assigner would recompute the config home on its next pass
and drag the task straight back onto the node you just evacuated.

## Deployment

See `deploy/README.md`. The short version for a follower:

1. Install the toolchain (`mise`) and the provider CLIs for the `sybra` user.
2. `SYBRA_AUTH_TOKEN` in the unit's `EnvironmentFile`.
3. `cluster.role: follower` + `bind_addr` in `~/.sybra/config.yaml`.
4. For the TLS tier: `sybra-cli cluster gen-cert --host <name>`, point
   `cluster.tls` at the output, and put the printed `tls_pin` on the leader.
5. On the leader, add the follower to `cluster.followers` with its
   `auth_token_env` and the projects it `homes:`.

Startup logs one line per listener (`server.listen addr=... tls=... role=...`), so
you can confirm a node's posture at a glance.
