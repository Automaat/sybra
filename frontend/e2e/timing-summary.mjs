#!/usr/bin/env node
// Parses test-results/results.json (Playwright JSON reporter) and writes
// a Markdown timing table to stdout for appending to $GITHUB_STEP_SUMMARY.
// Run from the frontend/ directory: node e2e/timing-summary.mjs
import { readFileSync } from 'node:fs'

const data = JSON.parse(readFileSync('test-results/results.json', 'utf8'))
const rows = []

function extractSpecs(suite) {
  for (const spec of suite.specs ?? []) {
    const results = spec.tests?.[0]?.results ?? []
    if (results.length > 0) {
      const last = results.at(-1)
      rows.push({
        title: spec.fullTitle ?? spec.title,
        duration: last.duration,
        status: last.status,
        retry: last.retry,
      })
    }
  }
  for (const child of suite.suites ?? []) {
    extractSpecs(child)
  }
}

for (const suite of data.suites ?? []) {
  extractSpecs(suite)
}

rows.sort((a, b) => b.duration - a.duration)

const totalMs = data.stats?.duration ?? 0

function statusIcon(status, retry) {
  if (status === 'passed' && retry > 0) return '⚠️ flaky'
  if (status === 'passed') return '✅'
  if (status === 'skipped') return '⏭️'
  return '❌'
}

const lines = [
  `**Total:** ${rows.length} tests · ${Math.round(totalMs / 1000)}s`,
  '',
  '| Test | Duration | Status |',
  '|------|----------|--------|',
  ...rows.map((r) => `| ${r.title} | ${r.duration}ms | ${statusIcon(r.status, r.retry)} |`),
  '',
]

process.stdout.write(lines.join('\n'))
