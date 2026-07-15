// Vitest setup — runs once before all tests in this project.
//
// The @wailsio/runtime package eagerly attaches drag/contextmenu side
// effects on import (sets up window.setInterval polling for the wails
// environment). Under jsdom that timer keeps firing after vitest tears
// the document down, producing "window is not defined" Unhandled Errors.
// Stub the module so tests that don't care about IPC don't pay for the
// side effect; tests that do exercise wails calls mock these names
// per-suite as needed.

import { vi } from 'vitest'

vi.mock('@wailsio/runtime', () => ({
  Application: {},
  Browser: { OpenURL: vi.fn() },
  Call: { ByID: vi.fn(), ByName: vi.fn() },
  Clipboard: {},
  Dialogs: {},
  Events: {
    On: vi.fn(() => () => {}),
    Once: vi.fn(() => () => {}),
    OnMultiple: vi.fn(),
    Off: vi.fn(),
    OffAll: vi.fn(),
    Emit: vi.fn(),
  },
  Flags: {},
  Screens: {},
  System: { invoke: vi.fn() },
  IOS: {},
  WML: {},
  Window: {},
  Create: {
    Any: (v: unknown) => v,
    Array: <T>(creator: (v: unknown) => T) => (arr: unknown[] = []) => arr.map(creator),
    Map: () => (m: unknown) => m,
    Nullable: <T>(creator: (v: unknown) => T) => (v: unknown) => v == null ? null : creator(v),
  },
  CancellablePromise: Promise,
  setTransport: vi.fn(),
  getTransport: vi.fn(),
  objectNames: {},
  clientId: 'test',
}))

// jsdom doesn't implement scrollIntoView, but several components call it when
// focusing/scrolling a card or column into view. Stub it globally so those
// side effects don't throw "not a function" Unhandled Errors during tests.
if (typeof Element !== 'undefined' && typeof Element.prototype.scrollIntoView !== 'function') {
  Element.prototype.scrollIntoView = vi.fn()
}
