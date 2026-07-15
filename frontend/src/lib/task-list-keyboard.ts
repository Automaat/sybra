// Pure decision logic for the TaskList keyboard handler. TaskList.svelte owns
// state mutation (focusedColIdx/focusedRowIdx, pickers, dialogs) and
// side effects (window.dispatchEvent, scrollIntoView, navigation); this
// module decides *what* given the key event and a snapshot of focus state.

import type { ViewMode } from './view-mode.svelte.js'

export type TaskListKeyAction =
  | { type: 'set-focus'; colIdx: number; rowIdx: number; scroll: boolean }
  | { type: 'clear-focus' }
  | { type: 'select-focused' }
  | { type: 'open-due-date-focused' }
  | { type: 'focus-search' }
  | { type: 'open-picker'; kind: 'status' | 'priority' | 'assign-project' }
  | { type: 'new-task' }
  | { type: 'timeline-zoom'; dir: 'in' | 'out' }

export interface TaskListKeyboardCtx {
  viewMode: ViewMode
  focusedColIdx: number
  focusedRowIdx: number
  focusedTaskId: string | null
  // Length of `allFilteredTasks` (list + timeline views).
  allFilteredTasksLength: number
  // Length of each visible board column.
  columnTasksLengths: number[]
  // Any modal/picker open suppresses keyboard handling.
  anyPickerOpen: boolean
}

export interface TaskListKeyboardEvent {
  key: string
  metaKey: boolean
  ctrlKey: boolean
  altKey: boolean
  shiftKey: boolean
  target: EventTarget | null
}

export interface TaskListKeyboardResult {
  action: TaskListKeyAction | null
  preventDefault: boolean
}

const NOOP: TaskListKeyboardResult = { action: null, preventDefault: false }

function inEditable(target: EventTarget | null): boolean {
  if (target === null) return false
  const el = target as HTMLElement
  return el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.isContentEditable === true
}

function focusFirst(ctx: TaskListKeyboardCtx): TaskListKeyAction {
  if (ctx.viewMode === 'list' || ctx.viewMode === 'timeline') {
    if (ctx.allFilteredTasksLength > 0) return { type: 'set-focus', colIdx: 0, rowIdx: 0, scroll: true }
    return { type: 'clear-focus' }
  }
  for (let ci = 0; ci < ctx.columnTasksLengths.length; ci++) {
    if (ctx.columnTasksLengths[ci] > 0) return { type: 'set-focus', colIdx: ci, rowIdx: 0, scroll: true }
  }
  return { type: 'clear-focus' }
}

export function handleTaskListKeydown(
  e: TaskListKeyboardEvent,
  ctx: TaskListKeyboardCtx,
): TaskListKeyboardResult {
  if (inEditable(e.target)) return NOOP
  if (ctx.anyPickerOpen) return NOOP

  const key = e.key
  const isCmd = e.metaKey || e.ctrlKey

  if (isCmd && key === 'd' && ctx.focusedTaskId) {
    return { action: { type: 'open-due-date-focused' }, preventDefault: true }
  }
  if (isCmd) return NOOP
  if (e.altKey) return NOOP

  if (key === '/' || key === 'F') {
    return { action: { type: 'focus-search' }, preventDefault: true }
  }

  // Navigation: j/k always vertical; h/l only for board view.
  const isListLike = ctx.viewMode === 'list' || ctx.viewMode === 'timeline'

  if (key === 'j' || key === 'ArrowDown') {
    if (ctx.focusedColIdx < 0 || ctx.focusedRowIdx < 0) {
      return { action: focusFirst(ctx), preventDefault: true }
    }
    if (isListLike) {
      const next = Math.min(ctx.focusedRowIdx + 1, ctx.allFilteredTasksLength - 1)
      return { action: { type: 'set-focus', colIdx: ctx.focusedColIdx, rowIdx: next, scroll: true }, preventDefault: true }
    }
    const len = ctx.columnTasksLengths[ctx.focusedColIdx] ?? 0
    const next = Math.min(ctx.focusedRowIdx + 1, len - 1)
    return { action: { type: 'set-focus', colIdx: ctx.focusedColIdx, rowIdx: next, scroll: true }, preventDefault: true }
  }
  if (key === 'k' || key === 'ArrowUp') {
    if (ctx.focusedColIdx < 0 || ctx.focusedRowIdx < 0) {
      return { action: focusFirst(ctx), preventDefault: true }
    }
    const next = Math.max(ctx.focusedRowIdx - 1, 0)
    return { action: { type: 'set-focus', colIdx: ctx.focusedColIdx, rowIdx: next, scroll: true }, preventDefault: true }
  }

  if (!isListLike && (key === 'h' || key === 'ArrowLeft')) {
    if (ctx.focusedColIdx < 0) {
      return { action: focusFirst(ctx), preventDefault: true }
    }
    for (let ci = ctx.focusedColIdx - 1; ci >= 0; ci--) {
      const len = ctx.columnTasksLengths[ci] ?? 0
      if (len > 0) {
        const row = Math.min(ctx.focusedRowIdx, len - 1)
        return { action: { type: 'set-focus', colIdx: ci, rowIdx: row, scroll: true }, preventDefault: true }
      }
    }
    return { ...NOOP, preventDefault: true }
  }
  if (!isListLike && (key === 'l' || key === 'ArrowRight')) {
    if (ctx.focusedColIdx < 0) {
      return { action: focusFirst(ctx), preventDefault: true }
    }
    for (let ci = ctx.focusedColIdx + 1; ci < ctx.columnTasksLengths.length; ci++) {
      const len = ctx.columnTasksLengths[ci] ?? 0
      if (len > 0) {
        const row = Math.min(ctx.focusedRowIdx, len - 1)
        return { action: { type: 'set-focus', colIdx: ci, rowIdx: row, scroll: true }, preventDefault: true }
      }
    }
    return { ...NOOP, preventDefault: true }
  }

  if (ctx.viewMode === 'timeline') {
    if (key === '+' || key === '=') {
      return { action: { type: 'timeline-zoom', dir: 'in' }, preventDefault: true }
    }
    if (key === '-') {
      return { action: { type: 'timeline-zoom', dir: 'out' }, preventDefault: true }
    }
  }

  if (key === 'Enter' || key === 'e') {
    if (ctx.focusedTaskId) return { action: { type: 'select-focused' }, preventDefault: true }
    return NOOP
  }
  if (key === 'c' && !e.shiftKey) {
    return { action: { type: 'new-task' }, preventDefault: true }
  }
  if (key === 'C' && e.shiftKey) {
    if (ctx.focusedTaskId) return { action: { type: 'open-picker', kind: 'assign-project' }, preventDefault: true }
    return { ...NOOP, preventDefault: true }
  }
  if (key === 's' && ctx.focusedTaskId) {
    return { action: { type: 'open-picker', kind: 'status' }, preventDefault: true }
  }
  if (key === 'p' && ctx.focusedTaskId) {
    return { action: { type: 'open-picker', kind: 'priority' }, preventDefault: true }
  }
  if (key === 'Escape') {
    return { action: { type: 'clear-focus' }, preventDefault: false }
  }

  return NOOP
}
