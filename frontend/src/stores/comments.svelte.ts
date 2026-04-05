import {
  ListReviewComments,
  AddReviewComment,
  ResolveReviewComment,
  DeleteReviewComment,
} from '../../wailsjs/go/main/App.js'
import { task } from '../../wailsjs/go/models.js'

class CommentStore {
  private byTask = $state<Map<string, task.ReviewComment[]>>(new Map())

  get(taskID: string): task.ReviewComment[] {
    return this.byTask.get(taskID) ?? []
  }

  async load(taskID: string): Promise<void> {
    const result = await ListReviewComments(taskID)
    this.byTask.set(taskID, result ?? [])
  }

  async add(taskID: string, line: number, body: string): Promise<task.ReviewComment> {
    const comment = await AddReviewComment(taskID, line, body)
    const existing = this.byTask.get(taskID) ?? []
    this.byTask.set(taskID, [...existing, comment])
    return comment
  }

  async resolve(taskID: string, commentID: string): Promise<void> {
    await ResolveReviewComment(taskID, commentID)
    const existing = this.byTask.get(taskID) ?? []
    this.byTask.set(
      taskID,
      existing.map((c) => (c.id === commentID ? task.ReviewComment.createFrom({ ...c, resolved: true }) : c)),
    )
  }

  async remove(taskID: string, commentID: string): Promise<void> {
    await DeleteReviewComment(taskID, commentID)
    const existing = this.byTask.get(taskID) ?? []
    this.byTask.set(
      taskID,
      existing.filter((c) => c.id !== commentID),
    )
  }

  byLine(taskID: string, line: number): task.ReviewComment[] {
    return this.get(taskID).filter((c) => c.line === line)
  }

  unresolvedCount(taskID: string): number {
    return this.get(taskID).filter((c) => !c.resolved).length
  }
}

export const commentStore = new CommentStore()
