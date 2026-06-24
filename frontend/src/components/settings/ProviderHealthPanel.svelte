<script lang="ts">
  import {
    EventsOn,
    GetProviderHealth,
    ProviderHealthEnabled,
    SetProviderAutoFailover,
    SetProviderEnabled,
  } from '$lib/api'
  import * as ev from '../../lib/events.js'
  import type { AppSettings } from '../../../bindings/github.com/Automaat/sybra/internal/sybra/models.js'

  interface Props {
    settings: AppSettings
    onsettingschange: () => void
  }

  const { settings, onsettingschange }: Props = $props()

  type ProviderHealthEntry = {
    provider: string
    healthy: boolean
    reason: string
    detail?: string
    lastCheck?: string
    ratelimitedUntil?: string
  }

  let enabled = $state(false)
  let map = $state<Record<string, ProviderHealthEntry>>({})
  let error = $state('')

  async function load() {
    try {
      enabled = await ProviderHealthEnabled()
      if (!enabled) return
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

  async function onProviderEnabledChange(name: 'claude' | 'codex', e: Event) {
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

  function badgeClass(p: ProviderHealthEntry): string {
    if (p.healthy) return 'bg-success-500/20 text-success-600 dark:text-success-300'
    if (p.reason === 'disabled') return 'bg-surface-300 text-surface-600 dark:bg-surface-600 dark:text-surface-300'
    return 'bg-error-500/20 text-error-600 dark:text-error-300'
  }
</script>

{#if enabled}
  <div class="rounded-xl border border-surface-200 bg-surface-50 p-5 shadow-sm dark:border-surface-700 dark:bg-surface-800 dark:shadow-none">
    <h2 class="mb-4 text-sm font-semibold uppercase tracking-wide text-surface-600 dark:text-surface-300">Providers</h2>
    {#if error}
      <div class="mb-3 text-xs text-error-500">{error}</div>
    {/if}
    <div class="flex flex-col gap-3">
      {#each ['claude', 'codex'] as name (name)}
        {@const p = map[name]}
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
            {#if p?.lastCheck}
              <span class="text-xs text-surface-500 dark:text-surface-400">last check: {new Date(p.lastCheck).toLocaleTimeString()}</span>
            {/if}
          </div>
          <label class="flex cursor-pointer items-center gap-2 text-sm">
            <input
              type="checkbox"
              class="h-4 w-4 cursor-pointer rounded border-surface-300 accent-primary-500"
              checked={name === 'claude' ? settings.providers.claude.enabled : settings.providers.codex.enabled}
              onchange={(e) => onProviderEnabledChange(name as 'claude' | 'codex', e)}
            />
            <span>Enabled</span>
          </label>
        </div>
      {/each}
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
    </div>
  </div>
{/if}
