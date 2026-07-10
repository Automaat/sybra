import { homedir } from 'node:os'
import { access, readdir, unlink } from 'node:fs/promises'
import { join, resolve } from 'node:path'

/**
 * Marker file the e2e bootstrap writes into a disposable SYBRA_HOME. Its
 * presence is the positive proof that authorizes destructive cleanup — a real
 * operator board never contains it. Seeded by CI (`.github/workflows/*.yml`)
 * and `mise run test:e2e` alongside the fixtures.
 */
export const E2E_HOME_MARKER = '.sybra-e2e-home'

/**
 * Resolve the Sybra home the e2e suite may touch. The suite seeds fixtures
 * and deletes every non-fixture task file in its target home between tests,
 * so a fallback to the real `~/.sybra` destroys the operator's board — which
 * is exactly what happened on 2026-07-06 when an agent ran the suite without
 * `SYBRA_HOME` (#1576). Fail closed: an explicit, disposable home is
 * required, and the real default home is rejected even when set explicitly.
 */
export function isolatedSybraHome(): string {
  const env = process.env.SYBRA_HOME?.trim()
  if (!env) {
    throw new Error(
      'SYBRA_HOME is not set — refusing to run the e2e suite against a default home. ' +
        'It deletes task files in its target dir; point SYBRA_HOME at a disposable dir ' +
        '(CI uses /tmp/sybra-e2e).',
    )
  }
  if (resolve(env) === resolve(join(homedir(), '.sybra'))) {
    throw new Error(
      `SYBRA_HOME=${env} is the real operator home — refusing to run the e2e suite ` +
        'against it. Use a disposable dir (CI uses /tmp/sybra-e2e).',
    )
  }
  return env
}

// A fixtures-only home accumulates at most a handful of test-created files
// between cleanups; hundreds of strays means a real board (#1576). This cap is
// a secondary alarm only — the marker file below is what authorizes deletion.
const MAX_CLEANUP_STRAYS = 25

/**
 * Delete every non-fixture `*.md` file in a bootstrapped e2e home's tasks dir.
 *
 * Deletion is authorized by a positive disposable-home proof: the
 * {@link E2E_HOME_MARKER} file must exist in the home. A count heuristic alone
 * still wipes a *small* real board (#1576 did not require a large one), so the
 * marker — not the stray count — is the gate. The count cap is kept as a
 * secondary tripwire against a mis-seeded home.
 */
export async function cleanupStrayTasks(
  home: string,
  tasksDir: string,
  fixtureFiles: ReadonlySet<string>,
): Promise<void> {
  const marker = join(home, E2E_HOME_MARKER)
  try {
    await access(marker)
  } catch {
    throw new Error(
      `cleanupStrayTasks: ${marker} is missing — ${home} is not a bootstrapped ` +
        'e2e home, refusing to delete. Seed it (mise run test:e2e / CI) first.',
    )
  }

  const files = await readdir(tasksDir)
  const strays = files.filter((f) => !fixtureFiles.has(f) && f.endsWith('.md'))
  if (strays.length > MAX_CLEANUP_STRAYS) {
    throw new Error(
      `cleanupStrayTasks: ${strays.length} non-fixture task files in ${tasksDir} — ` +
        'not a disposable e2e home, refusing to delete',
    )
  }
  for (const f of strays) {
    await unlink(join(tasksDir, f))
  }
}
