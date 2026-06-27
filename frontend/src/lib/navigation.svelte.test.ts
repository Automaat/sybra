import { describe, it, expect } from 'vitest'

const { navStore } = await import('./navigation.svelte.js')

describe('NavStore', () => {
  describe('navigate', () => {
    it('updates page on navigate', () => {
      navStore.reset({ kind: 'task-list' })
      navStore.navigate({ kind: 'settings' })
      expect(navStore.page.kind).toBe('settings')
    })

    it('pushes previous page to stack', () => {
      navStore.reset({ kind: 'task-list' })
      navStore.navigate({ kind: 'settings' })
      expect(navStore.stack).toHaveLength(1)
      expect(navStore.stack[0].kind).toBe('task-list')
    })

    it('does nothing when navigating to same page', () => {
      navStore.reset({ kind: 'settings' })
      navStore.navigate({ kind: 'settings' })
      expect(navStore.stack).toHaveLength(0)
    })

    it('task-detail same taskId is same page', () => {
      navStore.reset({ kind: 'task-detail', taskId: 'task-1' })
      navStore.navigate({ kind: 'task-detail', taskId: 'task-1' })
      expect(navStore.stack).toHaveLength(0)
    })

    it('task-detail different taskId is different page', () => {
      navStore.reset({ kind: 'task-detail', taskId: 'task-1' })
      navStore.navigate({ kind: 'task-detail', taskId: 'task-2' })
      expect(navStore.page.kind).toBe('task-detail')
      expect(navStore.stack).toHaveLength(1)
    })

    it('project-detail same projectId is same page', () => {
      navStore.reset({ kind: 'project-detail', projectId: 'org/repo' })
      navStore.navigate({ kind: 'project-detail', projectId: 'org/repo' })
      expect(navStore.stack).toHaveLength(0)
    })

    it('agent-detail same agentId is same page', () => {
      navStore.reset({ kind: 'agent-detail', agentId: 'a1' })
      navStore.navigate({ kind: 'agent-detail', agentId: 'a1' })
      expect(navStore.stack).toHaveLength(0)
    })

    it('agent-detail different agentId is different page', () => {
      navStore.reset({ kind: 'agent-detail', agentId: 'a1' })
      navStore.navigate({ kind: 'agent-detail', agentId: 'a2' })
      expect(navStore.stack).toHaveLength(1)
    })

    it('workflow-detail same workflowId is same page', () => {
      navStore.reset({ kind: 'workflow-detail', workflowId: 'wf-1' })
      navStore.navigate({ kind: 'workflow-detail', workflowId: 'wf-1' })
      expect(navStore.stack).toHaveLength(0)
    })

    it('task-list with different filter is different page', () => {
      navStore.reset({ kind: 'task-list' })
      navStore.navigate({ kind: 'task-list', filter: 'in-progress' })
      expect(navStore.stack).toHaveLength(1)
    })

    it('task-list with same filter is same page', () => {
      navStore.reset({ kind: 'task-list', filter: 'in-progress' })
      navStore.navigate({ kind: 'task-list', filter: 'in-progress' })
      expect(navStore.stack).toHaveLength(0)
    })
  })

  describe('back', () => {
    it('restores previous page', () => {
      navStore.reset({ kind: 'task-list' })
      navStore.navigate({ kind: 'settings' })
      navStore.back()
      expect(navStore.page.kind).toBe('task-list')
    })

    it('removes page from stack', () => {
      navStore.reset({ kind: 'task-list' })
      navStore.navigate({ kind: 'settings' })
      navStore.back()
      expect(navStore.stack).toHaveLength(0)
    })

    it('does nothing when stack is empty', () => {
      navStore.reset({ kind: 'task-list' })
      navStore.back()
      expect(navStore.page.kind).toBe('task-list')
    })
  })

  describe('canGoBack', () => {
    it('is false when stack is empty', () => {
      navStore.reset({ kind: 'task-list' })
      expect(navStore.canGoBack).toBe(false)
    })

    it('is true when stack has items', () => {
      navStore.reset({ kind: 'task-list' })
      navStore.navigate({ kind: 'settings' })
      expect(navStore.canGoBack).toBe(true)
    })
  })

  describe('replace', () => {
    it('updates page without touching stack', () => {
      navStore.reset({ kind: 'task-list' })
      navStore.navigate({ kind: 'settings' })
      navStore.replace({ kind: 'task-list' })
      expect(navStore.page.kind).toBe('task-list')
      expect(navStore.stack).toHaveLength(1)
    })
  })

  describe('reset', () => {
    it('clears stack and sets page', () => {
      navStore.reset({ kind: 'task-list' })
      navStore.navigate({ kind: 'settings' })
      navStore.navigate({ kind: 'task-list' })
      navStore.reset({ kind: 'reviews' })
      expect(navStore.page.kind).toBe('reviews')
      expect(navStore.stack).toHaveLength(0)
    })
  })

  describe('pageTitle', () => {
    it.each([
      [{ kind: 'task-list' }, 'Board'],
      [{ kind: 'task-detail', taskId: 't1' }, ''],
      [{ kind: 'project-list' }, 'Projects'],
      [{ kind: 'project-detail', projectId: 'p1' }, 'Project Detail'],
      [{ kind: 'chats' }, 'Chats'],
      [{ kind: 'chat-detail', agentId: 'a1' }, 'Chat'],
      [{ kind: 'agents' }, 'Agents'],
      [{ kind: 'agent-detail', agentId: 'a1' }, 'Agent Detail'],
      [{ kind: 'github' }, 'GitHub'],
      [{ kind: 'stats' }, 'Stats'],
      [{ kind: 'reviews' }, 'Reviews'],
      [{ kind: 'settings' }, 'Settings'],
      [{ kind: 'workflows' }, 'Workflows'],
      [{ kind: 'workflow-detail', workflowId: 'wf1' }, 'Workflow Editor'],
      [{ kind: 'logbook' }, 'Logbook'],
    ] as const)('pageTitle for %o is %s', (page: any, expected) => {
      navStore.reset(page)
      expect(navStore.pageTitle).toBe(expected)
    })
  })

  describe('activeTab', () => {
    it.each([
      [{ kind: 'task-list' }, 'board'],
      [{ kind: 'task-detail', taskId: 't1' }, 'board'],
      [{ kind: 'chats' }, 'chats'],
      [{ kind: 'chat-detail', agentId: 'a1' }, 'chats'],
      [{ kind: 'agents' }, 'agents'],
      [{ kind: 'agent-detail', agentId: 'a1' }, 'agents'],
      [{ kind: 'reviews' }, 'reviews'],
      [{ kind: 'settings' }, 'more'],
      [{ kind: 'stats' }, 'more'],
    ] as const)('activeTab for %o is %s', (page: any, expected) => {
      navStore.reset(page)
      expect(navStore.activeTab).toBe(expected)
    })
  })
})
