import type { agent } from '../../wailsjs/go/models.js'
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
 * Extract Bash tool activity from agent output buffers.
 *
 * Interactive mode: pairs ToolUseBlock (name=Bash) from assistant ConvoEvents
 * with matching ToolResultBlock by toolUseId. Unmatched tool_uses are
 * returned as status='running' (orphan — result not yet arrived).
 *
 * Headless mode: parses "[Bash] <command>" lines from StreamEvent content.
 * These carry no output text; output is left empty with status='done'.
 *
 * Mirrors the dual-mode pattern from agent-summary.ts.
 */
export function extractBashActivity(
  streamOutputs: TimestampedStreamEvent[],
  convoEvents: agent.ConvoEvent[],
): BashActivity[] {
  // Interactive mode: structured tool_use/tool_result pairs.
  if (convoEvents.length > 0) {
    const bashUses = new Map<string, { id: string; command: string; cwd?: string; ts: Date }>()

    for (const ev of convoEvents) {
      if (ev.type === 'assistant') {
        for (const tu of ev.toolUses ?? []) {
          if (tu.name === 'Bash') {
            bashUses.set(tu.id, {
              id: tu.id,
              command: String(tu.input?.command ?? ''),
              cwd: tu.input?.cwd ? String(tu.input.cwd) : undefined,
              ts: new Date(ev.timestamp),
            })
          }
        }
      }
    }

    const matched = new Map<string, BashActivity>()
    for (const ev of convoEvents) {
      for (const tr of ev.toolResults ?? []) {
        const use = bashUses.get(tr.toolUseId)
        if (use) {
          matched.set(use.id, {
            id: use.id,
            ts: use.ts,
            command: use.command,
            cwd: use.cwd,
            output: tr.content ?? '',
            isError: tr.isError ?? false,
            status: 'done',
          })
        }
      }
    }

    for (const [id, use] of bashUses) {
      if (!matched.has(id)) {
        matched.set(id, {
          id: use.id,
          ts: use.ts,
          command: use.command,
          cwd: use.cwd,
          output: '',
          isError: false,
          status: 'running',
        })
      }
    }

    return [...matched.values()].sort((a, b) => a.ts.getTime() - b.ts.getTime())
  }

  // Headless mode: parse "[Bash] <command>" lines from stream events.
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
