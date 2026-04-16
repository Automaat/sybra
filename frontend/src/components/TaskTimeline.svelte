<script lang="ts">
  import type { task } from '../../wailsjs/go/models.js'
  import {
    computeTimelineDomain,
    bucketTicks,
    taskBarPosition,
    dueDateMarkerPosition,
    type Zoom,
  } from '../lib/timeline-gantt.js'
  import { STATUS_MAP } from '../lib/statuses.js'
  import { PRIORITY_OPTIONS } from '../lib/priorities.js'

  interface Props {
    tasks: task.Task[]
    focusedTaskId: string | null
    onselect: (id: string) => void
    onfocus: (id: string) => void
  }

  const { tasks, focusedTaskId, onselect, onfocus }: Props = $props()

  let zoom = $state<Zoom>('week')

  const ROW_HEIGHT = 36
  const LABEL_COL_WIDTH = 220

  const now = new Date()

  const domain = $derived(computeTimelineDomain(tasks, now))
  const ticks = $derived(bucketTicks(domain, zoom))

  const ZOOMS: Zoom[] = ['day', 'week', 'month']
  const ZOOM_LABELS: Record<Zoom, string> = { day: 'Day', week: 'Week', month: 'Month' }

  export function cycleZoomIn(): void {
    const idx = ZOOMS.indexOf(zoom)
    if (idx > 0) zoom = ZOOMS[idx - 1]
  }

  export function cycleZoomOut(): void {
    const idx = ZOOMS.indexOf(zoom)
    if (idx < ZOOMS.length - 1) zoom = ZOOMS[idx + 1]
  }

  function statusBarClasses(status: string): string {
    const meta = STATUS_MAP[status]
    return meta ? meta.pillClasses : 'bg-surface-300 dark:bg-surface-600'
  }

  function priorityIcon(p: string | undefined): string {
    return PRIORITY_OPTIONS.find((o) => o.value === (p ?? ''))?.icon ?? '–'
  }

  function priorityClasses(p: string | undefined): string {
    return PRIORITY_OPTIONS.find((o) => o.value === (p ?? ''))?.classes ?? 'text-surface-400'
  }

  function formatDate(d: string | undefined): string {
    if (!d) return '—'
    return new Date(d).toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
  }
</script>

<div class="flex min-h-0 flex-1 flex-col overflow-hidden">
  <!-- Zoom controls -->
  <div class="flex shrink-0 items-center gap-2 border-b border-surface-200 px-4 py-1.5 dark:border-surface-800">
    <span class="text-xs font-medium text-surface-500">Zoom:</span>
    <div class="flex rounded-md border border-surface-300 dark:border-surface-700">
      {#each ZOOMS as z}
        <button
          type="button"
          class="px-2.5 py-0.5 text-xs font-medium transition-colors first:rounded-l-md last:rounded-r-md {zoom === z
            ? 'bg-primary-500 text-white dark:bg-primary-600'
            : 'bg-surface-50 text-surface-600 hover:bg-surface-200 dark:bg-surface-800 dark:text-surface-300 dark:hover:bg-surface-700'}"
          onclick={() => (zoom = z)}
        >
          {ZOOM_LABELS[z]}
        </button>
      {/each}
    </div>
    <span class="ml-2 text-xs text-surface-400">+/- to zoom  ·  J/K to navigate</span>
  </div>

  {#if tasks.length === 0}
    <p class="m-auto text-sm text-surface-400">No tasks match your filters</p>
  {:else}
    <!-- Gantt grid -->
    <div class="min-h-0 flex-1 overflow-auto">
      <!-- min-width ensures horizontal scroll for small ranges -->
      <div style="min-width: {LABEL_COL_WIDTH + 600}px">
        <!-- Header row: label col + time axis -->
        <div
          class="sticky top-0 z-10 flex border-b border-surface-200 bg-surface-100 dark:border-surface-700 dark:bg-surface-900"
          style="height: {ROW_HEIGHT}px"
        >
          <!-- Label column header -->
          <div
            class="sticky left-0 z-20 shrink-0 border-r border-surface-200 bg-surface-100 px-3 dark:border-surface-700 dark:bg-surface-900"
            style="width: {LABEL_COL_WIDTH}px; height: {ROW_HEIGHT}px; line-height: {ROW_HEIGHT}px"
          >
            <span class="text-xs font-semibold uppercase tracking-wider text-surface-500">Task</span>
          </div>
          <!-- Ticks -->
          <div class="relative flex-1 overflow-hidden">
            {#each ticks as tick}
              <span
                class="pointer-events-none absolute top-0 text-xs text-surface-400"
                style="left: {tick.leftPct}%; padding-left: 4px; line-height: {ROW_HEIGHT}px; white-space: nowrap"
              >
                {tick.label}
              </span>
              <!-- tick line -->
              <span
                class="pointer-events-none absolute top-0 w-px bg-surface-200 dark:bg-surface-700"
                style="left: {tick.leftPct}%; height: {ROW_HEIGHT}px"
              ></span>
            {/each}
          </div>
        </div>

        <!-- Task rows -->
        {#each tasks as t (t.id)}
          {@const bar = taskBarPosition(t, domain, now)}
          {@const duePct = dueDateMarkerPosition(t, domain)}
          {@const isFocused = focusedTaskId === t.id}
          <!-- svelte-ignore a11y_click_events_have_key_events -->
          <!-- svelte-ignore a11y_no_static_element_interactions -->
          <div
            data-focused-task={isFocused ? '' : undefined}
            class="flex border-b border-surface-100 transition-colors dark:border-surface-800 {isFocused
              ? 'bg-primary-50 dark:bg-primary-900/20'
              : 'hover:bg-surface-50 dark:hover:bg-surface-800/50'}"
            style="height: {ROW_HEIGHT}px"
            onclick={() => onselect(t.id)}
            onmouseenter={() => onfocus(t.id)}
          >
            <!-- Label column (sticky left) -->
            <div
              class="sticky left-0 z-10 flex shrink-0 cursor-pointer items-center gap-1.5 overflow-hidden border-r border-surface-200 px-2 dark:border-surface-700 {isFocused
                ? 'bg-primary-50 dark:bg-primary-900/20'
                : 'bg-surface-50 dark:bg-surface-950'}"
              style="width: {LABEL_COL_WIDTH}px"
            >
              <span class="shrink-0 font-mono text-sm {priorityClasses(t.priority)}" title="Priority">
                {priorityIcon(t.priority)}
              </span>
              <span class="truncate text-sm font-medium">{t.title}</span>
              <span class="ml-auto shrink-0 text-xs text-surface-400">
                {formatDate(t.createdAt)}
              </span>
            </div>

            <!-- Bar area -->
            <div class="relative flex-1">
              <!-- Tick grid lines -->
              {#each ticks as tick}
                <span
                  class="pointer-events-none absolute inset-y-0 w-px bg-surface-100 dark:bg-surface-800"
                  style="left: {tick.leftPct}%"
                ></span>
              {/each}

              <!-- Task bar -->
              <div
                class="absolute inset-y-1 cursor-pointer rounded {statusBarClasses(t.status)} opacity-80 transition-opacity hover:opacity-100"
                style="left: {bar.leftPct}%; width: {bar.widthPct}%"
                title="{t.title} · {t.status}"
              ></div>

              <!-- Due date marker (diamond) -->
              {#if duePct !== null}
                <span
                  class="pointer-events-none absolute text-warning-500 dark:text-warning-400"
                  style="left: calc({duePct}% - 5px); top: 50%; transform: translateY(-50%); font-size: 10px"
                  title="Due {formatDate(t.dueDate)}"
                >◆</span>
              {/if}
            </div>
          </div>
        {/each}
      </div>
    </div>
  {/if}
</div>
