// Extracts the "## Auto-review verdict[...]" section a human-review agent
// appends to a task body (see appendSection in internal/sybra/app_human_review.go)
// so the HumanRequiredPanel can surface the diagnosis inline instead of
// making the operator open the raw body.
export function extractAutoReviewVerdict(body: string): string | null {
  const lines = body.split('\n')
  const startIdx = lines.findIndex((line) => /^##\s+Auto-review verdict/i.test(line))
  if (startIdx === -1) return null

  const rest = lines.slice(startIdx + 1)
  const endIdx = rest.findIndex((line) => /^##\s+/.test(line))
  const sectionLines = endIdx === -1 ? rest : rest.slice(0, endIdx)

  const heading = lines[startIdx].replace(/^##\s+/, '').trim()
  const content = sectionLines.join('\n').trim()
  return content ? `${heading}\n\n${content}` : heading
}
