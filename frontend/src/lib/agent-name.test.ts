import { describe, it, expect } from 'vitest'
import { cleanAgentName, shortId, agentDisplayName } from './agent-name.js'

describe('cleanAgentName', () => {
  it('strips a role prefix, fixing the review:Review doubling', () => {
    expect(cleanAgentName('review:Review the PR #42')).toBe('Review the PR #42')
  })

  it('strips other known role prefixes', () => {
    expect(cleanAgentName('plan:Design auth')).toBe('Design auth')
    expect(cleanAgentName('triage:Investigate flaky test')).toBe('Investigate flaky test')
    expect(cleanAgentName('fix-review:Address feedback')).toBe('Address feedback')
  })

  it('leaves names without a known role prefix untouched', () => {
    expect(cleanAgentName('my-chat-session')).toBe('my-chat-session')
    expect(cleanAgentName('ext-12345')).toBe('ext-12345')
    // Human-typed "Review:" (capitalised) is not a technical role prefix.
    expect(cleanAgentName('Review: my notes')).toBe('Review: my notes')
  })

  it('returns empty for nullish', () => {
    expect(cleanAgentName(undefined)).toBe('')
    expect(cleanAgentName(null)).toBe('')
  })
})

describe('shortId', () => {
  it('keeps the trailing entropy of a long structured id (no prefix collision)', () => {
    // Two Codex sessions sharing the `ext-codex-` prefix stay distinct.
    expect(shortId('ext-codex-abc12345')).toBe('abc12345')
    expect(shortId('ext-codex-def67890')).toBe('def67890')
  })

  it('keeps short ids whole', () => {
    expect(shortId('ext-12')).toBe('ext-12')
    expect(shortId('agent-1')).toBe('agent-1')
  })
})

describe('agentDisplayName', () => {
  it('prefers the linked task title', () => {
    expect(agentDisplayName({ id: 'x', name: 'review:Fix the bug' }, 'Fix the bug')).toBe('Fix the bug')
  })

  it('falls back to the cleaned name', () => {
    expect(agentDisplayName({ id: 'x', name: 'review:Fix the bug' })).toBe('Fix the bug')
  })

  it('falls back to the project', () => {
    expect(agentDisplayName({ id: 'x', project: 'owner/repo' })).toBe('owner/repo')
  })

  it('falls back to the task id before a bare session label', () => {
    expect(agentDisplayName({ id: 'x', taskId: 'task-1' })).toBe('task-1')
  })

  it('labels a bare-id session instead of showing a raw hash', () => {
    expect(agentDisplayName({ id: 'ext-codex-9f8e7d6c' })).toBe('Session 9f8e7d6c')
  })
})
