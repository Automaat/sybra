---
name: synapse-test-plan
description: Build a manual test plan for a Synapse task in the testing phase — spawns three distinct expert-tester personas (Bach, Kaner, Hendrickson) in parallel, each drafting test cases from their own frame, then merges and reviews before publishing. Covers Kubernetes and GUI app changes. Use whenever a task enters the testing status, when asked to "write a test plan", "plan manual tests", "figure out how to test X", or when reviewing a change that needs manual verification before merge. Prefer this over ad-hoc test ideas — the three-persona approach catches bugs any single frame would miss.
allowed-tools: Bash, Read, Agent
user-invocable: true
---

<!-- justify: I2 flat single-file convention matches sibling synapse skills and the runtime ~/.synapse/skills/ sync; extracting to references/ would break parity -->
<!-- justify: I17 side-effect signals appear inside domain checklists as example cases the human runs during manual testing, not commands the skill itself executes -->

# Synapse Test Plan

Produce a rigorous manual test plan for a task by spawning three expert-tester personas in parallel, merging their outputs, sanity-checking the merged result, and publishing. Plan manual verification only — do not execute tests and do not write automated tests.

Runs inside an interactive tmux session. After publishing, stay at the prompt and wait for feedback — never exit, so the human can send revisions in the same chat.

## When not to use

- **Trivial changes** — typo, pure internal refactor with no behavior change, docs-only. Write a one-paragraph smoke-test plan and publish directly; the three-persona flow is overkill.
- **No PR or diff available** — personas can't frame out of thin air. Ask the human for the change surface before spawning anything.
- **Automated-test territory** — unit tests, integration tests, contract tests. This skill plans *manual* verification only; use `test-writer` for automated coverage.
- **Executing tests** — this skill plans, it does not run. A separate agent or human runs the cases.

## Why three personas

Three distinct voices from the testing world each generate **different** test cases because they start from different mental models:

- **James Bach** (Rapid Software Testing) — heuristic bug-hunter. Generates attack vectors, boundary assaults, oracle violations.
- **Cem Kaner** (Context-Driven, psychologist + lawyer) — user-harm analyst. Generates cases around data integrity, legibility, trust, ambiguous states.
- **Elisabeth Hendrickson** ("Explore It!") — charter-driven state explorer. Generates cases around state machines, interrupted flows, concurrency, soak.

Parallel drafting (not parallel reviewing of a shared draft) preserves each persona's generative frame — reviewers of a blended draft only patch gaps inside its existing structure.

## CLI reference

Test plan lives in the task's `--plan` field (same mechanism as `synapse-plan`). Valid `synapse-cli update` flags: `--title`, `--status`, `--body`, `--plan`, `--plan-file`, `--plan-critique`, `--plan-critique-file`, `--mode`, `--tags`, `--project`.

Status flow: `testing` → (parallel drafts + merge + review) → `test-plan-review`.

## Process

### 1. Read the task and the change

```bash
synapse-cli --json get <id>
```

Read the task body, `plan` field, and any linked PR (`gh pr view <n> --json title,body,files`). Then read the changed files directly, because a plan written against a prose description misses real behavior — the code is the only reliable source of the actual surface area. Build a short "change under test" summary to hand to every persona.

### 2. Classify the domain

- **Kubernetes** — manifests, controllers/operators, Helm, probes, RBAC, networking, storage, CRDs. Use the [k8s checklist](#kubernetes-checklist).
- **GUI app** — Svelte/Wails frontend, desktop windows, forms, panels, event streams. Use the [GUI checklist](#gui-app-checklist).
- **Both / neither** — combine or skip the domain section; the persona frames still apply.

For trivial changes see the "When not to use" section at the top — produce a one-paragraph smoke plan and publish directly, because the three-persona overhead isn't justified for a one-line change.

### 3. Spawn three personas in parallel

Invoke three `Agent` tool calls in a **single assistant message** — sequential spawning leaks context between personas and erodes the frame diversity that makes this skill work. Use `subagent_type: general-purpose`. Each prompt contains:

1. The change under test (same for all three)
2. The relevant domain checklist (same for all three)
3. A persona-specific brief (different — see templates below)

Ask each persona for **5–8 test cases** in their voice, using the standard test-case format. Fewer than 5 means the persona skipped its own heuristics; more than 8 dilutes the merge.

**Shared scaffold (append to every persona brief):**

```
## Change under test
<paste change summary + file list + PR description>

## Domain checklist
<paste k8s or GUI checklist>

## Output format
<paste test case format>

## Task
Generate 5–8 test cases in your voice. Stay in your frame — cover only
what you see, not what other testers would cover.
```

**Bach brief** — Rapid Software Testing, heuristic bug-hunter:

```
You are James Bach. Software is an opponent; your job is to attack it
where it's weakest. Don't trust any claim until you've tried to falsify
it.

Toolkit:
- FEW HICCUPPS oracles (Familiar, Explainable, World, History, Image,
  Comparable products, Claims, User expectations, Product, Purpose,
  Statutes) — for each letter, find an inconsistency you can expose.
- SFDPOT coverage (Structure, Function, Data, Platform, Operations, Time).
- Goldilocks / Zero-One-Many — test 0, 1, 2, many, way-too-many, and
  exact boundaries.
- Error guessing from known bug patterns.

Prefer cases a developer would NOT think to write: interrupt operations,
feed garbage, run twice simultaneously, falsify product claims. Cite the
heuristic or oracle each case came from.
```

**Kaner brief** — Context-Driven, user-harm analyst:

```
You are Cem Kaner — psychologist, lawyer, testing academic. Quality is
value to some person who matters. A bug is a threat to that value. Find
defects AND understand who is harmed, how badly, and whether behavior
can be explained to a reasonable user (or a courtroom).

Lenses:
- Who is the user of this change? What do they believe will happen?
- Oracle: how does a tester know pass vs fail, and can they defend that
  judgment with evidence?
- CRUSSPIC STMPL quality criteria: Capability, Reliability, Usability,
  Security, Scalability, Performance, Installability, Compatibility,
  Supportability, Testability, Maintainability, Portability,
  Localizability — which apply?
- Data integrity and legibility — can the user still trust what they
  see? Could silent data loss or ambiguous state slip through?
- Error messages, audit trails, reversibility, consent.

Prefer cases a developer would dismiss as "UX issue" or "technically
correct data". State the oracle explicitly for each case: what the
tester observes and why it counts as pass or fail.
```

**Hendrickson brief** — Explore It!, state-based tours:

```
You are Elisabeth Hendrickson — author of "Explore It!" and pioneer of
charter-driven exploratory testing. Software is a state machine; most
bugs live at transitions nobody mapped. Test by taking tours — deliberate
walks through the state space with a mission.

Toolkit:
- State modeling: what states can this feature be in? Which transitions
  are untested?
- Tours: Feature Tour, Data Tour, Interruption Tour, Back Button Tour,
  Configuration Tour, Soak Tour.
- Charters: "Explore <area> with <resources> to discover <information>."
- Concurrency: same operation twice, second user mid-flow, backend
  replies out of order.
- Persistence: what survives a restart? what shouldn't?

Frame each case as a mini-charter or tour. Name the tour or state
transition each case exercises.
```

### 4. Merge the three drafts

Merge in the main context, not a subagent — a merge needs all three outputs held simultaneously to reason about overlap vs uniqueness.

1. **Collect** all cases (typically 15–24 total).
2. **Dedupe by test intent**, not wording. Same behavior + same oracle = one case; keep the clearer wording, attribute to the finder so the human reviewer sees provenance.
3. **Resolve conflicts.** If personas disagree on expected behavior, flag as open question — silently picking a winner hides a real design ambiguity.
4. **Preserve diversity.** Keep cases unique to one persona even if weird; weird cases are usually the valuable ones, and discarding them defeats three frames.
5. **Reorganize** into the plan template below, grouped by area not persona (the tester executes by area; persona origin is provenance, not structure).
6. **Prune** over ~15 cases by dropping weakest oracles / smallest blast radius. Keep at least 6 — below that, one persona's contribution is lost, breaking the three-frame guarantee.

### 5. Consolidated sanity-check review

Spawn **one** final review subagent on the merged plan (not the original drafts) to catch merge-introduced gaps and verify coherence.

```
Review a merged manual test plan. No prior context — judge on merits.

## The change under test
<paste change summary + file list>

## The merged plan
<paste merged plan>

## Your job
Answer these six questions. Cite test case numbers. Under 400 words. Blunt.

1. **Coverage gaps**: failure modes the plan misses. k8s: probes/RBAC/
   PDB/rollback/resources/graceful shutdown. GUI: keyboard-only, focus
   order, resize, error states, state persistence.
2. **Concreteness**: cases too vague to execute without guessing.
3. **Scope match**: gold-plating outside the PR surface, or cases that
   belong in automated tests instead.
4. **Oracle clarity**: cases where pass/fail is ambiguous.
5. **Risk ranking**: are the "Risk areas" the actual 3 riskiest spots?
6. **Verdict**: Approve / Refine / Reject. If Refine, top 3 fixes.
```

### 6. Act on the review

| Verdict | Action |
|---------|--------|
| Approve | Publish plan as-is |
| Refine | Apply the top-3 fixes, publish revised plan |
| Reject | Return to step 3 with persona briefs updated to address the gap. Cap at one re-run — publish after the second attempt regardless, because more iterations past that point usually indicate the plan is fine and the review is chasing perfection rather than catching real gaps. |

### 7. Publish and hand off

```bash
synapse-cli --json update <id> --plan "<final plan>"
synapse-cli --json update <id> --plan-critique "<review verdict + persona attribution summary>"
synapse-cli --json update <id> --status test-plan-review
```

After publishing, stay at the chat prompt. Do not execute tests and do not exit, because the human reviewer will send feedback in the same chat session and needs the agent alive to receive it.

### 8. Respond to human feedback

The human may push back. Revise and re-publish. Skip re-running personas for small tweaks — only re-run them if the change's scope fundamentally shifts, because persona re-runs are expensive and rarely add value for wording changes.

---

## Test case format

Every test case — whether produced by a persona or written into the merged plan — uses this format:

```markdown
### TC-<n>: <short actionable name>
**Setup:** <pre-conditions specific to this case>
**Steps:**
1. ...
2. ...
**Expected:** <what should happen>
**Oracle:** <how the tester knows pass vs fail — be explicit>
**Source:** <Bach | Kaner | Hendrickson | merge> — <which heuristic / tour / lens>
```

## Merged plan template

```markdown
## Scope
<one paragraph — what's being tested, link PR / files>

## Pre-conditions
- Environment: ...
- Version under test: ...
- Fixtures/accounts: ...

## Test cases
<grouped by area, TC-1 through TC-N>

## Risk areas
- <the 3 riskiest spots, with reasoning — this is where bugs most likely hide>

## Out of scope
- <explicit non-goals>

## Open questions
- <anything the plan can't answer without human input, including conflicts between personas>

## Persona attribution
- Bach contributed: TC-x, TC-y
- Kaner contributed: TC-z
- Hendrickson contributed: TC-a, TC-b
- Merged/shared: TC-c
```

---

## Kubernetes checklist

Share with every persona as context. Not every item applies to every change — personas decide relevance through their own frame.

- **Rollout & lifecycle**: fresh install; rolling update (zero downtime); rollback via `rollout undo` / `helm rollback`; failed rollout → `ProgressDeadlineExceeded`, no traffic to bad pods; mid-rollout pod eviction recovery.
- **Probes**: liveness catches deadlock; readiness drops unready pod from LB when dependency unavailable; startup probe covers slow boot; timeouts/thresholds have headroom under load.
- **Resources & scheduling**: requests/limits under load; OOM → clean restart + events; CPU throttling visible; node drain respects PDB; anti-affinity spreads pods.
- **RBAC & security**: ServiceAccount only has claimed perms (forbidden verb → 403); non-root + read-only FS where applicable; secrets not in logs/env dumps; NetworkPolicy allows/blocks correctly.
- **Data & state**: PVC persists across pod restart; concurrent writes don't corrupt; backup/restore exercised.
- **Chaos / failure**: force-terminate pod — recovery within SLO; network partition (Chaos Mesh); API server briefly unreachable — controller resumes; etcd latency spike.
- **Operator/controller**: reconcile idempotent (apply twice, no drift/thrash); delete CR → GC owned resources; CR mutation picked up within requeue; invalid spec → rejected or graceful, never crash-loop; controller restart mid-reconcile resumes cleanly.
- **Helm**: `helm lint`/`helm template` clean across profiles; install → upgrade → rollback all succeed; default values work; each override actually changes rendered manifest.
- **Observability**: expected metrics present, cardinality bounded; structured logs with correlation IDs; traces propagate.
- **Version/platform**: lowest + highest supported k8s version; arm64 + amd64 if claimed.

---

## GUI app checklist

Synapse frontend is Svelte 5 + Wails v2 in a desktop window. Share with every persona.

- **Visual & layout**: components render without overlap at default size; no typos/Lorem Ipsum/debug text; loading/empty/error states distinct; dark + light theme (Skeleton UI); consistent font sizes.
- **Responsiveness & resize**: resize to ~800×600 and very large; content reflows, no clipped text or unintended scrollbars; split panes snap to min/max.
- **Input & forms**: empty, max+1, pasted, unicode, emoji, RTL; validation errors inline + clear; required-field check; submit disabled while in-flight (no double-submit); Escape closes modals, Enter submits where expected.
- **Keyboard & focus** (WCAG 2.4.3 / 2.4.7): Tab reaches every interactive element in logical order; Shift+Tab reverses; focus ring visible; modals trap focus, restore to trigger on close.
- **State transitions**: happy path; interrupt mid-flow (no orphan state); state persists across restart where intended; panels stay in sync via Wails events.
- **Backend interactions**: Wails bound methods succeed/error/timeout (simulate via slow Go handler); event stream (`agent:output:<id>`) renders each event, no drops/dupes; backend returns empty/one/many/thousands; long-running calls keep UI responsive.
- **Errors & edge cases**: disconnect network mid-op; malformed data (degrade, don't crash); filesystem permission denied; task/project deleted while UI has it open.
- **Platform & accessibility**: macOS primary, spot-check Windows/Linux if claimed; system dark-mode toggle mid-session; VoiceOver announces controls meaningfully; 3:1 contrast on focus, 4.5:1 on text.

---

## Examples

<example>
**GUI change** — task `task-xyz`: "Add rollback button to task detail panel. Calls `rollbackTask(id)` Wails method, shows confirmation dialog."

Classify as GUI change. Read `TaskDetail.svelte` and `app.go`. Spawn three personas in parallel with GUI checklist + change summary.

**Bach returns:** double-click assault, click-then-escape-mid-dialog, Wails method returning an error the UI doesn't handle, rollback of a task with no prior revision, concurrent rollback + edit, keyboard-only activation.

**Kaner returns:** confirmation dialog wording is ambiguous about reversibility, audit trail check (can user see what was rolled back?), state after rollback should match state before edit — is the oracle provable?, error message legibility when backend fails, button visible to users without permission to rollback.

**Hendrickson returns:** Feature Tour (happy path), Interruption Tour (close window mid-dialog, then reopen), Back Button Tour (Escape cancels, focus returns to button), Data Tour (rollback on task with 0, 1, many prior states), Soak Tour (rollback repeatedly, any state leak?).

**Merge:** 18 → 12 cases after dedupe. Bach's "click-then-escape-mid-dialog" dupes Hendrickson's Interruption Tour — keep Hendrickson's framing. Kaner's "audit trail" is unique, keep. Keep all of Hendrickson's tours. Group into sections: Happy path, Keyboard & focus, Error handling, State persistence.

**Review:** sanity-check agent flags that TC-7 ("rollback on task with 0 prior states") doesn't specify expected behavior. Fix: mark button as disabled with tooltip when no prior state exists. Verdict: Refine → applied.

**Publish:**
```bash
synapse-cli --json update task-xyz --plan "<merged plan>"
synapse-cli --json update task-xyz --plan-critique "<review + attribution>"
synapse-cli --json update task-xyz --status test-plan-review
```
Stay at prompt.
</example>

<example>
**Kubernetes change** — task `task-k8s-42`: "Add PodDisruptionBudget to synapse-agent Helm chart with `minAvailable: 1`; bump chart version to 0.4.0."

Classify as Kubernetes change. Read `charts/synapse-agent/templates/pdb.yaml`, `values.yaml`, and `Chart.yaml`. Spawn three personas in parallel with k8s checklist.

**Bach returns:** drain a node with only 1 replica running (does drain block forever?), set `minAvailable` higher than replicas (`helm lint` should catch), upgrade from 0.3.x → 0.4.0 then rollback, PDB with 0 replicas at time of drain, concurrent drain of 2 nodes.

**Kaner returns:** rollback from 0.4.0 → 0.3.x — does the PDB resource get cleaned up, or is it orphaned (user trust in "rollback = previous state")?, what does the operator see when drain is blocked (is the error legible)?, PDB label selector audit (does it actually match pods, or does it silently match nothing?).

**Hendrickson returns:** Configuration Tour (every `values.yaml` combination that affects PDB), Interruption Tour (`helm upgrade` interrupted mid-apply), State Tour (PDB state across pod restart, node cordoning, and autoscaler scale-down), Soak Tour (leave cluster for 24h under rolling updates, PDB count never drifts).

**Merge:** 16 → 11 cases. Bach's "minAvailable > replicas" is a `helm lint` case — demote to smoke check, not a full TC. Kaner's orphan-resource-after-rollback is unique — keep. Group: Install/upgrade/rollback, Drain behavior, Label selector, Observability.

**Review:** Approve. Publish, stay at prompt.
</example>

<example>
**Trivial change — skip personas** — task `task-typo-7`: "Fix typo in task detail panel: 'Recieved' → 'Received'."

No personas needed. Publish a one-line smoke plan:

```markdown
## Scope
Verify the word "Received" renders correctly in the task detail panel header.

## Test cases
1. Open any task in the detail panel — confirm the header reads "Received" (not "Recieved"). No other behavior should change.
```

```bash
synapse-cli --json update task-typo-7 --plan "<smoke plan>"
synapse-cli --json update task-typo-7 --status test-plan-review
```
</example>

## Guardrails

- Plan manual verification only; automated tests belong in `test-writer`.
- Don't execute cases — this skill produces the plan, a human runs it.
- Stay at the prompt after publishing, so the human can send feedback.
- Spawn personas in **parallel** (single message, three `Agent` calls). Sequential spawning leaks context and erodes frame diversity.
- Skip personas for trivial changes (docs, typos, pure refactor) — publish a one-paragraph smoke plan instead.
- Surface persona disagreements in Open questions; silently picking a winner hides real design ambiguity.
- If the change is unidentifiable (no PR, no diff, vague body), ask the human before spawning personas.
