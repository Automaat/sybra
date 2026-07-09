// Navigation store: page state, history stack, derived title/tab/primary action.
// Replaces the sprawling `page = {...}` reassigns in App.svelte.

export type Page =
  | { kind: 'task-list'; filter?: 'in-progress' }
  | { kind: 'task-detail'; taskId: string }
  | { kind: 'project-list' }
  | { kind: 'project-detail'; projectId: string }
  | { kind: 'chats' }
  | { kind: 'chat-detail'; agentId: string }
  | { kind: 'agents'; tab?: string }
  | { kind: 'agent-detail'; agentId: string }
  | { kind: 'github' }
  | { kind: 'stats' }
  | { kind: 'evaluation' }
  | { kind: 'reviews' }
  | { kind: 'settings' }
  | { kind: 'workflows' }
  | { kind: 'workflow-detail'; workflowId: string }
  | { kind: 'logbook' }

export type TabKey = 'board' | 'chats' | 'agents' | 'reviews' | 'more'

export type PrimaryAction = {
  label: string
  run: () => void
} | undefined

class NavStore {
  page = $state<Page>({ kind: 'task-list' })
  stack = $state<Page[]>([])

  // Set by startUrlRouting() in web mode; gates history writes and the
  // back()/browser-history fallback. Desktop and unit tests that never call
  // startUrlRouting keep the plain in-memory stack behavior.
  private urlRoutingActive = false
  // True while handling a popstate event, so a reactive navigate()/replace()
  // triggered as a side effect of the resulting page change doesn't write a
  // redundant/looping history entry.
  private handlingPopstate = false

  navigate(p: Page) {
    if (samePage(this.page, p)) return
    this.stack = [...this.stack, this.page]
    this.page = p
    this.pushHistory(p)
  }

  replace(p: Page) {
    this.page = p
    this.replaceHistory(p)
  }

  back() {
    if (this.urlRoutingActive) {
      window.history.back()
      return
    }
    const prev = this.stack[this.stack.length - 1]
    if (!prev) return
    this.stack = this.stack.slice(0, -1)
    this.page = prev
  }

  reset(p: Page) {
    this.stack = []
    this.page = p
    this.pushHistory(p)
  }

  private pushHistory(p: Page) {
    if (!this.urlRoutingActive || this.handlingPopstate) return
    window.history.pushState(null, '', pageToPath(p))
  }

  private replaceHistory(p: Page) {
    if (!this.urlRoutingActive || this.handlingPopstate) return
    window.history.replaceState(null, '', pageToPath(p))
  }

  /**
   * Wires browser back/forward + deep links into this store. Only meant to be
   * called in web mode (guarded by the caller); returns a teardown function.
   */
  startUrlRouting(): () => void {
    if (typeof window === 'undefined') return () => {}

    this.urlRoutingActive = true
    const initial = pageFromLocation(window.location)
    this.page = initial
    this.stack = []
    // Normalize the URL (e.g. drop an unknown path) without creating a
    // history entry for the page the user already landed on.
    window.history.replaceState(null, '', pageToPath(initial))

    const onPopState = () => {
      this.handlingPopstate = true
      try {
        this.page = pageFromLocation(window.location)
      } finally {
        this.handlingPopstate = false
      }
    }
    window.addEventListener('popstate', onPopState)

    return () => {
      window.removeEventListener('popstate', onPopState)
      this.urlRoutingActive = false
    }
  }

  get canGoBack(): boolean {
    return this.stack.length > 0
  }

  get pageTitle(): string {
    const p = this.page
    switch (p.kind) {
      case 'task-list': return 'Board'
      // The detail page promotes the task title to its own heading (with a
      // "Back to tasks" chevron), so a generic "Task Detail" chrome heading is
      // pure redundancy — suppress it.
      case 'task-detail': return ''
      case 'project-list': return 'Projects'
      case 'project-detail': return 'Project Detail'
      case 'chats': return 'Chats'
      case 'chat-detail': return 'Chat'
      case 'agents': return 'Agents'
      case 'agent-detail': return 'Agent Detail'
      case 'github': return 'GitHub'
      case 'stats': return 'Stats'
      case 'evaluation': return 'Evaluation'
      case 'reviews': return 'Reviews'
      case 'settings': return 'Settings'
      case 'workflows': return 'Workflows'
      case 'workflow-detail': return 'Workflow Editor'
      case 'logbook': return 'Logbook'
    }
  }

  get activeTab(): TabKey {
    const p = this.page
    switch (p.kind) {
      case 'task-list':
      case 'task-detail':
        return 'board'
      case 'chats':
      case 'chat-detail':
        return 'chats'
      case 'agents':
      case 'agent-detail':
        return 'agents'
      case 'reviews':
        return 'reviews'
      default:
        return 'more'
    }
  }
}

function safeDecode(segment: string): string | null {
  try {
    const decoded = decodeURIComponent(segment)
    return decoded === '' ? null : decoded
  } catch {
    return null
  }
}

/** Serializes a Page to a stable, bookmarkable URL path (+ query when needed). */
export function pageToPath(p: Page): string {
  switch (p.kind) {
    case 'task-list':
      return p.filter ? `/tasks?filter=${encodeURIComponent(p.filter)}` : '/tasks'
    case 'task-detail':
      return `/tasks/${encodeURIComponent(p.taskId)}`
    case 'project-list':
      return '/projects'
    case 'project-detail':
      return `/projects/${encodeURIComponent(p.projectId)}`
    case 'chats':
      return '/chats'
    case 'chat-detail':
      return `/chats/${encodeURIComponent(p.agentId)}`
    case 'agents':
      return p.tab ? `/agents?tab=${encodeURIComponent(p.tab)}` : '/agents'
    case 'agent-detail':
      return `/agents/${encodeURIComponent(p.agentId)}`
    case 'github':
      return '/github'
    case 'stats':
      return '/stats'
    case 'evaluation':
      return '/evaluation'
    case 'reviews':
      return '/reviews'
    case 'settings':
      return '/settings'
    case 'workflows':
      return '/workflows'
    case 'workflow-detail':
      return `/workflows/${encodeURIComponent(p.workflowId)}`
    case 'logbook':
      return '/logbook'
  }
}

/**
 * Parses a browser location into a Page. Defensive by design: any unknown
 * root, malformed percent-encoding, or empty dynamic segment falls back to
 * the board rather than throwing or producing an invalid Page.
 */
export function pageFromLocation(location: { pathname: string; search: string }): Page {
  const segments = location.pathname.split('/').filter(Boolean)
  const params = new URLSearchParams(location.search)
  const board: Page = { kind: 'task-list' }
  if (segments.length === 0) return board

  const [root, rawId] = segments

  switch (root) {
    case 'tasks': {
      if (segments.length === 1) {
        const filter = params.get('filter')
        return filter === 'in-progress' ? { kind: 'task-list', filter } : board
      }
      const taskId = safeDecode(rawId)
      return taskId ? { kind: 'task-detail', taskId } : board
    }
    case 'projects': {
      if (segments.length === 1) return { kind: 'project-list' }
      const projectId = safeDecode(rawId)
      return projectId ? { kind: 'project-detail', projectId } : { kind: 'project-list' }
    }
    case 'chats': {
      if (segments.length === 1) return { kind: 'chats' }
      const agentId = safeDecode(rawId)
      return agentId ? { kind: 'chat-detail', agentId } : { kind: 'chats' }
    }
    case 'agents': {
      if (segments.length === 1) {
        const tab = params.get('tab')
        return tab ? { kind: 'agents', tab } : { kind: 'agents' }
      }
      const agentId = safeDecode(rawId)
      return agentId ? { kind: 'agent-detail', agentId } : { kind: 'agents' }
    }
    case 'workflows': {
      if (segments.length === 1) return { kind: 'workflows' }
      const workflowId = safeDecode(rawId)
      return workflowId ? { kind: 'workflow-detail', workflowId } : { kind: 'workflows' }
    }
    case 'github': return { kind: 'github' }
    case 'stats': return { kind: 'stats' }
    case 'evaluation': return { kind: 'evaluation' }
    case 'reviews': return { kind: 'reviews' }
    case 'settings': return { kind: 'settings' }
    case 'logbook': return { kind: 'logbook' }
    default:
      return board
  }
}

function samePage(a: Page, b: Page): boolean {
  if (a.kind !== b.kind) return false
  if (a.kind === 'task-list' && b.kind === 'task-list') return (a.filter ?? '') === (b.filter ?? '')
  if (a.kind === 'task-detail' && b.kind === 'task-detail') return a.taskId === b.taskId
  if (a.kind === 'project-detail' && b.kind === 'project-detail') return a.projectId === b.projectId
  if (a.kind === 'chat-detail' && b.kind === 'chat-detail') return a.agentId === b.agentId
  if (a.kind === 'agent-detail' && b.kind === 'agent-detail') return a.agentId === b.agentId
  if (a.kind === 'workflow-detail' && b.kind === 'workflow-detail') return a.workflowId === b.workflowId
  return true
}

export const navStore = new NavStore()
