import { describe, expect, it } from 'vitest'
import { isRenovatePRReadyToMerge, passesRenovateMergeChecks } from './renovate.js'

describe('passesRenovateMergeChecks', () => {
  it('accepts mergeable renovate PRs with green CI', () => {
    expect(
      passesRenovateMergeChecks({
        isDraft: false,
        mergeable: 'MERGEABLE',
        ciStatus: 'SUCCESS',
      }),
    ).toBe(true)
  })

  it('blocks PRs that are still waiting for stability', () => {
    expect(
      passesRenovateMergeChecks({
        isDraft: false,
        mergeable: 'MERGEABLE',
        ciStatus: 'SUCCESS',
        waitingForStability: true,
      }),
    ).toBe(false)
  })
})

describe('isRenovatePRReadyToMerge', () => {
  it('requires approval or no review decision', () => {
    expect(
      isRenovatePRReadyToMerge({
        isDraft: false,
        mergeable: 'MERGEABLE',
        ciStatus: 'SUCCESS',
        reviewDecision: 'APPROVED',
      }),
    ).toBe(true)
  })

  it('blocks review-required PRs even when CI is green', () => {
    expect(
      isRenovatePRReadyToMerge({
        isDraft: false,
        mergeable: 'MERGEABLE',
        ciStatus: 'SUCCESS',
        reviewDecision: 'REVIEW_REQUIRED',
      }),
    ).toBe(false)
  })
})
