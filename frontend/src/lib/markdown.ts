import DOMPurify from 'dompurify'
import { marked } from 'marked'
import { markedHighlight } from 'marked-highlight'
// Import highlight.js core + a curated language set instead of the default
// `highlight.js` build, which registers ~190 languages and ships as a
// ~915 kB / 304 kB-gzip chunk loaded eagerly (markdown rendering is on the
// initial paint path). Registering only the languages we actually render in
// agent output / task bodies / plans cuts that chunk by ~80%. Unregistered
// languages fall back to 'plaintext' via the guard in highlight() below, so
// the only cost is no syntax colour on an exotic language — never a crash.
import hljs from 'highlight.js/lib/core'
import bash from 'highlight.js/lib/languages/bash'
import shell from 'highlight.js/lib/languages/shell'
import javascript from 'highlight.js/lib/languages/javascript'
import typescript from 'highlight.js/lib/languages/typescript'
import json from 'highlight.js/lib/languages/json'
import yaml from 'highlight.js/lib/languages/yaml'
import go from 'highlight.js/lib/languages/go'
import python from 'highlight.js/lib/languages/python'
import rust from 'highlight.js/lib/languages/rust'
import markdown from 'highlight.js/lib/languages/markdown'
import diff from 'highlight.js/lib/languages/diff'
import dockerfile from 'highlight.js/lib/languages/dockerfile'
import sql from 'highlight.js/lib/languages/sql'
import xml from 'highlight.js/lib/languages/xml'
import css from 'highlight.js/lib/languages/css'
import 'highlight.js/styles/github-dark.css'

for (const [name, lang] of Object.entries({
  bash, shell, javascript, typescript, json, yaml, go, python,
  rust, markdown, diff, dockerfile, sql, xml, css,
})) {
  hljs.registerLanguage(name, lang)
}
// Aliases so fenced blocks tagged with common short names still highlight.
hljs.registerAliases(['js'], { languageName: 'javascript' })
hljs.registerAliases(['ts'], { languageName: 'typescript' })
hljs.registerAliases(['py'], { languageName: 'python' })
hljs.registerAliases(['yml'], { languageName: 'yaml' })
hljs.registerAliases(['html'], { languageName: 'xml' })
hljs.registerAliases(['sh', 'zsh', 'console'], { languageName: 'shell' })

// Configure marked ONCE at module load. Previously this ran in the
// <script> block of TaskDetail.svelte and MessageBubble.svelte, which
// re-ran on every component mount — marked.use() is additive, so each
// mount stacked another copy of the highlight extension onto the global
// marked instance. After a chat with hundreds of MessageBubble mounts,
// marked.parse() iterated hundreds of extensions per call, saturating
// the WebKit main thread and freezing the UI.
marked.use(
  markedHighlight({
    langPrefix: 'hljs language-',
    highlight(code: string, lang: string) {
      const language = hljs.getLanguage(lang) ? lang : 'plaintext'
      return hljs.highlight(code, { language }).value
    },
  }),
)
marked.setOptions({ breaks: true, gfm: true })

// Bounded LRU cache so identical text doesn't re-parse across components.
// A plain Map grew without bound: streaming agent output re-renders the same
// message at every token length, so a single long message minted hundreds of
// distinct keys that were never evicted. Map preserves insertion order, so the
// first key is the oldest — evict it once we exceed the cap.
const CACHE_MAX = 500
const cache = new Map<string, string>()

export function renderMarkdown(text: string | undefined | null): string {
  if (!text) return ''
  const cached = cache.get(text)
  if (cached !== undefined) {
    // Mark as most-recently-used.
    cache.delete(text)
    cache.set(text, cached)
    return cached
  }
  const html = DOMPurify.sanitize(marked.parse(text) as string)
  cache.set(text, html)
  if (cache.size > CACHE_MAX) {
    cache.delete(cache.keys().next().value as string)
  }
  return html
}

// GFM task lists render as disabled <input type=checkbox> — the strongest
// "click me" signifier in the body, yet inert. Swap each for a non-interactive
// status glyph so a checklist reads as progress, not a dead control. Parse the
// (already-sanitised) HTML so we key off the real `checked` attribute, never a
// stray "checked"/"checkbox" sitting inside some other attribute's value.
export function renderChecklistMarkdown(text: string | undefined | null): string {
  const html = renderMarkdown(text)
  if (!html.includes('type="checkbox"') || typeof DOMParser === 'undefined') return html
  const doc = new DOMParser().parseFromString(html, 'text/html')
  for (const input of Array.from(doc.querySelectorAll('input[type="checkbox"]'))) {
    const done = input.hasAttribute('checked')
    const span = doc.createElement('span')
    span.className = done ? 'task-check task-check--done' : 'task-check'
    span.setAttribute('aria-hidden', 'true')
    span.textContent = done ? '✓' : '○'
    input.replaceWith(span)
  }
  return doc.body.innerHTML
}
