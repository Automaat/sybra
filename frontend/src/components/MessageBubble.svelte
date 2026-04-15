<script lang="ts">
  import type { agent } from '../../wailsjs/go/models.js'
  import { renderMarkdown } from '../lib/markdown.js'
  import DiffViewer from './DiffViewer.svelte'

  interface Props {
    event: agent.ConvoEvent
  }

  const { event }: Props = $props()

  const isUserInput = $derived(event.type === 'user_input')
  const isAssistant = $derived(event.type === 'assistant')
  const isToolResult = $derived(event.type === 'user')
  const isResult = $derived(event.type === 'result')
  const isSystem = $derived(event.type === 'system')

  const renderedText = $derived(renderMarkdown(event.text))

  function truncate(text: string, max: number): string {
    if (text.length <= max) return text
    return text.slice(0, max) + '...'
  }

  function isEditTool(name: string): boolean {
    return name === 'Edit' || name === 'Write'
  }
</script>

{#if isSystem}
  <!-- Skip system init events in chat display -->
{:else if isUserInput}
  <!-- User message -->
  <div class="flex justify-end">
    <div class="max-w-[80%] rounded-lg bg-primary-600 px-4 py-2.5 text-sm text-white dark:bg-primary-700">
      <pre class="whitespace-pre-wrap break-words font-sans">{event.text}</pre>
    </div>
  </div>

{:else if isAssistant}
  <div class="flex flex-col gap-2">
    <!-- Assistant text (markdown) -->
    {#if event.text}
      <div class="markdown-body max-w-[90%] rounded-lg bg-surface-100 px-4 py-2.5 text-sm text-surface-900 dark:bg-surface-800 dark:text-surface-100">
        {@html renderedText}
      </div>
    {/if}

    <!-- Tool uses -->
    {#if event.toolUses?.length}
      {#each event.toolUses as tool (tool.id)}
        <div class="rounded-lg border border-surface-300 bg-surface-50 dark:border-surface-600 dark:bg-surface-900">
          <div class="flex items-center gap-2 border-b border-surface-200 px-3 py-1.5 dark:border-surface-700">
            <span class="rounded bg-secondary-200 px-1.5 py-0.5 text-[10px] font-bold text-secondary-800 dark:bg-secondary-700 dark:text-secondary-200">
              TOOL
            </span>
            <span class="text-xs font-medium text-surface-700 dark:text-surface-300">{tool.name}</span>
            {#if tool.input?.file_path}
              <span class="text-xs text-surface-500">{tool.input.file_path}</span>
            {/if}
          </div>
          <div class="px-3 py-2">
            {#if isEditTool(tool.name) && tool.input?.old_string !== undefined}
              <DiffViewer
                oldText={String(tool.input.old_string ?? '')}
                newText={String(tool.input.new_string ?? '')}
                filePath={String(tool.input.file_path ?? '')}
              />
            {:else if tool.name === 'Bash' && tool.input?.command}
              <pre class="rounded bg-surface-900 px-2 py-1.5 text-xs text-success-400 dark:bg-surface-950">{tool.input.command}</pre>
            {:else if tool.name === 'Write' && tool.input?.content}
              <pre class="max-h-40 overflow-y-auto rounded bg-surface-900 px-2 py-1.5 text-xs text-surface-200 dark:bg-surface-950">{truncate(String(tool.input.content), 500)}</pre>
            {:else if tool.input}
              <pre class="max-h-32 overflow-y-auto text-xs text-surface-600 dark:text-surface-400">{JSON.stringify(tool.input, null, 2)}</pre>
            {/if}
          </div>
        </div>
      {/each}
    {/if}
  </div>

{:else if isToolResult}
  {#if event.toolResults?.length}
    {#each event.toolResults as result (result.toolUseId)}
      <div class="ml-4 rounded border-l-2 px-3 py-1.5 text-xs
        {result.isError
          ? 'border-error-500 bg-error-50 text-error-800 dark:bg-error-950 dark:text-error-200'
          : 'border-success-500 bg-success-50 text-success-800 dark:bg-success-950 dark:text-success-200'}">
        <pre class="max-h-40 overflow-y-auto whitespace-pre-wrap break-words">{truncate(result.content, 1000)}</pre>
      </div>
    {/each}
  {/if}

{:else if isResult}
  <div class="flex items-center gap-2 border-t border-surface-200 pt-2 text-xs text-surface-500 dark:border-surface-700">
    <span class="rounded bg-warning-200 px-1.5 py-0.5 text-[10px] font-bold text-warning-800 dark:bg-warning-700 dark:text-warning-200">
      DONE
    </span>
    {#if event.costUsd}
      <span>${event.costUsd.toFixed(4)}</span>
    {/if}
    {#if event.inputTokens || event.outputTokens}
      <span>{event.inputTokens?.toLocaleString()}↓ {event.outputTokens?.toLocaleString()}↑</span>
    {/if}
  </div>
{/if}

<style>
  :global(.markdown-body p) { margin: 0.25em 0; }
  :global(.markdown-body pre) {
    margin: 0.5em 0;
    border-radius: 0.375rem;
    overflow-x: auto;
    font-size: 0.75rem;
  }
  :global(.markdown-body pre code.hljs) {
    border-radius: 0.375rem;
    font-size: 0.75rem;
  }
  :global(.markdown-body code:not(.hljs)) {
    font-size: 0.8em;
    padding: 0.1em 0.3em;
    border-radius: 0.25rem;
    background: rgb(var(--color-surface-800) / 0.5);
  }
  :global(.markdown-body ul, .markdown-body ol) { padding-left: 1.5em; margin: 0.25em 0; }
  :global(.markdown-body h1, .markdown-body h2, .markdown-body h3) { margin: 0.5em 0 0.25em; font-weight: 600; }
  :global(.markdown-body blockquote) { border-left: 3px solid currentColor; padding-left: 0.75em; opacity: 0.8; margin: 0.25em 0; }
  :global(.markdown-body a) { text-decoration: underline; }
</style>
