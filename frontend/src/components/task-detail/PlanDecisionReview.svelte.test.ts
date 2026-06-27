import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'

vi.mock('../../lib/markdown.js', () => ({
  renderMarkdown: vi.fn((text: string | undefined | null) => text ?? ''),
}))

const PlanDecisionReview = (await import('./PlanDecisionReview.svelte')).default

const baseTask = {
  id: 't1',
  title: 'Plan it',
  status: 'plan-review',
  body: '',
  tags: [],
  agentMode: 'headless',
  allowedTools: [],
  createdAt: '2026-04-01T00:00:00Z',
  updatedAt: '2026-04-01T00:00:00Z',
  planBrief: '# Brief\nUse the compact execution contract.',
  planDecisions: `# Decisions

## Storage shape
Question: Which artifact should implementation agents read?
Recommended: Execution contract only

Options:
- Execution contract only - smallest prompt
- Full research bundle - more context
`,
}

describe('PlanDecisionReview', () => {
  afterEach(cleanup)

  it('renders the brief, open decisions, and recommended option', () => {
    render(PlanDecisionReview, { props: { task: baseTask as never } })

    expect(screen.getByText('Final brief')).toBeDefined()
    expect(screen.getByText('Storage shape')).toBeDefined()
    expect(screen.getByText('recommended')).toBeDefined()
  })

  it('requests revision with the selected decision choice', async () => {
    const onrequest = vi.fn()
    render(PlanDecisionReview, {
      props: { task: baseTask as never, onrequest },
    })

    await fireEvent.click(screen.getByText('Request Revision'))

    expect(onrequest).toHaveBeenCalledWith('Decision feedback:\n- Storage shape: Execution contract only')
  })

  it('sends custom choices and questions to a live planner', async () => {
    const onmessage = vi.fn()
    render(PlanDecisionReview, {
      props: { task: baseTask as never, hasLiveAgent: true, onmessage },
    })

    await fireEvent.click(screen.getByLabelText('Custom'))
    await fireEvent.input(screen.getByPlaceholderText('Write your preferred option...'), {
      target: { value: 'Split contract and evidence, but keep links between them' },
    })
    await fireEvent.input(screen.getByPlaceholderText('Ask a question or add direction for the planner...'), {
      target: { value: 'Can you show why this is still autonomous?' },
    })
    await fireEvent.click(screen.getByText('Ask Planner'))

    expect(onmessage).toHaveBeenCalledWith(
      'Decision feedback:\n' +
        '- Storage shape: Split contract and evidence, but keep links between them\n' +
        '\n' +
        'Question / additional direction:\n' +
        'Can you show why this is still autonomous?',
    )
  })

  it('resets local question and choices when the task changes', async () => {
    const onrequest = vi.fn()
    const view = render(PlanDecisionReview, {
      props: { task: baseTask as never, onrequest },
    })

    await fireEvent.input(screen.getByPlaceholderText('Ask a question or add direction for the planner...'), {
      target: { value: 'Carry-over question' },
    })

    await view.rerender({
      task: {
        ...baseTask,
        id: 't2',
        planDecisions: `# Decisions

## Runtime choice
Question: Which path should the planner use?
Recommended: Safe path

Options:
- Safe path - lower risk
- Fast path - more churn
`,
      } as never,
      onrequest,
    })
    await fireEvent.click(screen.getByText('Request Revision'))

    expect(onrequest).toHaveBeenCalledWith('Decision feedback:\n- Runtime choice: Safe path')
  })

  it('falls back to the first option when Recommended does not match an option', async () => {
    const onrequest = vi.fn()
    render(PlanDecisionReview, {
      props: {
        task: {
          ...baseTask,
          planDecisions: `# Decisions

## Storage shape
Question: Which artifact should implementation agents read?
Recommended: Missing option

Options:
- Execution contract only - smallest prompt
- Full research bundle - more context
`,
        } as never,
        onrequest,
      },
    })

    await fireEvent.click(screen.getByText('Request Revision'))

    expect(onrequest).toHaveBeenCalledWith('Decision feedback:\n- Storage shape: Execution contract only')
  })
})
