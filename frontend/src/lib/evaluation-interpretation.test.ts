import { describe, expect, it } from 'vitest'
import { ComparisonBreakdown } from '../../bindings/github.com/Automaat/sybra/internal/evaluation/models.js'
import { interpretExperiment } from './evaluation-interpretation.js'

function row(overrides: Partial<ComparisonBreakdown> = {}): ComparisonBreakdown {
  return ComparisonBreakdown.createFrom({
    key: 'exp-a:v1:implementation',
    experimentId: 'exp-a',
    variantId: 'v1',
    role: 'implementation',
    runs: 20,
    landed: 10,
    merged: 8,
    mergeRate: 0.8,
    mergedWithEditsRate: 0.1,
    ciFirstPassRate: 0.9,
    reworkRate: 0.1,
    revertRate: 0,
    durationP50S: 600,
    durationP90S: 1_000,
    totalCostUsd: 50,
    costPerLanded: 5,
    premiumRequests: 20,
    premiumRequestsPerLanded: 2,
    turnsPerLanded: 3,
    toolsPerLanded: 10,
    ...overrides,
  })
}

function peer(overrides: Partial<ComparisonBreakdown> = {}): ComparisonBreakdown {
  return row({
    key: 'exp-a:v2:implementation',
    variantId: 'v2',
    durationP90S: 900,
    costPerLanded: 5,
    premiumRequestsPerLanded: 2,
    ...overrides,
  })
}

describe('interpretExperiment', () => {
  it('marks low-sample rows as underpowered before other verdicts', () => {
    const result = interpretExperiment(row({ insufficientData: true, landed: 0, ciFirstPassRate: 0.2 }), [peer()])

    expect(result.verdict).toBe('underpowered')
    expect(result.verdictLabel).toBe('Underpowered')
  })

  it('marks rows with no landed tasks as needing more data', () => {
    const result = interpretExperiment(row({ landed: 0, qualityAttributionLimited: false }), [peer()])

    expect(result.verdict).toBe('needs-more-data')
    expect(result.verdictReason).toContain('No landed tasks')
  })

  it('marks limited quality attribution as needing more data', () => {
    const result = interpretExperiment(row({ landed: 0, qualityAttributionLimited: true }), [peer()])

    expect(result.verdict).toBe('needs-more-data')
    expect(result.limitedSignals).toContain(
      'Quality attribution is limited because this variant has runs but no landed tasks.',
    )
  })

  it('calculates the primary metric as landed per run', () => {
    const result = interpretExperiment(row({ runs: 25, landed: 5 }), [peer()])

    expect(result.primaryLabel).toBe('Landed/run')
    expect(result.primaryValue).toBe('20%')
    expect(result.primaryDetail).toBe('5/25 landed')
  })

  it('normalizes zero runs without producing a misleading ratio', () => {
    const result = interpretExperiment(row({ runs: 0, landed: 0 }), [peer()])

    expect(result.primaryValue).toBe('—')
    expect(result.primaryDetail).toBe('0/0 landed')
    expect(result.verdict).toBe('needs-more-data')
  })

  it('marks reliability and quality guardrail breaches as risky', () => {
    const result = interpretExperiment(row({ ciFirstPassRate: 0.59 }), [peer()])

    expect(result.verdict).toBe('risky')
    expect(result.guardrails.find((g) => g.key === 'ci-first-pass')?.status).toBe('breach')
    expect(result.guardrails.find((g) => g.key === 'ci-first-pass')?.category).toBe('risk')
  })

  it('marks peer-relative outlier breaches as costly', () => {
    const result = interpretExperiment(row({ durationP90S: 1_500, costPerLanded: 5 }), [
      peer({ durationP90S: 1_000, costPerLanded: 10, premiumRequestsPerLanded: 3 }),
    ])

    expect(result.verdict).toBe('costly')
    expect(result.guardrails.find((g) => g.key === 'durationP90S')?.status).toBe('breach')
    expect(result.guardrails.find((g) => g.key === 'durationP90S')?.category).toBe('cost')
  })

  it('assigns explicit categories to every guardrail', () => {
    const result = interpretExperiment(row(), [peer()])

    expect(result.guardrails.map((g) => [g.key, g.category])).toEqual([
      ['ci-first-pass', 'risk'],
      ['edited-merge', 'risk'],
      ['rework', 'risk'],
      ['revert', 'risk'],
      ['durationP90S', 'cost'],
      ['costPerLanded', 'cost'],
      ['premiumRequestsPerLanded', 'cost'],
    ])
  })

  it('marks rows as promising when no guardrail breaches', () => {
    const result = interpretExperiment(row(), [peer()])

    expect(result.verdict).toBe('promising')
    expect(result.guardrails.every((g) => g.status === 'ok' || g.status === 'limited')).toBe(true)
  })

  it('does not breach duration, cost, or premium without usable peers', () => {
    const result = interpretExperiment(row({ durationP90S: 10_000, costPerLanded: 500 }), [])

    expect(result.verdict).toBe('promising')
    expect(result.guardrails.filter((g) => g.status === 'limited').map((g) => g.key)).toEqual([
      'durationP90S',
      'costPerLanded',
      'premiumRequestsPerLanded',
    ])
    expect(result.limitedSignals).toContain('Duration p90 has no usable same-experiment peer baseline.')
  })

  it('ignores low-N peers and peers without positive values', () => {
    const result = interpretExperiment(row({ durationP90S: 10_000, costPerLanded: 500 }), [
      peer({ insufficientData: true, durationP90S: 1, costPerLanded: 1, premiumRequestsPerLanded: 1 }),
      peer({
        key: 'exp-a:v3:implementation',
        variantId: 'v3',
        durationP90S: 0,
        costPerLanded: 0,
        premiumRequestsPerLanded: 0,
      }),
    ])

    expect(result.verdict).toBe('promising')
    expect(result.guardrails.filter((g) => g.status === 'limited')).toHaveLength(3)
  })

  it('uses only same-experiment same-role positive peers for outlier medians', () => {
    const result = interpretExperiment(row({ durationP90S: 1_600 }), [
      peer({ key: 'exp-b:v2:implementation', experimentId: 'exp-b', durationP90S: 100 }),
      peer({ key: 'exp-a:v3:review', variantId: 'v3', role: 'review', durationP90S: 100 }),
      peer({ key: 'exp-a:v4:implementation', variantId: 'v4', durationP90S: 1_000 }),
    ])

    expect(result.verdict).toBe('costly')
    expect(result.guardrails.find((g) => g.key === 'durationP90S')?.detail).toContain('1.60x')
  })
})
