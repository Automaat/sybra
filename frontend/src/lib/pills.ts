/**
 * Pill roles.
 *
 * A card (and the detail metadata row) stacks several pill-shaped chips. Before
 * roles, they were one widget differentiated only by colour — so colour carried
 * all the meaning and a passive label looked as loud as an action. Each role now
 * gets a distinct, *restrained* shape treatment, so the role reads before the
 * colour does and saturated colour is freed up for the one thing that needs it.
 *
 *  - status     the canonical state pill (see StatusBadge). The role supplies a
 *               rounded, padded shape; the caller layers the status colour/fill.
 *  - attention  the single "needs you" signal — shape from the role, colour
 *               layered by the caller.
 *  - tag        outlined, monochrome — user labels; the quietest chip.
 *  - reference  monochrome + icon — an upstream PR/issue reference.
 *  - project    dot + plain text, no pill chrome — a passive association.
 */
export type PillRole = 'status' | 'attention' | 'tag' | 'reference' | 'project'

const BASE = 'inline-flex items-center whitespace-nowrap text-xs'

/**
 * Per-role shape treatment. `status` and `attention` intentionally carry no
 * colour — the caller layers the status colour on — while `tag`/`reference`/
 * `project` bake in a monochrome treatment so they can never shout.
 */
export const PILL_ROLE_CLASS: Record<PillRole, string> = {
  status: `${BASE} rounded-full px-2.5 py-0.5 font-medium`,
  attention: `${BASE} gap-1 rounded-full px-2 py-0.5 font-semibold`,
  tag: `${BASE} gap-0.5 rounded-full border px-1.5 py-0.5 border-surface-300 text-surface-500 dark:border-surface-600 dark:text-surface-400`,
  reference: `${BASE} gap-1 rounded px-1.5 py-0.5 font-medium text-surface-600 dark:text-surface-300`,
  project: `${BASE} gap-1.5 text-surface-500 dark:text-surface-400`,
}

/** Tailwind classes for a pill of the given role, optionally extended. */
export function pillClass(role: PillRole, extra = ''): string {
  const base = PILL_ROLE_CLASS[role]
  return extra ? `${base} ${extra}` : base
}
