<script lang="ts">
  import { MessageSquare, RefreshCw } from '@lucide/svelte'
  import type { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import { renderMarkdown } from '../../lib/markdown.js'
  import { parsePlanDecisions, type PlanDecision } from '../../lib/plan-decisions.js'

  interface Props {
    task: Task
    disabled?: boolean
    hasLiveAgent?: boolean
    onrequest?: (message: string) => void | Promise<void>
    onmessage?: (message: string) => void | Promise<void>
  }

  const { task, disabled = false, hasLiveAgent = false, onrequest, onmessage }: Props = $props()

  const parsed = $derived(parsePlanDecisions(task.planDecisions))
  const renderedBrief = $derived(renderMarkdown(task.planBrief))
  const renderedDecisions = $derived(renderMarkdown(task.planDecisions))

  let selected = $state<Record<string, string>>({})
  let custom = $state<Record<string, string>>({})
  let question = $state('')
  let lastTaskId = $state('')

  $effect(() => {
    if (lastTaskId !== task.id) {
      selected = {}
      custom = {}
      question = ''
      lastTaskId = task.id
    }
    for (const decision of parsed.decisions) {
      const current = selected[decision.id]
      if (!current || (current !== '__custom__' && !isOption(decision, current))) {
        selected[decision.id] = defaultChoice(decision)
      }
    }
  })

  function isOption(decision: PlanDecision, choice: string): boolean {
    return decision.options.some(option => option.label === choice)
  }

  function defaultChoice(decision: PlanDecision): string {
    if (decision.recommended && isOption(decision, decision.recommended)) return decision.recommended
    return decision.options[0]?.label || ''
  }

  function choiceFor(decision: PlanDecision): string {
    const choice = selected[decision.id] || defaultChoice(decision)
    if (choice === '__custom__') return custom[decision.id]?.trim() ?? ''
    if (isOption(decision, choice)) return choice
    return defaultChoice(decision)
  }

  function buildMessage(): string {
    const lines: string[] = []
    if (parsed.decisions.length > 0) {
      lines.push('Decision feedback:')
      for (const decision of parsed.decisions) {
        const choice = choiceFor(decision)
        if (choice) lines.push(`- ${decision.title}: ${choice}`)
      }
    }
    if (question.trim()) {
      if (lines.length > 0) lines.push('')
      lines.push('Question / additional direction:')
      lines.push(question.trim())
    }
    return lines.join('\n')
  }

  function canSend(): boolean {
    return buildMessage().trim().length > 0
  }

  async function requestRevision() {
    const message = buildMessage()
    if (!message.trim() || !onrequest) return
    await onrequest(message)
  }

  async function messagePlanner() {
    const message = buildMessage()
    if (!message.trim() || !onmessage) return
    await onmessage(message)
  }
</script>

{#if task.planBrief || task.planDecisions}
  <section class="flex flex-col gap-3">
    {#if task.planBrief}
      <div class="rounded-md border border-surface-300 bg-surface-50 p-3 dark:border-surface-700 dark:bg-surface-900">
        <div class="mb-2 text-xs font-semibold uppercase tracking-wide text-surface-500">Final brief</div>
        <div class="markdown-body text-sm text-surface-900 dark:text-surface-100">{@html renderedBrief}</div>
      </div>
    {/if}

    {#if parsed.hasOpenDecisions}
      <div class="rounded-md border border-primary-300 bg-primary-50 p-3 dark:border-primary-700 dark:bg-primary-900/20">
        <div class="mb-3 text-xs font-semibold uppercase tracking-wide text-primary-700 dark:text-primary-300">Decisions</div>
        <div class="flex flex-col gap-4">
          {#each parsed.decisions as decision}
            <fieldset class="flex flex-col gap-2">
              <legend class="text-sm font-semibold text-surface-900 dark:text-surface-100">{decision.title}</legend>
              <p class="text-sm text-surface-700 dark:text-surface-300">{decision.question}</p>
              <div class="flex flex-col gap-2">
                {#each decision.options as option}
                  <label class="flex cursor-pointer gap-2 rounded border border-surface-300 bg-white p-2 text-sm dark:border-surface-700 dark:bg-surface-900">
                    <input
                      type="radio"
                      class="mt-1 shrink-0"
                      name={`${task.id}-${decision.id}`}
                      value={option.label}
                      bind:group={selected[decision.id]}
                      disabled={disabled}
                    />
                    <span>
                      <span class="font-medium">{option.label}</span>
                      {#if option.label === decision.recommended}
                        <span class="ml-1 rounded bg-success-100 px-1.5 py-0.5 text-xs text-success-700 dark:bg-success-900/40 dark:text-success-300">recommended</span>
                      {/if}
                      {#if option.description}
                        <span class="block text-xs text-surface-500 dark:text-surface-400">{option.description}</span>
                      {/if}
                    </span>
                  </label>
                {/each}
                <div class="rounded border border-surface-300 bg-white p-2 text-sm dark:border-surface-700 dark:bg-surface-900">
                  <label class="flex cursor-pointer gap-2">
                    <input
                      type="radio"
                      class="mt-1 shrink-0"
                      name={`${task.id}-${decision.id}`}
                      value="__custom__"
                      bind:group={selected[decision.id]}
                      disabled={disabled}
                    />
                    <span class="font-medium">Custom</span>
                  </label>
                  {#if selected[decision.id] === '__custom__'}
                    <textarea
                      class="mt-2 min-h-16 w-full resize-y rounded border border-surface-300 bg-surface-50 p-2 text-sm dark:border-surface-600 dark:bg-surface-800"
                      placeholder="Write your preferred option..."
                      aria-label={`Custom option for ${decision.title}`}
                      bind:value={custom[decision.id]}
                      disabled={disabled}
                    ></textarea>
                  {/if}
                </div>
              </div>
            </fieldset>
          {/each}
        </div>
      </div>
    {:else if task.planDecisions}
      <details class="rounded-md border border-surface-300 bg-surface-50 dark:border-surface-700 dark:bg-surface-900">
        <summary class="cursor-pointer px-3 py-2 text-xs font-medium text-surface-600 dark:text-surface-300">Decision brief</summary>
        <div class="markdown-body border-t border-surface-300 p-3 text-sm dark:border-surface-700">{@html renderedDecisions}</div>
      </details>
    {/if}

    <div class="flex flex-col gap-2">
      <textarea
        class="min-h-20 resize-y rounded-lg border border-surface-300 bg-white p-3 text-sm dark:border-surface-600 dark:bg-surface-800"
        placeholder="Ask a question or add direction for the planner..."
        bind:value={question}
        disabled={disabled}
      ></textarea>
      <div class="flex flex-wrap gap-2">
        {#if onrequest}
          <button
            type="button"
            class="inline-flex items-center gap-2 rounded-lg bg-primary-500 px-3 py-2 text-sm font-medium text-white hover:bg-primary-600 disabled:opacity-50"
            onclick={requestRevision}
            disabled={disabled || !canSend()}
          >
            <RefreshCw size={16} />
            Request Revision
          </button>
        {/if}
        {#if onmessage}
          <button
            type="button"
            class="inline-flex items-center gap-2 rounded-lg bg-surface-800 px-3 py-2 text-sm font-medium text-white hover:bg-surface-900 disabled:opacity-50 dark:bg-surface-200 dark:text-surface-900 dark:hover:bg-white"
            onclick={messagePlanner}
            disabled={disabled || !hasLiveAgent || !canSend()}
            title={hasLiveAgent ? 'Send to live planner' : 'No live planner'}
          >
            <MessageSquare size={16} />
            Ask Planner
          </button>
        {/if}
      </div>
    </div>
  </section>
{/if}
