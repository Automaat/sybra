import type { Page } from '@playwright/test'

const PROJECT_ID = 'automaat/sybra'

const PROJECT = {
  id: PROJECT_ID,
  name: 'sybra',
  owner: 'automaat',
  repo: 'sybra',
  url: 'https://github.com/automaat/sybra',
  clonePath: '/data/synapse/clones/automaat-sybra',
  type: 'pet',
  status: 'cloned',
  setupCommands: ['mise install', 'cd frontend && npm install'],
  sandbox: {
    image: 'ubuntu:24.04',
    build: 'docker build -t sybra-dev .',
    with: ['docker', 'mise'],
    port: 8080,
    env: { NODE_ENV: 'development', GO_ENV: 'development' },
  },
  checks: {
    preCommit: ['golangci-lint run ./...', 'cd frontend && npx oxlint .'],
    prePush: ['go test ./...'],
  },
  createdAt: '2026-01-15T10:00:00Z',
  updatedAt: '2026-04-16T12:00:00Z',
}

const WORKTREES = [
  {
    path: '/data/synapse/worktrees/automaat-sybra-feat-streaming',
    branch: 'feat/streaming-output',
    taskId: 'a1b2c3d4',
    head: 'f3a8c21',
  },
  {
    path: '/data/synapse/worktrees/automaat-sybra-fix-kanban',
    branch: 'fix/kanban-mobile',
    taskId: 'e5f6a7b8',
    head: 'b9d4e12',
  },
]

export async function mockProjects(page: Page) {
  await page.route('**/api/ProjectService/ListProjects', (route) =>
    route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify([PROJECT]),
    }),
  )

  await page.route('**/api/ProjectService/GetProject', (route) =>
    route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify(PROJECT),
    }),
  )

  await page.route('**/api/ProjectService/ListWorktrees', (route) =>
    route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify(WORKTREES),
    }),
  )
}

export { PROJECT_ID }
