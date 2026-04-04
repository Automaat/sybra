import {
  ListProjects,
  GetProject,
  CreateProject,
  UpdateProject,
  DeleteProject,
} from '../../wailsjs/go/main/App.js'
import { project } from '../../wailsjs/go/models.js'
import { EntityStore } from './entity-store.svelte.js'

class ProjectStore extends EntityStore<project.Project> {
  constructor() {
    super(
      () => ListProjects(),
      (a, b) => {
        const ta = a.createdAt ? new Date(a.createdAt).getTime() : 0
        const tb = b.createdAt ? new Date(b.createdAt).getTime() : 0
        return tb - ta
      },
    )
  }

  get projects() {
    return this.items
  }
  set projects(v: Map<string, project.Project>) {
    this.items = v
  }

  async get(id: string): Promise<project.Project> {
    const result = await GetProject(id)
    this.set(result.id, result)
    return result
  }

  async create(url: string, type: string = 'pet'): Promise<project.Project> {
    const result = await CreateProject(url, type)
    this.set(result.id, result)
    return result
  }

  async update(id: string, type: string): Promise<project.Project> {
    const result = await UpdateProject(id, type)
    this.set(result.id, result)
    return result
  }

  async remove(id: string): Promise<void> {
    await DeleteProject(id)
    this.delete(id)
  }
}

export const projectStore = new ProjectStore()
