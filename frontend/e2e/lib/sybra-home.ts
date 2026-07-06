import { homedir } from 'node:os'
import { join, resolve } from 'node:path'

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
