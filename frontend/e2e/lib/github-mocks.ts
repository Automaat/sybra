import type { Page } from '@playwright/test'

// ─── Fixture data ─────────────────────────────────────────────────────────────

const MY_PRS = [
  {
    number: 512,
    title: 'feat(agent): streaming output improvements',
    url: 'https://github.com/automaat/sybra/pull/512',
    repository: 'automaat/sybra',
    repoName: 'sybra',
    author: 'mskalski',
    isDraft: false,
    labels: ['enhancement'],
    headRefName: 'feat/streaming-output',
    ciStatus: 'SUCCESS',
    hasPendingChecks: false,
    reviewDecision: 'APPROVED',
    mergeable: 'MERGEABLE',
    unresolvedCount: 0,
    viewerHasApproved: true,
    createdAt: '2026-04-10T09:00:00Z',
    updatedAt: '2026-04-16T14:30:00Z',
  },
  {
    number: 508,
    title: 'fix(tasks): kanban drag-and-drop on mobile',
    url: 'https://github.com/automaat/sybra/pull/508',
    repository: 'automaat/sybra',
    repoName: 'sybra',
    author: 'mskalski',
    isDraft: false,
    labels: ['bug', 'mobile'],
    headRefName: 'fix/kanban-mobile',
    ciStatus: 'PENDING',
    hasPendingChecks: true,
    reviewDecision: 'REVIEW_REQUIRED',
    mergeable: 'MERGEABLE',
    unresolvedCount: 1,
    viewerHasApproved: false,
    createdAt: '2026-04-14T11:00:00Z',
    updatedAt: '2026-04-16T13:00:00Z',
  },
  {
    number: 503,
    title: 'refactor(github): extract PR card into component',
    url: 'https://github.com/automaat/sybra/pull/503',
    repository: 'automaat/sybra',
    repoName: 'sybra',
    author: 'mskalski',
    isDraft: true,
    labels: ['refactor'],
    headRefName: 'refactor/pr-card',
    ciStatus: 'FAILURE',
    hasPendingChecks: false,
    reviewDecision: '',
    mergeable: 'CONFLICTING',
    unresolvedCount: 0,
    viewerHasApproved: false,
    createdAt: '2026-04-12T16:00:00Z',
    updatedAt: '2026-04-15T10:00:00Z',
  },
  {
    number: 497,
    title: 'docs(readme): update installation guide',
    url: 'https://github.com/automaat/sybra/pull/497',
    repository: 'automaat/synapse-infra',
    repoName: 'synapse-infra',
    author: 'mskalski',
    isDraft: false,
    labels: ['documentation'],
    headRefName: 'docs/install-guide',
    ciStatus: 'SUCCESS',
    hasPendingChecks: false,
    reviewDecision: 'APPROVED',
    mergeable: 'MERGEABLE',
    unresolvedCount: 0,
    viewerHasApproved: true,
    createdAt: '2026-04-08T08:30:00Z',
    updatedAt: '2026-04-13T17:00:00Z',
  },
]

const REVIEW_REQUESTED = [
  {
    number: 215,
    title: 'feat(orchestrator): multi-agent task routing',
    url: 'https://github.com/automaat/sybra/pull/215',
    repository: 'automaat/sybra',
    repoName: 'sybra',
    author: 'jdoe',
    isDraft: false,
    labels: ['enhancement', 'architecture'],
    headRefName: 'feat/multi-agent-routing',
    ciStatus: 'SUCCESS',
    hasPendingChecks: false,
    reviewDecision: 'REVIEW_REQUIRED',
    mergeable: 'MERGEABLE',
    unresolvedCount: 3,
    viewerHasApproved: false,
    createdAt: '2026-04-09T14:00:00Z',
    updatedAt: '2026-04-16T09:00:00Z',
  },
  {
    number: 209,
    title: 'fix(auth): token refresh race condition',
    url: 'https://github.com/automaat/synapse-infra/pull/209',
    repository: 'automaat/synapse-infra',
    repoName: 'synapse-infra',
    author: 'asmith',
    isDraft: false,
    labels: ['bug', 'security'],
    headRefName: 'fix/token-refresh',
    ciStatus: 'SUCCESS',
    hasPendingChecks: false,
    reviewDecision: 'REVIEW_REQUIRED',
    mergeable: 'MERGEABLE',
    unresolvedCount: 0,
    viewerHasApproved: false,
    createdAt: '2026-04-15T10:00:00Z',
    updatedAt: '2026-04-16T11:30:00Z',
  },
]

const RENOVATE_PRS = [
  // ── automaat/sybra ───────────────────────────────────────────────────────────

  // State: ready to merge — green dot, Approved badge, Merge button
  {
    number: 514,
    title: 'chore(deps): update dependency vite to v8.2.1',
    url: 'https://github.com/automaat/sybra/pull/514',
    repository: 'automaat/sybra',
    repoName: 'sybra',
    author: 'renovate[bot]',
    isDraft: false,
    labels: ['dependencies'],
    headRefName: 'renovate/vite-8.x',
    ciStatus: 'SUCCESS',
    hasPendingChecks: false,
    reviewDecision: 'APPROVED',
    mergeable: 'MERGEABLE',
    unresolvedCount: 0,
    viewerHasApproved: true,
    createdAt: '2026-04-16T08:00:00Z',
    updatedAt: '2026-04-16T10:00:00Z',
    checkRuns: [
      { name: 'lint-go', status: 'COMPLETED', conclusion: 'SUCCESS' },
      { name: 'lint-frontend', status: 'COMPLETED', conclusion: 'SUCCESS' },
      { name: 'build', status: 'COMPLETED', conclusion: 'SUCCESS' },
    ],
  },

  // State: needs approval, CI passing — green dot, Approve + Merge buttons
  {
    number: 511,
    title: 'chore(deps): update dependency @skeletonlabs/skeleton-svelte to v4.1.0',
    url: 'https://github.com/automaat/sybra/pull/511',
    repository: 'automaat/sybra',
    repoName: 'sybra',
    author: 'renovate[bot]',
    isDraft: false,
    labels: ['dependencies'],
    headRefName: 'renovate/skeleton-svelte-4.x',
    ciStatus: 'SUCCESS',
    hasPendingChecks: false,
    reviewDecision: '',
    mergeable: 'MERGEABLE',
    unresolvedCount: 0,
    viewerHasApproved: false,
    createdAt: '2026-04-16T06:00:00Z',
    updatedAt: '2026-04-16T07:30:00Z',
    checkRuns: [
      { name: 'lint-go', status: 'COMPLETED', conclusion: 'SUCCESS' },
      { name: 'lint-frontend', status: 'COMPLETED', conclusion: 'SUCCESS' },
      { name: 'build', status: 'COMPLETED', conclusion: 'SUCCESS' },
    ],
  },

  // State: CI failing — red dot, Approve + Rerun + Fix buttons
  {
    number: 509,
    title: 'chore(deps): update golang.org/x/net to v0.37.0',
    url: 'https://github.com/automaat/sybra/pull/509',
    repository: 'automaat/sybra',
    repoName: 'sybra',
    author: 'renovate[bot]',
    isDraft: false,
    labels: ['dependencies'],
    headRefName: 'renovate/golang-net',
    ciStatus: 'FAILURE',
    hasPendingChecks: false,
    reviewDecision: '',
    mergeable: 'MERGEABLE',
    unresolvedCount: 0,
    viewerHasApproved: false,
    createdAt: '2026-04-15T06:00:00Z',
    updatedAt: '2026-04-15T08:00:00Z',
    checkRuns: [
      { name: 'lint-go', status: 'COMPLETED', conclusion: 'SUCCESS' },
      { name: 'lint-frontend', status: 'COMPLETED', conclusion: 'SUCCESS' },
      { name: 'build', status: 'COMPLETED', conclusion: 'FAILURE' },
    ],
  },

  // State: CI pending — yellow dot, Approve button, no Merge
  {
    number: 507,
    title: 'chore(deps): update dependency typescript to v6.1.0',
    url: 'https://github.com/automaat/sybra/pull/507',
    repository: 'automaat/sybra',
    repoName: 'sybra',
    author: 'renovate[bot]',
    isDraft: false,
    labels: ['dependencies'],
    headRefName: 'renovate/typescript-6.x',
    ciStatus: 'PENDING',
    hasPendingChecks: true,
    reviewDecision: '',
    mergeable: 'MERGEABLE',
    unresolvedCount: 0,
    viewerHasApproved: false,
    createdAt: '2026-04-16T09:00:00Z',
    updatedAt: '2026-04-16T09:10:00Z',
    checkRuns: [
      { name: 'lint-go', status: 'COMPLETED', conclusion: 'SUCCESS' },
      { name: 'lint-frontend', status: 'IN_PROGRESS', conclusion: '' },
      { name: 'build', status: 'QUEUED', conclusion: '' },
    ],
  },

  // ── automaat/synapse-infra ───────────────────────────────────────────────────

  // State: approved, CI pending — yellow dot, Approved badge, no Merge yet
  {
    number: 88,
    title: 'chore(deps): update dependency ansible to v11.4.0',
    url: 'https://github.com/automaat/synapse-infra/pull/88',
    repository: 'automaat/synapse-infra',
    repoName: 'synapse-infra',
    author: 'renovate[bot]',
    isDraft: false,
    labels: ['dependencies'],
    headRefName: 'renovate/ansible-11.x',
    ciStatus: 'PENDING',
    hasPendingChecks: true,
    reviewDecision: 'APPROVED',
    mergeable: 'MERGEABLE',
    unresolvedCount: 0,
    viewerHasApproved: true,
    createdAt: '2026-04-15T12:00:00Z',
    updatedAt: '2026-04-16T08:30:00Z',
    checkRuns: [
      { name: 'ansible-lint', status: 'IN_PROGRESS', conclusion: '' },
    ],
  },

  // State: merge conflict — Conflicts badge, Approve button, no Merge
  {
    number: 85,
    title: 'chore(deps): update node.js to v24.1.0',
    url: 'https://github.com/automaat/synapse-infra/pull/85',
    repository: 'automaat/synapse-infra',
    repoName: 'synapse-infra',
    author: 'renovate[bot]',
    isDraft: false,
    labels: ['dependencies'],
    headRefName: 'renovate/node-24.x',
    ciStatus: 'SUCCESS',
    hasPendingChecks: false,
    reviewDecision: '',
    mergeable: 'CONFLICTING',
    unresolvedCount: 0,
    viewerHasApproved: false,
    createdAt: '2026-04-14T06:00:00Z',
    updatedAt: '2026-04-14T09:00:00Z',
    checkRuns: [
      { name: 'lint', status: 'COMPLETED', conclusion: 'SUCCESS' },
      { name: 'test', status: 'COMPLETED', conclusion: 'SUCCESS' },
    ],
  },
]

const ISSUES = [
  {
    number: 487,
    title: 'Agent output panel should auto-scroll when new lines arrive',
    body: 'Currently the output panel does not scroll to the bottom when new stream events come in. The user has to manually scroll.',
    url: 'https://github.com/automaat/sybra/issues/487',
    repository: 'automaat/sybra',
    repoName: 'sybra',
    labels: ['bug', 'ux'],
    author: 'user42',
    createdAt: '2026-04-11T10:00:00Z',
    updatedAt: '2026-04-16T08:00:00Z',
  },
  {
    number: 473,
    title: 'Add keyboard shortcut to approve plan from task detail',
    body: 'Power-users want a keyboard shortcut (e.g. `a`) to approve a plan without reaching for the mouse.',
    url: 'https://github.com/automaat/sybra/issues/473',
    repository: 'automaat/sybra',
    repoName: 'sybra',
    labels: ['enhancement'],
    author: 'poweruser99',
    createdAt: '2026-04-07T15:00:00Z',
    updatedAt: '2026-04-14T12:00:00Z',
  },
  {
    number: 461,
    title: 'Workflow editor loses unsaved changes on navigation',
    body: 'If you click away from the workflow editor without saving, changes are silently discarded with no confirmation dialog.',
    url: 'https://github.com/automaat/sybra/issues/461',
    repository: 'automaat/sybra',
    repoName: 'sybra',
    labels: ['bug', 'workflow'],
    author: 'contributor7',
    createdAt: '2026-04-03T09:00:00Z',
    updatedAt: '2026-04-13T16:00:00Z',
  },
]

// ─── Route interceptor ────────────────────────────────────────────────────────

/**
 * Call before navigating to the GitHub page.
 * Intercepts all three GitHub-data endpoints and returns realistic mock data
 * so screenshots show a populated UI instead of empty/error states.
 */
export async function mockGitHub(page: Page) {
  await page.route('**/api/ReviewService/FetchReviews', (route) =>
    route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify({ createdByMe: MY_PRS, reviewRequested: REVIEW_REQUESTED }),
    }),
  )

  await page.route('**/api/IntegrationService/FetchRenovatePRs', (route) =>
    route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify(RENOVATE_PRS),
    }),
  )

  await page.route('**/api/IntegrationService/FetchAssignedIssues', (route) =>
    route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify(ISSUES),
    }),
  )
}
