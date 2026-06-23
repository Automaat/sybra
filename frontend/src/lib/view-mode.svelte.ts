export type ViewMode = 'list' | 'board' | 'timeline'

const MODES: ViewMode[] = ['list', 'board', 'timeline']
const STORAGE_KEY = 'taskViewMode'

function loadStored(): ViewMode {
  try {
    const v = localStorage.getItem(STORAGE_KEY)
    if (v === 'list' || v === 'board' || v === 'timeline') return v
  } catch {
    // localStorage unavailable (SSR / restricted context)
  }
  // List is the most scannable surface and stays useful even when the board's
  // columns are mostly empty, so it's the default first paint.
  return 'list'
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
      const next = MODES[(MODES.indexOf(mode) + 1) % MODES.length]
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
