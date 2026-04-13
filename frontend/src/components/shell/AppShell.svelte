<script lang="ts">
  import type { Snippet } from 'svelte'
  import { AppBar } from '@skeletonlabs/skeleton-svelte'
  import { viewport } from '../../lib/viewport.svelte.js'
  import { navStore } from '../../lib/navigation.svelte.js'
  import SideRail from './SideRail.svelte'
  import BottomTabBar from './BottomTabBar.svelte'
  import MobileAppBar from './MobileAppBar.svelte'
  import MoreSheet from './MoreSheet.svelte'

  interface PrimaryAction {
    label: string
    run: () => void
  }

  interface Props {
    children: Snippet
    onsearch: () => void
    primaryAction?: PrimaryAction | null
  }

  const { children, onsearch, primaryAction }: Props = $props()

  let moreOpen = $state(false)

  // Close MoreSheet when navigation changes (e.g. via back button)
  let lastPageKind = $state<string>(navStore.page.kind)
  $effect(() => {
    if (navStore.page.kind !== lastPageKind) {
      lastPageKind = navStore.page.kind
      moreOpen = false
    }
  })
</script>

<div class="flex h-full min-h-dvh">
  {#if viewport.isDesktop}
    <SideRail />
  {/if}

  <div class="flex min-w-0 flex-1 flex-col overflow-hidden">
    {#if viewport.isDesktop}
      <AppBar>
        <AppBar.Toolbar>
          <AppBar.Lead>
            <h2 class="text-lg font-semibold">{navStore.pageTitle}</h2>
          </AppBar.Lead>
          <AppBar.Trail>
            {#if primaryAction}
              <button
                type="button"
                class="rounded-lg bg-primary-500 px-3 py-1.5 text-sm font-medium text-white hover:bg-primary-600"
                onclick={primaryAction.run}
              >
                + {primaryAction.label}
              </button>
            {/if}
          </AppBar.Trail>
        </AppBar.Toolbar>
      </AppBar>
    {:else}
      <MobileAppBar {onsearch} {primaryAction} />
    {/if}

    <div class="flex flex-1 flex-col overflow-hidden {viewport.isDesktop ? '' : 'pb-[calc(env(safe-area-inset-bottom)+56px)]'}">
      {@render children()}
    </div>
  </div>

  {#if !viewport.isDesktop}
    <BottomTabBar onmore={() => (moreOpen = true)} />
    <MoreSheet open={moreOpen} onOpenChange={(o) => (moreOpen = o)} />
  {/if}
</div>
