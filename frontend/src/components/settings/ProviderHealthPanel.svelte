<script lang="ts">
  import {
    EventsOn,
    GetProviderHealth,
    UpdateSettings,
    SetProviderAutoFailover,
    SetProviderEnabled,
  } from '$lib/api'
  import * as ev from '../../lib/events.js'
  import type { AppSettings, RuntimeInfo } from '../../../bindings/github.com/Automaat/sybra/internal/sybra/models.js'

  interface Props {
    settings: AppSettings
    // Whether provider health checks run server-side. Resolved once by the
    // parent (Settings) so the rail entry and this pane share one source of
    // truth — the tab is never shown while the panel renders blank.
    enabled: boolean
    runtimes: RuntimeInfo[]
    onsettingschange: () => void
  }

  const { settings, enabled, runtimes, onsettingschange }: Props = $props()

  function ensureProviderLimitDefaults() {
    const providers = settings.providers as any
    providers.limits ??= {
      enabled: true,
      sessionThresholdPercent: 85,
      weeklyThresholdPercent: 90,
      preferUnderused: true,
      backfillDays: 14,
    }
    for (const name of ['claude', 'codex', 'copilot', 'opencode']) {
      providers[name] ??= { enabled: false, rateLimitCooldownSeconds: 900, monthlySubscriptionUsd: 0 }
      providers[name].monthlySubscriptionUsd ??= 0
    }
  }

  function ensureProviderRoutingDefaults() {
    const routing = (settings as any).providerRouting ??= {}
    routing.abTestingEnabled ??= false
    routing.abTestingMinSamplesPerVariant ??= 20
    routing.summary ??= {}
    routing.summary.providerPreference ??= settings.agent?.provider ?? 'claude'
    routing.summary.abTestingEnabled ??= Boolean(routing.abTestingEnabled)
    routing.summary.abTestingExplicit ??= routing.abTestingEnabled !== null && routing.abTestingEnabled !== undefined
    routing.summary.adaptiveRoutingEnabled ??= false
    routing.summary.autoFailoverEnabled ??= settings.providers?.autoFailover ?? false
    routing.summary.providerLimitsEnabled ??= settings.providers?.limits?.enabled ?? true
    routing.summary.precedence ??= ['agent.provider']
    routing.summary.eligibleVariants ??= []
    routing.summary.warnings ??= []
  }

  ensureProviderLimitDefaults()
  ensureProviderRoutingDefaults()

  type ProviderHealthEntry = {
    provider: string
    healthy: boolean
    reason: string
    detail?: string
    lastCheck?: string
    ratelimitedUntil?: string
  }

  let map = $state<Record<string, ProviderHealthEntry>>({})
  let error = $state('')

  async function load() {
    if (!enabled) return
    try {
      const list = (await GetProviderHealth()) ?? []
      const next: Record<string, ProviderHealthEntry> = {}
      for (const p of list) next[p.provider] = p as ProviderHealthEntry
      map = next
    } catch (e) {
      error = String(e)
    }
  }

  $effect(() => {
    load()
    const unsub = EventsOn(ev.ProviderHealth, (p: ProviderHealthEntry) => {
      if (!p?.provider) return
      map = { ...map, [p.provider]: p }
    })
    return () => unsub()
  })

  async function onAutoFailoverChange(e: Event) {
    const value = (e.target as HTMLInputElement).checked
    try {
      await SetProviderAutoFailover(value)
      settings.providers.autoFailover = value
      onsettingschange()
    } catch (err) {
      error = String(err)
    }
  }

  type ProviderName = 'claude' | 'codex' | 'copilot' | 'opencode'
  const providerNames: ProviderName[] = ['claude', 'codex', 'copilot', 'opencode']
  const runtimeProviderNames = new Set<ProviderName>(['claude', 'codex', 'opencode'])

  const runtimeMap = $derived.by(() => {
    const next: Record<string, RuntimeInfo> = {}
    for (const runtime of runtimes) next[runtime.id] = runtime
    return next
  })
  const hermesRuntime = $derived.by(() => runtimeMap.hermes)
  const routingWarnings = $derived.by(() => settings.providerRouting.summary.warnings ?? [])
  const eligibleVariants = $derived.by(() => settings.providerRouting.summary.eligibleVariants ?? [])

  async function onProviderEnabledChange(name: ProviderName, e: Event) {
    const value = (e.target as HTMLInputElement).checked
    try {
      await SetProviderEnabled(name, value)
      settings.providers[name].enabled = value
      onsettingschange()
      await load()
    } catch (err) {
      error = String(err)
    }
  }

  async function persistProviderSettings() {
    try {
      await UpdateSettings(settings)
      onsettingschange()
      await load()
    } catch (err) {
      error = String(err)
    }
  }

  async function onLimitEnabledChange(e: Event) {
    settings.providers.limits.enabled = (e.target as HTMLInputElement).checked
    await persistProviderSettings()
  }

  async function onPreferUnderusedChange(e: Event) {
    settings.providers.limits.preferUnderused = (e.target as HTMLInputElement).checked
    await persistProviderSettings()
  }

  type LimitNumberKey = 'sessionThresholdPercent' | 'weeklyThresholdPercent' | 'backfillDays'

  async function onLimitNumberChange(key: LimitNumberKey, e: Event) {
    const value = Number((e.target as HTMLInputElement).value)
    if (!Number.isFinite(value)) return
    settings.providers.limits[key] = value
    await persistProviderSettings()
  }

  async function onSubscriptionChange(name: ProviderName, e: Event) {
    const value = Number((e.target as HTMLInputElement).value)
    if (!Number.isFinite(value) || value < 0) return
    settings.providers[name].monthlySubscriptionUsd = value
    await persistProviderSettings()
  }

  async function onABTestingEnabledChange(e: Event) {
    const checked = (e.target as HTMLInputElement).checked
    settings.providerRouting.abTestingEnabled = checked
    settings.providerRouting.summary.abTestingEnabled = checked
    settings.providerRouting.summary.abTestingExplicit = true
    const precedence = settings.providerRouting.summary.precedence.filter((value) => value !== 'ab_testing')
    settings.providerRouting.summary.precedence = checked ? ['ab_testing', ...precedence] : precedence
    await persistProviderSettings()
  }

  function badgeClass(p: ProviderHealthEntry): string {
    if (p.healthy) return 'bg-success-500/20 text-success-600 dark:text-success-300'
    if (p.reason === 'disabled') return 'bg-surface-300 text-surface-600 dark:bg-surface-600 dark:text-surface-300'
    return 'bg-error-500/20 text-error-600 dark:text-error-300'
  }

  function showRuntime(name: ProviderName): boolean {
    return runtimeProviderNames.has(name)
  }
</script>

{#if enabled}
  <div class="rounded-xl border border-surface-200 bg-surface-50 p-5 shadow-sm dark:border-surface-700 dark:bg-surface-800 dark:shadow-none">
    <h2 class="mb-4 text-sm font-semibold uppercase tracking-wide text-surface-600 dark:text-surface-300">Providers</h2>
    {#if error}
      <div class="mb-3 text-xs text-error-500">{error}</div>
    {/if}
    <div class="flex flex-col gap-3">
      {#each providerNames as name (name)}
        {@const p = map[name]}
        {@const runtime = runtimeMap[name]}
        <div class="flex items-center justify-between gap-3 rounded border border-surface-200 bg-white px-3 py-2 dark:border-surface-700 dark:bg-surface-900">
          <div class="flex flex-col">
            <div class="flex items-center gap-2">
              <span class="font-medium capitalize">{name}</span>
              {#if p}
                <span class="rounded px-1.5 py-0.5 text-xs {badgeClass(p)}">
                  {p.healthy ? 'healthy' : p.reason}
                </span>
              {/if}
            </div>
            {#if p?.detail}
              <span class="text-xs text-surface-500 dark:text-surface-400">{p.detail}</span>
            {/if}
            {#if showRuntime(name) && runtime}
              <span class="font-mono text-[11px] text-surface-500 dark:text-surface-400">
                {runtime.installed ? runtime.path : 'CLI not found on PATH'}
              </span>
              {#if runtime.version}
                <span class="text-xs text-surface-500 dark:text-surface-400">version: {runtime.version}</span>
              {/if}
              {#if runtime.error}
                <span class="text-xs text-warning-700 dark:text-warning-300">probe: {runtime.error}</span>
              {/if}
            {/if}
            {#if p?.lastCheck}
              <span class="text-xs text-surface-500 dark:text-surface-400">last check: {new Date(p.lastCheck).toLocaleTimeString()}</span>
            {/if}
          </div>
          <div class="flex items-center gap-3">
            <label class="flex cursor-pointer items-center gap-2 text-sm">
              <input
                type="checkbox"
                class="h-4 w-4 cursor-pointer rounded border-surface-300 accent-primary-500"
                checked={settings.providers[name as ProviderName]?.enabled ?? false}
                onchange={(e) => onProviderEnabledChange(name as ProviderName, e)}
              />
              <span>Enabled</span>
            </label>
            <label class="flex items-center gap-1 text-xs text-surface-500">
              <span>$/mo</span>
              <input
                aria-label={`${name} monthly subscription`}
                class="w-16 rounded border border-surface-300 bg-white px-1 py-0.5 text-right text-xs dark:border-surface-600 dark:bg-surface-950"
                min="0"
                step="1"
                type="number"
                value={settings.providers[name as ProviderName]?.monthlySubscriptionUsd ?? 0}
                onchange={(e) => onSubscriptionChange(name as ProviderName, e)}
              />
            </label>
          </div>
        </div>
      {/each}
      {#if hermesRuntime}
        <div class="flex items-center justify-between gap-3 rounded border border-dashed border-surface-300 bg-surface-100 px-3 py-2 dark:border-surface-700 dark:bg-surface-900/60">
          <div class="flex flex-col">
            <div class="flex items-center gap-2">
              <span class="font-medium text-surface-800 dark:text-surface-100">{hermesRuntime.name}</span>
              <span class="rounded px-1.5 py-0.5 text-xs bg-primary-500/10 text-primary-700 dark:text-primary-300">informational only</span>
            </div>
            <span class="font-mono text-[11px] text-surface-500 dark:text-surface-400">
              {hermesRuntime.installed ? hermesRuntime.path : 'CLI not found on PATH'}
            </span>
            {#if hermesRuntime.version}
              <span class="text-xs text-surface-500 dark:text-surface-400">version: {hermesRuntime.version}</span>
            {/if}
            {#if hermesRuntime.error}
              <span class="text-xs text-warning-700 dark:text-warning-300">probe: {hermesRuntime.error}</span>
            {/if}
          </div>
          <span class="text-xs text-surface-500 dark:text-surface-400">No provider toggle</span>
        </div>
      {/if}
      <label class="flex cursor-pointer items-center gap-3 pt-2">
        <input
          type="checkbox"
          class="h-4 w-4 cursor-pointer rounded border-surface-300"
          checked={settings.providers.autoFailover}
          onchange={onAutoFailoverChange}
        />
        <span class="text-sm">Auto-failover between providers when one is unhealthy</span>
      </label>
      <span class="text-xs text-surface-500 dark:text-surface-400">
        Health check interval: {settings.providers.healthCheck.intervalSeconds}s. Edit config.yaml to change.
      </span>
      <div class="mt-2 rounded border border-surface-200 bg-white px-3 py-3 dark:border-surface-700 dark:bg-surface-900">
        <div class="flex flex-col gap-2">
          <div class="flex items-center justify-between gap-3">
            <div class="flex flex-col">
              <span class="text-sm font-medium">A/B provider routing opt-in</span>
              <span class="text-xs text-surface-500 dark:text-surface-400">Built-in experiments stay in code and only activate when this toggle is enabled.</span>
            </div>
            <input
              type="checkbox"
              class="h-4 w-4 cursor-pointer rounded border-surface-300 accent-primary-500"
              checked={settings.providerRouting.abTestingEnabled ?? false}
              onchange={onABTestingEnabledChange}
            />
          </div>
          <div class="grid gap-2 text-xs text-surface-600 dark:text-surface-300 sm:grid-cols-2">
            <div>Provider preference: <span class="font-medium">{settings.providerRouting.summary.providerPreference || 'claude'}</span></div>
            <div>Adaptive routing: <span class="font-medium">{settings.providerRouting.summary.adaptiveRoutingEnabled ? 'enabled' : 'disabled'}</span></div>
            <div>A/B enabled: <span class="font-medium">{settings.providerRouting.summary.abTestingEnabled ? 'yes' : 'no'}</span></div>
            <div>Min samples per variant: <span class="font-medium">{settings.providerRouting.abTestingMinSamplesPerVariant}</span></div>
          </div>
          <div class="text-xs text-surface-500 dark:text-surface-400">
            Precedence: {settings.providerRouting.summary.precedence.join(' → ')}
          </div>
          {#if routingWarnings.length > 0}
            <div class="flex flex-col gap-1 rounded border border-warning-300/70 bg-warning-500/10 px-3 py-2 text-xs text-warning-800 dark:border-warning-800/60 dark:text-warning-300">
              {#each routingWarnings as warning (warning)}
                <span>{warning}</span>
              {/each}
            </div>
          {/if}
          {#if eligibleVariants.length > 0}
            <div class="overflow-x-auto">
              <table class="min-w-full text-left text-xs">
                <thead class="text-surface-500 dark:text-surface-400">
                  <tr>
                    <th class="py-1 pr-3 font-medium">Experiment</th>
                    <th class="py-1 pr-3 font-medium">Variant</th>
                    <th class="py-1 pr-3 font-medium">Provider</th>
                    <th class="py-1 pr-3 font-medium">State</th>
                  </tr>
                </thead>
                <tbody>
                  {#each eligibleVariants as variant (`${variant.experimentId}:${variant.variantId}`)}
                    <tr class="border-t border-surface-200 dark:border-surface-800">
                      <td class="py-1 pr-3">{variant.experimentId}</td>
                      <td class="py-1 pr-3">{variant.variantId}</td>
                      <td class="py-1 pr-3">{variant.provider}</td>
                      <td class="py-1 pr-3">{variant.reason}</td>
                    </tr>
                  {/each}
                </tbody>
              </table>
            </div>
          {/if}
        </div>
      </div>
      <div class="mt-2 border-t border-surface-200 pt-3 dark:border-surface-700">
        <div class="mb-3 flex flex-col gap-2">
          <label class="flex cursor-pointer items-center gap-3">
            <input
              type="checkbox"
              class="h-4 w-4 cursor-pointer rounded border-surface-300"
              checked={settings.providers.limits.enabled}
              onchange={onLimitEnabledChange}
            />
            <span class="text-sm">Use session and weekly limits when selecting providers</span>
          </label>
          <label class="flex cursor-pointer items-center gap-3">
            <input
              type="checkbox"
              class="h-4 w-4 cursor-pointer rounded border-surface-300"
              checked={settings.providers.limits.preferUnderused}
              onchange={onPreferUnderusedChange}
            />
            <span class="text-sm">Prefer underused providers when quota pressure is high</span>
          </label>
        </div>
        <div class="grid grid-cols-1 gap-2 sm:grid-cols-3">
          <label class="text-xs text-surface-500">
            <span>Session threshold %</span>
            <input
              class="mt-1 w-full rounded border border-surface-300 bg-white px-2 py-1 text-sm dark:border-surface-600 dark:bg-surface-950"
              min="1"
              max="100"
              step="1"
              type="number"
              value={settings.providers.limits.sessionThresholdPercent}
              onchange={(e) => onLimitNumberChange('sessionThresholdPercent', e)}
            />
          </label>
          <label class="text-xs text-surface-500">
            <span>Weekly threshold %</span>
            <input
              class="mt-1 w-full rounded border border-surface-300 bg-white px-2 py-1 text-sm dark:border-surface-600 dark:bg-surface-950"
              min="1"
              max="100"
              step="1"
              type="number"
              value={settings.providers.limits.weeklyThresholdPercent}
              onchange={(e) => onLimitNumberChange('weeklyThresholdPercent', e)}
            />
          </label>
          <label class="text-xs text-surface-500">
            <span>Backfill days</span>
            <input
              class="mt-1 w-full rounded border border-surface-300 bg-white px-2 py-1 text-sm dark:border-surface-600 dark:bg-surface-950"
              min="1"
              max="90"
              step="1"
              type="number"
              value={settings.providers.limits.backfillDays}
              onchange={(e) => onLimitNumberChange('backfillDays', e)}
            />
          </label>
        </div>
      </div>
    </div>
  </div>
{/if}
