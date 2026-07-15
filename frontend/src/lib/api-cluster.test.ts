import { describe, it, expect, vi, beforeEach } from 'vitest'

const local = {
  StopAgent: vi.fn(),
  SendMessage: vi.fn(),
  RespondApproval: vi.fn(),
  ApprovePlan: vi.fn(),
}
const remote = {
  StopAgentOnNode: vi.fn(),
  SendMessageToNode: vi.fn(),
  RespondApprovalOnNode: vi.fn(),
  ApprovePlanOnNode: vi.fn(),
}

vi.mock('$lib/api', () => ({
  StopAgent: (...a: unknown[]) => local.StopAgent(...a),
  SendMessage: (...a: unknown[]) => local.SendMessage(...a),
  RespondApproval: (...a: unknown[]) => local.RespondApproval(...a),
  ApprovePlan: (...a: unknown[]) => local.ApprovePlan(...a),
  StopAgentOnNode: (...a: unknown[]) => remote.StopAgentOnNode(...a),
  SendMessageToNode: (...a: unknown[]) => remote.SendMessageToNode(...a),
  RespondApprovalOnNode: (...a: unknown[]) => remote.RespondApprovalOnNode(...a),
  ApprovePlanOnNode: (...a: unknown[]) => remote.ApprovePlanOnNode(...a),
}))

const { stopAgentForTask, sendMessageForTask, respondApprovalForTask, approvePlanForTask } = await import('./api-cluster.js')

describe('node-aware control routing', () => {
  beforeEach(() => vi.clearAllMocks())

  it('routes a homed-away task to the follower proxy', async () => {
    await stopAgentForTask('pet-box', 'ag-1')
    expect(remote.StopAgentOnNode).toHaveBeenCalledWith('pet-box', 'ag-1')
    expect(local.StopAgent).not.toHaveBeenCalled()

    await sendMessageForTask('pet-box', 'ag-1', 'hi')
    expect(remote.SendMessageToNode).toHaveBeenCalledWith('pet-box', 'ag-1', 'hi')

    await respondApprovalForTask('pet-box', 'tool-1', true)
    expect(remote.RespondApprovalOnNode).toHaveBeenCalledWith('pet-box', 'tool-1', true)

    await approvePlanForTask('pet-box', 'task-1')
    expect(remote.ApprovePlanOnNode).toHaveBeenCalledWith('pet-box', 'task-1')
  })

  it('routes a local task to the leader IPC/HTTP path', async () => {
    await stopAgentForTask(undefined, 'ag-1')
    expect(local.StopAgent).toHaveBeenCalledWith('ag-1')
    expect(remote.StopAgentOnNode).not.toHaveBeenCalled()

    await stopAgentForTask('', 'ag-2')
    expect(local.StopAgent).toHaveBeenCalledWith('ag-2')
    expect(remote.StopAgentOnNode).not.toHaveBeenCalled()

    await approvePlanForTask(undefined, 'task-1')
    expect(local.ApprovePlan).toHaveBeenCalledWith('task-1')
  })
})
