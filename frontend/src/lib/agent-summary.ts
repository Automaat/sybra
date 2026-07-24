import type { TimestampedStreamEvent } from './timeline.js'

const EDIT_TOOLS = new Set(['Edit', 'Write', 'MultiEdit'])

export interface AgentSummary {
  filesEdited: string[]
  commandsRun: number
  toolUseCount: number
  assistantMessageCount: number
  finalMessage: string
}

/** Derive a human-readable summary from a headless agent's output buffer. */
export function summarizeAgent(streamEvents: TimestampedStreamEvent[]): AgentSummary {
  const filesEdited = new Set<string>()
  let commandsRun = 0
  let toolUseCount = 0
  let assistantMessageCount = 0
  let finalMessage = ''

  // Assistant events carry "[ToolName] arg" lines in content.
  for (const tse of streamEvents) {
    const ev = tse.event
    if (ev.type === 'assistant') {
      assistantMessageCount++
      if (ev.content) {
        finalMessage = ev.content
        for (const line of ev.content.split('\n')) {
          const m = line.match(/^\[(\w+)\]\s+(.+)$/)
          if (m) {
            const name = m[1]
            const arg = m[2].trim()
            toolUseCount++
            if (EDIT_TOOLS.has(name) && arg) filesEdited.add(arg)
            else if (name === 'Bash') commandsRun++
          }
        }
      }
    } else if (ev.type === 'result' && ev.content) {
      finalMessage = ev.content
    }
  }

  return {
    filesEdited: [...filesEdited],
    commandsRun,
    toolUseCount,
    assistantMessageCount,
    finalMessage,
  }
}
