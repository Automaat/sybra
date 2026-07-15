// Focus mode — a persisted UI preference that trades power-user density for a
// clean, first-time-friendly surface (minimal sidebar, list-first board).
const STORAGE_KEY = 'focusMode'

function loadStored(): boolean {
  try {
    return localStorage.getItem(STORAGE_KEY) === 'true'
  } catch {
    // localStorage unavailable (SSR / restricted context)
    return false
  }
}

function createFocusModeStore() {
  let enabled = $state<boolean>(loadStored())

  function persist(): void {
    try {
      localStorage.setItem(STORAGE_KEY, String(enabled))
    } catch {
      // ignore
    }
  }

  return {
    get enabled(): boolean {
      return enabled
    },
    set(v: boolean): void {
      enabled = v
      persist()
    },
    toggle(): void {
      enabled = !enabled
      persist()
    },
  }
}

export const focusModeStore = createFocusModeStore()
