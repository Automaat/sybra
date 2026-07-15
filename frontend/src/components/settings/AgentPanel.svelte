<script lang="ts">
  import type { AppSettings, RuntimeInfo } from '../../../bindings/github.com/Automaat/sybra/internal/sybra/models.js'
  import Section from './fields/Section.svelte'
  import SelectField from './fields/SelectField.svelte'
  import NumberField from './fields/NumberField.svelte'
  import TextField from './fields/TextField.svelte'
  import AdvancedDisclosure from './fields/AdvancedDisclosure.svelte'

  interface Props {
    settings: AppSettings
    defaults: AppSettings
    modelOptions: { value: string; label: string }[]
    runtimes: RuntimeInfo[]
    /** Force the advanced disclosure open (search/modified-filter match). */
    advancedOpen?: boolean
  }

  let { settings = $bindable(), defaults, modelOptions, runtimes, advancedOpen = false }: Props = $props()
  const a = $derived(settings.agent)
  const d = $derived(defaults.agent)

  // *bool tri-state: null = "default", true/false explicit. Rendered as a select.
  const triOptions = [
    { value: 'default', label: 'Default' },
    { value: 'true', label: 'Enabled' },
    { value: 'false', label: 'Disabled' },
  ]
  function triToStr(v: boolean | null): string {
    return v === null || v === undefined ? 'default' : v ? 'true' : 'false'
  }
  function strToTri(s: string): boolean | null {
    return s === 'default' ? null : s === 'true'
  }
</script>

<Section title="Agent defaults" description="Provider, model, and execution defaults applied to new tasks.">
  <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
    <SelectField id="agent-provider" label="Agent type" keyPath="agent.provider"
      options={[
        { value: 'claude', label: 'Claude' },
        { value: 'codex', label: 'Codex' },
        { value: 'copilot', label: 'Copilot' },
        { value: 'opencode', label: 'OpenCode' },
      ]}
      bind:value={settings.agent.provider}
      modified={a.provider !== d.provider}
      onreset={() => (settings.agent.provider = d.provider)} />
    <SelectField id="agent-model" label="Default model" keyPath="agent.model"
      options={modelOptions}
      bind:value={settings.agent.model}
      modified={a.model !== d.model}
      onreset={() => (settings.agent.model = d.model)} />
    <SelectField id="agent-mode" label="Default mode" keyPath="agent.mode"
      options={[
        { value: '', label: '— none —' },
        { value: 'headless', label: 'Headless' },
        { value: 'interactive', label: 'Interactive' },
      ]}
      bind:value={settings.agent.mode}
      modified={a.mode !== d.mode}
      onreset={() => (settings.agent.mode = d.mode)} />
    <NumberField id="agent-concurrency" label="Max concurrent" keyPath="agent.max_concurrent" min={1} max={100}
      description="1–100"
      bind:value={settings.agent.maxConcurrent}
      modified={a.maxConcurrent !== d.maxConcurrent}
      onreset={() => (settings.agent.maxConcurrent = d.maxConcurrent)} />
  </div>

  <div class="mt-4 rounded-lg border border-surface-200 bg-white p-4 dark:border-surface-700 dark:bg-surface-900">
    <div class="mb-3 flex items-center justify-between gap-3">
      <div class="flex flex-col">
        <h3 class="text-sm font-medium text-surface-800 dark:text-surface-100">Detected runtimes</h3>
        <span class="text-xs text-surface-500 dark:text-surface-400">Read-only startup snapshot from PATH</span>
      </div>
    </div>
    {#if runtimes.length === 0}
      <p class="text-xs text-surface-500 dark:text-surface-400">Runtime scan unavailable.</p>
    {:else}
      <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
        {#each runtimes as runtime (runtime.id)}
          <div class="rounded-lg border border-surface-200 bg-surface-50 px-3 py-2 dark:border-surface-700 dark:bg-surface-800">
            <div class="flex items-center gap-2">
              <span class="font-medium text-surface-800 dark:text-surface-100">{runtime.name}</span>
              <span class="rounded px-1.5 py-0.5 text-xs {runtime.installed ? 'bg-success-500/15 text-success-700 dark:text-success-300' : 'bg-surface-300 text-surface-600 dark:bg-surface-700 dark:text-surface-300'}">
                {runtime.installed ? 'installed' : 'missing'}
              </span>
              {#if runtime.informationalOnly}
                <span class="rounded px-1.5 py-0.5 text-xs bg-primary-500/10 text-primary-700 dark:text-primary-300">info only</span>
              {/if}
            </div>
            <div class="mt-2 flex flex-col gap-1">
              <span class="font-mono text-[11px] text-surface-500 dark:text-surface-400">
                {runtime.path || 'Not found on PATH'}
              </span>
              {#if runtime.version}
                <span class="text-xs text-surface-600 dark:text-surface-300">Version: {runtime.version}</span>
              {/if}
              {#if runtime.error}
                <span class="text-xs text-warning-700 dark:text-warning-300">Probe: {runtime.error}</span>
              {/if}
            </div>
          </div>
        {/each}
      </div>
    {/if}
  </div>

  <AdvancedDisclosure open={advancedOpen}>
    <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
      <SelectField id="agent-permmode" label="Headless permission mode" keyPath="agent.headless_permission_mode"
        description="auto activates the destructive-op classifier; bypass keeps full skip-permissions"
        options={[
          { value: '', label: 'Bypass (default)' },
          { value: 'bypass', label: 'Bypass (explicit)' },
          { value: 'auto', label: 'Auto (classifier)' },
        ]}
        bind:value={settings.agent.headlessPermissionMode}
        modified={a.headlessPermissionMode !== d.headlessPermissionMode}
        onreset={() => (settings.agent.headlessPermissionMode = d.headlessPermissionMode)} />

      <div class="flex flex-col gap-1 border-l-2 border-transparent pl-3">
        <label class="text-sm font-medium text-surface-800 dark:text-surface-100" for="agent-reqperm">Require permissions</label>
        <select id="agent-reqperm"
          class="w-full max-w-xs rounded-lg border border-surface-300 bg-white px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
          value={triToStr(settings.agent.requirePermissions)}
          onchange={(e) => (settings.agent.requirePermissions = strToTri((e.target as HTMLSelectElement).value))}
        >
          {#each triOptions as o (o.value)}<option value={o.value}>{o.label}</option>{/each}
        </select>
        <span class="text-xs text-surface-500 dark:text-surface-400">Default = true (approval hook gates each tool call)</span>
      </div>

      <TextField id="agent-fallback" label="Fallback model" placeholder="e.g. sonnet" keyPath="agent.fallback_model"
        bind:value={settings.agent.fallbackModel}
        modified={a.fallbackModel !== d.fallbackModel}
        onreset={() => (settings.agent.fallbackModel = d.fallbackModel)} />

      <div class="flex flex-col gap-1 border-l-2 border-transparent pl-3">
        <label class="text-sm font-medium text-surface-800 dark:text-surface-100" for="agent-survive">Survive restart</label>
        <select id="agent-survive"
          class="w-full max-w-xs rounded-lg border border-surface-300 bg-white px-3 py-2 text-sm dark:border-surface-600 dark:bg-surface-700"
          value={triToStr(settings.agent.surviveRestart)}
          onchange={(e) => (settings.agent.surviveRestart = strToTri((e.target as HTMLSelectElement).value))}
        >
          {#each triOptions as o (o.value)}<option value={o.value}>{o.label}</option>{/each}
        </select>
        <span class="text-xs text-surface-500 dark:text-surface-400">Keep agents running across an app restart (default true)</span>
      </div>

      <NumberField id="agent-maxcost" label="Max cost per run (USD)" keyPath="agent.max_cost_usd" min={0} step={0.5}
        bind:value={settings.agent.maxCostUsd}
        modified={a.maxCostUsd !== d.maxCostUsd}
        onreset={() => (settings.agent.maxCostUsd = d.maxCostUsd)} />
      <NumberField id="agent-maxturns" label="Max turns" keyPath="agent.max_turns" min={0}
        bind:value={settings.agent.maxTurns}
        modified={a.maxTurns !== d.maxTurns}
        onreset={() => (settings.agent.maxTurns = d.maxTurns)} />
      <NumberField id="agent-bash" label="Bash timeout (seconds)" keyPath="agent.bash_timeout_seconds" min={0}
        description="0 = default (300)"
        bind:value={settings.agent.bashTimeoutSeconds}
        modified={a.bashTimeoutSeconds !== d.bashTimeoutSeconds}
        onreset={() => (settings.agent.bashTimeoutSeconds = d.bashTimeoutSeconds)} />
      <NumberField id="agent-watchdog" label="Retry watchdog" keyPath="agent.retry_watchdog" min={-1}
        description="0 = default (30); -1 disables"
        bind:value={settings.agent.retryWatchdog}
        modified={a.retryWatchdog !== d.retryWatchdog}
        onreset={() => (settings.agent.retryWatchdog = d.retryWatchdog)} />
      <NumberField id="agent-jitter" label="Dispatch jitter (ms)" keyPath="agent.dispatch_jitter_ms" min={0}
        description="Random pre-dispatch delay to de-sync ready tasks (0 = off)"
        bind:value={settings.agent.dispatchJitterMs}
        modified={a.dispatchJitterMs !== d.dispatchJitterMs}
        onreset={() => (settings.agent.dispatchJitterMs = d.dispatchJitterMs)} />
      <NumberField id="agent-logret" label="Log retention (days)" keyPath="agent.log_retention_days" min={-1}
        description="-1 disables age pruning; 0 = default (14)"
        bind:value={settings.agent.logRetentionDays}
        modified={a.logRetentionDays !== d.logRetentionDays}
        onreset={() => (settings.agent.logRetentionDays = d.logRetentionDays)} />
      <NumberField id="agent-loggzip" label="Log gzip after (days)" keyPath="agent.log_gzip_after_days" min={-1}
        description="-1 disables compression; 0 = default (3)"
        bind:value={settings.agent.logGzipAfterDays}
        modified={a.logGzipAfterDays !== d.logGzipAfterDays}
        onreset={() => (settings.agent.logGzipAfterDays = d.logGzipAfterDays)} />
      <NumberField id="agent-logmax" label="Log max size (MB)" keyPath="agent.log_retention_max_size_mb" min={-1}
        description="-1 disables size cap; 0 = default (1024)"
        bind:value={settings.agent.logRetentionMaxSizeMb}
        modified={a.logRetentionMaxSizeMb !== d.logRetentionMaxSizeMb}
        onreset={() => (settings.agent.logRetentionMaxSizeMb = d.logRetentionMaxSizeMb)} />
      <NumberField id="agent-approval-port" label="Approval port" keyPath="agent.approval_port" min={0} max={65535}
        description="Pin the localhost approval-server port (0 = random)"
        bind:value={settings.agent.approvalPort}
        modified={a.approvalPort !== d.approvalPort}
        onreset={() => (settings.agent.approvalPort = d.approvalPort)} />
    </div>
  </AdvancedDisclosure>
</Section>
