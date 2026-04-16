/**
 * Command palette unified command list.
 *
 * ID stability contract: all command ids are deterministic string literals
 * (e.g. "action:new-task", "page:settings", "task:<id>"). They must NOT be
 * derived from user-visible labels (which may change). The recents store
 * depends on id stability; stale ids are silently dropped on lookup.
 */

import type { Page } from './navigation.svelte.js'
import { taskStore } from '../stores/tasks.svelte.js'
import { projectStore } from '../stores/projects.svelte.js'
import { agentStore } from '../stores/agents.svelte.js'

export type CommandSection = 'action' | 'page' | 'task' | 'project' | 'agent'

export interface Command {
  id: string
  title: string
  subtitle?: string
  section: CommandSection
  shortcut?: string
  keywords?: string[]
  run: () => void
}

export interface PaletteCtx {
  navigate: (page: Page) => void
  openNewTask: () => void
  openNewProject: () => void
  openKeyboardHelp: () => void
}

const ACTIVE_AGENT_STATES = new Set(['running', 'waiting', 'errored'])

export function buildCommands(ctx: PaletteCtx): Command[] {
  const cmds: Command[] = []

  // --- ACTIONS ---
  cmds.push({
    id: 'action:new-task',
    title: 'New Task',
    section: 'action',
    shortcut: '⌘N',
    run: ctx.openNewTask,
  })
  cmds.push({
    id: 'action:new-project',
    title: 'New Project',
    section: 'action',
    run: ctx.openNewProject,
  })
  cmds.push({
    id: 'action:new-chat',
    title: 'New Chat',
    section: 'action',
    run: () => ctx.navigate({ kind: 'chats' }),
  })
  cmds.push({
    id: 'action:keyboard-help',
    title: 'Keyboard Shortcuts',
    section: 'action',
    shortcut: '⌘/',
    run: ctx.openKeyboardHelp,
  })

  // --- PAGES ---
  const pageEntries: { id: string; title: string; shortcut?: string; page: Page }[] = [
    { id: 'page:dashboard', title: 'Dashboard', shortcut: '⌘1', page: { kind: 'dashboard' } },
    { id: 'page:task-list', title: 'Tasks', shortcut: '⌘2', page: { kind: 'task-list' } },
    { id: 'page:project-list', title: 'Projects', shortcut: '⌘3', page: { kind: 'project-list' } },
    { id: 'page:agents', title: 'Agents', shortcut: '⌘4', page: { kind: 'agents' } },
    { id: 'page:github', title: 'GitHub', shortcut: '⌘5', page: { kind: 'github' } },
    { id: 'page:reviews', title: 'Reviews', shortcut: '⌘6', page: { kind: 'reviews' } },
    { id: 'page:stats', title: 'Stats', shortcut: '⌘7', page: { kind: 'stats' } },
    { id: 'page:settings', title: 'Settings', shortcut: '⌘,', page: { kind: 'settings' } },
    { id: 'page:chats', title: 'Chats', page: { kind: 'chats' } },
    { id: 'page:workflows', title: 'Workflows', page: { kind: 'workflows' } },
  ]
  for (const p of pageEntries) {
    cmds.push({
      id: p.id,
      title: p.title,
      section: 'page',
      shortcut: p.shortcut,
      run: () => ctx.navigate(p.page),
    })
  }

  // --- TASKS ---
  for (const t of taskStore.list) {
    cmds.push({
      id: `task:${t.id}`,
      title: t.title,
      subtitle: t.status,
      section: 'task',
      keywords: [t.id, ...(t.tags ?? [])],
      run: () => ctx.navigate({ kind: 'task-detail', taskId: t.id }),
    })
  }

  // --- PROJECTS ---
  for (const p of projectStore.list) {
    cmds.push({
      id: `project:${p.id}`,
      title: `${p.owner}/${p.repo}`,
      section: 'project',
      run: () => ctx.navigate({ kind: 'project-detail', projectId: p.id }),
    })
  }

  // --- AGENTS (active states only — excludes terminated/stopped) ---
  for (const a of agentStore.list) {
    if (!ACTIVE_AGENT_STATES.has(a.state)) continue
    cmds.push({
      id: `agent:${a.id}`,
      title: agentStore.stepTexts.get(a.id) ?? a.id,
      subtitle: a.state,
      section: 'agent',
      run: () => ctx.navigate({ kind: 'agent-detail', agentId: a.id }),
    })
  }

  return cmds
}
