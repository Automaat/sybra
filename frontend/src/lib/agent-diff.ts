import { GetAgentDiff } from './api.js'

export interface DiffLine {
  type: 'add' | 'del' | 'ctx'
  content: string
}

export interface DiffHunk {
  header: string
  lines: DiffLine[]
}

export interface FileDiff {
  path: string
  isNew: boolean
  isDeleted: boolean
  isBinary: boolean
  isRenamed: boolean
  oldPath?: string
  additions: number
  deletions: number
  hunks: DiffHunk[]
}

export function parseUnifiedDiff(text: string): FileDiff[] {
  if (!text.trim()) return []
  const files: FileDiff[] = []
  const lines = text.split('\n')
  let i = 0

  while (i < lines.length) {
    const line = lines[i]

    if (line.startsWith('diff --git')) {
      const result = parseDiffBlock(lines, i)
      files.push(result.diff)
      i = result.nextIndex
      continue
    }

    // Synthetic new-file block from untracked ls-files section.
    if (line.startsWith('--- /dev/null') && i + 1 < lines.length && lines[i + 1].startsWith('+++ b/')) {
      const path = lines[i + 1].slice(6)
      const hunk: DiffHunk = { header: '', lines: [] }
      i += 2
      if (i < lines.length && lines[i].startsWith('@@')) {
        hunk.header = lines[i]
        i++
      }
      let additions = 0
      while (i < lines.length && !lines[i].startsWith('---') && !lines[i].startsWith('diff --git')) {
        const l = lines[i]
        if (l.startsWith('+')) {
          hunk.lines.push({ type: 'add', content: l.slice(1) })
          additions++
        } else if (l.startsWith(' ')) {
          hunk.lines.push({ type: 'ctx', content: l.slice(1) })
        }
        i++
      }
      files.push({
        path,
        isNew: true,
        isDeleted: false,
        isBinary: false,
        isRenamed: false,
        additions,
        deletions: 0,
        hunks: hunk.lines.length > 0 ? [hunk] : [],
      })
      continue
    }

    i++
  }

  return files
}

function parseDiffBlock(
  lines: string[],
  startIndex: number,
): { diff: FileDiff; nextIndex: number } {
  let i = startIndex + 1
  const headerLine = lines[startIndex]
  const pathMatch = headerLine.match(/^diff --git a\/(.*) b\/(.*)$/)
  const path = pathMatch ? pathMatch[2] : ''

  let isNew = false
  let isDeleted = false
  let isBinary = false
  let isRenamed = false
  let oldPath: string | undefined

  while (
    i < lines.length &&
    !lines[i].startsWith('@@') &&
    !lines[i].startsWith('diff --git')
  ) {
    const l = lines[i]
    if (l.startsWith('new file mode')) isNew = true
    else if (l.startsWith('deleted file mode')) isDeleted = true
    else if (l === 'Binary files differ' || l.includes('Binary files')) isBinary = true
    else if (l.startsWith('rename from ')) { oldPath = l.slice(12); isRenamed = true }
    i++
  }

  const hunks: DiffHunk[] = []
  let additions = 0
  let deletions = 0

  while (i < lines.length && !lines[i].startsWith('diff --git')) {
    if (lines[i].startsWith('@@')) {
      const result = parseHunk(lines, i)
      hunks.push(result.hunk)
      additions += result.additions
      deletions += result.deletions
      i = result.nextIndex
    } else {
      i++
    }
  }

  return {
    diff: { path, isNew, isDeleted, isBinary, isRenamed, oldPath, additions, deletions, hunks },
    nextIndex: i,
  }
}

function parseHunk(
  lines: string[],
  startIndex: number,
): { hunk: DiffHunk; additions: number; deletions: number; nextIndex: number } {
  const header = lines[startIndex]
  const hunkLines: DiffLine[] = []
  let i = startIndex + 1
  let additions = 0
  let deletions = 0

  while (
    i < lines.length &&
    !lines[i].startsWith('@@') &&
    !lines[i].startsWith('diff --git')
  ) {
    const l = lines[i]
    if (l.startsWith('+') && !l.startsWith('+++')) {
      hunkLines.push({ type: 'add', content: l.slice(1) })
      additions++
    } else if (l.startsWith('-') && !l.startsWith('---')) {
      hunkLines.push({ type: 'del', content: l.slice(1) })
      deletions++
    } else if (l.startsWith(' ')) {
      hunkLines.push({ type: 'ctx', content: l.slice(1) })
    }
    i++
  }

  return { hunk: { header, lines: hunkLines }, additions, deletions, nextIndex: i }
}

// Module-level diff cache — invalidated by edit/bash tool_use ID changes.
let _cache: { taskId: string; toolUseId: string; diff: FileDiff[] } | null = null

export async function fetchAgentDiff(taskId: string, latestToolUseId: string): Promise<FileDiff[]> {
  if (_cache?.taskId === taskId && _cache?.toolUseId === latestToolUseId) {
    return _cache.diff
  }
  const text = await GetAgentDiff(taskId)
  const diff = parseUnifiedDiff(text ?? '')
  _cache = { taskId, toolUseId: latestToolUseId, diff }
  return diff
}

export function invalidateDiffCache(): void {
  _cache = null
}
