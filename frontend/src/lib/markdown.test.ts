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

  // Regression: a bare ``` fence (and any fence whose language isn't in the
  // curated highlight.js set) makes marked fall back to the 'plaintext'
  // grammar. In the `highlight.js/lib/core` build that grammar isn't
  // registered unless imported, so hljs.highlight() threw "Unknown language:
  // plaintext", aborting marked.parse() and blanking every view rendering the
  // markdown — a plan with a bare fence left the task detail stuck on "Loading…".
  it('does not throw on a bare ``` fence (empty language)', () => {
    const md = 'before\n\n```\nplain code line\n```\n\nafter'
    expect(() => renderMarkdown(md)).not.toThrow()
    expect(renderMarkdown(md)).toContain('plain code line')
  })

  it('does not throw on an unregistered fence language', () => {
    expect(() => renderMarkdown('```brainfuck\n+[-]\n```')).not.toThrow()
  })

  it('still highlights a registered language', () => {
    expect(renderMarkdown('```go\npackage main\n```')).toContain('language-go')
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
