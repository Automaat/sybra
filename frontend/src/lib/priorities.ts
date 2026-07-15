export interface PriorityMeta {
  value: string
  label: string
  icon: string
  classes: string
}

export const PRIORITY_OPTIONS: PriorityMeta[] = [
  { value: '', label: 'None', icon: '–', classes: 'text-surface-400' },
  { value: 'low', label: 'Low', icon: '↓', classes: 'text-secondary-500' },
  { value: 'medium', label: 'Medium', icon: '→', classes: 'text-warning-500' },
  { value: 'high', label: 'High', icon: '↑', classes: 'text-error-400' },
  { value: 'urgent', label: 'Urgent', icon: '‼', classes: 'text-error-600 font-bold' },
]
