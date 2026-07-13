import * as api from '$lib/api'
import type { Agent } from '../../bindings/github.com/Automaat/sybra/internal/agent/models.js'

// Agents running on followers, each stamped with its node. The leader's own
// ListAgents only ever returns local agents, so without this the board cannot
// see a remote run at all — and every *OnNode action below would be
// unreachable. A cluster-less leader returns [], so this is a no-op then.
export async function listRemoteAgents(): Promise<Agent[]> {
  try {
    return (await api.ListNodeAgents()) ?? []
  } catch {
    return []
  }
}

export function getAgentOutputForNode(node: string | undefined, agentID: string) {
  return node ? api.GetAgentOutputOnNode(node, agentID) : api.GetAgentOutput(agentID)
}

export function getConvoOutputForNode(node: string | undefined, agentID: string) {
  return node ? api.GetConvoOutputOnNode(node, agentID) : api.GetConvoOutput(agentID)
}

export function stopAgentForTask(node: string | undefined, agentID: string): Promise<void> {
  return node ? api.StopAgentOnNode(node, agentID) : api.StopAgent(agentID)
}

export function sendMessageForTask(node: string | undefined, agentID: string, text: string): Promise<void> {
  return node ? api.SendMessageToNode(node, agentID, text) : api.SendMessage(agentID, text)
}

export function respondApprovalForTask(node: string | undefined, toolUseID: string, approved: boolean): Promise<void> {
  return node ? api.RespondApprovalOnNode(node, toolUseID, approved) : api.RespondApproval(toolUseID, approved)
}

export function approvePlanForTask(node: string | undefined, taskID: string): Promise<unknown> {
  return node ? api.ApprovePlanOnNode(node, taskID) : api.ApprovePlan(taskID)
}

export function rejectPlanForTask(node: string | undefined, taskID: string, feedback: string): Promise<unknown> {
  return node ? api.RejectPlanOnNode(node, taskID, feedback) : api.RejectPlan(taskID, feedback)
}
