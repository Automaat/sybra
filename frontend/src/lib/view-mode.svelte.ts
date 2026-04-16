export type ViewMode = 'list' | 'board' | 'timeline'

const MODES: ViewMode[] = ['list', 'board', 'timeline']
const STORAGE_KEY = 'taskViewMode'

function loadStored(): ViewMode {
  try {
    const v = sessionStorage.getItem(STORAGE_KEY)
    if (v === 'list' || v === 'board' || v === 'timeline') return v
  } catch {
    // sessionStorage unavailable (SSR / restricted context)
  }
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
        sessionStorage.setItem(STORAGE_KEY, v)
      } catch {
        // ignore
      }
    },
    cycle(): void {
      const next = MODES[(MODES.indexOf(mode) + 1) % MODES.length]
      mode = next
      try {
        sessionStorage.setItem(STORAGE_KEY, next)
      } catch {
        // ignore
      }
    },
  }
}

export const viewModeStore = createViewModeStore()
