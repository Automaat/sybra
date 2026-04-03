import { ListProjects, GetProject, CreateProject, DeleteProject } from '../../wailsjs/go/main/App.js'
import { project } from '../../wailsjs/go/models.js'

class ProjectStore {
  projects = $state<Map<string, project.Project>>(new Map())
  loading = $state(false)
  error = $state('')
  private pollTimer: ReturnType<typeof setInterval> | null = null

  get list(): project.Project[] {
    return [...this.projects.values()].sort((a, b) => {
      const ta = a.createdAt ? new Date(a.createdAt).getTime() : 0
      const tb = b.createdAt ? new Date(b.createdAt).getTime() : 0
      return tb - ta
    })
  }

  async load(): Promise<void> {
    this.loading = true
    this.error = ''
    try {
      const result = await ListProjects()
      const map = new Map<string, project.Project>()
      for (const p of result ?? []) {
        map.set(p.id, p)
      }
      this.projects = map
    } catch (e) {
      this.error = String(e)
    } finally {
      this.loading = false
    }
  }

  async get(id: string): Promise<project.Project> {
    const result = await GetProject(id)
    this.projects.set(result.id, result)
    return result
  }

  async create(url: string): Promise<project.Project> {
    const result = await CreateProject(url)
    this.projects.set(result.id, result)
    return result
  }

  async remove(id: string): Promise<void> {
    await DeleteProject(id)
    this.projects.delete(id)
  }

  startPolling(): void {
    this.stopPolling()
    this.pollTimer = setInterval(() => this.load(), 5000)
  }

  stopPolling(): void {
    if (this.pollTimer) {
      clearInterval(this.pollTimer)
      this.pollTimer = null
    }
  }
}

export const projectStore = new ProjectStore()
