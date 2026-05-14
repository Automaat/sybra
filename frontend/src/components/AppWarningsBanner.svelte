<script lang="ts">
  import { Cloud, AlertTriangle } from '@lucide/svelte'
  import type { ProviderHealth, DegradedWarning } from '../lib/app-lifecycle.js'

  interface Props {
    online: boolean
    networkOnline: boolean
    unhealthyProviders: ProviderHealth[]
    degradedWarnings: DegradedWarning[]
    ondismissDegraded: (index: number) => void
  }

  const {
    online,
    networkOnline,
    unhealthyProviders,
    degradedWarnings,
    ondismissDegraded,
  }: Props = $props()
</script>

{#if !online}
  <div class="flex shrink-0 items-center gap-2 border-b border-warning-600 bg-warning-800/90 px-4 py-2 text-sm text-warning-100">
    <Cloud size={16} class="shrink-0" />
    <span>
      <strong>Offline</strong> — task board is read-only.
      {networkOnline ? 'Backend unreachable.' : 'No network connection.'}
      Agents cannot start; GitHub sync will resume when reconnected.
    </span>
  </div>
{/if}

{#if unhealthyProviders.length > 0}
  <div class="flex shrink-0 flex-col gap-0.5">
    {#each unhealthyProviders as p (p.provider)}
      <div class="flex items-center gap-2 bg-error-800/90 border-b border-error-600 px-4 py-2 text-error-100 text-sm">
        <AlertTriangle size={16} class="shrink-0" />
        <span>
          <strong>{p.provider}</strong> unavailable — {p.reason}
          {#if p.ratelimitedUntil}· until {new Date(p.ratelimitedUntil).toLocaleTimeString()}{/if}
          {#if p.failoverActive}· failing over to peer{/if}
        </span>
      </div>
    {/each}
  </div>
{/if}

{#if degradedWarnings.length > 0}
  <div class="flex shrink-0 flex-col gap-0.5">
    {#each degradedWarnings as w, i (w.subsystem)}
      <div class="flex items-center gap-2 bg-warning-800/90 border-b border-warning-600 px-4 py-2 text-warning-100 text-sm">
        <AlertTriangle size={16} class="shrink-0" />
        <span><strong>{w.subsystem}</strong> degraded — {w.reason}</span>
        <button
          type="button"
          class="ml-auto opacity-60 hover:opacity-100 text-xs"
          onclick={() => ondismissDegraded(i)}
          aria-label="Dismiss"
        >✕</button>
      </div>
    {/each}
  </div>
{/if}
