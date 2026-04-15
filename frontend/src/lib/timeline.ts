import type { agent } from '../../wailsjs/go/models.js'

export interface TimestampedStreamEvent {
  event: agent.StreamEvent
  receivedAt: Date
}

export interface TimelineEntry {
  index: number
  timestamp: Date
  type: string
  summary: string
}

const MAX_SUMMARY = 60

function trunc(s: string): string {
  return s.length > MAX_SUMMARY ? s.slice(0, MAX_SUMMARY) + '…' : s
}

function summarize(event: agent.StreamEvent): string {
  switch (event.type) {
    case 'init':
      return 'Session started'
    case 'assistant': {
      if (!event.content) return 'Assistant'
      const lines = event.content.split('\n')
      for (let i = lines.length - 1; i >= 0; i--) {
        const line = lines[i].trim()
        if (line.startsWith('[')) return trunc(line)
      }
      return trunc(lines[0].trim() || 'Assistant')
    }
    case 'tool_use':
      return event.content ? trunc(event.content.trim()) : 'Tool use'
    case 'tool_result':
      return event.content ? 'Result: ' + trunc(event.content.trim()) : 'Result'
    case 'result': {
      const cost = event.cost_usd ? ` — $${event.cost_usd.toFixed(2)}` : ''
      return `Done${cost}`
    }
    default:
      return event.type
  }
}

export function buildStreamTimeline(events: TimestampedStreamEvent[]): TimelineEntry[] {
  return events.map((e, i) => ({
    index: i,
    timestamp: e.receivedAt,
    type: e.event.type,
    summary: summarize(e.event),
  }))
}
