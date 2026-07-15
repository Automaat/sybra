---
name: sybra-eval-report
description: Analyze the Sybra evaluation scorecard, breakdowns, and weaknesses, then recommend concrete improvements toward an autonomous mid-level engineering team. Use when asked how well/efficiently Sybra is working, to review the evaluation data, or to act on the fleet scorecard.
allowed-tools: Bash, Read
user-invocable: true
---

# Sybra Evaluation Analysis

The Go side already computes the fleet scorecard (autonomy, throughput,
reliability, efficiency), per-provider/role breakdowns, and a ranked list of
`weaknesses` with suggested actions. Your job is to **read those pre-computed
results, ground them in context, and produce a prioritized improvement plan** —
not to recompute the metrics. The north star: Sybra should ship like a team of
at least mid-level software engineers, autonomously.

## Step 1: Pull the scorecard

```bash
sybra-cli --json evaluation scan
```

On the server, run it inside the container:

```bash
ssh root@192.168.20.219 "docker exec sybra sybra-cli --json evaluation scan"
```

Returns an `evaluation.Report`:

- `overall` — the `Scorecard`:
  - throughput/outcomes: `tasksLanded`, `merged`, `closed`, `mergeRate`,
    `leadTimeP50H`/`leadTimeP90H` (created → landed),
    `cycleTimeP50H`/`cycleTimeP90H` (first agent run → landed)
  - autonomy: `autonomyRate`, `autonomousLandings`, `humanTouchedLandings`
  - reliability: `failureRate`, `agentRuns`, `agentFailures`, `ciFirstPassRate`
  - efficiency: `costPerLanded`, `totalCostUsd`, `turnsPerLanded`,
    `toolsPerLanded`, `reworkTasks`
  - `windowDays`
- `byProvider[]` / `byRole[]` — `Breakdown` per group: `key`, `runs`,
  `failures`, `failureRate`, `totalCostUsd`, `turns`, `tools`
- `weaknesses[]` — ranked (warn before info): `metric`, `severity`, `detail`,
  `suggestion`
- `notes[]` — metrics deliberately deferred (don't treat their absence as a
  measured zero)

## Step 2: Read confidence first

Landing-derived metrics (autonomy, lead/cycle time, cost-per-landed) only count
tasks that emitted a `task.landed` event — i.e. tasks that landed recently.

- If `overall.tasksLanded` is small (say < 5), treat autonomy / lead-time /
  cost-per-landed as **low-confidence** and say so. They fill in as more tasks
  land; don't over-read a 0% or a single-task ratio.
- `failureRate`, `totalCostUsd`, and the `byProvider`/`byRole` breakdowns are
  computed from all runs **within the configured window** (`window_days`), not
  just landed tasks — so they're trustworthy even when few tasks have landed,
  but they reflect the window, not all-time.

## Step 3: Interpret the scorecard against the north star

For a mid-level autonomous team, the bar is roughly:

- **autonomyRate** high (most work lands with no human touch) — the headline.
- **ciFirstPassRate** high (agents push code that passes CI first try).
- **failureRate** low; **reworkTasks** low (tasks converge, don't bounce).
- **costPerLanded / turnsPerLanded / toolsPerLanded** stable or trending down.
- **leadTime / cycleTime** percentiles not dominated by a long tail (p90).

Call out the 2–3 metrics furthest from that bar. Frame each as "what a
mid-level engineer would do that the fleet isn't."

## Step 4: Ground each weakness

`weaknesses[]` is the Go-computed feedback loop — already ranked with a
suggestion. For the top weaknesses (warn first):

- **Find the culprit** in the breakdowns. For a `failure_rate` or
  `role:<x>` / `provider:<y>` weakness, cite the offending `byRole`/`byProvider`
  row (its `failureRate` vs overall, `runs`, `totalCostUsd`).
- **Confirm with history** when useful: `sybra-cli --json audit --since 7d --summary`
  for cycle-time / plan-rejection / bottleneck context, or
  `sybra-cli --json health` for live findings.
- **Don't restate the suggestion verbatim** — sharpen it into a specific edit:
  name the prompt, skill, workflow step, model, or config knob to change, and
  the metric it should move.

## Step 5 (optional): Judge a specific landed change

To inspect the code quality of one landed task with the LLM rubric:

```bash
sybra-cli --json evaluation judge <task-id>
```

Returns per-dimension scores (correctness, code quality, scope discipline, test
coverage, review-worthiness), an `overall`, and whether it agrees with the
recorded outcome. Use this to make a code-quality weakness concrete with a real
example.

## Step 6: Produce the improvement plan

Output, in priority order (highest-impact first):

1. **One-line health verdict** — is the fleet at mid-level-autonomous bar? Which
   dimension is the binding constraint right now?
2. **Top 3 actions** — each: the weakness it addresses, the **specific** change
   (file/prompt/skill/workflow/config), the metric it should move, and a
   confidence note if the data is sparse.
3. **What to watch next window** — the one metric whose movement confirms the
   change worked.

## Guardrails

- Cite the Go-computed numbers; never invent metrics the report doesn't contain.
- Respect `notes[]` — those signals (e.g. merge-with-edits, change-failure rate)
  aren't measured yet; don't claim them.
- Keep recommendations grounded in this project's data, not generic advice.
- Don't run `evaluation golden` unless asked; it's a CI regression gate, not a
  reporting command.
