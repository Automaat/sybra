export interface Shortcut {
  keys: string
  description: string
}

export const SHORTCUTS: { scope: string; label: string; shortcuts: Shortcut[] }[] = [
  {
    scope: 'global',
    label: 'Global',
    shortcuts: [
      { keys: '⌘K', description: 'Command palette' },
      { keys: '⌘/', description: 'Keyboard shortcuts help' },
      { keys: '⌘N', description: 'New task' },
      { keys: '⌘1 – ⌘7', description: 'Navigate pages' },
      { keys: '⌘,', description: 'Settings' },
      { keys: '⌘F', description: 'Focus task search' },
      { keys: '⌘=  /  ⌘-  /  ⌘0', description: 'Zoom in / out / reset' },
    ],
  },
  {
    scope: 'task-list',
    label: 'Task Board',
    shortcuts: [
      { keys: 'J  /  ↓', description: 'Move focus down' },
      { keys: 'K  /  ↑', description: 'Move focus up' },
      { keys: 'H  /  ←', description: 'Move to previous column' },
      { keys: 'L  /  →', description: 'Move to next column' },
      { keys: 'Enter', description: 'Open focused task' },
      { keys: 'Esc', description: 'Clear focus' },
    ],
  },
  {
    scope: 'task-detail',
    label: 'Task Detail',
    shortcuts: [
      { keys: 'E', description: 'Edit description' },
      { keys: 'S', description: 'Focus status selector' },
      { keys: 'D', description: 'Delete task' },
      { keys: 'Esc', description: 'Back to board' },
    ],
  },
  {
    scope: 'reviews',
    label: 'Reviews',
    shortcuts: [
      { keys: 'A', description: 'Approve selected plan' },
      { keys: 'R', description: 'Reject selected plan' },
      { keys: 'J  /  K', description: 'Navigate between plans' },
      { keys: 'C', description: 'Focus feedback input' },
    ],
  },
]
