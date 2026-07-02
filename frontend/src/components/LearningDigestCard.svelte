<script lang="ts">
  import type { Digest, EvidenceRef, Status, Takeaway } from '../../bindings/github.com/Automaat/sybra/internal/learning/models.js'

  const HISTORY_CAP = 12

  type Props = {
    digests?: Digest[]
    status?: Status | null
    loading?: boolean
    error?: string
  }

  type ViewState = 'error' | 'loading' | 'disabled' | 'empty' | 'invalid' | 'populated'

  let { digests = [], status = null, loading = false, error = '' }: Props = $props()

  const latest = $derived(digests[0] ?? null)
  const history = $derived(digests.slice(1, HISTORY_CAP + 1))
  const hiddenHistoryCount = $derived(Math.max(digests.length - 1 - HISTORY_CAP, 0))
  const state = $derived<ViewState>(deriveState({ digests, status, loading, error }))

  function hasItems<T>(items: T[] | undefined): items is T[] {
    return Array.isArray(items) && items.length > 0
  }

  function validDigest(digest: Digest | null): digest is Digest {
    return !!digest
      && typeof digest.generatedAt === 'string'
      && typeof digest.since === 'string'
      && typeof digest.until === 'string'
      && typeof digest.reportDigest === 'string'
  }

  function hasRenderableSections(digest: Digest): boolean {
    return hasItems(digest.worked)
      || hasItems(digest.notWorked)
      || hasItems(digest.uncertain)
      || hasItems(digest.nextBets)
      || hasItems(digest.promptTakeaways)
      || hasItems(digest.skillTakeaways)
      || hasItems(digest.modelTakeaways)
  }

  function deriveState(input: Props): ViewState {
    if (input.error) return 'error'
    if (input.loading) return 'loading'
    if (input.status && !input.status.enabled) return 'disabled'
    if (!input.digests || input.digests.length === 0) return 'empty'
    const first = input.digests[0] ?? null
    if (!validDigest(first) || !hasRenderableSections(first)) return 'invalid'
    return 'populated'
  }

  function formatDate(value: string | null | undefined): string {
    if (!value) return 'unknown'
    const date = new Date(value)
    if (Number.isNaN(date.getTime())) return value
    return new Intl.DateTimeFormat(undefined, {
      month: 'short',
      day: 'numeric',
      hour: '2-digit',
      minute: '2-digit',
    }).format(date)
  }

  function shortDigest(value: string): string {
    return value.length > 10 ? value.slice(0, 10) : value
  }

  function author(digest: Digest): string {
    const parts = [digest.authorProvider, digest.authorModel].filter(Boolean)
    return parts.length > 0 ? parts.join(' / ') : 'unknown author'
  }
</script>

{#snippet chip(text: string, tone: 'neutral' | 'warning' = 'neutral')}
  <span
    class={[
      'inline-flex max-w-full items-center truncate rounded px-1.5 py-0.5 text-[10px] font-medium',
      tone === 'warning'
        ? 'bg-warning-200 text-warning-800 dark:bg-warning-800 dark:text-warning-200'
        : 'bg-surface-200 text-surface-600 dark:bg-surface-700 dark:text-surface-200',
    ].join(' ')}
  >
    {text}
  </span>
{/snippet}

{#snippet textList(title: string, items: string[] | undefined, tentative = false)}
  {#if hasItems(items)}
    <section class="flex flex-col gap-2">
      <div class="flex items-center gap-2">
        <h4 class="text-xs font-semibold uppercase tracking-normal text-surface-500">{title}</h4>
        {#if tentative}
          {@render chip('tentative · low N', 'warning')}
        {/if}
      </div>
      <ul class="flex flex-col gap-1.5">
        {#each items as item, index (`${index}-${item}`)}
          <li class="rounded border border-surface-200 bg-white px-3 py-2 text-sm dark:border-surface-700 dark:bg-surface-900">
            {item}
          </li>
        {/each}
      </ul>
    </section>
  {/if}
{/snippet}

{#snippet takeawayList(title: string, kind: string, items: Takeaway[] | undefined)}
  {#if hasItems(items)}
    <section class="flex flex-col gap-2">
      <h4 class="text-xs font-semibold uppercase tracking-normal text-surface-500">{title}</h4>
      <ul class="flex flex-col gap-1.5">
        {#each items as item (`${kind}:${item.text}:${item.experimentRef ?? ''}:${item.variantRef ?? ''}`)}
          <li class="rounded border border-surface-200 bg-white px-3 py-2 text-sm dark:border-surface-700 dark:bg-surface-900">
            <div class="flex flex-wrap items-center gap-2">
              {@render chip(kind)}
              <span>{item.text}</span>
            </div>
            {#if item.experimentRef || item.variantRef}
              <div class="mt-2 flex flex-wrap gap-1.5">
                {#if item.experimentRef}
                  {@render chip(`experiment ${item.experimentRef}`)}
                {/if}
                {#if item.variantRef}
                  {@render chip(`variant ${item.variantRef}`)}
                {/if}
              </div>
            {/if}
          </li>
        {/each}
      </ul>
    </section>
  {/if}
{/snippet}

{#snippet evidenceList(items: EvidenceRef[] | undefined)}
  {#if hasItems(items)}
    <div class="flex flex-wrap gap-1.5">
      {#each items as item (`${item.kind}:${item.id}`)}
        {@render chip(`${item.kind} ${item.id}`)}
      {/each}
    </div>
  {/if}
{/snippet}

{#snippet digestBody(digest: Digest)}
  <div class="flex flex-col gap-4">
    <div class="flex flex-wrap items-center gap-2 text-xs text-surface-400">
      <span>{formatDate(digest.since)} → {formatDate(digest.until)}</span>
      <span>generated {formatDate(digest.generatedAt)}</span>
      <span>{author(digest)}</span>
      {@render chip(`report ${shortDigest(digest.reportDigest)}`)}
    </div>

    {@render textList('Worked', digest.worked)}
    {@render textList('Not worked', digest.notWorked)}
    {@render textList('Uncertain', digest.uncertain, true)}
    {@render textList('Next bets', digest.nextBets)}
    {@render takeawayList('Prompt takeaways', 'prompt', digest.promptTakeaways)}
    {@render takeawayList('Skill takeaways', 'skill', digest.skillTakeaways)}
    {@render takeawayList('Model takeaways', 'model', digest.modelTakeaways)}
    {@render evidenceList(digest.evidence)}
  </div>
{/snippet}

<section class="rounded-lg border border-surface-300 bg-surface-50 p-4 dark:border-surface-600 dark:bg-surface-800">
  <div class="mb-3 flex flex-wrap items-center justify-between gap-2">
    <h3 class="text-sm font-semibold text-surface-500">Agent learning journal</h3>
    {#if status?.nextRun}
      <span class="text-xs text-surface-400">next run {formatDate(status.nextRun)}</span>
    {/if}
  </div>

  {#if state === 'error'}
    <p class="text-sm text-error-500">Could not load the learning journal: {error}</p>
  {:else if state === 'loading'}
    <p class="text-sm text-surface-400">Loading the latest learning digest…</p>
  {:else if state === 'disabled'}
    <p class="text-sm text-surface-400">Learning digest pipeline is off. No journal entries will appear until it is enabled.</p>
  {:else if state === 'empty'}
    <p class="text-sm text-surface-400">No learning digests have been generated yet.</p>
  {:else if state === 'invalid' || !latest}
    <p class="text-sm text-warning-600 dark:text-warning-300">Latest learning digest is incomplete and cannot be rendered safely.</p>
  {:else}
    {@render digestBody(latest)}

    {#if history.length > 0}
      <div class="mt-5 border-t border-surface-200 pt-4 dark:border-surface-700">
        <div class="mb-2 flex flex-wrap items-baseline justify-between gap-2">
          <h4 class="text-xs font-semibold uppercase tracking-normal text-surface-500">History</h4>
          {#if hiddenHistoryCount > 0}
            <span class="text-xs text-surface-400">showing {history.length} of {digests.length - 1} older digests</span>
          {/if}
        </div>
        <div class="flex flex-col gap-2">
          {#each history as digest (`${digest.since}:${digest.until}:${digest.reportDigest}`)}
            <details class="rounded border border-surface-200 bg-white p-3 dark:border-surface-700 dark:bg-surface-900">
              <summary class="cursor-pointer text-sm text-surface-600 dark:text-surface-300">
                {formatDate(digest.since)} → {formatDate(digest.until)} · generated {formatDate(digest.generatedAt)}
              </summary>
              <div class="mt-3">
                {@render digestBody(digest)}
              </div>
            </details>
          {/each}
        </div>
      </div>
    {/if}
  {/if}
</section>
