import { SvelteMap } from 'svelte/reactivity'
import {
  GetConvoOutput,
  EventsOn,
} from '$lib/api'
import { sendMessageForTask, respondApprovalForTask } from '$lib/api-cluster'
import { agentStore } from './agents.svelte.js'
import { taskStore } from './tasks.svelte.js'
import type { ConvoEvent } from '../../bindings/github.com/Automaat/sybra/internal/agent/models.js'
import { agentConvo, agentApproval } from '../lib/events.js'

function nodeForAgent(agentId: string): string | undefined {
  const taskId = agentStore.items.get(agentId)?.taskId
  return taskId ? taskStore.tasks.get(taskId)?.assignedNode : undefined
}

export interface ApprovalRequest {
  toolUseId: string
  toolName: string
  input: Record<string, unknown>
}

class ConvoStore {
  conversations = new SvelteMap<string, ConvoEvent[]>()
  // Keyed by agentId, then toolUseId — so a toolUseId is only ever looked up
  // within its own agent's chat, and one agent's approvals never bleed into
  // another agent's ChatView/BlockedLayout.
  pendingApprovals = new SvelteMap<string, SvelteMap<string, ApprovalRequest>>()

  async getOutput(agentId: string): Promise<ConvoEvent[]> {
    const events = (await GetConvoOutput(agentId)) ?? []
    this.conversations.set(agentId, events)
    return events
  }

  appendEvent(agentId: string, event: ConvoEvent): void {
    const existing = this.conversations.get(agentId) ?? []
    this.conversations.set(agentId, [...existing, event])
  }

  async sendMessage(agentId: string, text: string): Promise<void> {
    await sendMessageForTask(nodeForAgent(agentId), agentId, text)
  }

  approvalsFor(agentId: string): ApprovalRequest[] {
    return [...(this.pendingApprovals.get(agentId)?.values() ?? [])]
  }

  async respondApproval(agentId: string, toolUseId: string, approved: boolean): Promise<void> {
    await respondApprovalForTask(nodeForAgent(agentId), toolUseId, approved)
    this.pendingApprovals.get(agentId)?.delete(toolUseId)
  }

  subscribe(agentId: string): () => void {
    const unsubConvo = EventsOn(
      agentConvo(agentId),
      (event: ConvoEvent) => {
        this.appendEvent(agentId, event)
      },
    )

    const unsubApproval = EventsOn(
      agentApproval(agentId),
      (req: ApprovalRequest) => {
        let forAgent = this.pendingApprovals.get(agentId)
        if (!forAgent) {
          forAgent = new SvelteMap<string, ApprovalRequest>()
          this.pendingApprovals.set(agentId, forAgent)
        }
        forAgent.set(req.toolUseId, req)
      },
    )

    return () => {
      unsubConvo()
      unsubApproval()
    }
  }
}

export const convoStore = new ConvoStore()
