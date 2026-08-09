import { readFile } from 'node:fs/promises'
import { join } from 'node:path'
import { load as parseYAML } from 'js-yaml'

const API_BASE = 'http://localhost:8080'

/**
 * The bearer token the server running out of `sybraHome` uses.
 *
 * CI writes an explicit `server.auth_token` into the home's config, while a
 * local run leaves it empty and the server generates one into
 * `server_auth_token`. Both are checked so callers work either way.
 */
export async function authToken(sybraHome: string): Promise<string> {
  try {
    const cfg = parseYAML(
      await readFile(join(sybraHome, 'config.yaml'), 'utf8'),
    ) as { server?: { auth_token?: string } } | undefined
    const fromConfig = cfg?.server?.auth_token?.trim()
    if (fromConfig) {
      return fromConfig
    }
  } catch {
    /* no config file, fall through to the generated token */
  }
  return (await readFile(join(sybraHome, 'server_auth_token'), 'utf8')).trim()
}

/** POST one JSON-RPC-style call against a bound service method. */
export async function apiCall(
  sybraHome: string,
  service: string,
  method: string,
  args: unknown[],
): Promise<Response> {
  const res = await fetch(`${API_BASE}/api/${service}/${method}`, {
    method: 'POST',
    headers: {
      Authorization: `Bearer ${await authToken(sybraHome)}`,
      'Content-Type': 'application/json',
    },
    body: JSON.stringify(args),
  })
  if (!res.ok) {
    throw new Error(`${service}/${method} failed: ${res.status} ${await res.text()}`)
  }
  return res
}
