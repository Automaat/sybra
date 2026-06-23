import { describe, it, expect, afterEach, beforeEach } from 'vitest'
import { render, screen, cleanup, fireEvent } from '@testing-library/svelte'
import { createRawSnippet } from 'svelte'
import ThreePanelLayout from './ThreePanelLayout.svelte'

const snip = (text: string) =>
  createRawSnippet(() => ({ render: () => `<div>${text}</div>` }))

function renderLayout() {
  return render(ThreePanelLayout, {
    props: {
      sidebar: snip('SIDEBAR') as never,
      workspace: snip('WORKSPACE') as never,
      children: snip('CENTER') as never,
    },
  })
}

describe('ThreePanelLayout', () => {
  beforeEach(() => localStorage.clear())
  afterEach(() => {
    cleanup()
    localStorage.clear()
  })

  it('defaults to the two center panes — sidebar and workspace collapsed', () => {
    renderLayout()
    expect(screen.getByText('CENTER')).toBeDefined()
    expect(screen.queryByText('SIDEBAR')).toBeNull()
    expect(screen.queryByText('WORKSPACE')).toBeNull()
  })

  it('reveals the agents sidebar when its toggle is clicked', async () => {
    renderLayout()
    await fireEvent.click(screen.getByTitle('Show agents'))
    expect(screen.getByText('SIDEBAR')).toBeDefined()
  })

  it('reveals the workspace when its toggle is clicked', async () => {
    renderLayout()
    await fireEvent.click(screen.getByTitle('Show workspace'))
    expect(screen.getByText('WORKSPACE')).toBeDefined()
  })

  it('respects a persisted expanded workspace preference', () => {
    localStorage.setItem('sybra.workspace.rightCollapsed', 'false')
    renderLayout()
    expect(screen.getByText('WORKSPACE')).toBeDefined()
  })

  it('respects a persisted expanded sidebar preference', () => {
    localStorage.setItem('sybra.workspace.leftCollapsed', 'false')
    renderLayout()
    expect(screen.getByText('SIDEBAR')).toBeDefined()
  })
})
