import { notificationStore } from '../stores/notifications.svelte.js'

/**
 * Wraps an async callback so errors are caught and shown as notifications
 * instead of propagating and crashing the component.
 */
export function guard<Args extends unknown[]>(
  fn: (...args: Args) => void | Promise<void>,
  title = 'Action failed',
): (...args: Args) => Promise<void> {
  return async (...args: Args) => {
    try {
      await fn(...args)
    } catch (err) {
      notificationStore.pushLocal('error', title, String(err))
    }
  }
}
