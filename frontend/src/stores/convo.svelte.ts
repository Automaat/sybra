import { SvelteMap } from 'svelte/reactivity'
import {
  GetConvoOutput,
  SendMessage,
  RespondApproval,
  EventsOn,
} from '$lib/api'
import type { agent } from '../../wailsjs/go/models.js'
import { agentConvo, agentApproval } from '../lib/events.js'

export interface ApprovalRequest {
  toolUseId: string
  toolName: string
  input: Record<string, unknown>
}

class ConvoStore {
  conversations = new SvelteMap<string, agent.ConvoEvent[]>()
  pendingApprovals = new SvelteMap<string, ApprovalRequest>()

  async getOutput(agentId: string): Promise<agent.ConvoEvent[]> {
    const events = (await GetConvoOutput(agentId)) ?? []
    this.conversations.set(agentId, events)
    return events
  }

  appendEvent(agentId: string, event: agent.ConvoEvent): void {
    const existing = this.conversations.get(agentId) ?? []
    this.conversations.set(agentId, [...existing, event])
  }

  async sendMessage(agentId: string, text: string): Promise<void> {
    await SendMessage(agentId, text)
  }

  async respondApproval(toolUseId: string, approved: boolean): Promise<void> {
    await RespondApproval(toolUseId, approved)
    this.pendingApprovals.delete(toolUseId)
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
        this.pendingApprovals.set(req.toolUseId, req)
      },
    )

    return () => {
      unsubConvo()
      unsubApproval()
    }
  }
}

export const convoStore = new ConvoStore()
