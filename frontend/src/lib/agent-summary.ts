import type { agent } from '../../wailsjs/go/models.js'
import type { TimestampedStreamEvent } from './timeline.js'

const EDIT_TOOLS = new Set(['Edit', 'Write', 'MultiEdit'])

export interface AgentSummary {
  filesEdited: string[]
  commandsRun: number
  toolUseCount: number
  assistantMessageCount: number
  finalMessage: string
}

/**
 * Derive a human-readable summary from agent output buffers.
 * Pass stream events for headless agents, convo events for interactive agents.
 * Both slices can be populated simultaneously; results are merged.
 */
export function summarizeAgent(
  streamEvents: TimestampedStreamEvent[],
  convoEvents: agent.ConvoEvent[],
): AgentSummary {
  const filesEdited = new Set<string>()
  let commandsRun = 0
  let toolUseCount = 0
  let assistantMessageCount = 0
  let finalMessage = ''

  // Interactive mode: convo events carry structured tool use data.
  for (const ev of convoEvents) {
    if (ev.type === 'assistant') {
      assistantMessageCount++
      if (ev.text) finalMessage = ev.text
      for (const tu of ev.toolUses ?? []) {
        toolUseCount++
        if (EDIT_TOOLS.has(tu.name)) {
          const fp = tu.input?.file_path
          if (typeof fp === 'string' && fp) filesEdited.add(fp)
        } else if (tu.name === 'Bash') {
          commandsRun++
        }
      }
    } else if (ev.type === 'result' && ev.text) {
      finalMessage = ev.text
    }
  }

  // Headless mode: assistant events carry "[ToolName] arg" lines in content.
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
