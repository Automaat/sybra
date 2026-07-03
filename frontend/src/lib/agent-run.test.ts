import { describe, it, expect } from 'vitest'
import { runStateClasses, runRoleLabel, runRoleClasses } from './agent-run.js'

describe('runStateClasses', () => {
  // The actual persisted terminal state for finished runs — must be neutral,
  // never the amber (primary) action colour that made it look like an alarm.
  it('renders the real "stopped" finished state as neutral grey, not amber', () => {
    const cls = runStateClasses('stopped')
    expect(cls).toMatch(/surface/)
    expect(cls).not.toMatch(/primary/)
  })

  it('only the live running state keeps the amber action colour', () => {
    expect(runStateClasses('running')).toMatch(/primary/)
  })

  it('maps explicit positive outcomes to muted green', () => {
    for (const s of ['completed', 'done', 'success']) {
      expect(runStateClasses(s)).toMatch(/success/)
      expect(runStateClasses(s)).not.toMatch(/primary/)
    }
  })

  it('maps failures to coral, never amber', () => {
    for (const s of ['failed', 'error']) {
      expect(runStateClasses(s)).toMatch(/error/)
      expect(runStateClasses(s)).not.toMatch(/primary/)
    }
  })

  it('falls back to neutral grey for unknown states', () => {
    expect(runStateClasses('mystery')).toMatch(/surface/)
    expect(runStateClasses('mystery')).not.toMatch(/primary/)
  })
})

describe('runRoleLabel', () => {
  it('maps the known pipeline roles to friendly labels', () => {
    expect(runRoleLabel('triage')).toBe('Triage')
    expect(runRoleLabel('plan')).toBe('Plan')
    expect(runRoleLabel('plan-critic')).toBe('Plan Critic')
    expect(runRoleLabel('implementation')).toBe('Implementation')
    expect(runRoleLabel('review')).toBe('Review')
    expect(runRoleLabel('fix-review')).toBe('Fix Review')
    expect(runRoleLabel('pr-fix')).toBe('PR Fix')
    expect(runRoleLabel('test-runner')).toBe('Test')
    expect(runRoleLabel('eval')).toBe('Eval')
    expect(runRoleLabel('human-review')).toBe('Human Review')
    expect(runRoleLabel('chat')).toBe('Chat')
  })

  // Empty/absent role is ambiguous (legacy impl runs and un-tagged runs both
  // look like "") — return '' so the caller renders no badge rather than guessing.
  it('returns empty string for an absent role so no badge renders', () => {
    expect(runRoleLabel('')).toBe('')
    expect(runRoleLabel(undefined)).toBe('')
    expect(runRoleLabel(null)).toBe('')
  })

  it('passes an unknown non-empty role through verbatim', () => {
    expect(runRoleLabel('custom-role')).toBe('custom-role')
  })
})

describe('runRoleClasses', () => {
  it('groups planning roles under tertiary', () => {
    for (const r of ['triage', 'plan', 'plan-critic']) {
      expect(runRoleClasses(r)).toMatch(/tertiary/)
    }
  })

  it('colours implementation primary and review/fix warning', () => {
    expect(runRoleClasses('implementation')).toMatch(/primary/)
    for (const r of ['review', 'fix-review', 'pr-fix']) {
      expect(runRoleClasses(r)).toMatch(/warning/)
    }
  })

  it('colours testing roles secondary and everything else neutral', () => {
    expect(runRoleClasses('test-runner')).toMatch(/secondary/)
    expect(runRoleClasses('eval')).toMatch(/surface/)
    expect(runRoleClasses(undefined)).toMatch(/surface/)
  })
})
