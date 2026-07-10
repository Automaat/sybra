import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent, waitFor } from '@testing-library/svelte'

const mockListTaskArtifacts = vi.fn()
const mockGetTaskSetupLog = vi.fn()
const mockListTaskAuditEvents = vi.fn()

vi.mock('$lib/api', () => ({
  ListTaskArtifacts: (...args: unknown[]) => mockListTaskArtifacts(...args),
  GetTaskSetupLog: (...args: unknown[]) => mockGetTaskSetupLog(...args),
  ListTaskAuditEvents: (...args: unknown[]) => mockListTaskAuditEvents(...args),
}))

const TaskDiagnosticsPanel = (await import('./TaskDiagnosticsPanel.svelte')).default

const baseTask = {
  id: 't1',
  slug: 'demo',
  title: 'Test Task',
  status: 'todo',
  taskType: 'normal',
  body: '',
  tags: [],
  agentMode: 'headless',
  allowedTools: [],
  projectId: 'foo/bar',
  prNumber: 0,
  reviewed: false,
  createdAt: '2026-04-01T00:00:00Z',
  updatedAt: '2026-04-01T00:00:00Z',
}

describe('TaskDiagnosticsPanel', () => {
  beforeEach(() => {
    mockListTaskArtifacts.mockReset()
    mockGetTaskSetupLog.mockReset()
    mockListTaskAuditEvents.mockReset()
  })

  afterEach(() => {
    cleanup()
  })

  it('renders artifacts, setup log, and audit events once loaded', async () => {
    mockListTaskArtifacts.mockResolvedValue([
      { name: 'trace.jsonl', kind: 'trace', size: 42, createdAt: '2026-04-01T00:00:00Z', content: 'hello' },
    ])
    mockGetTaskSetupLog.mockResolvedValue({ taskId: 't1', exists: true, path: '/tmp/t1-setup.log', content: 'setup ok' })
    mockListTaskAuditEvents.mockResolvedValue([
      { ts: '2026-04-01T00:00:00Z', type: 'task.created', taskId: 't1' },
    ])

    render(TaskDiagnosticsPanel, { props: { task: baseTask as never } })

    await waitFor(() => {
      expect(screen.getByText('trace.jsonl')).toBeDefined()
    })
    expect(screen.getByText('setup ok')).toBeDefined()
    expect(screen.getByText('task.created')).toBeDefined()
  })

  it('shows an error message when a diagnostics call fails', async () => {
    mockListTaskArtifacts.mockRejectedValue(new Error('boom'))
    mockGetTaskSetupLog.mockResolvedValue({ taskId: 't1', exists: false })
    mockListTaskAuditEvents.mockResolvedValue([])

    render(TaskDiagnosticsPanel, { props: { task: baseTask as never } })

    await waitFor(() => {
      expect(screen.getByText(/boom/)).toBeDefined()
    })
  })

  it('refetches diagnostics when the refresh button is clicked', async () => {
    mockListTaskArtifacts.mockResolvedValue([])
    mockGetTaskSetupLog.mockResolvedValue({ taskId: 't1', exists: false })
    mockListTaskAuditEvents.mockResolvedValue([])

    render(TaskDiagnosticsPanel, { props: { task: baseTask as never } })

    await waitFor(() => {
      expect(mockListTaskArtifacts).toHaveBeenCalledTimes(1)
    })

    await fireEvent.click(screen.getByRole('button', { name: /refresh/i }))

    await waitFor(() => {
      expect(mockListTaskArtifacts).toHaveBeenCalledTimes(2)
    })
  })
})
