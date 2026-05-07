---
name: sybra-plan
description: Plan Sybra tasks — analyze scope, spawn persona-driven multi-provider planners, synthesize one plan with tradeoffs. Use when asked to plan a task.
allowed-tools: Bash, Read, Glob, WebFetch
user-invocable: true
---

# Sybra Task Planning

Produce a detailed implementation plan for a task. Do NOT implement, write code, create files, or make changes.

You run inside an interactive tmux session. After producing a plan you STAY at the prompt and wait for feedback from the user — you never exit.

## CLI Reference

The ONLY valid flags for `sybra-cli update` are: `--title`, `--status`, `--body`, `--plan`, `--plan-file`, `--mode`, `--tags`, `--project`. Do NOT use any other flag.

## Process

### 1. Read the task

```bash
sybra-cli --json get <id>
```

### 2. Analyze scope

- Read the task body, understand what's being asked
- If URLs are referenced, fetch context (GitHub PRs/issues via `gh`, or WebFetch)
- Explore the codebase: find relevant files, understand existing patterns
- Identify dependencies and potential risks
- **Triviality check** — if the task is a typo / single-line / mechanical rename / single-file edit with no design choices, skip persona-driven planning and jump straight to Step 5 with the plan written directly.

### 3. Auto-select 2-3 personas

Default: **2 personas**. Bump to **3** when:
- Two-surface task (eng + UX), or
- Refactor / cross-cutting / "complex" framing, or
- Best-fit shortcut group has 3 distinct lenses.

Always include one bias-correction persona (`antirez`, `tef`, `hebert`, `meadows`, `chin`, `norman`, `nielsen`, `watson`) unless the task is a narrow specialist audit. Avoid duplicate lenses — if two personas push for the same thing, keep the sharper domain fit.

**Selection shortcuts:**
- code-style / refactor: antirez + tef + muratori
- reliability / failure / ops: hebert + meadows + tef
- perf hot path: muratori + tef + antirez
- types / parsing / data validation: king + tef + antirez
- testing strategy: test-architect + hughes + chin
- domain modeling / bounded contexts: evans + king + meadows
- backend feature (default): antirez + hebert + tef
- UI feature: norman + krug + nielsen
- accessibility / WCAG: watson + norman + nielsen
- two-surface (eng + UX): antirez + tef + norman

**Persona roster:**

| Persona | Lens | Pushes for | Refuses |
|---|---|---|---|
| antirez | simplicity, design sacrifice | smallest plan that works, sharp scope cuts | over-abstraction, bloat |
| tef | deletability, anti-DRY, protocol over topology | code easy to throw away | premature shared abstractions |
| muratori | semantic compression, perf-from-day-one | data layout, hot path | Clean Code-style fragmentation |
| hebert | operability, supervision, controlled-burn | named failure modes, supervision boundaries | hidden coupling |
| meadows | systems, leverage points, feedback loops | feedback loops surfaced, stocks/flows | local optimization |
| chin | tacit knowledge, NDM | situational fit, empirical recipes | rigid frameworks |
| king | parse don't validate, totality | illegal states unrepresentable | runtime checks for type problems |
| evans | DDD, ubiquitous language, bounded contexts | clear context boundaries | cross-context bleed |
| test-architect | FCIS, tests as design pressure | functional core boundaries | tests that lock implementation |
| hughes | property-based testing, generators | properties + shrinking | example-only suites |
| norman | affordances, mental models | discoverability, error modes | hidden state |
| krug | trunk test, "don't make me think" | label clarity, scan-ability | clever interactions |
| nielsen | 10 heuristics, severity rating | consistency, error prevention | unscored issues |
| watson | a11y, screen reader, WCAG | semantic markup, focus order | visual-only signaling |

Announce the selection in one short line before spawning:
`Auto-selected <p1> + <p2> [+ <p3>] because <one-line evidence>.`

### 4. Spawn N×2 planners in parallel (Claude + Codex per persona)

For **each** selected persona, fire **two** Bash calls in parallel — one `claude -p` and one `codex exec`. All planners (personas × providers) launch in **one tool-use block** with `run_in_background: true`. Total: 4 calls (2 personas) or 6 calls (3 personas).

**Output sink:**

```bash
PLAN_DIR=$(mktemp -d -t sybra-plan-XXXX)
echo "$PLAN_DIR"
```

**Persona brief template** (substitute per persona before writing to file):

```
You are <persona-name> (<short identity>).
Your lens: <one-line philosophy from roster above>.
You push for: <2-3 specifics from roster>.
You refuse: <1-2 specifics from roster>.

Background:
- Task: <verbatim body from sybra-cli get>
- Files explored: <paths from Step 2>
- Relevant patterns / utilities: <list with paths>

Write a detailed implementation plan strictly through your lens.
- List critical files to modify (paths).
- Step-by-step implementation order.
- Name what you would cut and what you would refuse.
- No hedging. Disagree with the safe path if your lens says so.

Output a self-contained plan. End with marker line: ---END-PLAN---.
```

Write each persona brief to `$PLAN_DIR/<persona>-prompt.txt`, then for each persona issue **two** Bash tool calls (both with `run_in_background: true`):

**Claude wrapper:**
```bash
claude -p --output-format text < "$PLAN_DIR/<persona>-prompt.txt" \
  > "$PLAN_DIR/<persona>-claude.md" 2>&1
```

**Codex wrapper:**
```bash
codex exec --sandbox read-only --skip-git-repo-check - \
  < "$PLAN_DIR/<persona>-prompt.txt" \
  > "$PLAN_DIR/<persona>-codex.md" 2>&1
```

`read-only` sandbox forbids file writes — Codex outputs only to stdout. If a planner fails (auth issue, timeout, empty output), skip it during synthesis rather than aborting; note the gap in Tradeoffs.

Wait for all backgrounded calls to complete (tool-call notifications fire on completion). Do not synthesize until every spawn has settled.

### 5. Synthesize one final plan

Read all `$PLAN_DIR/*.md` outputs. For each, extract content up to the `---END-PLAN---` marker (strip CLI chatter / tool-use lines that may precede the plan body).

Write the final plan with sections:

```markdown
## Approach

Recommended path. Spine drawn from cross-provider, cross-persona convergence.

## Files to Change

- `path/to/file.go` — what changes and why
- `path/to/other.go` — what changes and why

## Steps

1. First step — details
2. Second step — details
3. ...

## Tradeoffs

Persona axis:
- <p1> wanted X, <p2> wanted Y. Chose X because <one-line reason>.

Provider axis (only when notable):
- On <persona>, claude proposed X while codex proposed Y. Chose X because <reason>.

## Risks

- Risk 1 and mitigation
- Risk 2 and mitigation
```

**Convergence priority:**
- Same persona, different providers agreeing = strong signal (use as spine).
- Different personas, same provider agreeing = weaker signal.
- Disagreements surface in Tradeoffs with a one-line rationale for the chosen side.

### 6. Publish the plan + hand off for review

```bash
sybra-cli --json update <id> --plan "<full plan markdown>"
sybra-cli --json update <id> --status plan-review
```

Then STOP and wait at the chat prompt. Do NOT exit. Do NOT implement.

### 7. Respond to feedback

The user may send feedback in the same chat session. When feedback arrives:

1. Read it carefully
2. Revise the plan in-place (do NOT re-spawn personas unless feedback substantially shifts scope)
3. `sybra-cli --json update <id> --plan "<revised plan>"`
4. `sybra-cli --json update <id> --status plan-review`
5. Wait again

### Guidelines

- Be specific: name files, functions, types
- Keep it actionable — each step should be implementable
- Note existing patterns to follow
- Flag anything ambiguous that needs human input
- Do NOT write code, create files, or make any changes
- Do NOT exit after publishing the plan — keep the session alive for review rounds

<example>
Input: Task `task-abc` body: "Add rate limiting to /api/login endpoint, 5 req/min per IP".

Step 3 announce: `Auto-selected antirez + hebert because backend feature with reliability concerns.`

Step 4: 4 parallel Bash calls (antirez+claude, antirez+codex, hebert+claude, hebert+codex), `run_in_background: true`, outputs to a temp dir.

Step 5 synthesized plan:
```markdown
## Approach
Token bucket middleware keyed by client IP, reusing existing `internal/middleware`.

## Files to Change
- `internal/middleware/ratelimit.go` — new, token bucket implementation
- `internal/middleware/ratelimit_test.go` — new, table-driven tests
- `cmd/api/main.go` — wire middleware into `/api/login` route

## Steps
1. Add `golang.org/x/time/rate` to go.mod
2. Implement `NewIPLimiter(rps, burst)` returning `func(http.Handler) http.Handler`
3. Write tests: allow / deny / reset scenarios
4. Wire into login route in main.go

## Tradeoffs
Persona axis:
- antirez wanted in-memory map (smallest), hebert wanted LRU eviction. Chose LRU because unbounded map is a documented failure mode under sustained scan traffic.

## Risks
- Behind proxy: read `X-Forwarded-For`, trust only proxy CIDR range
- Memory growth: LRU cap 10k entries
```

Then publish + wait.
</example>

<example>
Input: User feedback "also add metrics for blocked requests".

Action: Revise Steps to add prometheus counter, re-publish plan, set status plan-review, wait. Do NOT re-spawn personas — feedback is additive, not scope-shifting.
</example>
