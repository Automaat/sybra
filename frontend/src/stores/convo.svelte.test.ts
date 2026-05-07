import { describe, it, expect, vi, beforeEach } from 'vitest'
import { ConvoEvent } from '../../bindings/github.com/Automaat/sybra/internal/agent/models.js'
import { agentConvo, agentApproval } from '../lib/events.js'

const mockGetConvoOutput = vi.fn()
const mockSendMessage = vi.fn()
const mockRespondApproval = vi.fn()

let eventCallbacks: Record<string, (data: unknown) => void> = {}

vi.mock('$lib/api', () => ({
  GetConvoOutput: (...args: unknown[]) => mockGetConvoOutput(...args),
  SendMessage: (...args: unknown[]) => mockSendMessage(...args),
  RespondApproval: (...args: unknown[]) => mockRespondApproval(...args),
  EventsOn: (event: string, cb: (data: unknown) => void) => {
    eventCallbacks[event] = cb
    return () => { delete eventCallbacks[event] }
  },
}))

const { convoStore } = await import('./convo.svelte.js')

function makeConvoEvent(type: string, text = ''): ConvoEvent {
  return ConvoEvent.createFrom({ type, text, timestamp: new Date().toISOString() })
}

describe('ConvoStore', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    eventCallbacks = {}
    convoStore.conversations.clear()
    convoStore.pendingApprovals.clear()
  })

  describe('getOutput', () => {
    it('fetches and stores events', async () => {
      const events = [makeConvoEvent('assistant', 'hello')]
      mockGetConvoOutput.mockResolvedValue(events)

      const result = await convoStore.getOutput('a1')

      expect(mockGetConvoOutput).toHaveBeenCalledWith('a1')
      expect(result).toHaveLength(1)
      expect(convoStore.conversations.get('a1')).toHaveLength(1)
    })

    it('handles null result', async () => {
      mockGetConvoOutput.mockResolvedValue(null)

      const result = await convoStore.getOutput('a1')

      expect(result).toEqual([])
      expect(convoStore.conversations.get('a1')).toEqual([])
    })

    it('overwrites existing conversations', async () => {
      convoStore.conversations.set('a1', [makeConvoEvent('assistant', 'old')])
      mockGetConvoOutput.mockResolvedValue([makeConvoEvent('assistant', 'new')])

      await convoStore.getOutput('a1')

      const stored = convoStore.conversations.get('a1')!
      expect(stored).toHaveLength(1)
      expect(stored[0].text).toBe('new')
    })
  })

  describe('appendEvent', () => {
    it('appends to existing conversation', () => {
      convoStore.conversations.set('a1', [makeConvoEvent('assistant', 'first')])
      convoStore.appendEvent('a1', makeConvoEvent('assistant', 'second'))

      const events = convoStore.conversations.get('a1')!
      expect(events).toHaveLength(2)
      expect(events[1].text).toBe('second')
    })

    it('creates conversation if none exists', () => {
      convoStore.appendEvent('a1', makeConvoEvent('assistant', 'hello'))

      expect(convoStore.conversations.get('a1')).toHaveLength(1)
    })
  })

  describe('sendMessage', () => {
    it('calls SendMessage with agentId and text', async () => {
      mockSendMessage.mockResolvedValue(undefined)

      await convoStore.sendMessage('a1', 'hello')

      expect(mockSendMessage).toHaveBeenCalledWith('a1', 'hello')
    })
  })

  describe('respondApproval', () => {
    it('calls RespondApproval and removes from pendingApprovals', async () => {
      convoStore.pendingApprovals.set('tool-1', {
        toolUseId: 'tool-1',
        toolName: 'Bash',
        input: {},
      })
      mockRespondApproval.mockResolvedValue(undefined)

      await convoStore.respondApproval('tool-1', true)

      expect(mockRespondApproval).toHaveBeenCalledWith('tool-1', true)
      expect(convoStore.pendingApprovals.has('tool-1')).toBe(false)
    })

    it('calls RespondApproval with false for denial', async () => {
      convoStore.pendingApprovals.set('tool-2', {
        toolUseId: 'tool-2',
        toolName: 'Write',
        input: {},
      })
      mockRespondApproval.mockResolvedValue(undefined)

      await convoStore.respondApproval('tool-2', false)

      expect(mockRespondApproval).toHaveBeenCalledWith('tool-2', false)
    })
  })

  describe('subscribe', () => {
    it('registers convo event listener', () => {
      const unsub = convoStore.subscribe('a1')

      expect(eventCallbacks[agentConvo('a1')]).toBeDefined()
      unsub()
    })

    it('registers approval event listener', () => {
      const unsub = convoStore.subscribe('a1')

      expect(eventCallbacks[agentApproval('a1')]).toBeDefined()
      unsub()
    })

    it('appends events from convo stream', () => {
      const unsub = convoStore.subscribe('a1')
      const event = makeConvoEvent('assistant', 'streamed')

      eventCallbacks[agentConvo('a1')](event)

      expect(convoStore.conversations.get('a1')!).toHaveLength(1)
      expect(convoStore.conversations.get('a1')![0].text).toBe('streamed')
      unsub()
    })

    it('stores pending approval from approval stream', () => {
      const unsub = convoStore.subscribe('a1')
      const req = { toolUseId: 'tu-1', toolName: 'Bash', input: { cmd: 'ls' } }

      eventCallbacks[agentApproval('a1')](req)

      expect(convoStore.pendingApprovals.get('tu-1')).toEqual(req)
      unsub()
    })

    it('unsubscribes both listeners', () => {
      const unsub = convoStore.subscribe('a1')

      unsub()

      expect(eventCallbacks[agentConvo('a1')]).toBeUndefined()
      expect(eventCallbacks[agentApproval('a1')]).toBeUndefined()
    })
  })
})
