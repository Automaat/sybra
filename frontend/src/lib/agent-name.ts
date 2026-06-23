// Friendly display names for agents and interactive sessions, so the UI shows
// readable labels instead of opaque hashes or technical role prefixes.

interface NamedAgent {
  id: string
  name?: string
  project?: string
  taskId?: string
}

// Agents are launched as `<role>:<title>` (see internal/agent/role.go). When
// the title itself starts with a word like "Review", the raw name reads as a
// doubled "review:Review …" prefix — stripping the role prefix fixes that.
const ROLE_PREFIXES = new Set([
  'triage',
  'plan',
  'plan-critic',
  'eval',
  'pr-fix',
  'review',
  'fix-review',
  'test-plan',
  'test-plan-critic',
  'test-runner',
  'implementation',
  'human-review',
])

/**
 * Strip a leading technical role prefix (`review:`, `plan:`, …) from an agent
 * name so the human-readable title is shown on its own. Roles are lowercase
 * (matched case-sensitively); names without a known role prefix are untouched.
 */
export function cleanAgentName(name: string | undefined | null): string {
  if (!name) return ''
  const idx = name.indexOf(':')
  if (idx > 0 && ROLE_PREFIXES.has(name.slice(0, idx))) {
    return name.slice(idx + 1).trim()
  }
  return name.trim()
}

/**
 * Compact form of an opaque id for an ID chip / fallback label. Opaque ids
 * often share a long structured prefix (e.g. `ext-codex-<sid>`), so the
 * trailing, high-entropy portion is kept — truncating the front would collide.
 */
export function shortId(id: string): string {
  return id.length > 12 ? id.slice(-8) : id
}

/**
 * Best human-readable name for an agent/session: the linked task title, else
 * the cleaned session name, else the project, else the task id — never a bare
 * hash (an id-only fallback is labelled "Session <shortId>").
 */
export function agentDisplayName(a: NamedAgent, taskTitle?: string | null): string {
  if (taskTitle) return taskTitle
  const name = cleanAgentName(a.name)
  if (name) return name
  if (a.project) return a.project
  if (a.taskId) return a.taskId
  return `Session ${shortId(a.id)}`
}
