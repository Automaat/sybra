import {
  GetConvoOutput,
  SendMessage,
  RespondApproval,
} from '../../wailsjs/go/main/AgentService.js'
import type { agent } from '../../wailsjs/go/models.js'
import { EventsOn } from '../../wailsjs/runtime/runtime.js'
import { agentConvo, agentApproval } from '../lib/events.js'

export interface ApprovalRequest {
  toolUseId: string
  toolName: string
  input: Record<string, unknown>
}

class ConvoStore {
  conversations = $state<Map<string, agent.ConvoEvent[]>>(new Map())
  pendingApprovals = $state<Map<string, ApprovalRequest>>(new Map())

  async getOutput(agentId: string): Promise<agent.ConvoEvent[]> {
    const events = (await GetConvoOutput(agentId)) ?? []
    this.conversations = new Map(this.conversations).set(agentId, events)
    return events
  }

  appendEvent(agentId: string, event: agent.ConvoEvent): void {
    const existing = this.conversations.get(agentId) ?? []
    this.conversations = new Map(this.conversations).set(agentId, [...existing, event])
  }

  async sendMessage(agentId: string, text: string): Promise<void> {
    await SendMessage(agentId, text)
  }

  async respondApproval(toolUseId: string, approved: boolean): Promise<void> {
    await RespondApproval(toolUseId, approved)
    const next = new Map(this.pendingApprovals)
    next.delete(toolUseId)
    this.pendingApprovals = next
  }

  subscribe(agentId: string): () => void {
    const unsubConvo = EventsOn(
      agentConvo(agentId),
      (event: agent.ConvoEvent) => {
        this.appendEvent(agentId, event)
      },
    )

    const unsubApproval = EventsOn(
      agentApproval(agentId),
      (req: ApprovalRequest) => {
        this.pendingApprovals = new Map(this.pendingApprovals).set(req.toolUseId, req)
      },
    )

    return () => {
      unsubConvo()
      unsubApproval()
    }
  }
}

export const convoStore = new ConvoStore()
