import { describe, it, expect } from 'vitest'
import {
  ALL_STATUSES,
  AWAITS_HUMAN,
  CORE_STATUSES,
  CORE_STATUS_OPTIONS,
  awaitsHuman,
  awaitsHumanLabel,
  coreStatus,
  isPromptLabProposal,
  needsPlanApproval,
  statusLabel,
  statusOptionsFor,
  BOARD_COLUMNS,
  STATUS_MAP,
} from './statuses.js'

describe('CORE_STATUSES', () => {
  it('is the active board columns plus the terminal states', () => {
    expect(CORE_STATUSES).toEqual([...BOARD_COLUMNS.map((c) => c.status), 'done', 'cancelled'])
  })

  it('is a small set (<= 10) and excludes granular states', () => {
    expect(CORE_STATUSES.length).toBeLessThanOrEqual(10)
    for (const granular of ['new', 'plan-review', 'ready-pr']) {
      expect(CORE_STATUSES).not.toContain(granular)
    }
  })

  it('exposes options with labels from STATUS_MAP', () => {
    for (const opt of CORE_STATUS_OPTIONS) {
      expect(opt.label).toBe(STATUS_MAP[opt.value].label)
    }
  })
})

describe('coreStatus', () => {
  it('rolls granular states up to their board column', () => {
    expect(coreStatus('new')).toBe('todo')
    expect(coreStatus('plan-review')).toBe('planning')
    expect(coreStatus('ready-review')).toBe('ready-review')
    expect(coreStatus('ready-pr')).toBe('in-review')
    expect(coreStatus('blocked')).toBe('blocked')
  })

  it('maps a column status to itself', () => {
    for (const col of BOARD_COLUMNS) {
      expect(coreStatus(col.status)).toBe(col.status)
    }
  })

  it('passes terminal/unknown states through unchanged', () => {
    expect(coreStatus('done')).toBe('done')
    expect(coreStatus('cancelled')).toBe('cancelled')
    expect(coreStatus('mystery')).toBe('mystery')
  })

  it('always lands on a core status for every folded state', () => {
    for (const col of BOARD_COLUMNS) {
      for (const folded of col.includes) {
        expect(CORE_STATUSES).toContain(coreStatus(folded))
      }
    }
  })
})

describe('statusOptionsFor', () => {
  it('returns just the core options for a core status', () => {
    expect(statusOptionsFor('in-progress')).toBe(CORE_STATUS_OPTIONS)
  })

  it('returns just the core options for a folded or terminal status', () => {
    expect(statusOptionsFor('blocked')).toBe(CORE_STATUS_OPTIONS)
    expect(statusOptionsFor('cancelled')).toBe(CORE_STATUS_OPTIONS)
  })

  it('appends an unknown current status so it stays selectable', () => {
    const opts = statusOptionsFor('mystery')
    expect(opts).toHaveLength(CORE_STATUS_OPTIONS.length + 1)
    expect(opts.at(-1)).toEqual({ value: 'mystery', label: 'mystery' })
  })
})

describe('statusLabel — one canonical vocabulary', () => {
  it('returns the STATUS_MAP label for every known status', () => {
    for (const meta of ALL_STATUSES) {
      expect(statusLabel(meta.value)).toBe(meta.label)
    }
  })

  it('passes unknown statuses through verbatim', () => {
    expect(statusLabel('mystery')).toBe('mystery')
  })

  it('is the label every picker option uses', () => {
    for (const opt of CORE_STATUS_OPTIONS) {
      expect(opt.label).toBe(statusLabel(opt.value))
    }
  })
})

describe('awaitsHumanLabel — canonical, never a divergent name', () => {
  it('matches the canonical status label for every awaits-human status', () => {
    for (const status of AWAITS_HUMAN) {
      expect(awaitsHuman(status)).toBe(true)
      // The attention pill must reuse the same word the list/detail show —
      // no invented "Needs Review"/"Needs You" vocabulary.
      expect(awaitsHumanLabel(status)).toBe(statusLabel(status))
    }
  })

  it('is empty for statuses that do not await the user', () => {
    for (const meta of ALL_STATUSES) {
      if (AWAITS_HUMAN.has(meta.value)) continue
      expect(awaitsHumanLabel(meta.value)).toBe('')
    }
  })
})

describe('needsPlanApproval — keyed off workflow, not stale status (issue #1642)', () => {
  it('is true when status is plan-review and no workflow is attached', () => {
    expect(needsPlanApproval({ status: 'plan-review' })).toBe(true)
  })

  it('is false when status is anything else and no workflow is attached', () => {
    expect(needsPlanApproval({ status: 'planning' })).toBe(false)
  })

  it('is true when the workflow is waiting on review_plan, even if status desynced away from plan-review', () => {
    expect(
      needsPlanApproval({
        status: 'planning',
        workflow: { currentStep: 'review_plan', state: 'waiting' },
      }),
    ).toBe(true)
  })

  it('is false when the workflow has moved past review_plan, even if status is stale plan-review', () => {
    expect(
      needsPlanApproval({
        status: 'plan-review',
        workflow: { currentStep: 'implement', state: 'running' },
      }),
    ).toBe(false)
  })

  it('is false when review_plan is reached but not yet waiting (still running)', () => {
    expect(
      needsPlanApproval({
        status: 'plan-review',
        workflow: { currentStep: 'review_plan', state: 'running' },
      }),
    ).toBe(false)
  })

  it('falls back to status when only currentStep is populated (state missing)', () => {
    expect(
      needsPlanApproval({
        status: 'plan-review',
        workflow: { currentStep: 'review_plan' },
      }),
    ).toBe(true)
  })

  it('falls back to status when only state is populated (currentStep missing)', () => {
    expect(
      needsPlanApproval({
        status: 'plan-review',
        workflow: { state: 'waiting' },
      }),
    ).toBe(true)
  })
})

describe('isPromptLabProposal', () => {
  it('is true when human-required and tagged prompt-lab-proposal', () => {
    expect(isPromptLabProposal({ status: 'human-required', tags: ['prompt-lab-proposal'] })).toBe(true)
  })

  it('is true when other tags are also present', () => {
    expect(
      isPromptLabProposal({ status: 'human-required', tags: ['requires-human', 'prompt-lab-proposal'] }),
    ).toBe(true)
  })

  it('is false when status is not human-required, even if tagged', () => {
    expect(isPromptLabProposal({ status: 'todo', tags: ['prompt-lab-proposal'] })).toBe(false)
    expect(isPromptLabProposal({ status: 'cancelled', tags: ['prompt-lab-proposal'] })).toBe(false)
  })

  it('is false when human-required but missing the tag', () => {
    expect(isPromptLabProposal({ status: 'human-required', tags: ['requires-human'] })).toBe(false)
    expect(isPromptLabProposal({ status: 'human-required', tags: [] })).toBe(false)
  })

  it('is false when tags is null/undefined', () => {
    expect(isPromptLabProposal({ status: 'human-required', tags: null })).toBe(false)
    expect(isPromptLabProposal({ status: 'human-required' })).toBe(false)
  })

  it('is false when status is missing', () => {
    expect(isPromptLabProposal({ tags: ['prompt-lab-proposal'] })).toBe(false)
  })
})
