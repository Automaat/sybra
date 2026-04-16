// Navigation store: page state, history stack, derived title/tab/primary action.
// Replaces the sprawling `page = {...}` reassigns in App.svelte.

export type Page =
  | { kind: 'dashboard' }
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
  | { kind: 'reviews' }
  | { kind: 'settings' }
  | { kind: 'workflows' }
  | { kind: 'workflow-detail'; workflowId: string }

export type TabKey = 'board' | 'chats' | 'agents' | 'reviews' | 'more'

export type PrimaryAction = {
  label: string
  run: () => void
} | undefined

class NavStore {
  page = $state<Page>({ kind: 'task-list' })
  stack = $state<Page[]>([])

  navigate(p: Page) {
    if (samePage(this.page, p)) return
    this.stack = [...this.stack, this.page]
    this.page = p
  }

  replace(p: Page) {
    this.page = p
  }

  back() {
    const prev = this.stack[this.stack.length - 1]
    if (!prev) return
    this.stack = this.stack.slice(0, -1)
    this.page = prev
  }

  reset(p: Page) {
    this.stack = []
    this.page = p
  }

  get canGoBack(): boolean {
    return this.stack.length > 0
  }

  get pageTitle(): string {
    const p = this.page
    switch (p.kind) {
      case 'dashboard': return 'Dashboard'
      case 'task-list': return 'Tasks'
      case 'task-detail': return 'Task Detail'
      case 'project-list': return 'Projects'
      case 'project-detail': return 'Project Detail'
      case 'chats': return 'Chats'
      case 'chat-detail': return 'Chat'
      case 'agents': return 'Agents'
      case 'agent-detail': return 'Agent Detail'
      case 'github': return 'GitHub'
      case 'stats': return 'Stats'
      case 'reviews': return 'Reviews'
      case 'settings': return 'Settings'
      case 'workflows': return 'Workflows'
      case 'workflow-detail': return 'Workflow Editor'
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
