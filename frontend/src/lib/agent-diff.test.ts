import { describe, it, expect, vi, beforeEach } from 'vitest'

const mockGetAgentDiff = vi.fn()

vi.mock('./api.js', () => ({
  GetAgentDiff: (...args: unknown[]) => mockGetAgentDiff(...args),
}))

const { parseUnifiedDiff, fetchAgentDiff, invalidateDiffCache } = await import('./agent-diff.js')

describe('parseUnifiedDiff', () => {
  it('returns empty array for empty input', () => {
    expect(parseUnifiedDiff('')).toEqual([])
    expect(parseUnifiedDiff('   ')).toEqual([])
  })

  it('parses a simple diff --git block', () => {
    const diff = [
      'diff --git a/src/foo.ts b/src/foo.ts',
      'index abc123..def456 100644',
      '--- a/src/foo.ts',
      '+++ b/src/foo.ts',
      '@@ -1,3 +1,4 @@',
      ' line one',
      '-old line',
      '+new line',
      '+added line',
    ].join('\n')

    const files = parseUnifiedDiff(diff)
    expect(files).toHaveLength(1)
    expect(files[0].path).toBe('src/foo.ts')
    expect(files[0].additions).toBe(2)
    expect(files[0].deletions).toBe(1)
    expect(files[0].isNew).toBe(false)
    expect(files[0].isDeleted).toBe(false)
  })

  it('detects new file from "new file mode" header', () => {
    const diff = [
      'diff --git a/new.ts b/new.ts',
      'new file mode 100644',
      '--- /dev/null',
      '+++ b/new.ts',
      '@@ -0,0 +1,2 @@',
      '+line 1',
      '+line 2',
    ].join('\n')

    const files = parseUnifiedDiff(diff)
    expect(files[0].isNew).toBe(true)
  })

  it('detects deleted file from "deleted file mode" header', () => {
    const diff = [
      'diff --git a/old.ts b/old.ts',
      'deleted file mode 100644',
      '--- a/old.ts',
      '+++ /dev/null',
      '@@ -1,2 +0,0 @@',
      '-line 1',
      '-line 2',
    ].join('\n')

    const files = parseUnifiedDiff(diff)
    expect(files[0].isDeleted).toBe(true)
  })

  it('detects binary file', () => {
    const diff = [
      'diff --git a/image.png b/image.png',
      'Binary files differ',
    ].join('\n')

    const files = parseUnifiedDiff(diff)
    expect(files[0].isBinary).toBe(true)
  })

  it('detects renamed file', () => {
    const diff = [
      'diff --git a/old.ts b/new.ts',
      'rename from old.ts',
      'rename to new.ts',
    ].join('\n')

    const files = parseUnifiedDiff(diff)
    expect(files[0].isRenamed).toBe(true)
    expect(files[0].oldPath).toBe('old.ts')
  })

  it('parses synthetic new-file block (--- /dev/null)', () => {
    const diff = [
      '--- /dev/null',
      '+++ b/untracked.ts',
      '@@ -0,0 +1,2 @@',
      '+first line',
      '+second line',
    ].join('\n')

    const files = parseUnifiedDiff(diff)
    expect(files).toHaveLength(1)
    expect(files[0].path).toBe('untracked.ts')
    expect(files[0].isNew).toBe(true)
    expect(files[0].additions).toBe(2)
  })

  it('parses multiple files from one diff', () => {
    const diff = [
      'diff --git a/a.ts b/a.ts',
      '@@ -1 +1 @@',
      '-old',
      '+new',
      'diff --git a/b.ts b/b.ts',
      '@@ -1 +1 @@',
      '-x',
      '+y',
    ].join('\n')

    const files = parseUnifiedDiff(diff)
    expect(files).toHaveLength(2)
    expect(files[0].path).toBe('a.ts')
    expect(files[1].path).toBe('b.ts')
  })

  it('correctly classifies hunk lines', () => {
    const diff = [
      'diff --git a/foo.ts b/foo.ts',
      '@@ -1,3 +1,3 @@',
      ' context line',
      '-deleted line',
      '+added line',
    ].join('\n')

    const files = parseUnifiedDiff(diff)
    const hunk = files[0].hunks[0]
    expect(hunk.lines).toHaveLength(3)
    expect(hunk.lines[0].type).toBe('ctx')
    expect(hunk.lines[1].type).toBe('del')
    expect(hunk.lines[2].type).toBe('add')
  })

  it('strips leading +/- from line content', () => {
    const diff = [
      'diff --git a/foo.ts b/foo.ts',
      '@@ -1 +1 @@',
      '-deleted content',
      '+added content',
    ].join('\n')

    const files = parseUnifiedDiff(diff)
    expect(files[0].hunks[0].lines[0].content).toBe('deleted content')
    expect(files[0].hunks[0].lines[1].content).toBe('added content')
  })

  it('handles hunk header correctly', () => {
    const header = '@@ -10,5 +10,6 @@ function foo()'
    const diff = [
      'diff --git a/foo.ts b/foo.ts',
      header,
      ' ctx',
    ].join('\n')

    const files = parseUnifiedDiff(diff)
    expect(files[0].hunks[0].header).toBe(header)
  })
})

describe('fetchAgentDiff', () => {
  beforeEach(() => {
    mockGetAgentDiff.mockReset()
    invalidateDiffCache()
  })

  it('fetches and parses diff from API', async () => {
    const diffText = [
      'diff --git a/foo.ts b/foo.ts',
      '@@ -1 +1 @@',
      '-old',
      '+new',
    ].join('\n')
    mockGetAgentDiff.mockResolvedValue(diffText)

    const result = await fetchAgentDiff('task-1', 'tool-1')

    expect(mockGetAgentDiff).toHaveBeenCalledWith('task-1')
    expect(result).toHaveLength(1)
    expect(result[0].path).toBe('foo.ts')
  })

  it('returns cached result for same taskId and toolUseId', async () => {
    mockGetAgentDiff.mockResolvedValue('diff --git a/f.ts b/f.ts\n')

    await fetchAgentDiff('task-1', 'tool-1')
    await fetchAgentDiff('task-1', 'tool-1')

    expect(mockGetAgentDiff).toHaveBeenCalledTimes(1)
  })

  it('re-fetches when toolUseId changes', async () => {
    mockGetAgentDiff.mockResolvedValue('diff --git a/f.ts b/f.ts\n')

    await fetchAgentDiff('task-1', 'tool-1')
    await fetchAgentDiff('task-1', 'tool-2')

    expect(mockGetAgentDiff).toHaveBeenCalledTimes(2)
  })

  it('handles null response from API', async () => {
    mockGetAgentDiff.mockResolvedValue(null)

    const result = await fetchAgentDiff('task-1', 'tool-1')
    expect(result).toEqual([])
  })
})

describe('invalidateDiffCache', () => {
  it('clears cache so next fetch goes to API', async () => {
    mockGetAgentDiff.mockResolvedValue('diff --git a/f.ts b/f.ts\n')

    await fetchAgentDiff('task-1', 'tool-1')
    invalidateDiffCache()
    await fetchAgentDiff('task-1', 'tool-1')

    expect(mockGetAgentDiff).toHaveBeenCalledTimes(2)
  })
})
