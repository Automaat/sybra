import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, screen, cleanup, fireEvent, waitFor } from '@testing-library/svelte'

const ToolApproval = (await import('./ToolApproval.svelte')).default

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

describe('ToolApproval', () => {
  afterEach(cleanup)

  it('calls onrespond with approved=true when Approve clicked', async () => {
    const onrespond = vi.fn().mockResolvedValue(undefined)
    render(ToolApproval, {
      props: { toolUseId: 'tool-1', toolName: 'SomeTool', input: {}, onrespond },
    })

    await fireEvent.click(screen.getByText('Approve'))

    expect(onrespond).toHaveBeenCalledWith('tool-1', true)
  })

  it('calls onrespond with approved=false when Reject clicked', async () => {
    const onrespond = vi.fn().mockResolvedValue(undefined)
    render(ToolApproval, {
      props: { toolUseId: 'tool-1', toolName: 'SomeTool', input: {}, onrespond },
    })

    await fireEvent.click(screen.getByText('Reject'))

    expect(onrespond).toHaveBeenCalledWith('tool-1', false)
  })

  it('disables both buttons while a response is pending', async () => {
    const { promise } = deferred<void>()
    const onrespond = vi.fn().mockReturnValue(promise)
    render(ToolApproval, {
      props: { toolUseId: 'tool-1', toolName: 'SomeTool', input: {}, onrespond },
    })

    await fireEvent.click(screen.getByText('Approve'))

    expect((screen.getByText('Approve').closest('button') as HTMLButtonElement).disabled).toBe(true)
    expect((screen.getByText('Reject').closest('button') as HTMLButtonElement).disabled).toBe(true)
  })

  it('suppresses duplicate submissions while pending', async () => {
    const { promise } = deferred<void>()
    const onrespond = vi.fn().mockReturnValue(promise)
    render(ToolApproval, {
      props: { toolUseId: 'tool-1', toolName: 'SomeTool', input: {}, onrespond },
    })

    await fireEvent.click(screen.getByText('Approve'))
    await fireEvent.click(screen.getByText('Approve'))
    await fireEvent.click(screen.getByText('Reject'))

    expect(onrespond).toHaveBeenCalledTimes(1)
  })

  it('re-enables buttons and shows an error when submission fails', async () => {
    const onrespond = vi.fn().mockRejectedValue(new Error('network error'))
    render(ToolApproval, {
      props: { toolUseId: 'tool-1', toolName: 'SomeTool', input: {}, onrespond },
    })

    await fireEvent.click(screen.getByText('Approve'))

    await waitFor(() => {
      expect(screen.getByRole('alert')).toBeDefined()
    })
    expect(screen.getByRole('alert').textContent).toContain('network error')
    expect((screen.getByText('Approve').closest('button') as HTMLButtonElement).disabled).toBe(false)
    expect((screen.getByText('Reject').closest('button') as HTMLButtonElement).disabled).toBe(false)
  })

  it('allows a retry after a failed submission and clears the error', async () => {
    const onrespond = vi.fn().mockRejectedValueOnce(new Error('network error')).mockResolvedValueOnce(undefined)
    render(ToolApproval, {
      props: { toolUseId: 'tool-1', toolName: 'SomeTool', input: {}, onrespond },
    })

    await fireEvent.click(screen.getByText('Approve'))
    await waitFor(() => {
      expect(screen.getByRole('alert')).toBeDefined()
    })

    await fireEvent.click(screen.getByText('Approve'))

    await waitFor(() => {
      expect(screen.queryByRole('alert')).toBeNull()
    })
    expect(onrespond).toHaveBeenCalledTimes(2)
  })
})
