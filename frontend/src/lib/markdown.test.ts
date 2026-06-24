import { describe, it, expect } from 'vitest'
import { renderMarkdown, renderChecklistMarkdown } from './markdown.js'

describe('renderMarkdown', () => {
  it('renders markdown and sanitises raw HTML', () => {
    const html = renderMarkdown('# Hi\n\n<script>alert(1)</script>')
    expect(html).toContain('<h1')
    expect(html).not.toContain('<script')
  })

  it('returns empty string for empty input', () => {
    expect(renderMarkdown('')).toBe('')
    expect(renderMarkdown(null)).toBe('')
  })
})

describe('renderChecklistMarkdown', () => {
  it('replaces GFM task-list checkboxes with non-interactive glyphs', () => {
    const html = renderChecklistMarkdown('- [x] done\n- [ ] todo')
    expect(html).not.toContain('<input')
    // The done glyph must be the checked one, the todo glyph the unchecked one
    // (would catch an inverted checked/unchecked mapping).
    expect(html).toMatch(/<span class="task-check task-check--done"[^>]*>✓<\/span>/)
    expect(html).toMatch(/<span class="task-check"[^>]*>○<\/span>/)
  })

  it('does not mistake a non-checkbox input with "checked" in a value', () => {
    // A raw text input whose value mentions checkbox/checked must not become a
    // checklist glyph.
    const html = renderChecklistMarkdown('<input type="text" value="type=checkbox checked">')
    expect(html).not.toContain('task-check')
  })

  it('leaves non-checklist markdown untouched', () => {
    const html = renderChecklistMarkdown('**bold** text')
    expect(html).toContain('<strong>')
    expect(html).not.toContain('task-check')
  })
})
