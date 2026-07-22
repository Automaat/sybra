<script lang="ts">
  import { onMount } from 'svelte'
  import type { AppSettings } from '../../../bindings/github.com/Automaat/sybra/internal/sybra/models.js'
  import { EventsOn } from '$lib/api'
  import { IssuesUpdated, ReviewsUpdated } from '$lib/events.js'
  import Section from './fields/Section.svelte'
  import ToggleField from './fields/ToggleField.svelte'
  import SelectField from './fields/SelectField.svelte'
  import NumberField from './fields/NumberField.svelte'
  import TextField from './fields/TextField.svelte'
  import AdvancedDisclosure from './fields/AdvancedDisclosure.svelte'

  interface Props {
    settings: AppSettings
    defaults: AppSettings
  }

  let { settings = $bindable(), defaults }: Props = $props()
  const g = $derived(settings.github)
  const d = $derived(defaults.github)
  let issuesLastSeen = $state<Date | null>(null)
  let reviewsLastSeen = $state<Date | null>(null)

  const ownsSearches = $derived((settings.github.pollerRole ?? '').trim().toLowerCase() !== 'secondary')

  function lastSeenLabel(value: Date | null, fallback: string): string {
    if (!value) return fallback
    return `Last live update: ${value.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' })}`
  }

  function issuesActivity(): string {
    if (!settings.github.enabled) return 'Disabled by top-level GitHub toggle'
    if (!settings.github.polling.issues.enabled) return 'Disabled by Issues stream toggle'
    if (!ownsSearches) return 'Inactive on this machine; secondary skips issue searches'
    return 'Active on this machine'
  }

  function sybraPRsActivity(): string {
    if (!settings.github.enabled) return 'Disabled by top-level GitHub toggle'
    if (!settings.github.polling.sybraPrs.enabled) return 'Disabled by Sybra PR stream toggle'
    if (!ownsSearches) return 'Active in local-only mode; known linked task PRs still reconcile'
    return 'Active on this machine'
  }

  function assignedPRsActivity(): string {
    if (!settings.github.enabled) return 'Disabled by top-level GitHub toggle'
    if (!settings.github.polling.assignedPrs.enabled) return 'Disabled by Assigned PR stream toggle'
    if (!ownsSearches) return 'Inactive on this machine; secondary skips assigned/reviewed searches'
    return 'Active on this machine'
  }

  onMount(() => {
    const offIssues = EventsOn(IssuesUpdated, () => { issuesLastSeen = new Date() })
    const offReviews = EventsOn(ReviewsUpdated, () => { reviewsLastSeen = new Date() })
    return () => {
      offIssues?.()
      offReviews?.()
    }
  })
</script>

<Section
  title="GitHub"
  description="Issue/PR sync, poller role, and native merge automation. On-demand per-PR calls always run; only the periodic search polls are gated."
>
  <ToggleField
    label="Enable GitHub integration"
    description="Fetch issues, poll reviews, and drive Renovate/PR automations for registered projects"
    keyPath="integrations.github.enabled"
    bind:checked={settings.github.enabled}
    modified={g.enabled !== d.enabled}
    onreset={() => (settings.github.enabled = d.enabled)}
  />

  {#if settings.github.enabled}
    <SelectField
      id="gh-poller-role"
      label="Poller role"
      description="secondary skips periodic search polls so a sibling instance sharing the token isn't billed twice"
      keyPath="integrations.github.poller_role"
      options={[
        { value: '', label: 'Primary (runs search polls)' },
        { value: 'primary', label: 'Primary (explicit)' },
        { value: 'secondary', label: 'Secondary (skip search polls)' },
      ]}
      bind:value={settings.github.pollerRole}
      modified={g.pollerRole !== d.pollerRole}
      onreset={() => (settings.github.pollerRole = d.pollerRole)}
    />

    <ToggleField
      label="Native auto-merge"
      description="Arm GitHub's `gh pr merge --auto` on pet-project PRs once Sybra's review/fix cycle is green"
      keyPath="integrations.github.native_auto_merge"
      bind:checked={settings.github.nativeAutoMerge}
      modified={g.nativeAutoMerge !== d.nativeAutoMerge}
      onreset={() => (settings.github.nativeAutoMerge = d.nativeAutoMerge)}
    />

    <ToggleField
      label="Auto-resolve clean merges"
      description="Attempt a deterministic git merge of the base branch before dispatching a conflict-recovery agent"
      keyPath="integrations.github.auto_resolve_clean_merges"
      bind:checked={settings.github.autoResolveCleanMerges}
      modified={g.autoResolveCleanMerges !== d.autoResolveCleanMerges}
      onreset={() => (settings.github.autoResolveCleanMerges = d.autoResolveCleanMerges)}
    />

    <AdvancedDisclosure label="Poll intervals (seconds)">
      <p class="text-xs text-surface-500 dark:text-surface-400">Zero uses the built-in default. Raise to cut request volume; lower only on a high-limit App-token instance.</p>
      <div class="grid grid-cols-1 gap-4">
        <div class="rounded-xl border border-surface-200/70 bg-surface-50/70 p-4 dark:border-surface-700/70 dark:bg-surface-900/40">
          <div class="mb-4 flex items-start justify-between gap-4">
            <div>
              <h4 class="text-sm font-semibold">Issues</h4>
              <p class="text-xs text-surface-500 dark:text-surface-400">{issuesActivity()}</p>
              <p class="text-xs text-surface-500 dark:text-surface-400">{lastSeenLabel(issuesLastSeen, ownsSearches ? 'No live issue update seen in this session' : 'No live issue search runs on secondary')}</p>
            </div>
            <ToggleField
              label="Enable Issues stream"
              description="Assigned/labeled issue ingestion"
              keyPath="integrations.github.polling.issues.enabled"
              bind:checked={settings.github.polling.issues.enabled}
              modified={g.polling.issues.enabled !== d.polling.issues.enabled}
              onreset={() => (settings.github.polling.issues.enabled = d.polling.issues.enabled)}
            />
          </div>
          <NumberField id="gh-issues" label="Issues interval" keyPath="integrations.github.polling.issues.interval" min={0}
            bind:value={settings.github.polling.issues.intervalSeconds}
            modified={g.polling.issues.intervalSeconds !== d.polling.issues.intervalSeconds}
            onreset={() => (settings.github.polling.issues.intervalSeconds = d.polling.issues.intervalSeconds)} />
        </div>
        <div class="rounded-xl border border-surface-200/70 bg-surface-50/70 p-4 dark:border-surface-700/70 dark:bg-surface-900/40">
          <div class="mb-4 flex items-start justify-between gap-4">
            <div>
              <h4 class="text-sm font-semibold">Sybra PRs</h4>
              <p class="text-xs text-surface-500 dark:text-surface-400">{sybraPRsActivity()}</p>
              <p class="text-xs text-surface-500 dark:text-surface-400">{lastSeenLabel(reviewsLastSeen, ownsSearches ? 'No live PR update seen in this session' : 'No review search event on secondary; linked PRs still reconcile locally')}</p>
            </div>
            <ToggleField
              label="Enable Sybra PR stream"
              description="Self-authored and linked task PR monitoring"
              keyPath="integrations.github.polling.sybra_prs.enabled"
              bind:checked={settings.github.polling.sybraPrs.enabled}
              modified={g.polling.sybraPrs.enabled !== d.polling.sybraPrs.enabled}
              onreset={() => (settings.github.polling.sybraPrs.enabled = d.polling.sybraPrs.enabled)}
            />
          </div>
          <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <NumberField id="gh-sybra-prs-fast" label="Active interval" keyPath="integrations.github.polling.sybra_prs.active_interval" min={0}
              bind:value={settings.github.polling.sybraPrs.activeIntervalSeconds}
              modified={g.polling.sybraPrs.activeIntervalSeconds !== d.polling.sybraPrs.activeIntervalSeconds}
              onreset={() => (settings.github.polling.sybraPrs.activeIntervalSeconds = d.polling.sybraPrs.activeIntervalSeconds)} />
            <NumberField id="gh-sybra-prs-slow" label="Idle interval" keyPath="integrations.github.polling.sybra_prs.idle_interval" min={0}
              bind:value={settings.github.polling.sybraPrs.idleIntervalSeconds}
              modified={g.polling.sybraPrs.idleIntervalSeconds !== d.polling.sybraPrs.idleIntervalSeconds}
              onreset={() => (settings.github.polling.sybraPrs.idleIntervalSeconds = d.polling.sybraPrs.idleIntervalSeconds)} />
          </div>
        </div>
        <div class="rounded-xl border border-surface-200/70 bg-surface-50/70 p-4 dark:border-surface-700/70 dark:bg-surface-900/40">
          <div class="mb-4 flex items-start justify-between gap-4">
            <div>
              <h4 class="text-sm font-semibold">Assigned PRs</h4>
              <p class="text-xs text-surface-500 dark:text-surface-400">{assignedPRsActivity()}</p>
              <p class="text-xs text-surface-500 dark:text-surface-400">{lastSeenLabel(reviewsLastSeen, ownsSearches ? 'No live assigned-review update seen in this session' : 'No assigned-review searches run on secondary')}</p>
            </div>
            <ToggleField
              label="Enable Assigned PR stream"
              description="Review-requested and reviewed-by discovery"
              keyPath="integrations.github.polling.assigned_prs.enabled"
              bind:checked={settings.github.polling.assignedPrs.enabled}
              modified={g.polling.assignedPrs.enabled !== d.polling.assignedPrs.enabled}
              onreset={() => (settings.github.polling.assignedPrs.enabled = d.polling.assignedPrs.enabled)}
            />
          </div>
          <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <NumberField id="gh-assigned-prs-fast" label="Active interval" keyPath="integrations.github.polling.assigned_prs.active_interval" min={0}
              bind:value={settings.github.polling.assignedPrs.activeIntervalSeconds}
              modified={g.polling.assignedPrs.activeIntervalSeconds !== d.polling.assignedPrs.activeIntervalSeconds}
              onreset={() => (settings.github.polling.assignedPrs.activeIntervalSeconds = d.polling.assignedPrs.activeIntervalSeconds)} />
            <NumberField id="gh-assigned-prs-slow" label="Idle interval" keyPath="integrations.github.polling.assigned_prs.idle_interval" min={0}
              bind:value={settings.github.polling.assignedPrs.idleIntervalSeconds}
              modified={g.polling.assignedPrs.idleIntervalSeconds !== d.polling.assignedPrs.idleIntervalSeconds}
              onreset={() => (settings.github.polling.assignedPrs.idleIntervalSeconds = d.polling.assignedPrs.idleIntervalSeconds)} />
          </div>
        </div>
        <NumberField id="gh-renovate-fast" label="Renovate (fast)" keyPath="integrations.github.renovate_fast" min={0}
          bind:value={settings.github.renovateFastSeconds}
          modified={g.renovateFastSeconds !== d.renovateFastSeconds}
          onreset={() => (settings.github.renovateFastSeconds = d.renovateFastSeconds)} />
        <NumberField id="gh-renovate-slow" label="Renovate (slow)" keyPath="integrations.github.renovate_slow" min={0}
          bind:value={settings.github.renovateSlowSeconds}
          modified={g.renovateSlowSeconds !== d.renovateSlowSeconds}
          onreset={() => (settings.github.renovateSlowSeconds = d.renovateSlowSeconds)} />
      </div>
    </AdvancedDisclosure>

    <AdvancedDisclosure label="GitHub App (installation-token auth)">
      <p class="text-xs text-surface-500 dark:text-surface-400">Mints a short-lived installation token (15k/hr ceiling). The private key stays on disk — only its path is stored.</p>
      <ToggleField label="Enable App auth" keyPath="integrations.github.app.enabled"
        bind:checked={settings.github.app.enabled}
        modified={g.app.enabled !== d.app.enabled}
        onreset={() => (settings.github.app.enabled = d.app.enabled)} />
      {#if settings.github.app.enabled}
        <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
          <NumberField id="gh-app-id" label="App ID" keyPath="integrations.github.app.app_id" min={0}
            bind:value={settings.github.app.appId}
            modified={g.app.appId !== d.app.appId}
            onreset={() => (settings.github.app.appId = d.app.appId)} />
          <NumberField id="gh-app-inst" label="Installation ID" keyPath="integrations.github.app.installation_id" min={0}
            bind:value={settings.github.app.installationId}
            modified={g.app.installationId !== d.app.installationId}
            onreset={() => (settings.github.app.installationId = d.app.installationId)} />
        </div>
        <TextField id="gh-app-key" label="Private key path" placeholder="/path/to/app-private-key.pem"
          keyPath="integrations.github.app.private_key_path"
          bind:value={settings.github.app.privateKeyPath}
          modified={g.app.privateKeyPath !== d.app.privateKeyPath}
          onreset={() => (settings.github.app.privateKeyPath = d.app.privateKeyPath)} />
      {/if}
    </AdvancedDisclosure>
  {/if}
</Section>
