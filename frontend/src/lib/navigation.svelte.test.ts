import { describe, it, expect, afterEach } from 'vitest'

const { navStore, pageToPath, pageFromLocation } = await import('./navigation.svelte.js')

function loc(pathname: string, search = ''): { pathname: string; search: string } {
  return { pathname, search }
}

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

describe('pageToPath', () => {
  it.each([
    [{ kind: 'task-list' }, '/tasks'],
    [{ kind: 'task-list', filter: 'in-progress' }, '/tasks?filter=in-progress'],
    [{ kind: 'task-detail', taskId: 't1' }, '/tasks/t1'],
    [{ kind: 'project-list' }, '/projects'],
    [{ kind: 'project-detail', projectId: 'org/repo' }, '/projects/org%2Frepo'],
    [{ kind: 'chats' }, '/chats'],
    [{ kind: 'chat-detail', agentId: 'a1' }, '/chats/a1'],
    [{ kind: 'agents' }, '/agents'],
    [{ kind: 'agents', tab: 'loop' }, '/agents?tab=loop'],
    [{ kind: 'agent-detail', agentId: 'a1' }, '/agents/a1'],
    [{ kind: 'github' }, '/github'],
    [{ kind: 'stats' }, '/stats'],
    [{ kind: 'evaluation' }, '/evaluation'],
    [{ kind: 'reviews' }, '/reviews'],
    [{ kind: 'settings' }, '/settings'],
    [{ kind: 'workflows' }, '/workflows'],
    [{ kind: 'workflow-detail', workflowId: 'wf1' }, '/workflows/wf1'],
    [{ kind: 'logbook' }, '/logbook'],
  ] as const)('serializes %o to %s', (page: any, expected) => {
    expect(pageToPath(page)).toBe(expected)
  })
})

describe('pageFromLocation', () => {
  it.each([
    ['/', '', { kind: 'task-list' }],
    ['/tasks', '', { kind: 'task-list' }],
    ['/tasks', '?filter=in-progress', { kind: 'task-list', filter: 'in-progress' }],
    ['/tasks/t1', '', { kind: 'task-detail', taskId: 't1' }],
    ['/projects', '', { kind: 'project-list' }],
    ['/projects/org%2Frepo', '', { kind: 'project-detail', projectId: 'org/repo' }],
    ['/chats', '', { kind: 'chats' }],
    ['/chats/a1', '', { kind: 'chat-detail', agentId: 'a1' }],
    ['/agents', '', { kind: 'agents' }],
    ['/agents', '?tab=loop', { kind: 'agents', tab: 'loop' }],
    ['/agents/a1', '', { kind: 'agent-detail', agentId: 'a1' }],
    ['/github', '', { kind: 'github' }],
    ['/stats', '', { kind: 'stats' }],
    ['/evaluation', '', { kind: 'evaluation' }],
    ['/reviews', '', { kind: 'reviews' }],
    ['/settings', '', { kind: 'settings' }],
    ['/workflows', '', { kind: 'workflows' }],
    ['/workflows/wf1', '', { kind: 'workflow-detail', workflowId: 'wf1' }],
    ['/logbook', '', { kind: 'logbook' }],
    // trailing / duplicate slashes collapse the same as the bare route
    ['/tasks/', '', { kind: 'task-list' }],
    ['//tasks//t1//', '', { kind: 'task-detail', taskId: 't1' }],
    // unknown root falls back to the board
    ['/nope', '', { kind: 'task-list' }],
    ['/nope/123', '', { kind: 'task-list' }],
    // routing is case-sensitive; unmatched case falls back
    ['/Tasks/t1', '', { kind: 'task-list' }],
    // malformed percent-encoding falls back instead of throwing
    ['/tasks/%E0%A4%A', '', { kind: 'task-list' }],
    ['/projects/%', '', { kind: 'project-list' }],
    // query/hash on a route that ignores them are simply not consulted
    ['/settings', '?foo=bar', { kind: 'settings' }],
  ] as const)('parses %s%s', (pathname, search, expected) => {
    expect(pageFromLocation(loc(pathname, search))).toEqual(expected)
  })

  it('does not throw on a malformed path', () => {
    expect(() => pageFromLocation(loc('/tasks/%E0%A4%A'))).not.toThrow()
  })
})

describe('startUrlRouting', () => {
  afterEach(() => {
    window.history.replaceState(null, '', '/')
  })

  it('parses the initial deep link on startup', () => {
    window.history.pushState(null, '', '/tasks/deep-linked')
    const stop = navStore.startUrlRouting()
    expect(navStore.page).toEqual({ kind: 'task-detail', taskId: 'deep-linked' })
    stop()
  })

  it('normalizes an unknown initial path via replaceState (no extra history entry)', () => {
    const before = window.history.length
    window.history.pushState(null, '', '/nope')
    const stop = navStore.startUrlRouting()
    expect(navStore.page).toEqual({ kind: 'task-list' })
    expect(window.location.pathname).toBe('/tasks')
    expect(window.history.length).toBe(before + 1)
    stop()
  })

  it('navigate() pushes a new history entry with the serialized path', () => {
    window.history.pushState(null, '', '/tasks')
    const stop = navStore.startUrlRouting()
    navStore.navigate({ kind: 'settings' })
    expect(window.location.pathname).toBe('/settings')
    stop()
  })

  it('reset() creates a browser-back waypoint while clearing the internal stack', () => {
    window.history.pushState(null, '', '/tasks')
    const stop = navStore.startUrlRouting()
    navStore.navigate({ kind: 'settings' })
    navStore.reset({ kind: 'agents' })
    expect(navStore.page.kind).toBe('agents')
    expect(navStore.stack).toHaveLength(0)
    expect(window.location.pathname).toBe('/agents')
    stop()
  })

  it('replace() rewrites history without pushing a new entry', () => {
    window.history.pushState(null, '', '/tasks')
    const stop = navStore.startUrlRouting()
    const before = window.history.length
    navStore.replace({ kind: 'reviews' })
    expect(window.location.pathname).toBe('/reviews')
    expect(window.history.length).toBe(before)
    stop()
  })

  it('back() delegates to browser history and popstate updates the page', () => {
    window.history.pushState(null, '', '/tasks')
    const stop = navStore.startUrlRouting()
    // Simulate what a real browser back() does: the URL moves, then a
    // popstate event fires — avoids relying on jsdom's async history timers.
    window.history.pushState(null, '', '/settings')
    window.history.pushState(null, '', '/tasks')
    window.dispatchEvent(new PopStateEvent('popstate'))
    expect(navStore.page.kind).toBe('task-list')
    stop()
  })

  it('a popstate-driven back pops the internal stack so canGoBack stays accurate', () => {
    window.history.pushState(null, '', '/tasks')
    const stop = navStore.startUrlRouting()
    navStore.navigate({ kind: 'settings' })
    expect(navStore.stack).toHaveLength(1)
    expect(navStore.canGoBack).toBe(true)

    // A real browser back moves the URL and fires popstate. The stack must be
    // popped in lockstep, otherwise navigate()'s push leaves it non-empty and
    // canGoBack keeps reporting true — a second back() would then walk the
    // tab's session history past the app entirely.
    window.history.pushState(null, '', '/tasks')
    window.dispatchEvent(new PopStateEvent('popstate'))

    expect(navStore.page).toEqual({ kind: 'task-list' })
    expect(navStore.stack).toHaveLength(0)
    expect(navStore.canGoBack).toBe(false)
    stop()
  })

  it('back() falls back to the internal stack once routing is stopped', () => {
    window.history.pushState(null, '', '/tasks')
    const stop = navStore.startUrlRouting()
    stop()
    navStore.reset({ kind: 'task-list' })
    navStore.navigate({ kind: 'settings' })
    navStore.back()
    expect(navStore.page.kind).toBe('task-list')
  })
})
