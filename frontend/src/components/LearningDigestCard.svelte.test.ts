import { cleanup, render, screen } from '@testing-library/svelte'
import { afterEach, describe, expect, it } from 'vitest'
import LearningDigestCard from './LearningDigestCard.svelte'
import type { Digest, Status } from '../../bindings/github.com/Automaat/sybra/internal/learning/models.js'

function makeDigest(overrides: Partial<Digest> = {}): Digest {
  return {
    schemaVersion: 1,
    generatedAt: '2026-07-02T10:30:00Z',
    since: '2026-07-02T08:00:00Z',
    until: '2026-07-02T10:00:00Z',
    reportDigest: 'abcdef1234567890',
    authorProvider: 'codex',
    authorModel: 'gpt-5',
    worked: ['Small scoped changes landed cleanly.'],
    notWorked: ['Late review feedback created rework.'],
    uncertain: ['Prompt variant might help, but sample size is low.'],
    nextBets: ['Try the tighter reviewer prompt.'],
    promptTakeaways: [{ text: 'Ask for evidence before edits.', experimentRef: 'exp-prompt', variantRef: 'v2' }],
    evidence: [{ kind: 'task', id: '0271ee7f' }],
    ...overrides,
  } as Digest
}

function makeStatus(overrides: Partial<Status> = {}): Status {
  return { enabled: true, nextRun: '2026-07-02T12:00:00Z', ...overrides } as Status
}

describe('LearningDigestCard', () => {
  afterEach(() => cleanup())

  it('renders a populated digest with bounded refs and history', () => {
    const older = makeDigest({
      reportDigest: 'old1234567890',
      since: '2026-07-01T08:00:00Z',
      until: '2026-07-01T10:00:00Z',
      generatedAt: '2026-07-01T10:30:00Z',
      worked: ['Older lesson.'],
      notWorked: [],
      uncertain: [],
      nextBets: [],
      promptTakeaways: [],
      evidence: [],
    })

    render(LearningDigestCard, { props: { digests: [makeDigest(), older], status: makeStatus() } })

    expect(screen.getByText('Agent learning journal')).toBeDefined()
    expect(screen.getAllByText('Worked').length).toBeGreaterThan(0)
    expect(screen.getByText('Small scoped changes landed cleanly.')).toBeDefined()
    expect(screen.getByText('Prompt takeaways')).toBeDefined()
    expect(screen.getByText('experiment exp-prompt')).toBeDefined()
    expect(screen.getByText('variant v2')).toBeDefined()
    expect(screen.getByText('task 0271ee7f')).toBeDefined()
    expect(screen.queryByText('abcdef1234567890')).toBeNull()
    expect(screen.getByText('History')).toBeDefined()
    // latest digest body + at least one history entry each render a "generated" timestamp
    expect(screen.getAllByText(/generated/i).length).toBeGreaterThan(1)
  })

  it('renders empty, disabled, and invalid states distinctly', () => {
    const { unmount } = render(LearningDigestCard, { props: { digests: [], status: makeStatus() } })
    expect(screen.getByText('No learning digests have been generated yet.')).toBeDefined()
    unmount()

    render(LearningDigestCard, { props: { digests: [], status: makeStatus({ enabled: false }) } })
    expect(screen.getByText('Learning digest pipeline is off. No journal entries will appear until it is enabled.')).toBeDefined()
    cleanup()

    render(LearningDigestCard, { props: { digests: [makeDigest({ worked: [], notWorked: [], uncertain: [], nextBets: [], promptTakeaways: [] })], status: makeStatus() } })
    expect(screen.getByText('Latest learning digest is incomplete and cannot be rendered safely.')).toBeDefined()
  })

  it('renders loading and error states as visible lines', () => {
    const { unmount } = render(LearningDigestCard, { props: { loading: true } })
    expect(screen.getByText('Loading the latest learning digest…')).toBeDefined()
    unmount()

    render(LearningDigestCard, { props: { error: 'boom' } })
    expect(screen.getByText('Could not load the learning journal: boom')).toBeDefined()
  })

  it('marks uncertain claims as tentative low-sample evidence', () => {
    render(LearningDigestCard, { props: { digests: [makeDigest()], status: makeStatus() } })

    expect(screen.getByText('tentative · low N')).toBeDefined()
    expect(screen.getByText('Prompt variant might help, but sample size is low.')).toBeDefined()
  })
})
