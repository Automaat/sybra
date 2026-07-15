import { SvelteMap } from 'svelte/reactivity'
import {
  ListReviewComments,
  AddReviewComment,
  ResolveReviewComment,
  DeleteReviewComment,
} from '$lib/api'
import { ReviewComment } from '../../bindings/github.com/Automaat/sybra/internal/task/models.js'

class CommentStore {
  private byTask = new SvelteMap<string, ReviewComment[]>()

  get(taskID: string): ReviewComment[] {
    return this.byTask.get(taskID) ?? []
  }

  async load(taskID: string): Promise<void> {
    const result = await ListReviewComments(taskID)
    this.byTask.set(taskID, result ?? [])
  }

  async add(taskID: string, line: number, body: string): Promise<ReviewComment> {
    // Optimistic placeholder so UI updates instantly
    const optimistic = ReviewComment.createFrom({
      id: crypto.randomUUID().slice(0, 8),
      line,
      body,
      resolved: false,
      createdAt: new Date().toISOString(),
    })
    const existing = this.byTask.get(taskID) ?? []
    this.byTask.set(taskID, [...existing, optimistic])

    try {
      const persisted = await AddReviewComment(taskID, line, body)
      // Replace optimistic with real server-assigned comment
      const current = this.byTask.get(taskID) ?? []
      this.byTask.set(
        taskID,
        current.map((c) => (c.id === optimistic.id ? persisted : c)),
      )
      return persisted
    } catch (e) {
      // Rollback on failure
      const current = this.byTask.get(taskID) ?? []
      this.byTask.set(
        taskID,
        current.filter((c) => c.id !== optimistic.id),
      )
      throw e
    }
  }

  async resolve(taskID: string, commentID: string): Promise<void> {
    // Optimistic update
    const existing = this.byTask.get(taskID) ?? []
    this.byTask.set(
      taskID,
      existing.map((c) => (c.id === commentID ? ReviewComment.createFrom({ ...c, resolved: true }) : c)),
    )
    try {
      await ResolveReviewComment(taskID, commentID)
    } catch (e) {
      // Rollback
      this.byTask.set(
        taskID,
        existing.map((c) => (c.id === commentID ? ReviewComment.createFrom({ ...c, resolved: false }) : c)),
      )
      throw e
    }
  }

  async remove(taskID: string, commentID: string): Promise<void> {
    // Optimistic update
    const existing = this.byTask.get(taskID) ?? []
    this.byTask.set(
      taskID,
      existing.filter((c) => c.id !== commentID),
    )
    try {
      await DeleteReviewComment(taskID, commentID)
    } catch (e) {
      // Rollback
      this.byTask.set(taskID, existing)
      throw e
    }
  }

  byLine(taskID: string, line: number): ReviewComment[] {
    return this.get(taskID).filter((c) => c.line === line)
  }

  unresolvedCount(taskID: string): number {
    return this.get(taskID).filter((c) => !c.resolved).length
  }
}

export const commentStore = new CommentStore()
