<script lang="ts">
  import { onMount } from 'svelte'
  import {
    GetSettings, GetDefaultSettings, UpdateSettings, UpdateTodoistToken,
    GetVersion, GetCodexModels, GetCopilotModels, ProviderHealthEnabled,
  } from '$lib/api'
  import { CLAUDE_MODEL_OPTIONS } from '$lib/claude-models'
  import type { AppSettings } from '../../bindings/github.com/Automaat/sybra/internal/sybra/models.js'
  import ProviderHealthPanel from '../components/settings/ProviderHealthPanel.svelte'
  import LoggingPanel from '../components/settings/LoggingPanel.svelte'
  import TodoistPanel from '../components/settings/TodoistPanel.svelte'
  import RenovatePanel from '../components/settings/RenovatePanel.svelte'
  import AgentPanel from '../components/settings/AgentPanel.svelte'
  import OrchestratorPanel from '../components/settings/OrchestratorPanel.svelte'
  import GitHubPanel from '../components/settings/GitHubPanel.svelte'
  import MonitorPanel from '../components/settings/MonitorPanel.svelte'
  import AutomationPanel from '../components/settings/AutomationPanel.svelte'
  import SystemPanel from '../components/settings/SystemPanel.svelte'
  import RawConfigPanel from '../components/settings/RawConfigPanel.svelte'
  import { focusModeStore } from '../lib/focus-mode.svelte.js'
  import { viewModeStore } from '../lib/view-mode.svelte.js'
  import { inAppBrowserStore } from '../lib/browser.svelte.js'

  function setFocusMode(on: boolean) {
    focusModeStore.set(on)
    if (on) viewModeStore.set('list')
  }

  type ColorScheme = 'system' | 'light' | 'dark'
  let colorScheme = $state<ColorScheme>((localStorage.getItem('colorScheme') ?? 'system') as ColorScheme)
  function applyColorScheme(scheme: ColorScheme) {
    const prefersDark = window.matchMedia('(prefers-color-scheme: dark)').matches
    const isDark = scheme === 'dark' || (scheme === 'system' && prefersDark)
    document.documentElement.classList.toggle('dark', isDark)
    localStorage.setItem('colorScheme', scheme)
  }
  $effect(() => { applyColorScheme(colorScheme) })
  const colorSchemes: { value: ColorScheme; label: string }[] = [
    { value: 'system', label: 'System' },
    { value: 'light', label: 'Light' },
    { value: 'dark', label: 'Dark' },
  ]

  let settings = $state<AppSettings | null>(null)
  let defaults = $state<AppSettings | null>(null)
  let original = $state<string>('')
  let saving = $state(false)
  let error = $state('')
  let successMsg = $state('')

  const dirty = $derived(settings !== null && JSON.stringify(settings) !== original)
  const dirOrder = ['tasks', 'skills', 'projects', 'clones', 'worktrees', 'logs', 'audit']

  let serverVersion = $state<string | null>(null)
  const clientVersion = String(import.meta.env.VITE_APP_VERSION || 'dev')

  type ModelOption = { value: string; label: string }
  const codexFallbackModels: ModelOption[] = [
    { value: '', label: 'Default (gpt-5.5)' },
    { value: 'gpt-5.4', label: 'GPT-5.4' },
    { value: 'gpt-5.4-mini', label: 'GPT-5.4 Mini' },
    { value: 'gpt-5.3-codex', label: 'GPT-5.3 Codex' },
  ]
  let codexDynamicModels = $state<ModelOption[]>([])
  const copilotFallbackModels: ModelOption[] = [
    { value: '', label: 'Default (GPT-5.4)' },
    { value: 'gpt-5.4', label: 'GPT-5.4' },
    { value: 'gpt-5.4-mini', label: 'GPT-5.4 Mini' },
    { value: 'gpt-5.3-codex', label: 'GPT-5.3 Codex' },
    { value: 'claude-opus-4.6', label: 'Claude Opus 4.6' },
    { value: 'claude-sonnet-4.6', label: 'Claude Sonnet 4.6' },
    { value: 'claude-haiku-4.5', label: 'Claude Haiku 4.5' },
    { value: 'gemini-3-pro-preview', label: 'Gemini 3 Pro' },
    { value: 'auto', label: 'Auto' },
  ]
  let copilotDynamicModels = $state<ModelOption[]>([])
  let providerHealthEnabled = $state(false)

  $effect(() => { load() })

  onMount(() => {
    GetVersion().then(v => { serverVersion = v.server }).catch(() => { serverVersion = 'unavailable' })
    GetCodexModels().then(models => {
      if (models && models.length > 0) {
        codexDynamicModels = [
          { value: '', label: codexFallbackModels[0].label },
          ...models.map(m => ({ value: m.slug, label: m.display_name })),
        ]
      }
    }).catch(() => {})
    GetCopilotModels().then(models => {
      if (models && models.length > 0) copilotDynamicModels = models.map(m => ({ value: m.slug, label: m.display_name }))
    }).catch(() => {})
    ProviderHealthEnabled().then(v => { providerHealthEnabled = v }).catch(() => {})
  })

  async function load() {
    try {
      const [s, d] = await Promise.all([GetSettings() as Promise<AppSettings>, GetDefaultSettings() as Promise<AppSettings>])
      settings = s
      defaults = d
      original = JSON.stringify(s)
      prevProvider = s.agent.provider
    } catch (e) {
      error = String(e)
    }
  }

  async function save() {
    if (!settings) return
    saving = true
    error = ''
    successMsg = ''
    try {
      await UpdateSettings(settings)
      original = JSON.stringify(settings)
      successMsg = 'Settings saved'
      setTimeout(() => { successMsg = '' }, 3000)
    } catch (e) {
      error = String(e).replace(/^Error:\s*/, '')
    } finally {
      saving = false
    }
  }

  function reset() {
    if (!original) return
    settings = JSON.parse(original)
    prevProvider = settings?.agent.provider ?? null
  }

  // Fold a sub-tree back into the saved baseline without discarding unsaved edits
  // elsewhere — used by out-of-band writes (provider toggles, token rotation).
  function foldIntoBaseline(patch: (base: AppSettings) => void) {
    if (!settings) return
    try {
      const base = original ? JSON.parse(original) : JSON.parse(JSON.stringify($state.snapshot(settings)))
      patch(base)
      original = JSON.stringify(base)
    } catch {
      original = JSON.stringify(settings)
    }
  }

  function syncOriginal() {
    if (!settings) return
    foldIntoBaseline((base) => { base.providers = JSON.parse(JSON.stringify($state.snapshot(settings!.providers))) })
  }

  // Token rotates through the dedicated write-only path, not the generic Save.
  async function saveToken(token: string) {
    await UpdateTodoistToken(token)
    if (!settings) return
    settings.todoistTokenSet = token !== ''
    settings.todoist.apiToken = ''
    foldIntoBaseline((base) => { base.todoistTokenSet = settings!.todoistTokenSet; base.todoist.apiToken = '' })
  }

  const modelOptions = $derived.by(() => {
    if (!settings) return [] as ModelOption[]
    if (settings.agent.provider === 'codex') return codexDynamicModels.length > 0 ? codexDynamicModels : codexFallbackModels
    if (settings.agent.provider === 'copilot') return copilotDynamicModels.length > 0 ? copilotDynamicModels : copilotFallbackModels
    return [{ value: '', label: 'Default (Sonnet)' }, ...CLAUDE_MODEL_OPTIONS]
  })

  // Only realign the model when the provider actually changes — never on initial
  // load or when async model lists arrive — so the form isn't spuriously dirty.
  let prevProvider = $state<string | null>(null)
  $effect(() => {
    if (!settings) return
    const p = settings.agent.provider
    if (prevProvider === null) { prevProvider = p; return }
    if (p !== prevProvider) {
      prevProvider = p
      if (!modelOptions.map((o) => o.value).includes(settings.agent.model)) settings.agent.model = ''
    }
  })

  // ---- rail + filtering -----------------------------------------------------
  type TabId =
    | 'appearance' | 'notifications' | 'agent' | 'provider-health'
    | 'orchestrator' | 'automation' | 'github' | 'monitor' | 'todoist' | 'renovate'
    | 'system' | 'logging' | 'version' | 'directories' | 'raw'

  type RailItem = { id: TabId; label: string; keywords: string; keys: (keyof AppSettings)[] }
  type RailGroup = { label: string; items: RailItem[] }

  const allGroups = $derived<RailGroup[]>([
    { label: 'General', items: [
      { id: 'appearance', label: 'Appearance', keywords: 'theme color dark light focus mode browser', keys: [] },
      { id: 'notifications', label: 'Notifications', keywords: 'desktop macos notify', keys: ['notification'] },
    ] },
    { label: 'Agents', items: [
      { id: 'agent', label: 'Defaults', keywords: 'provider model mode concurrent permissions fallback turns cost claude codex copilot', keys: ['agent'] },
      ...(providerHealthEnabled ? [{ id: 'provider-health' as TabId, label: 'Providers', keywords: 'health limits failover subscription', keys: ['providers'] as (keyof AppSettings)[] }] : []),
    ] },
    { label: 'Automation', items: [
      { id: 'orchestrator', label: 'Orchestrator', keywords: 'auto triage plan dispatch maintenance interval', keys: ['orchestrator'] },
      { id: 'automation', label: 'Triage & Umbrella', keywords: 'triage classify umbrella grounding expand', keys: ['triage', 'umbrella'] },
      { id: 'github', label: 'GitHub', keywords: 'github issues pr poller role app token merge renovate', keys: ['github'] },
      { id: 'monitor', label: 'Monitor', keywords: 'monitor anomaly self-monitor bottleneck failure lost agent', keys: ['monitor', 'selfMonitor'] },
      { id: 'todoist', label: 'Todoist', keywords: 'todoist token sync poll project', keys: ['todoist'] },
      { id: 'renovate', label: 'Renovate', keywords: 'renovate dependency bot author', keys: ['renovate'] },
    ] },
    { label: 'System', items: [
      { id: 'system', label: 'Machine & Testing', keywords: 'project types machine routing testing experience metrics prometheus', keys: ['testing', 'experience', 'metrics', 'projectTypes'] },
      { id: 'logging', label: 'Logging & Audit', keywords: 'log level size files audit retention', keys: ['logging', 'audit'] },
    ] },
    { label: 'Advanced', items: [
      { id: 'raw', label: 'Config file (YAML)', keywords: 'yaml raw config file editor everything advanced', keys: [] },
      { id: 'version', label: 'Version', keywords: 'version server client build', keys: [] },
      { id: 'directories', label: 'Directories', keywords: 'paths dirs tasks clones worktrees', keys: [] },
    ] },
  ])

  let query = $state('')
  let modifiedOnly = $state(false)

  function sectionModified(keys: (keyof AppSettings)[]): boolean {
    if (!settings || !defaults || keys.length === 0) return false
    return keys.some((k) => JSON.stringify(settings![k]) !== JSON.stringify(defaults![k]))
  }

  function itemVisible(it: RailItem): boolean {
    const q = query.trim().toLowerCase()
    if (q && !(`${it.label} ${it.keywords}`.toLowerCase().includes(q))) return false
    if (modifiedOnly && !sectionModified(it.keys)) return false
    return true
  }

  const groups = $derived(allGroups
    .map((g) => ({ label: g.label, items: g.items.filter(itemVisible) }))
    .filter((g) => g.items.length > 0))
  const tabs = $derived(groups.flatMap(g => g.items))

  const modifiedCount = $derived(allGroups.flatMap(g => g.items).filter(it => sectionModified(it.keys)).length)

  let active = $state<TabId>('appearance')
  $effect(() => { if (tabs.length > 0 && !tabs.some(t => t.id === active)) active = tabs[0].id })
</script>

<div class="flex min-h-full flex-col">
  <!-- Sticky save bar -->
  <div class="sticky top-0 z-20 flex items-center justify-between gap-3 border-b border-surface-200 bg-surface-50/95 px-4 py-2.5 backdrop-blur dark:border-surface-700 dark:bg-surface-900/95 md:px-6">
    <p class="hidden min-w-0 truncate text-xs text-surface-500 dark:text-surface-400 sm:block">
      Appearance, provider &amp; token changes apply instantly · other settings save together
    </p>
    <div class="flex shrink-0 items-center gap-2">
      {#if successMsg}<span class="text-sm font-medium text-success-600 dark:text-success-400">{successMsg}</span>{/if}
      {#if error}<span class="max-w-[16rem] truncate text-sm text-error-600 dark:text-error-400">{error}</span>{/if}
      {#if dirty}
        <span class="text-xs font-medium text-warning-600 dark:text-warning-400">Unsaved changes</span>
        <button type="button" class="rounded-lg px-3 py-1.5 text-sm font-medium text-surface-700 hover:bg-surface-200 dark:text-surface-200 dark:hover:bg-surface-700" onclick={reset}>Reset</button>
      {/if}
      <button
        type="button"
        class="rounded-lg bg-primary-500 px-4 py-1.5 text-sm font-semibold text-primary-contrast-500 shadow-sm transition-colors hover:bg-primary-600 disabled:cursor-not-allowed disabled:opacity-40 disabled:shadow-none"
        onclick={save}
        disabled={!dirty || saving}
      >
        {saving ? 'Saving…' : 'Save'}
      </button>
    </div>
  </div>

  <div class="flex min-h-0 flex-1 flex-col md:flex-row">
    <!-- Rail with search + modified filter -->
    <nav class="shrink-0 border-b border-surface-200 px-3 py-2 dark:border-surface-700 md:w-56 md:border-b-0 md:border-r md:py-4" aria-label="Settings sections">
      <div class="mb-2 flex flex-col gap-2 px-1">
        <input
          type="search"
          placeholder="Search settings…"
          class="w-full rounded-lg border border-surface-300 bg-white px-3 py-1.5 text-sm dark:border-surface-600 dark:bg-surface-700"
          bind:value={query}
        />
        <label class="flex cursor-pointer items-center gap-2 px-1 text-xs text-surface-500 dark:text-surface-400">
          <input type="checkbox" class="h-3.5 w-3.5 accent-primary-500" bind:checked={modifiedOnly} disabled={modifiedCount === 0} />
          Modified only{#if modifiedCount > 0}<span class="rounded bg-primary-500/12 px-1.5 text-[10px] font-semibold text-primary-700 dark:text-primary-300">{modifiedCount}</span>{/if}
        </label>
      </div>
      <div class="flex gap-1 overflow-x-auto md:flex-col md:gap-0.5 md:overflow-visible">
        {#each groups as group (group.label)}
          <p class="hidden px-2.5 pt-4 pb-1 text-[11px] font-semibold uppercase tracking-wider text-surface-500 first:pt-0 dark:text-surface-400 md:block">{group.label}</p>
          {#each group.items as item (item.id)}
            <button
              type="button"
              aria-current={active === item.id ? 'page' : undefined}
              class="flex shrink-0 items-center gap-2 rounded-md px-2.5 py-1.5 text-left text-sm transition-colors
                {active === item.id
                  ? 'bg-primary-500/12 font-medium text-primary-700 dark:bg-primary-500/15 dark:text-primary-300'
                  : 'text-surface-600 hover:bg-surface-200/70 hover:text-surface-900 dark:text-surface-300 dark:hover:bg-surface-800 dark:hover:text-surface-50'}"
              onclick={() => (active = item.id)}
            >
              {item.label}
              {#if sectionModified(item.keys)}<span class="ml-auto h-1.5 w-1.5 shrink-0 rounded-full bg-primary-500" title="Modified"></span>{/if}
            </button>
          {/each}
        {/each}
        {#if tabs.length === 0}
          <p class="px-2.5 py-2 text-sm text-surface-500 dark:text-surface-400">No settings match.</p>
        {/if}
      </div>
    </nav>

    <!-- Content -->
    <div class="min-w-0 flex-1">
      <div class="mx-auto max-w-3xl space-y-4 p-4 md:p-6">
        {#if active === 'appearance'}
          <section class="rounded-xl border border-surface-200 bg-surface-50 p-5 shadow-sm dark:border-surface-700 dark:bg-surface-800 dark:shadow-none">
            <h2 class="mb-4 text-sm font-semibold uppercase tracking-wide text-surface-600 dark:text-surface-300">Appearance</h2>
            <div class="flex flex-col gap-1.5">
              <span class="text-sm font-medium" id="color-scheme-label">Color scheme</span>
              <div role="group" aria-labelledby="color-scheme-label" class="inline-flex w-fit rounded-lg border border-surface-300 bg-surface-100 p-0.5 dark:border-surface-600 dark:bg-surface-900">
                {#each colorSchemes as opt (opt.value)}
                  <button type="button" aria-pressed={colorScheme === opt.value}
                    class="rounded-md px-3.5 py-1.5 text-sm font-medium transition-colors
                      {colorScheme === opt.value ? 'bg-primary-500 text-primary-contrast-500 shadow-sm' : 'text-surface-600 hover:text-surface-900 dark:text-surface-300 dark:hover:text-surface-50'}"
                    onclick={() => (colorScheme = opt.value)}>{opt.label}</button>
                {/each}
              </div>
              <span class="text-xs text-surface-500 dark:text-surface-400">Applied immediately, no save needed</span>
            </div>
            <label class="mt-5 flex items-start gap-3">
              <input type="checkbox" class="mt-0.5 h-4 w-4 accent-primary-500" checked={focusModeStore.enabled} onchange={(e) => setFocusMode((e.target as HTMLInputElement).checked)} />
              <span class="flex flex-col">
                <span class="text-sm font-medium">Focus mode</span>
                <span class="text-xs text-surface-500 dark:text-surface-400">Cleaner, minimal surface — collapses the sidebar and leads with the list view.</span>
              </span>
            </label>
            <label class="mt-5 flex items-start gap-3">
              <input type="checkbox" class="mt-0.5 h-4 w-4 accent-primary-500" checked={inAppBrowserStore.enabled} onchange={(e) => inAppBrowserStore.set((e.target as HTMLInputElement).checked)} />
              <span class="flex flex-col">
                <span class="text-sm font-medium">Open links in-app</span>
                <span class="text-xs text-surface-500 dark:text-surface-400">Open GitHub issue &amp; PR links in a Sybra browser window. Hold ⌘/Ctrl when clicking to use the system browser instead.</span>
              </span>
            </label>
          </section>
        {/if}

        {#if settings && defaults}
          {#if active === 'notifications'}
            <section class="rounded-xl border border-surface-200 bg-surface-50 p-5 shadow-sm dark:border-surface-700 dark:bg-surface-800 dark:shadow-none">
              <h2 class="mb-4 text-sm font-semibold uppercase tracking-wide text-surface-600 dark:text-surface-300">Notifications</h2>
              <label class="flex cursor-pointer items-center gap-3">
                <input type="checkbox" class="h-4 w-4 cursor-pointer rounded border-surface-300 accent-primary-500" bind:checked={settings.notification.desktop} />
                <span class="text-sm">Desktop notifications (macOS)</span>
              </label>
            </section>
          {/if}

          {#if active === 'agent'}
            <AgentPanel bind:settings {defaults} {modelOptions} advancedOpen={query.trim().length > 0} />
          {/if}
          {#if active === 'provider-health'}
            <ProviderHealthPanel {settings} enabled={providerHealthEnabled} onsettingschange={syncOriginal} />
          {/if}
          {#if active === 'orchestrator'}
            <OrchestratorPanel bind:settings {defaults} />
          {/if}
          {#if active === 'automation'}
            <AutomationPanel bind:settings {defaults} />
          {/if}
          {#if active === 'github'}
            <GitHubPanel bind:settings {defaults} />
          {/if}
          {#if active === 'monitor'}
            <MonitorPanel bind:settings {defaults} />
          {/if}
          {#if active === 'todoist'}
            <TodoistPanel bind:settings {defaults} onsavetoken={saveToken} />
          {/if}
          {#if active === 'renovate'}
            <RenovatePanel bind:settings {defaults} />
          {/if}
          {#if active === 'system'}
            <SystemPanel bind:settings {defaults} />
          {/if}
          {#if active === 'logging'}
            <LoggingPanel bind:settings {defaults} />
          {/if}
          {#if active === 'raw'}
            <RawConfigPanel onsaved={load} />
          {/if}

          {#if active === 'version'}
            <section class="rounded-xl border border-surface-200 bg-surface-50 p-5 shadow-sm dark:border-surface-700 dark:bg-surface-800 dark:shadow-none">
              <h2 class="mb-4 text-sm font-semibold uppercase tracking-wide text-surface-600 dark:text-surface-300">Version</h2>
              <div class="flex flex-col gap-2">
                <div class="flex items-center gap-3"><span class="w-20 shrink-0 text-xs font-medium text-surface-500 dark:text-surface-400">Server</span><span class="flex-1 font-mono text-xs text-surface-600 dark:text-surface-300">{serverVersion ?? '…'}</span></div>
                <div class="flex items-center gap-3"><span class="w-20 shrink-0 text-xs font-medium text-surface-500 dark:text-surface-400">Client</span><span class="flex-1 font-mono text-xs text-surface-600 dark:text-surface-300">{clientVersion}</span></div>
              </div>
            </section>
          {/if}
          {#if active === 'directories'}
            <section class="rounded-xl border border-surface-200 bg-surface-50 p-5 shadow-sm dark:border-surface-700 dark:bg-surface-800 dark:shadow-none">
              <h2 class="mb-4 text-sm font-semibold uppercase tracking-wide text-surface-600 dark:text-surface-300">Directories</h2>
              <div class="flex flex-col gap-2">
                {#each dirOrder as key (key)}
                  {#if settings.directories[key]}
                    <div class="flex items-center gap-3">
                      <span class="w-20 shrink-0 text-xs font-medium text-surface-500 capitalize dark:text-surface-400">{key}</span>
                      <input type="text" value={settings.directories[key]} disabled class="flex-1 rounded-lg border border-surface-200 bg-surface-100 px-3 py-1.5 font-mono text-xs text-surface-600 dark:border-surface-700 dark:bg-surface-900 dark:text-surface-300" />
                    </div>
                  {/if}
                {/each}
              </div>
            </section>
          {/if}
        {:else if error}
          <p class="text-error-600 dark:text-error-400">{error}</p>
        {:else}
          <p class="text-surface-500 dark:text-surface-400">Loading…</p>
        {/if}
      </div>
    </div>
  </div>
</div>
