import type { TimestampedStreamEvent } from './timeline.js'

export interface BashActivity {
  id: string
  ts: Date
  command: string
  cwd?: string
  output: string
  isError: boolean
  status: 'done' | 'running'
}

/**
 * Extract Bash tool activity from a headless agent's output buffer by
 * parsing "[Bash] <command>" lines from StreamEvent content. These carry no
 * output text; output is left empty with status='done'.
 */
export function extractBashActivity(streamOutputs: TimestampedStreamEvent[]): BashActivity[] {
  const activities: BashActivity[] = []
  for (const tse of streamOutputs) {
    const ev = tse.event
    if (ev.type === 'assistant' && ev.content) {
      for (const line of ev.content.split('\n')) {
        const m = line.match(/^\[Bash\]\s+(.+)$/)
        if (m) {
          activities.push({
            id: `stream-${tse.receivedAt.getTime()}-${activities.length}`,
            ts: tse.receivedAt,
            command: m[1].trim(),
            output: '',
            isError: false,
            status: 'done',
          })
        }
      }
    }
  }
  return activities
}

/** Strip ANSI escape sequences for terminal-safe display in plain HTML. */
export function stripAnsi(text: string): string {
  // eslint-disable-next-line no-control-regex
  return text.replace(/\x1b\[[0-9;]*[mGKJHF]/g, '')
}

/** Truncate output to 100 KB and append a marker if truncated. */
export function truncateOutput(text: string, maxBytes = 100 * 1024): string {
  if (text.length <= maxBytes) return text
  return text.slice(0, maxBytes) + '\n…truncated'
}
