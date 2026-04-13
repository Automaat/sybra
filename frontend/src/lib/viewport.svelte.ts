// Reactive viewport helpers backed by matchMedia.
// Used by AppShell and conditional mobile/desktop renders.

const PHONE_MAX = '(max-width: 767px)'
const TABLET_RANGE = '(min-width: 768px) and (max-width: 1023px)'
const DESKTOP_MIN = '(min-width: 1024px)'
const COARSE_POINTER = '(pointer: coarse)'
const STANDALONE = '(display-mode: standalone)'

function bind(query: string): { value: boolean } {
  const state = $state({ value: false })
  if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return state
  const mq = window.matchMedia(query)
  state.value = mq.matches
  mq.addEventListener('change', (e) => { state.value = e.matches })
  return state
}

const _phone = bind(PHONE_MAX)
const _tablet = bind(TABLET_RANGE)
const _desktop = bind(DESKTOP_MIN)
const _coarse = bind(COARSE_POINTER)
const _standalone = bind(STANDALONE)

export const viewport = {
  get isPhone() { return _phone.value },
  get isTablet() { return _tablet.value },
  get isDesktop() { return _desktop.value },
  get isMobile() { return !_desktop.value },
  get hasCoarsePointer() { return _coarse.value },
  get hasFinePointer() { return !_coarse.value },
  get isStandalone() { return _standalone.value },
}
