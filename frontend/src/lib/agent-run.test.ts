import { describe, it, expect } from 'vitest'
import { runStateClasses } from './agent-run.js'

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
