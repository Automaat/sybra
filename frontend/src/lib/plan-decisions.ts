export interface PlanDecisionOption {
  label: string
  description: string
}

export interface PlanDecision {
  id: string
  title: string
  question: string
  recommended: string
  options: PlanDecisionOption[]
}

export interface ParsedPlanDecisions {
  decisions: PlanDecision[]
  hasOpenDecisions: boolean
}

export function parsePlanDecisions(markdown: string | undefined | null): ParsedPlanDecisions {
  const text = markdown?.trim() ?? ''
  if (!text || /no open decisions/i.test(text)) {
    return { decisions: [], hasOpenDecisions: false }
  }

  const headingRe = /^##\s+(.+)$/gm
  const headings = [...text.matchAll(headingRe)]
  const decisions: PlanDecision[] = []

  for (let i = 0; i < headings.length; i += 1) {
    const match = headings[i]
    const title = match[1]?.trim() ?? ''
    const start = (match.index ?? 0) + match[0].length
    const end = i + 1 < headings.length ? headings[i + 1].index ?? text.length : text.length
    const body = text.slice(start, end).trim()
    const question = lineValue(body, 'Question') || title
    const recommended = lineValue(body, 'Recommended')
    const options = parseOptions(body)
    if (title && options.length > 0) {
      decisions.push({
        id: slugifyDecision(title),
        title,
        question,
        recommended,
        options,
      })
    }
  }

  return { decisions, hasOpenDecisions: decisions.length > 0 }
}

function lineValue(body: string, label: string): string {
  const re = new RegExp(`^${label}:\\s*(.+)$`, 'im')
  return body.match(re)?.[1]?.trim() ?? ''
}

function parseOptions(body: string): PlanDecisionOption[] {
  const lines = body.split('\n')
  const start = lines.findIndex(line => /^Options:\s*$/i.test(line.trim()))
  if (start < 0) return []

  const out: PlanDecisionOption[] = []
  for (const raw of lines.slice(start + 1)) {
    const line = raw.trim()
    if (!line) continue
    if (line.startsWith('#') || /^[A-Za-z]+:\s+/.test(line)) break
    const bullet = line.match(/^[-*]\s+(.+)$/)?.[1]
    if (!bullet) continue
    const [label, description = ''] = splitOption(bullet)
    if (label) out.push({ label, description })
  }
  return out
}

function splitOption(text: string): [string, string] {
  for (const sep of [` ${String.fromCharCode(0x2014)} `, ' - ', ': ']) {
    const idx = text.indexOf(sep)
    if (idx > 0) return [text.slice(0, idx).trim(), text.slice(idx + sep.length).trim()]
  }
  return [text.trim(), '']
}

function slugifyDecision(title: string): string {
  return title.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '') || 'decision'
}
