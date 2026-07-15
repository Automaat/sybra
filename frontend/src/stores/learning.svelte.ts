import { EventsOn, GetLearningDigestStatus, ListDigests } from '$lib/api'
import { LearningSummary } from '../lib/events.js'
import type { Digest, Status } from '../../bindings/github.com/Automaat/sybra/internal/learning/models.js'

function isRecord(value: unknown): value is Record<string, unknown> {
  return !!value && typeof value === 'object'
}

function isStringArray(value: unknown): value is string[] {
  return Array.isArray(value) && value.every((item) => typeof item === 'string')
}

function isTakeawayArray(value: unknown): boolean {
  return Array.isArray(value) && value.every((item) => isRecord(item) && typeof item.text === 'string')
}

function isEvidenceArray(value: unknown): boolean {
  return Array.isArray(value) && value.every((item) => isRecord(item) && typeof item.kind === 'string' && typeof item.id === 'string')
}

function isDigest(value: unknown): value is Digest {
  if (!isRecord(value)) return false
  if (typeof value.schemaVersion !== 'number') return false
  if (typeof value.generatedAt !== 'string') return false
  if (typeof value.since !== 'string') return false
  if (typeof value.until !== 'string') return false
  if (typeof value.reportDigest !== 'string') return false

  for (const key of ['worked', 'notWorked', 'uncertain', 'nextBets']) {
    if (key in value && value[key] !== undefined && !isStringArray(value[key])) return false
  }
  for (const key of ['promptTakeaways', 'skillTakeaways', 'modelTakeaways']) {
    if (key in value && value[key] !== undefined && !isTakeawayArray(value[key])) return false
  }
  if ('evidence' in value && value.evidence !== undefined && !isEvidenceArray(value.evidence)) return false

  return true
}

function digestKey(digest: Digest): string {
  return `${digest.since}|${digest.until}|${digest.reportDigest}`
}

export class LearningStore {
  digests = $state<Digest[]>([])
  status = $state<Status | null>(null)
  loading = $state(false)
  error = $state('')
  private cancelListener: (() => void) | null = null

  async load(): Promise<void> {
    this.loading = true
    this.error = ''
    try {
      const [digests, status] = await Promise.all([ListDigests(), GetLearningDigestStatus()])
      this.digests = digests
      this.status = status
    } catch (e) {
      this.error = String(e)
    } finally {
      this.loading = false
    }
  }

  listen(): void {
    this.stopListening()
    this.cancelListener = EventsOn(LearningSummary, (digest: unknown) => {
      if (!isDigest(digest)) return
      const key = digestKey(digest)
      this.digests = [digest, ...this.digests.filter((d) => digestKey(d) !== key)]
    })
  }

  stopListening(): void {
    if (this.cancelListener) {
      this.cancelListener()
      this.cancelListener = null
    }
  }
}

export const learningStore = new LearningStore()
