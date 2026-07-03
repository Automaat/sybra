export type ViewMode = 'list' | 'board' | 'timeline'

// The quick-switch cycle (⌘B) rotates only the primary views. Timeline is a
// de-emphasized advanced view, reachable via its own button, not the cycle.
const CYCLE_MODES: ViewMode[] = ['list', 'board']
const STORAGE_KEY = 'taskViewMode'

function loadStored(): ViewMode {
  try {
    const v = localStorage.getItem(STORAGE_KEY)
    if (v === 'list' || v === 'board' || v === 'timeline') return v
  } catch {
    // localStorage unavailable (SSR / restricted context)
  }
  // Board is the default first paint — the pipeline columns give the clearest
  // at-a-glance picture of where work sits. Users can switch to list (⌘B) and
  // that choice is remembered.
  return 'board'
}

function createViewModeStore() {
  let mode = $state<ViewMode>(loadStored())

  return {
    get mode(): ViewMode {
      return mode
    },
    set(v: ViewMode): void {
      mode = v
      try {
        localStorage.setItem(STORAGE_KEY, v)
      } catch {
        // ignore
      }
    },
    cycle(): void {
      // From timeline (or any non-primary), indexOf is -1 → lands on 'list'.
      const next = CYCLE_MODES[(CYCLE_MODES.indexOf(mode) + 1) % CYCLE_MODES.length]
      mode = next
      try {
        localStorage.setItem(STORAGE_KEY, next)
      } catch {
        // ignore
      }
    },
  }
}

export const viewModeStore = createViewModeStore()
