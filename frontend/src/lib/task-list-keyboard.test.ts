import { describe, it, expect } from 'vitest'
import {
  handleTaskListKeydown,
  type TaskListKeyboardCtx,
  type TaskListKeyboardEvent,
} from './task-list-keyboard.js'

function ev(p: Partial<TaskListKeyboardEvent> & { key: string }): TaskListKeyboardEvent {
  return {
    metaKey: false,
    ctrlKey: false,
    altKey: false,
    shiftKey: false,
    target: null,
    ...p,
  }
}

function ctx(o: Partial<TaskListKeyboardCtx> = {}): TaskListKeyboardCtx {
  return {
    viewMode: 'board',
    focusedColIdx: -1,
    focusedRowIdx: -1,
    focusedTaskId: null,
    allFilteredTasksLength: 0,
    columnTasksLengths: [0, 0, 0],
    anyPickerOpen: false,
    ...o,
  }
}

describe('handleTaskListKeydown — gating', () => {
  it('suppresses everything inside input', () => {
    const target = { tagName: 'INPUT' } as unknown as EventTarget
    const r = handleTaskListKeydown(ev({ key: 'j', target }), ctx())
    expect(r.action).toBeNull()
    expect(r.preventDefault).toBe(false)
  })

  it('suppresses everything inside textarea', () => {
    const target = { tagName: 'TEXTAREA' } as unknown as EventTarget
    const r = handleTaskListKeydown(ev({ key: 'j', target }), ctx())
    expect(r.action).toBeNull()
  })

  it('suppresses everything when picker open', () => {
    const r = handleTaskListKeydown(ev({ key: 'j' }), ctx({ anyPickerOpen: true }))
    expect(r.action).toBeNull()
  })
})

describe('handleTaskListKeydown — Cmd shortcuts', () => {
  it('Cmd+D on focused task opens due date', () => {
    const r = handleTaskListKeydown(ev({ key: 'd', metaKey: true }), ctx({ focusedTaskId: 't1' }))
    expect(r.action).toEqual({ type: 'open-due-date-focused' })
    expect(r.preventDefault).toBe(true)
  })

  it('Cmd+D without focus is a no-op', () => {
    const r = handleTaskListKeydown(ev({ key: 'd', metaKey: true }), ctx())
    expect(r.action).toBeNull()
  })

  it('other Cmd combos bail', () => {
    const r = handleTaskListKeydown(ev({ key: 'k', metaKey: true }), ctx())
    expect(r.action).toBeNull()
  })
})

describe('handleTaskListKeydown — search', () => {
  it('"/" dispatches focus-search', () => {
    const r = handleTaskListKeydown(ev({ key: '/' }), ctx())
    expect(r.action).toEqual({ type: 'focus-search' })
    expect(r.preventDefault).toBe(true)
  })

  it('Shift+F dispatches focus-search', () => {
    const r = handleTaskListKeydown(ev({ key: 'F', shiftKey: true }), ctx())
    expect(r.action).toEqual({ type: 'focus-search' })
  })
})

describe('handleTaskListKeydown — vertical nav (list view)', () => {
  it('j with no focus falls to focusFirst', () => {
    const r = handleTaskListKeydown(
      ev({ key: 'j' }),
      ctx({ viewMode: 'list', allFilteredTasksLength: 5 }),
    )
    expect(r.action).toEqual({ type: 'set-focus', colIdx: 0, rowIdx: 0, scroll: true })
  })

  it('j increments rowIdx clamped to length-1', () => {
    const r = handleTaskListKeydown(
      ev({ key: 'j' }),
      ctx({ viewMode: 'list', focusedColIdx: 0, focusedRowIdx: 1, allFilteredTasksLength: 3 }),
    )
    expect(r.action).toEqual({ type: 'set-focus', colIdx: 0, rowIdx: 2, scroll: true })
  })

  it('j at last row stays clamped', () => {
    const r = handleTaskListKeydown(
      ev({ key: 'j' }),
      ctx({ viewMode: 'list', focusedColIdx: 0, focusedRowIdx: 4, allFilteredTasksLength: 5 }),
    )
    expect(r.action).toEqual({ type: 'set-focus', colIdx: 0, rowIdx: 4, scroll: true })
  })

  it('k decrements rowIdx clamped to 0', () => {
    const r = handleTaskListKeydown(
      ev({ key: 'k' }),
      ctx({ viewMode: 'list', focusedColIdx: 0, focusedRowIdx: 0, allFilteredTasksLength: 5 }),
    )
    expect(r.action).toEqual({ type: 'set-focus', colIdx: 0, rowIdx: 0, scroll: true })
  })

  it('ArrowDown == j', () => {
    const r = handleTaskListKeydown(
      ev({ key: 'ArrowDown' }),
      ctx({ viewMode: 'list', focusedColIdx: 0, focusedRowIdx: 0, allFilteredTasksLength: 5 }),
    )
    expect(r.action).toEqual({ type: 'set-focus', colIdx: 0, rowIdx: 1, scroll: true })
  })
})

describe('handleTaskListKeydown — board nav', () => {
  it('j in board uses per-column length', () => {
    const r = handleTaskListKeydown(
      ev({ key: 'j' }),
      ctx({ viewMode: 'board', focusedColIdx: 1, focusedRowIdx: 0, columnTasksLengths: [2, 5, 3] }),
    )
    expect(r.action).toEqual({ type: 'set-focus', colIdx: 1, rowIdx: 1, scroll: true })
  })

  it('h jumps left to next non-empty column', () => {
    const r = handleTaskListKeydown(
      ev({ key: 'h' }),
      ctx({ viewMode: 'board', focusedColIdx: 3, focusedRowIdx: 1, columnTasksLengths: [4, 0, 0, 2] }),
    )
    expect(r.action).toEqual({ type: 'set-focus', colIdx: 0, rowIdx: 1, scroll: true })
  })

  it('h with no further non-empty col is no-op (preventDefault)', () => {
    const r = handleTaskListKeydown(
      ev({ key: 'h' }),
      ctx({ viewMode: 'board', focusedColIdx: 0, focusedRowIdx: 0, columnTasksLengths: [3, 0, 0] }),
    )
    expect(r.action).toBeNull()
    expect(r.preventDefault).toBe(true)
  })

  it('l jumps right to next non-empty column', () => {
    const r = handleTaskListKeydown(
      ev({ key: 'l' }),
      ctx({ viewMode: 'board', focusedColIdx: 0, focusedRowIdx: 2, columnTasksLengths: [3, 0, 1] }),
    )
    expect(r.action).toEqual({ type: 'set-focus', colIdx: 2, rowIdx: 0, scroll: true })
  })

  it('l clamps new row to shorter column', () => {
    const r = handleTaskListKeydown(
      ev({ key: 'l' }),
      ctx({ viewMode: 'board', focusedColIdx: 0, focusedRowIdx: 5, columnTasksLengths: [6, 2] }),
    )
    expect(r.action).toEqual({ type: 'set-focus', colIdx: 1, rowIdx: 1, scroll: true })
  })

  it('h/l in list view do nothing', () => {
    const r = handleTaskListKeydown(
      ev({ key: 'h' }),
      ctx({ viewMode: 'list', focusedColIdx: 0, focusedRowIdx: 0, allFilteredTasksLength: 5 }),
    )
    expect(r.action).toBeNull()
  })
})

describe('handleTaskListKeydown — timeline zoom', () => {
  it('+ zooms in', () => {
    const r = handleTaskListKeydown(ev({ key: '+' }), ctx({ viewMode: 'timeline' }))
    expect(r.action).toEqual({ type: 'timeline-zoom', dir: 'in' })
  })
  it('- zooms out', () => {
    const r = handleTaskListKeydown(ev({ key: '-' }), ctx({ viewMode: 'timeline' }))
    expect(r.action).toEqual({ type: 'timeline-zoom', dir: 'out' })
  })
  it('+/- only active in timeline view', () => {
    const r = handleTaskListKeydown(ev({ key: '+' }), ctx({ viewMode: 'list' }))
    expect(r.action).toBeNull()
  })
})

describe('handleTaskListKeydown — actions', () => {
  it('Enter on focused task selects', () => {
    const r = handleTaskListKeydown(ev({ key: 'Enter' }), ctx({ focusedTaskId: 't1' }))
    expect(r.action).toEqual({ type: 'select-focused' })
  })
  it('e on focused task selects', () => {
    const r = handleTaskListKeydown(ev({ key: 'e' }), ctx({ focusedTaskId: 't1' }))
    expect(r.action).toEqual({ type: 'select-focused' })
  })
  it('Enter without focus is no-op', () => {
    const r = handleTaskListKeydown(ev({ key: 'Enter' }), ctx())
    expect(r.action).toBeNull()
  })
  it('c triggers new task', () => {
    const r = handleTaskListKeydown(ev({ key: 'c' }), ctx())
    expect(r.action).toEqual({ type: 'new-task' })
  })
  it('Shift+C with focus opens assign-project picker', () => {
    const r = handleTaskListKeydown(ev({ key: 'C', shiftKey: true }), ctx({ focusedTaskId: 't1' }))
    expect(r.action).toEqual({ type: 'open-picker', kind: 'assign-project' })
  })
  it('s with focus opens status picker', () => {
    const r = handleTaskListKeydown(ev({ key: 's' }), ctx({ focusedTaskId: 't1' }))
    expect(r.action).toEqual({ type: 'open-picker', kind: 'status' })
  })
  it('p with focus opens priority picker', () => {
    const r = handleTaskListKeydown(ev({ key: 'p' }), ctx({ focusedTaskId: 't1' }))
    expect(r.action).toEqual({ type: 'open-picker', kind: 'priority' })
  })
  it('s/p without focus do nothing', () => {
    expect(handleTaskListKeydown(ev({ key: 's' }), ctx()).action).toBeNull()
    expect(handleTaskListKeydown(ev({ key: 'p' }), ctx()).action).toBeNull()
  })
  it('Escape clears focus', () => {
    const r = handleTaskListKeydown(ev({ key: 'Escape' }), ctx({ focusedColIdx: 0, focusedRowIdx: 0 }))
    expect(r.action).toEqual({ type: 'clear-focus' })
  })
})
