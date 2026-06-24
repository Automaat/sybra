<script lang="ts">
  import type { Snippet } from 'svelte'
  import { pillClass, type PillRole } from '../lib/pills.js'

  interface Props {
    role: PillRole
    children: Snippet
    title?: string
    /** Extra classes layered on the role treatment (e.g. status colour). */
    class?: string
  }

  const { role, children, title = undefined, class: extra = '' }: Props = $props()
</script>

<span class={pillClass(role, extra)} {title}>{@render children()}</span>
