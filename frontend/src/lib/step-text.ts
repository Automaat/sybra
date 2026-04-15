import type { agent } from '../../wailsjs/go/models.js'

const MAX_LEN = 80

function truncate(s: string): string {
  return s.length > MAX_LEN ? s.slice(0, MAX_LEN) + '…' : s
}

/**
 * Extract a human-readable step description from a stream event.
 * Returns null when the event carries no useful step information.
 *
 * Claude headless: assistant events carry "[ToolName] description" lines.
 * Codex headless: tool_use events carry the shell command as content.
 */
export function extractStepText(event: agent.StreamEvent): string | null {
  // Codex: tool_use event, content is the shell command
  if (event.type === 'tool_use' && event.content) {
    const text = event.content.trim()
    return text ? truncate(text) : null
  }

  // Claude: assistant event, content is text + "[ToolName] desc" lines joined by \n
  if (event.type === 'assistant' && event.content) {
    const lines = event.content.split('\n')
    // Prefer the last tool-use line (starts with '[')
    for (let i = lines.length - 1; i >= 0; i--) {
      const line = lines[i].trim()
      if (line.startsWith('[')) {
        return truncate(line)
      }
    }
  }

  return null
}
