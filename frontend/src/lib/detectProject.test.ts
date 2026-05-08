import { describe, it, expect } from 'vitest'
import { detectProject } from './detectProject.js'
import type { project } from '../../wailsjs/go/models.js'

function makeProject(owner: string, repo: string, name = ''): project.Project {
  return {
    id: `${owner}/${repo}`,
    owner,
    repo,
    name: name || repo,
    url: `https://github.com/${owner}/${repo}`,
    clonePath: '',
    type: 'pet',
    setup: [],
    setupCommands: [],
    lastFetchAt: '',
    createdAt: '',
  } as project.Project
}

describe('detectProject', () => {
  it('returns null for empty input', () => {
    expect(detectProject('', [makeProject('org', 'repo')])).toBeNull()
  })

  it('returns null for empty projects list', () => {
    expect(detectProject('some text', [])).toBeNull()
  })

  it('matches GitHub URL', () => {
    const p = makeProject('myorg', 'myrepo')
    const result = detectProject('https://github.com/myorg/myrepo/pull/1', [p])
    expect(result).not.toBeNull()
    expect(result?.matchType).toBe('url')
    expect(result?.project).toBe(p)
  })

  it('matches GitHub URL without https', () => {
    const p = makeProject('myorg', 'myrepo')
    const result = detectProject('github.com/myorg/myrepo', [p])
    expect(result).not.toBeNull()
    expect(result?.matchType).toBe('url')
  })

  it('URL match is case-insensitive', () => {
    const p = makeProject('MyOrg', 'MyRepo')
    const result = detectProject('github.com/MYORG/MYREPO', [p])
    expect(result).not.toBeNull()
  })

  it('strips .git suffix from URL repo', () => {
    const p = makeProject('org', 'repo')
    const result = detectProject('github.com/org/repo.git', [p])
    expect(result).not.toBeNull()
  })

  it('matches repo by exact word', () => {
    const p = makeProject('org', 'myrepo')
    const result = detectProject('working on myrepo today', [p])
    expect(result).not.toBeNull()
    expect(result?.matchType).toBe('name')
    expect(result?.matchedText).toBe('myrepo')
  })

  it('matches by project name field', () => {
    const p = makeProject('org', 'repo', 'myproject')
    const result = detectProject('fix bug in myproject', [p])
    expect(result).not.toBeNull()
    expect(result?.matchedText).toBe('myproject')
  })

  it('returns matched text positions', () => {
    const p = makeProject('org', 'myrepo')
    const input = 'fix in myrepo now'
    const result = detectProject(input, [p])
    expect(result).not.toBeNull()
    expect(result?.matchStart).toBe(7)
    expect(result?.matchEnd).toBe(13)
  })

  it('URL match returns owner/repo portion as matchedText', () => {
    const p = makeProject('org', 'repo')
    const result = detectProject('https://github.com/org/repo/issues/5', [p])
    expect(result?.matchedText).toBe('org/repo')
  })

  it('returns null when no match', () => {
    const p = makeProject('org', 'repo')
    const result = detectProject('unrelated text here', [p])
    expect(result).toBeNull()
  })

  it('prefers URL match over name match', () => {
    const p1 = makeProject('org', 'repo')
    const p2 = makeProject('org2', 'repo2')
    const result = detectProject('github.com/org/repo repo2 text', [p1, p2])
    expect(result?.matchType).toBe('url')
    expect(result?.project).toBe(p1)
  })
})
