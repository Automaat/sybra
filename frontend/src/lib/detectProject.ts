import type { project } from '../../wailsjs/go/models.js'

export interface DetectionResult {
  project: project.Project
  matchType: 'url' | 'name'
  matchedText: string
  matchStart: number
  matchEnd: number
}

const githubURLRe = /(?:https?:\/\/)?github\.com\/(([^/\s]+)\/([^/\s#?]+))/gi

export function detectProject(
  input: string,
  projects: project.Project[],
): DetectionResult | null {
  if (!input || projects.length === 0) return null

  // 1. URL match (highest priority)
  for (const m of input.matchAll(githubURLRe)) {
    const owner = m[2].toLowerCase()
    const repo = m[3].replace(/\.git$/, '').toLowerCase()
    const found = projects.find(
      (p) => p.owner.toLowerCase() === owner && p.repo.toLowerCase() === repo,
    )
    if (found) {
      // Highlight only the owner/repo portion (group 1), not the full URL
      const ownerRepoStart = m.index! + m[0].indexOf(m[1])
      return {
        project: found,
        matchType: 'url',
        matchedText: m[1],
        matchStart: ownerRepoStart,
        matchEnd: ownerRepoStart + m[1].length,
      }
    }
  }

  // 2. Exact word match on repo name
  const words = input.split(/[\s,;:!?()[\]{}]+/).filter(Boolean)
  for (const word of words) {
    const w = word.toLowerCase()
    const found = projects.find((p) => p.repo.toLowerCase() === w || p.name.toLowerCase() === w)
    if (found) {
      const idx = input.indexOf(word)
      return {
        project: found,
        matchType: 'name',
        matchedText: word,
        matchStart: idx,
        matchEnd: idx + word.length,
      }
    }
  }

  return null
}
