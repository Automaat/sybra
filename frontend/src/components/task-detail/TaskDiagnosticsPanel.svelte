<script lang="ts">
  import { ListTaskArtifacts, GetTaskSetupLog, ListTaskAuditEvents } from '$lib/api'
  import type { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'

  interface Props {
    task: Task
  }

  interface TaskArtifact {
    name: string
    kind: string
    producerRole?: string
    stepId?: string
    createdAt: string
    size: number
    stream?: boolean
    content?: string
    error?: string
  }

  interface TaskSetupLog {
    taskId: string
    path?: string
    exists: boolean
    content?: string
    truncated?: boolean
  }

  interface TaskAuditEvent {
    ts: string
    type: string
    taskId?: string
    agentId?: string
    data?: Record<string, unknown>
  }

  const { task }: Props = $props()

  let loading = $state(false)
  let error = $state('')
  let artifacts = $state<TaskArtifact[]>([])
  let setupLog = $state<TaskSetupLog | null>(null)
  let auditEvents = $state<TaskAuditEvent[]>([])
  let selectedArtifact = $state('')

  const selectedArtifactRecord = $derived(
    artifacts.find((a) => a.name === selectedArtifact) ?? artifacts[0] ?? null,
  )

  $effect(() => {
    void loadDiagnostics(task.id)
  })

  async function loadDiagnostics(taskId: string) {
    loading = true
    error = ''
    try {
      const [artifactRows, setup, auditRows] = await Promise.all([
        ListTaskArtifacts(taskId) as Promise<TaskArtifact[]>,
        GetTaskSetupLog(taskId) as Promise<TaskSetupLog>,
        ListTaskAuditEvents(taskId, 30) as Promise<TaskAuditEvent[]>,
      ])
      artifacts = artifactRows ?? []
      setupLog = setup
      auditEvents = auditRows ?? []
      selectedArtifact = artifacts[0]?.name ?? ''
    } catch (e) {
      error = String(e)
    } finally {
      loading = false
    }
  }

  function formatDate(value: string | undefined): string {
    if (!value) return 'unknown'
    const d = new Date(value)
    if (Number.isNaN(d.getTime())) return value
    return d.toLocaleString()
  }

  function formatData(data: Record<string, unknown> | undefined): string {
    if (!data || Object.keys(data).length === 0) return '{}'
    return JSON.stringify(data, null, 2)
  }
</script>

<section class="flex flex-col gap-4">
  <div class="flex items-center justify-between gap-3">
    <div>
      <h2 class="text-sm font-semibold">Task diagnostics</h2>
      <p class="text-xs text-surface-500">Local artifacts, setup log, and audit events for this task.</p>
    </div>
    <button
      type="button"
      class="rounded-md border border-surface-300 px-2.5 py-1 text-xs font-medium hover:bg-surface-100 disabled:opacity-50 dark:border-surface-700 dark:hover:bg-surface-800"
      onclick={() => loadDiagnostics(task.id)}
      disabled={loading}
    >
      {loading ? 'Refreshing...' : 'Refresh'}
    </button>
  </div>

  {#if error}
    <p class="rounded-md border border-error-200 bg-error-50 p-3 text-sm text-error-700 dark:border-error-800 dark:bg-error-950 dark:text-error-300">{error}</p>
  {/if}

  <div class="grid gap-4 xl:grid-cols-2">
    <section class="flex min-w-0 flex-col gap-3 rounded-lg border border-surface-200 p-3 dark:border-surface-700">
      <div class="flex items-center justify-between gap-2">
        <h3 class="text-xs font-semibold uppercase tracking-wide text-surface-500">Artifacts</h3>
        <span class="text-xs text-surface-400">{artifacts.length}</span>
      </div>
      {#if artifacts.length === 0}
        <p class="text-sm text-surface-500">No artifacts recorded for this task.</p>
      {:else}
        <div class="flex flex-wrap gap-2">
          {#each artifacts as artifact (artifact.name)}
            <button
              type="button"
              class="rounded-md border px-2 py-1 text-xs font-medium {selectedArtifactRecord?.name === artifact.name ? 'border-primary-400 bg-primary-50 text-primary-700 dark:bg-primary-950 dark:text-primary-300' : 'border-surface-300 hover:bg-surface-100 dark:border-surface-700 dark:hover:bg-surface-800'}"
              onclick={() => (selectedArtifact = artifact.name)}
            >
              {artifact.name}
            </button>
          {/each}
        </div>
        {#if selectedArtifactRecord}
          <div class="flex flex-wrap gap-x-3 gap-y-1 text-xs text-surface-500">
            <span>{selectedArtifactRecord.kind}</span>
            <span>{selectedArtifactRecord.size} bytes</span>
            <span>{formatDate(selectedArtifactRecord.createdAt)}</span>
            {#if selectedArtifactRecord.producerRole}<span>{selectedArtifactRecord.producerRole}</span>{/if}
          </div>
          {#if selectedArtifactRecord.error}
            <p class="text-xs text-warning-600 dark:text-warning-400">{selectedArtifactRecord.error}</p>
          {/if}
          <pre class="max-h-80 overflow-auto rounded-md bg-surface-100 p-3 text-xs text-surface-900 dark:bg-surface-900 dark:text-surface-100">{selectedArtifactRecord.content || '(empty)'}</pre>
        {/if}
      {/if}
    </section>

    <section class="flex min-w-0 flex-col gap-3 rounded-lg border border-surface-200 p-3 dark:border-surface-700">
      <div class="flex items-center justify-between gap-2">
        <h3 class="text-xs font-semibold uppercase tracking-wide text-surface-500">Worktree setup log</h3>
        {#if setupLog?.truncated}<span class="text-xs text-warning-600 dark:text-warning-400">tail shown</span>{/if}
      </div>
      {#if !setupLog?.exists}
        <p class="text-sm text-surface-500">No setup log found for this task.</p>
        {#if setupLog?.path}<code class="break-all text-xs text-surface-400">{setupLog.path}</code>{/if}
      {:else}
        {#if setupLog.path}<code class="break-all text-xs text-surface-500">{setupLog.path}</code>{/if}
        <pre class="max-h-80 overflow-auto rounded-md bg-surface-100 p-3 text-xs text-surface-900 dark:bg-surface-900 dark:text-surface-100">{setupLog.content || '(empty)'}</pre>
      {/if}
    </section>
  </div>

  <section class="flex flex-col gap-3 rounded-lg border border-surface-200 p-3 dark:border-surface-700">
    <div class="flex items-center justify-between gap-2">
      <h3 class="text-xs font-semibold uppercase tracking-wide text-surface-500">Audit events</h3>
      <span class="text-xs text-surface-400">{auditEvents.length}</span>
    </div>
    {#if auditEvents.length === 0}
      <p class="text-sm text-surface-500">No audit events found in the last 30 days.</p>
    {:else}
      <div class="flex flex-col divide-y divide-surface-200 dark:divide-surface-800">
        {#each auditEvents as ev, i (`${ev.ts}-${ev.type}-${i}`)}
          <article class="grid gap-2 py-3 md:grid-cols-[14rem_1fr]">
            <div class="text-xs text-surface-500">
              <div>{formatDate(ev.ts)}</div>
              {#if ev.agentId}<div class="break-all">agent: {ev.agentId}</div>{/if}
            </div>
            <div class="min-w-0">
              <div class="text-sm font-medium">{ev.type}</div>
              <pre class="mt-1 max-h-48 overflow-auto rounded-md bg-surface-100 p-2 text-xs text-surface-900 dark:bg-surface-900 dark:text-surface-100">{formatData(ev.data)}</pre>
            </div>
          </article>
        {/each}
      </div>
    {/if}
  </section>
</section>
