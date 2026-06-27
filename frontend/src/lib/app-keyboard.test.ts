import { describe, it, expect } from 'vitest'
import { handleAppKeydown, type AppKeyboardCtx, type AppKeyboardEvent } from './app-keyboard.js'

function ev(partial: Partial<AppKeyboardEvent> & { key: string }): AppKeyboardEvent {
  return {
    metaKey: false,
    ctrlKey: false,
    altKey: false,
    shiftKey: false,
    target: null,
    ...partial,
  }
}

function ctx(overrides: Partial<AppKeyboardCtx> = {}): AppKeyboardCtx {
  return {
    pendingG: false,
    currentPageKind: 'task-list',
    ...overrides,
  }
}

describe('handleAppKeydown — Cmd shortcuts', () => {
  it.each([
    ['n', { type: 'open-quickadd' }],
    ['k', { type: 'open-palette' }],
    ['/', { type: 'open-help' }],
    ['b', { type: 'toggle-view' }],
  ] as const)('Cmd+%s -> %j', (key, expected) => {
    const r = handleAppKeydown(ev({ key, metaKey: true }), ctx())
    expect(r.preventDefault).toBe(true)
    expect(r.action).toEqual(expected)
    expect(r.nextPendingG).toBe(false)
  })

  it.each([
    ['1', 'task-list'],
    ['2', 'project-list'],
    ['3', 'agents'],
    ['4', 'github'],
    ['5', 'reviews'],
    ['6', 'stats'],
    [',', 'settings'],
  ] as const)('Cmd+%s navigates to %s', (key, kind) => {
    const r = handleAppKeydown(ev({ key, metaKey: true }), ctx())
    expect(r.preventDefault).toBe(true)
    expect(r.action).toEqual({ type: 'nav-reset', page: { kind } })
  })

  it('Cmd+F on github focuses renovate search', () => {
    const r = handleAppKeydown(ev({ key: 'f', metaKey: true }), ctx({ currentPageKind: 'github' }))
    expect(r.action).toEqual({ type: 'focus-search', target: 'renovate' })
  })

  it('Cmd+F off github focuses task search', () => {
    const r = handleAppKeydown(ev({ key: 'f', metaKey: true }), ctx({ currentPageKind: 'stats' }))
    expect(r.action).toEqual({ type: 'focus-search', target: 'tasks' })
  })

  it('Cmd+I on task-list toggles sidebar', () => {
    const r = handleAppKeydown(ev({ key: 'i', metaKey: true }), ctx({ currentPageKind: 'task-list' }))
    expect(r.action).toEqual({ type: 'toggle-sidebar' })
    expect(r.preventDefault).toBe(true)
  })

  it('Cmd+I off task-list is a no-op (preventDefault still claims the combo)', () => {
    const r = handleAppKeydown(ev({ key: 'i', metaKey: true }), ctx({ currentPageKind: 'stats' }))
    expect(r.action).toBeNull()
    expect(r.preventDefault).toBe(true)
  })
})

describe('handleAppKeydown — bare-key shortcuts', () => {
  it('Shift+? opens help outside inputs', () => {
    const r = handleAppKeydown(ev({ key: '?', shiftKey: true }), ctx())
    expect(r.action).toEqual({ type: 'open-help' })
    expect(r.preventDefault).toBe(true)
  })

  it('Shift+? inside an input is ignored', () => {
    const target = { tagName: 'INPUT', isContentEditable: false } as unknown as EventTarget
    const r = handleAppKeydown(ev({ key: '?', shiftKey: true, target }), ctx())
    expect(r.action).toBeNull()
    expect(r.preventDefault).toBe(false)
  })

  it('contenteditable target also blocks bare-key shortcuts', () => {
    const target = { tagName: 'DIV', isContentEditable: true } as unknown as EventTarget
    const r = handleAppKeydown(ev({ key: '?', shiftKey: true, target }), ctx())
    expect(r.action).toBeNull()
  })
})

describe('handleAppKeydown — G-chord', () => {
  it('pressing g arms the chord', () => {
    const r = handleAppKeydown(ev({ key: 'g' }), ctx())
    expect(r.nextPendingG).toBe(true)
    expect(r.preventDefault).toBe(true)
    expect(r.action).toBeNull()
  })

  it('g then i navigates to task-list', () => {
    const r = handleAppKeydown(ev({ key: 'i' }), ctx({ pendingG: true }))
    expect(r.action).toEqual({ type: 'nav-reset', page: { kind: 'task-list' } })
    expect(r.nextPendingG).toBe(false)
  })

  it('g then a navigates to filtered task-list', () => {
    const r = handleAppKeydown(ev({ key: 'a' }), ctx({ pendingG: true }))
    expect(r.action).toEqual({ type: 'nav-reset', page: { kind: 'task-list', filter: 'in-progress' } })
  })

  it('g then p navigates to project-list', () => {
    const r = handleAppKeydown(ev({ key: 'p' }), ctx({ pendingG: true }))
    expect(r.action).toEqual({ type: 'nav-reset', page: { kind: 'project-list' } })
  })

  it('g then s navigates to settings', () => {
    const r = handleAppKeydown(ev({ key: 's' }), ctx({ pendingG: true }))
    expect(r.action).toEqual({ type: 'nav-reset', page: { kind: 'settings' } })
  })

  it('g then unmapped letter swallows silently', () => {
    const r = handleAppKeydown(ev({ key: 'z' }), ctx({ pendingG: true }))
    expect(r.action).toBeNull()
    expect(r.nextPendingG).toBe(false)
  })

  it('modifier press while G pending clears the chord', () => {
    const r = handleAppKeydown(ev({ key: 'k', metaKey: true }), ctx({ pendingG: true }))
    expect(r.action).toEqual({ type: 'open-palette' })
    expect(r.nextPendingG).toBe(false)
  })
})

describe('handleAppKeydown — unhandled', () => {
  it('returns no-op for unmapped keys', () => {
    const r = handleAppKeydown(ev({ key: 'x' }), ctx())
    expect(r.action).toBeNull()
    expect(r.preventDefault).toBe(false)
    expect(r.nextPendingG).toBe(false)
  })

  it('Cmd+x is not a shortcut', () => {
    const r = handleAppKeydown(ev({ key: 'x', metaKey: true }), ctx())
    expect(r.action).toBeNull()
    expect(r.preventDefault).toBe(false)
  })
})
