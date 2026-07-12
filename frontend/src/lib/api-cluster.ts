import * as api from '$lib/api'

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
