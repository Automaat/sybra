// Pure decision logic for global App-level keyboard shortcuts.
// App.svelte owns the side effects (preventDefault, window dispatchEvent,
// state mutation, G-chord timer); this module decides *what* to do given
// the key event and current context. Pure + sync so it can be unit-tested
// without mounting Svelte or dispatching real KeyboardEvents.

import type { Page } from './navigation.svelte.js'

export type AppKeyAction =
  | { type: 'open-quickadd' }
  | { type: 'open-palette' }
  | { type: 'open-help' }
  | { type: 'nav-reset'; page: Page }
  | { type: 'focus-search'; target: 'tasks' | 'renovate' }
  | { type: 'toggle-view' }
  | { type: 'toggle-sidebar' }

export interface AppKeyboardCtx {
  pendingG: boolean
  currentPageKind: Page['kind']
}

export interface AppKeyboardResult {
  action: AppKeyAction | null
  preventDefault: boolean
  // Next value for the caller's pendingG flag. Caller is responsible for
  // (re)arming or clearing its 1.5s G-chord timeout when this transitions.
  nextPendingG: boolean
}

// Subset of KeyboardEvent fields the handler actually reads. Lets tests pass
// a plain object literal instead of constructing a real event.
export interface AppKeyboardEvent {
  key: string
  metaKey: boolean
  ctrlKey: boolean
  altKey: boolean
  shiftKey: boolean
  target: EventTarget | null
}

const NOOP: AppKeyboardResult = { action: null, preventDefault: false, nextPendingG: false }

function inEditable(target: EventTarget | null): boolean {
  if (target === null) return false
  const el = target as HTMLElement
  return el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.isContentEditable === true
}

function metaNav(key: string): Page | null {
  switch (key) {
    case '1': return { kind: 'dashboard' }
    case '2': return { kind: 'task-list' }
    case '3': return { kind: 'project-list' }
    case '4': return { kind: 'agents' }
    case '5': return { kind: 'github' }
    case '6': return { kind: 'reviews' }
    case '7': return { kind: 'stats' }
    case ',': return { kind: 'settings' }
    default: return null
  }
}

function gChordTarget(key: string): Page | null {
  switch (key) {
    case 'i': return { kind: 'task-list' }
    case 'a': return { kind: 'task-list', filter: 'in-progress' }
    case 'p': return { kind: 'project-list' }
    case 's': return { kind: 'settings' }
    default: return null
  }
}

export function handleAppKeydown(e: AppKeyboardEvent, ctx: AppKeyboardCtx): AppKeyboardResult {
  const hasMod = e.metaKey || e.ctrlKey || e.altKey

  // A modifier press while a G-chord is pending cancels the chord.
  if (hasMod && ctx.pendingG) {
    // Fall through to handle the actual shortcut; just reset chord state.
    // Build the rest of the result below with nextPendingG = false.
  }

  // Cmd-shortcuts (use metaKey only; ctrl variants are not bound today).
  if (e.metaKey && !e.altKey) {
    if (e.key === 'n') return { action: { type: 'open-quickadd' }, preventDefault: true, nextPendingG: false }
    if (e.key === 'k') return { action: { type: 'open-palette' }, preventDefault: true, nextPendingG: false }
    if (e.key === '/') return { action: { type: 'open-help' }, preventDefault: true, nextPendingG: false }

    const navPage = metaNav(e.key)
    if (navPage !== null) {
      return { action: { type: 'nav-reset', page: navPage }, preventDefault: true, nextPendingG: false }
    }

    if (e.key === 'f') {
      const target = ctx.currentPageKind === 'github' ? 'renovate' : 'tasks'
      return { action: { type: 'focus-search', target }, preventDefault: true, nextPendingG: false }
    }
    if (e.key === 'b') {
      return { action: { type: 'toggle-view' }, preventDefault: true, nextPendingG: false }
    }
    if (e.key === 'i') {
      if (ctx.currentPageKind !== 'task-list') return { action: null, preventDefault: true, nextPendingG: false }
      return { action: { type: 'toggle-sidebar' }, preventDefault: true, nextPendingG: false }
    }
  }

  // Bare-key shortcuts only fire outside text inputs.
  if (!hasMod && inEditable(e.target)) {
    // Pending G-chord stays armed across input keystrokes — matches prior
    // behavior. Caller's timer is responsible for eventual expiry.
    return { ...NOOP, nextPendingG: ctx.pendingG }
  }

  if (!hasMod && e.shiftKey && e.key === '?') {
    return { action: { type: 'open-help' }, preventDefault: true, nextPendingG: ctx.pendingG }
  }

  if (!hasMod && !e.shiftKey) {
    if (ctx.pendingG) {
      const target = gChordTarget(e.key)
      if (target !== null) {
        return { action: { type: 'nav-reset', page: target }, preventDefault: true, nextPendingG: false }
      }
      // Unmapped letter while chord is pending: swallow silently, clear chord.
      return { action: null, preventDefault: false, nextPendingG: false }
    }
    if (e.key === 'g') {
      return { action: null, preventDefault: true, nextPendingG: true }
    }
  }

  return { ...NOOP, nextPendingG: ctx.pendingG && !hasMod }
}
