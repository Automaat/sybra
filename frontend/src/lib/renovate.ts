import type { PullRequest } from '../../bindings/github.com/Automaat/sybra/internal/github/models.js'

type MergeCheckPR = Pick<PullRequest, 'isDraft' | 'mergeable' | 'ciStatus'> & {
  waitingForStability?: boolean
}

type MergeReadyPR = MergeCheckPR & Pick<PullRequest, 'reviewDecision'>

export function passesRenovateMergeChecks(pr: MergeCheckPR): boolean {
  return (
    !pr.isDraft &&
    pr.mergeable === 'MERGEABLE' &&
    !pr.waitingForStability &&
    (pr.ciStatus === 'SUCCESS' || pr.ciStatus === '')
  )
}

export function isRenovatePRReadyToMerge(pr: MergeReadyPR): boolean {
  return (
    passesRenovateMergeChecks(pr) &&
    (pr.reviewDecision === 'APPROVED' || pr.reviewDecision === '')
  )
}
