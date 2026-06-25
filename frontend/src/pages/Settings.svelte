<script lang="ts">
  import { onMount } from 'svelte'
  import { GetSettings, UpdateSettings, GetVersion, GetCodexModels, GetCopilotModels, ProviderHealthEnabled } from '$lib/api'
  import { CLAUDE_MODEL_OPTIONS } from '$lib/claude-models'
  import type { AppSettings } from '../../bindings/github.com/Automaat/sybra/internal/sybra/models.js'
  import ProviderHealthPanel from '../components/settings/ProviderHealthPanel.svelte'
  import LoggingPanel from '../components/settings/LoggingPanel.svelte'
  import TodoistPanel from '../components/settings/TodoistPanel.svelte'
  import RenovatePanel from '../components/settings/RenovatePanel.svelte'
  import { focusModeStore } from '../lib/focus-mode.svelte.js'
  import { viewModeStore } from '../lib/view-mode.svelte.js'
  import { inAppBrowserStore } from '../lib/browser.svelte.js'

  function setFocusMode(on: boolean) {
    focusModeStore.set(on)
    // Focus mode leads with the simpler list view.
    if (on) viewModeStore.set('list')
  }

  type ColorScheme = 'system' | 'light' | 'dark'

  let colorScheme = $state<ColorScheme>(
    (localStorage.getItem('colorScheme') ?? 'system') as ColorScheme
  )

  function applyColorScheme(scheme: ColorScheme) {
    const prefersDark = window.matchMedia('(prefers-color-scheme: dark)').matches
    const isDark = scheme === 'dark' || (scheme === 'system' && prefersDark)
    document.documentElement.classList.toggle('dark', isDark)
    localStorage.setItem('colorScheme', scheme)
  }

  $effect(() => {
    applyColorScheme(colorScheme)
  })

  const colorSchemes: { value: ColorScheme; label: string }[] = [
    { value: 'system', label: 'System' },
    { value: 'light', label: 'Light' },
    { value: 'dark', label: 'Dark' },
  ]

  let settings = $state<AppSettings | null>(null)
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

  // Copilot has no machine-readable model list command; the backend returns a
  // curated catalog (latest of each vendor). Fallback mirrors it for offline.
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

  // Provider health is server-gated; only surface the Providers pane (and its
  // rail entry) when the backend actually runs health checks.
  let providerHealthEnabled = $state(false)

  $effect(() => {
    load()
  })

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
      if (models && models.length > 0) {
        copilotDynamicModels = models.map(m => ({ value: m.slug, label: m.display_name }))
      }
    }).catch(() => {})
    ProviderHealthEnabled().then(v => { providerHealthEnabled = v }).catch(() => {})
  })

  async function load() {
    try {
      const s = await GetSettings() as AppSettings
      settings = s
      original = JSON.stringify(s)
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
      error = String(e)
    } finally {
      saving = false
    }
  }

  function reset() {
    if (!original) return
    settings = JSON.parse(original)
  }

  function syncOriginal() {
    if (!settings) return
    // Provider toggles persist immediately on their own; fold ONLY the providers
    // sub-tree into the saved baseline. Resetting the whole baseline here would
    // silently clear (and on reload drop) unsaved edits in other sections.
    try {
      const base = original ? JSON.parse(original) : {}
      base.providers = JSON.parse(JSON.stringify($state.snapshot(settings.providers)))
      original = JSON.stringify(base)
    } catch {
      original = JSON.stringify(settings)
    }
  }

  const modelOptions = $derived.by(() => {
    if (!settings) return []
    if (settings.agent.provider === 'codex') {
      return codexDynamicModels.length > 0 ? codexDynamicModels : codexFallbackModels
    }
    if (settings.agent.provider === 'copilot') {
      return copilotDynamicModels.length > 0 ? copilotDynamicModels : copilotFallbackModels
    }
    return [{ value: '', label: 'Default (Sonnet)' }, ...CLAUDE_MODEL_OPTIONS]
  })

  // Keep provider/model pair valid in-memory: when provider flips and the
  // current model is not in that provider's option list, reset to default.
  $effect(() => {
    if (!settings) return
    const allowed = modelOptions.map((o) => o.value)
    if (!allowed.includes(settings.agent.model)) {
      settings.agent.model = ''
    }
  })

  // Left-rail layout: each pane is a single section, grouped for scannability.
  // The Providers entry only exists when health checks are enabled, so the rail
  // never offers a link that resolves to nothing.
  type TabId =
    | 'appearance' | 'notifications' | 'agent-defaults' | 'provider-health'
    | 'orchestrator' | 'todoist' | 'renovate' | 'logging' | 'version' | 'directories'

  type RailItem = { id: TabId; label: string }
  type RailGroup = { label: string; items: RailItem[] }

  const groups = $derived<RailGroup[]>([
    { label: 'General', items: [
      { id: 'appearance', label: 'Appearance' },
      { id: 'notifications', label: 'Notifications' },
    ] },
    { label: 'Agents', items: [
      { id: 'agent-defaults', label: 'Defaults' },
      ...(providerHealthEnabled ? [{ id: 'provider-health' as TabId, label: 'Providers' }] : []),
    ] },
    { label: 'Automation', items: [
      { id: 'orchestrator', label: 'Orchestrator' },
      { id: 'todoist', label: 'Todoist' },
      { id: 'renovate', label: 'Renovate' },
    ] },
    { label: 'Advanced', items: [
      { id: 'logging', label: 'Logging' },
      { id: 'version', label: 'Version' },
      { id: 'directories', label: 'Directories' },
    ] },
  ])

  const tabs = $derived(groups.flatMap(g => g.items))

  let active = $state<TabId>('appearance')

  // If the active pane disappears (only Providers can), fall back to Appearance.
  $effect(() => {
    if (!tabs.some(t => t.id === active)) active = 'appearance'
  })
</script>

<div class="flex min-h-full flex-col">
  <!-- Sticky save bar: visible from every pane so cross-pane dirty state is never hidden -->
  <div class="sticky top-0 z-20 flex items-center justify-between gap-3 border-b border-surface-200 bg-surface-50/95 px-4 py-2.5 backdrop-blur dark:border-surface-700 dark:bg-surface-900/95 md:px-6">
    <p class="hidden min-w-0 truncate text-xs text-surface-500 dark:text-surface-400 sm:block">
      Appearance &amp; provider changes apply instantly · other settings save together
    </p>
    <div class="flex shrink-0 items-center gap-2">
      {#if successMsg}
        <span class="text-sm font-medium text-success-600 dark:text-success-400">{successMsg}</span>
      {/if}
      {#if error}
        <span class="max-w-[16rem] truncate text-sm text-error-600 dark:text-error-400">{error}</span>
      {/if}
      {#if dirty}
        <span class="text-xs font-medium text-warning-600 dark:text-warning-400">Unsaved changes</span>
        <button
          type="button"
          class="rounded-lg px-3 py-1.5 text-sm font-medium text-surface-700 hover:bg-surface-200 dark:text-surface-200 dark:hover:bg-surface-700"
          onclick={reset}
        >
          Reset
        </button>
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
    <!-- One responsive rail: vertical sidebar on md+, horizontal tab strip below -->
    <nav
      class="shrink-0 border-b border-surface-200 px-3 py-2 dark:border-surface-700 md:w-52 md:border-b-0 md:border-r md:py-4"
      aria-label="Settings sections"
    >
      <div class="flex gap-1 overflow-x-auto md:flex-col md:gap-0.5 md:overflow-visible">
        {#each groups as group (group.label)}
          <p class="hidden px-2.5 pt-4 pb-1 text-[11px] font-semibold uppercase tracking-wider text-surface-500 first:pt-0 dark:text-surface-400 md:block">
            {group.label}
          </p>
          {#each group.items as item (item.id)}
            <button
              type="button"
              aria-current={active === item.id ? 'page' : undefined}
              class="flex shrink-0 items-center rounded-md px-2.5 py-1.5 text-left text-sm transition-colors
                {active === item.id
                  ? 'bg-primary-500/12 font-medium text-primary-700 dark:bg-primary-500/15 dark:text-primary-300'
                  : 'text-surface-600 hover:bg-surface-200/70 hover:text-surface-900 dark:text-surface-300 dark:hover:bg-surface-800 dark:hover:text-surface-50'}"
              onclick={() => (active = item.id)}
            >
              {item.label}
            </button>
          {/each}
        {/each}
      </div>
    </nav>

    <!-- Content column -->
    <div class="min-w-0 flex-1">
      <div class="mx-auto max-w-3xl p-4 md:p-6">
        <!-- Appearance (localStorage-backed, no save required) -->
        {#if active === 'appearance'}
          <section class="rounded-xl border border-surface-200 bg-surface-50 p-5 shadow-sm dark:border-surface-700 dark:bg-surface-800 dark:shadow-none">
            <h2 class="mb-4 text-sm font-semibold uppercase tracking-wide text-surface-600 dark:text-surface-300">Appearance</h2>
            <div class="flex flex-col gap-1.5">
              <span class="text-sm font-medium" id="color-scheme-label">Color Scheme</span>
              <div
                role="group"
                aria-labelledby="color-scheme-label"
                class="inline-flex w-fit rounded-lg border border-surface-300 bg-surface-100 p-0.5 dark:border-surface-600 dark:bg-surface-900"
              >
                {#each colorSchemes as opt (opt.value)}
                  <button
                    type="button"
                    aria-pressed={colorScheme === opt.value}
                    class="rounded-md px-3.5 py-1.5 text-sm font-medium transition-colors
                      {colorScheme === opt.value
                        ? 'bg-primary-500 text-primary-contrast-500 shadow-sm'
                        : 'text-surface-600 hover:text-surface-900 dark:text-surface-300 dark:hover:text-surface-50'}"
                    onclick={() => (colorScheme = opt.value)}
                  >
                    {opt.label}
                  </button>
                {/each}
              </div>
              <span class="text-xs text-surface-500 dark:text-surface-400">Applied immediately, no save needed</span>
            </div>
            <label class="mt-5 flex items-start gap-3">
              <input
                type="checkbox"
                class="mt-0.5 h-4 w-4 accent-primary-500"
                checked={focusModeStore.enabled}
                onchange={(e) => setFocusMode((e.target as HTMLInputElement).checked)}
              />
              <span class="flex flex-col">
                <span class="text-sm font-medium">Focus mode</span>
                <span class="text-xs text-surface-500 dark:text-surface-400">Cleaner, minimal surface — collapses the sidebar and leads with the list view. Advanced views stay reachable via “More”.</span>
              </span>
            </label>
            <label class="mt-5 flex items-start gap-3">
              <input
                type="checkbox"
                class="mt-0.5 h-4 w-4 accent-primary-500"
                checked={inAppBrowserStore.enabled}
                onchange={(e) => inAppBrowserStore.set((e.target as HTMLInputElement).checked)}
              />
              <span class="flex flex-col">
                <span class="text-sm font-medium">Open links in-app</span>
                <span class="text-xs text-surface-500 dark:text-surface-400">Open GitHub issue &amp; PR links in a Sybra browser window — log in once, stay in one app. Hold ⌘/Ctrl when clicking to use the system browser instead.</span>
              </span>
            </label>
          </section>
        {/if}

        {#if settings}
          <!-- Notifications -->
          {#if active === 'notifications'}
            <section class="rounded-xl border border-surface-200 bg-surface-50 p-5 shadow-sm dark:border-surface-700 dark:bg-surface-800 dark:shadow-none">
              <h2 class="mb-4 text-sm font-semibold uppercase tracking-wide text-surface-600 dark:text-surface-300">Notifications</h2>
              <label class="flex cursor-pointer items-center gap-3">
                <input
                  type="checkbox"
                  class="h-4 w-4 cursor-pointer rounded border-surface-300 accent-primary-500"
                  bind:checked={settings.notification.desktop}
                />
                <span class="text-sm">Desktop notifications (macOS)</span>
              </label>
            </section>
          {/if}

          <!-- Agent Defaults -->
          {#if active === 'agent-defaults'}
            <section class="rounded-xl border border-surface-200 bg-surface-50 p-5 shadow-sm dark:border-surface-700 dark:bg-surface-800 dark:shadow-none">
              <h2 class="mb-4 text-sm font-semibold uppercase tracking-wide text-surface-600 dark:text-surface-300">Agent Defaults</h2>
              <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
                <div class="flex flex-col gap-1">
                  <label class="text-sm font-medium" for="agent-provider">Agent Type</label>
                  <select
                    id="agent-provider"
                    class="rounded-lg border border-surface-300 bg-white px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
                    bind:value={settings.agent.provider}
                  >
                    <option value="claude">Claude</option>
                    <option value="codex">Codex</option>
                    <option value="copilot">Copilot</option>
                  </select>
                </div>
                <div class="flex flex-col gap-1">
                  <label class="text-sm font-medium" for="agent-model">Default Model</label>
                  <select
                    id="agent-model"
                    class="rounded-lg border border-surface-300 bg-white px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
                    bind:value={settings.agent.model}
                  >
                    {#each modelOptions as option}
                      <option value={option.value}>{option.label}</option>
                    {/each}
                  </select>
                </div>
                <div class="flex flex-col gap-1">
                  <label class="text-sm font-medium" for="agent-mode">Default Mode</label>
                  <select
                    id="agent-mode"
                    class="rounded-lg border border-surface-300 bg-white px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
                    bind:value={settings.agent.mode}
                  >
                    <option value="">— none —</option>
                    <option value="headless">Headless</option>
                    <option value="interactive">Interactive</option>
                  </select>
                </div>
                <div class="flex flex-col gap-1">
                  <label class="text-sm font-medium" for="agent-concurrency">Max Concurrent</label>
                  <input
                    id="agent-concurrency"
                    type="number"
                    min="1"
                    max="100"
                    class="rounded-lg border border-surface-300 bg-white px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
                    bind:value={settings.agent.maxConcurrent}
                  />
                  <span class="text-xs text-surface-500 dark:text-surface-400">1–100</span>
                </div>
              </div>
            </section>
          {/if}

          <!-- Providers (server-gated) -->
          {#if active === 'provider-health'}
            <ProviderHealthPanel {settings} enabled={providerHealthEnabled} onsettingschange={syncOriginal} />
          {/if}

          <!-- Orchestrator -->
          {#if active === 'orchestrator'}
            <section class="rounded-xl border border-surface-200 bg-surface-50 p-5 shadow-sm dark:border-surface-700 dark:bg-surface-800 dark:shadow-none">
              <h2 class="mb-4 text-sm font-semibold uppercase tracking-wide text-surface-600 dark:text-surface-300">Orchestrator</h2>
              <div class="flex flex-col gap-3">
                <label class="flex cursor-pointer items-start gap-3">
                  <input
                    type="checkbox"
                    class="mt-0.5 h-4 w-4 cursor-pointer rounded border-surface-300 accent-primary-500"
                    bind:checked={settings.orchestrator.autoTriage}
                  />
                  <div>
                    <span class="text-sm font-medium">Auto-triage</span>
                    <p class="text-xs text-surface-500 dark:text-surface-400">Automatically dispatch triage agents on task creation</p>
                  </div>
                </label>
                <label class="flex cursor-pointer items-start gap-3">
                  <input
                    type="checkbox"
                    class="mt-0.5 h-4 w-4 cursor-pointer rounded border-surface-300 accent-primary-500"
                    bind:checked={settings.orchestrator.autoPlan}
                  />
                  <div>
                    <span class="text-sm font-medium">Auto-plan</span>
                    <p class="text-xs text-surface-500 dark:text-surface-400">Automatically dispatch planning agents on complex tasks</p>
                  </div>
                </label>
              </div>
            </section>
          {/if}

          {#if active === 'todoist'}
            <TodoistPanel settings={settings} />
          {/if}

          {#if active === 'renovate'}
            <RenovatePanel settings={settings} />
          {/if}

          {#if active === 'logging'}
            <LoggingPanel settings={settings} />
          {/if}

          <!-- Version (read-only) -->
          {#if active === 'version'}
            <section class="rounded-xl border border-surface-200 bg-surface-50 p-5 shadow-sm dark:border-surface-700 dark:bg-surface-800 dark:shadow-none">
              <h2 class="mb-4 text-sm font-semibold uppercase tracking-wide text-surface-600 dark:text-surface-300">Version</h2>
              <div class="flex flex-col gap-2">
                <div class="flex items-center gap-3">
                  <span class="w-20 shrink-0 text-xs font-medium text-surface-500 dark:text-surface-400">Server</span>
                  <span class="flex-1 font-mono text-xs text-surface-600 dark:text-surface-300">{serverVersion ?? '…'}</span>
                </div>
                <div class="flex items-center gap-3">
                  <span class="w-20 shrink-0 text-xs font-medium text-surface-500 dark:text-surface-400">Client</span>
                  <span class="flex-1 font-mono text-xs text-surface-600 dark:text-surface-300">{clientVersion}</span>
                </div>
              </div>
            </section>
          {/if}

          <!-- Directories (read-only) -->
          {#if active === 'directories'}
            <section class="rounded-xl border border-surface-200 bg-surface-50 p-5 shadow-sm dark:border-surface-700 dark:bg-surface-800 dark:shadow-none">
              <h2 class="mb-4 text-sm font-semibold uppercase tracking-wide text-surface-600 dark:text-surface-300">Directories</h2>
              <div class="flex flex-col gap-2">
                {#each dirOrder as key (key)}
                  {#if settings.directories[key]}
                    <div class="flex items-center gap-3">
                      <span class="w-20 shrink-0 text-xs font-medium text-surface-500 capitalize dark:text-surface-400">{key}</span>
                      <input
                        type="text"
                        value={settings.directories[key]}
                        disabled
                        class="flex-1 rounded-lg border border-surface-200 bg-surface-100 px-3 py-1.5 font-mono text-xs text-surface-600 dark:border-surface-700 dark:bg-surface-900 dark:text-surface-300"
                      />
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
