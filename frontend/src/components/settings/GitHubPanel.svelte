<script lang="ts">
  import type { AppSettings } from '../../../bindings/github.com/Automaat/sybra/internal/sybra/models.js'
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
</script>

<Section
  title="GitHub"
  description="Issue/PR sync, poller role, and native merge automation. On-demand per-PR calls always run; only the periodic search polls are gated."
>
  <ToggleField
    label="Enable GitHub integration"
    description="Fetch issues, poll reviews, and drive Renovate/PR automations for registered projects"
    keyPath="github.enabled"
    bind:checked={settings.github.enabled}
    modified={g.enabled !== d.enabled}
    onreset={() => (settings.github.enabled = d.enabled)}
  />

  {#if settings.github.enabled}
    <SelectField
      id="gh-poller-role"
      label="Poller role"
      description="secondary skips periodic search polls so a sibling instance sharing the token isn't billed twice"
      keyPath="github.poller_role"
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
      keyPath="github.native_auto_merge"
      bind:checked={settings.github.nativeAutoMerge}
      modified={g.nativeAutoMerge !== d.nativeAutoMerge}
      onreset={() => (settings.github.nativeAutoMerge = d.nativeAutoMerge)}
    />

    <ToggleField
      label="Auto-resolve clean merges"
      description="Attempt a deterministic git merge of the base branch before dispatching a conflict-recovery agent"
      keyPath="github.auto_resolve_clean_merges"
      bind:checked={settings.github.autoResolveCleanMerges}
      modified={g.autoResolveCleanMerges !== d.autoResolveCleanMerges}
      onreset={() => (settings.github.autoResolveCleanMerges = d.autoResolveCleanMerges)}
    />

    <AdvancedDisclosure label="Poll intervals (seconds)">
      <p class="text-xs text-surface-500 dark:text-surface-400">Zero uses the built-in default. Raise to cut request volume; lower only on a high-limit App-token instance.</p>
      <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <NumberField id="gh-reviews-fast" label="Reviews (fast)" keyPath="github.reviews_fast_seconds" min={0}
          bind:value={settings.github.reviewsFastSeconds}
          modified={g.reviewsFastSeconds !== d.reviewsFastSeconds}
          onreset={() => (settings.github.reviewsFastSeconds = d.reviewsFastSeconds)} />
        <NumberField id="gh-reviews-slow" label="Reviews (slow)" keyPath="github.reviews_slow_seconds" min={0}
          bind:value={settings.github.reviewsSlowSeconds}
          modified={g.reviewsSlowSeconds !== d.reviewsSlowSeconds}
          onreset={() => (settings.github.reviewsSlowSeconds = d.reviewsSlowSeconds)} />
        <NumberField id="gh-issues" label="Issues" keyPath="github.issues_seconds" min={0}
          bind:value={settings.github.issuesSeconds}
          modified={g.issuesSeconds !== d.issuesSeconds}
          onreset={() => (settings.github.issuesSeconds = d.issuesSeconds)} />
        <NumberField id="gh-renovate-fast" label="Renovate (fast)" keyPath="github.renovate_fast_seconds" min={0}
          bind:value={settings.github.renovateFastSeconds}
          modified={g.renovateFastSeconds !== d.renovateFastSeconds}
          onreset={() => (settings.github.renovateFastSeconds = d.renovateFastSeconds)} />
        <NumberField id="gh-renovate-slow" label="Renovate (slow)" keyPath="github.renovate_slow_seconds" min={0}
          bind:value={settings.github.renovateSlowSeconds}
          modified={g.renovateSlowSeconds !== d.renovateSlowSeconds}
          onreset={() => (settings.github.renovateSlowSeconds = d.renovateSlowSeconds)} />
      </div>
    </AdvancedDisclosure>

    <AdvancedDisclosure label="GitHub App (installation-token auth)">
      <p class="text-xs text-surface-500 dark:text-surface-400">Mints a short-lived installation token (15k/hr ceiling). The private key stays on disk — only its path is stored.</p>
      <ToggleField label="Enable App auth" keyPath="github.app.enabled"
        bind:checked={settings.github.app.enabled}
        modified={g.app.enabled !== d.app.enabled}
        onreset={() => (settings.github.app.enabled = d.app.enabled)} />
      {#if settings.github.app.enabled}
        <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
          <NumberField id="gh-app-id" label="App ID" keyPath="github.app.app_id" min={0}
            bind:value={settings.github.app.appId}
            modified={g.app.appId !== d.app.appId}
            onreset={() => (settings.github.app.appId = d.app.appId)} />
          <NumberField id="gh-app-inst" label="Installation ID" keyPath="github.app.installation_id" min={0}
            bind:value={settings.github.app.installationId}
            modified={g.app.installationId !== d.app.installationId}
            onreset={() => (settings.github.app.installationId = d.app.installationId)} />
        </div>
        <TextField id="gh-app-key" label="Private key path" placeholder="/path/to/app-private-key.pem"
          keyPath="github.app.private_key_path"
          bind:value={settings.github.app.privateKeyPath}
          modified={g.app.privateKeyPath !== d.app.privateKeyPath}
          onreset={() => (settings.github.app.privateKeyPath = d.app.privateKeyPath)} />
      {/if}
    </AdvancedDisclosure>
  {/if}
</Section>
