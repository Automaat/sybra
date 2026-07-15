export type TabKey = 'shell' | 'editor' | 'planner'

export const TAB_KEYS: TabKey[] = ['shell', 'editor', 'planner']

// Tools that map to a specific workspace tab. Unmapped tools (Read, Grep,
// Glob, Task, WebFetch, etc.) return undefined — the caller ignores them
// and the following-mode does not switch tabs for those.
export const TOOL_TO_TAB: Partial<Record<string, TabKey>> = {
  Edit: 'editor',
  Write: 'editor',
  MultiEdit: 'editor',
  Bash: 'shell',
  TodoWrite: 'planner',
}

export function tabForTool(toolName: string): TabKey | undefined {
  return TOOL_TO_TAB[toolName]
}

export interface ToolUseSignal {
  id: string
  name: string
  ts: Date
}
