import { marked } from 'marked'
import { markedHighlight } from 'marked-highlight'
import hljs from 'highlight.js'
import 'highlight.js/styles/github-dark.css'

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

// Shared cache so identical text doesn't re-parse across components.
const cache = new Map<string, string>()

export function renderMarkdown(text: string | undefined | null): string {
  if (!text) return ''
  const cached = cache.get(text)
  if (cached !== undefined) return cached
  const html = marked.parse(text) as string
  cache.set(text, html)
  return html
}
