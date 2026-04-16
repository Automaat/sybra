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
      { keys: '?', description: 'Keyboard shortcuts help' },
      { keys: '⌘/', description: 'Keyboard shortcuts help' },
      { keys: '⌘N', description: 'New task (quick add)' },
      { keys: '⌘1 – ⌘7', description: 'Navigate pages' },
      { keys: '⌘,', description: 'Settings' },
      { keys: '⌘F  /  /', description: 'Focus task search' },
      { keys: '⌘B', description: 'Cycle view: List → Board → Timeline' },
      { keys: '⌘I', description: 'Open task detail sidebar' },
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
      { keys: 'Enter  /  E', description: 'Open focused task' },
      { keys: 'C', description: 'New task' },
      { keys: 'S', description: 'Change status of focused task' },
      { keys: 'P', description: 'Change priority of focused task' },
      { keys: '⇧C', description: 'Add focused task to project' },
      { keys: '⌘D', description: 'Set due date on focused task' },
      { keys: 'Esc', description: 'Clear focus' },
      { keys: '+  /  -', description: 'Zoom in / out (Timeline view)' },
    ],
  },
  {
    scope: 'task-detail',
    label: 'Task Detail',
    shortcuts: [
      { keys: 'E', description: 'Edit description' },
      { keys: 'S', description: 'Focus status selector' },
      { keys: '⌘D', description: 'Set due date' },
      { keys: 'D', description: 'Delete task' },
      { keys: 'Esc', description: 'Back to board' },
      { keys: '⌘.', description: 'Copy task ID' },
      { keys: '⇧⌘.', description: 'Copy branch name' },
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
  {
    scope: 'global',
    label: 'Quick Navigation',
    shortcuts: [
      { keys: 'G  I', description: 'All Tasks' },
      { keys: 'G  A', description: 'Active (In Progress)' },
      { keys: 'G  P', description: 'Projects' },
      { keys: 'G  S', description: 'Settings' },
    ],
  },
]
