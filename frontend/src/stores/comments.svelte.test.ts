import { describe, it, expect, vi, beforeEach } from 'vitest'
import { task } from '../../wailsjs/go/models.js'

const mockListReviewComments = vi.fn()
const mockAddReviewComment = vi.fn()
const mockResolveReviewComment = vi.fn()
const mockDeleteReviewComment = vi.fn()

vi.mock('$lib/api', () => ({
  ListReviewComments: (...args: unknown[]) => mockListReviewComments(...args),
  AddReviewComment: (...args: unknown[]) => mockAddReviewComment(...args),
  ResolveReviewComment: (...args: unknown[]) => mockResolveReviewComment(...args),
  DeleteReviewComment: (...args: unknown[]) => mockDeleteReviewComment(...args),
}))

const { commentStore } = await import('./comments.svelte.js')

function makeComment(overrides: Partial<task.ReviewComment> = {}): task.ReviewComment {
  return task.ReviewComment.createFrom({
    id: 'c-1',
    line: 10,
    body: 'Looks good',
    resolved: false,
    createdAt: new Date().toISOString(),
    ...overrides,
  })
}

describe('CommentStore', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    // Reset store by loading empty array
    mockListReviewComments.mockResolvedValue([])
  })

  describe('load', () => {
    it('fetches and stores comments for task', async () => {
      const comments = [makeComment({ id: 'c1' }), makeComment({ id: 'c2' })]
      mockListReviewComments.mockResolvedValue(comments)

      await commentStore.load('task-1')

      expect(mockListReviewComments).toHaveBeenCalledWith('task-1')
      expect(commentStore.get('task-1')).toHaveLength(2)
    })

    it('handles null result', async () => {
      mockListReviewComments.mockResolvedValue(null)

      await commentStore.load('task-1')

      expect(commentStore.get('task-1')).toHaveLength(0)
    })
  })

  describe('get', () => {
    it('returns empty array for unknown task', () => {
      expect(commentStore.get('nonexistent')).toEqual([])
    })

    it('returns stored comments after load', async () => {
      mockListReviewComments.mockResolvedValue([makeComment({ id: 'c1', line: 5 })])
      await commentStore.load('task-2')

      expect(commentStore.get('task-2')).toHaveLength(1)
      expect(commentStore.get('task-2')[0].line).toBe(5)
    })
  })

  describe('add', () => {
    it('adds comment optimistically then replaces with server response', async () => {
      await commentStore.load('task-3')
      const persisted = makeComment({ id: 'server-id', line: 10, body: 'comment' })
      mockAddReviewComment.mockResolvedValue(persisted)

      const result = await commentStore.add('task-3', 10, 'comment')

      expect(result.id).toBe('server-id')
      const stored = commentStore.get('task-3')
      expect(stored).toHaveLength(1)
      expect(stored[0].id).toBe('server-id')
    })

    it('rolls back optimistic add on API failure', async () => {
      await commentStore.load('task-4')
      mockAddReviewComment.mockRejectedValue(new Error('server error'))

      await expect(commentStore.add('task-4', 10, 'fail')).rejects.toThrow('server error')

      expect(commentStore.get('task-4')).toHaveLength(0)
    })
  })

  describe('resolve', () => {
    it('marks comment resolved optimistically', async () => {
      mockListReviewComments.mockResolvedValue([makeComment({ id: 'c1', resolved: false })])
      await commentStore.load('task-5')
      mockResolveReviewComment.mockResolvedValue(undefined)

      await commentStore.resolve('task-5', 'c1')

      expect(commentStore.get('task-5')[0].resolved).toBe(true)
      expect(mockResolveReviewComment).toHaveBeenCalledWith('task-5', 'c1')
    })

    it('rolls back on API failure', async () => {
      mockListReviewComments.mockResolvedValue([makeComment({ id: 'c1', resolved: false })])
      await commentStore.load('task-6')
      mockResolveReviewComment.mockRejectedValue(new Error('fail'))

      await expect(commentStore.resolve('task-6', 'c1')).rejects.toThrow('fail')

      expect(commentStore.get('task-6')[0].resolved).toBe(false)
    })
  })

  describe('remove', () => {
    it('removes comment optimistically', async () => {
      mockListReviewComments.mockResolvedValue([
        makeComment({ id: 'c1' }),
        makeComment({ id: 'c2' }),
      ])
      await commentStore.load('task-7')
      mockDeleteReviewComment.mockResolvedValue(undefined)

      await commentStore.remove('task-7', 'c1')

      const stored = commentStore.get('task-7')
      expect(stored).toHaveLength(1)
      expect(stored[0].id).toBe('c2')
    })

    it('rolls back on API failure', async () => {
      mockListReviewComments.mockResolvedValue([makeComment({ id: 'c1' })])
      await commentStore.load('task-8')
      mockDeleteReviewComment.mockRejectedValue(new Error('fail'))

      await expect(commentStore.remove('task-8', 'c1')).rejects.toThrow('fail')

      expect(commentStore.get('task-8')).toHaveLength(1)
    })
  })

  describe('byLine', () => {
    it('returns comments for specific line', async () => {
      mockListReviewComments.mockResolvedValue([
        makeComment({ id: 'c1', line: 5 }),
        makeComment({ id: 'c2', line: 10 }),
        makeComment({ id: 'c3', line: 5 }),
      ])
      await commentStore.load('task-9')

      expect(commentStore.byLine('task-9', 5)).toHaveLength(2)
      expect(commentStore.byLine('task-9', 10)).toHaveLength(1)
      expect(commentStore.byLine('task-9', 99)).toHaveLength(0)
    })
  })

  describe('unresolvedCount', () => {
    it('counts unresolved comments', async () => {
      mockListReviewComments.mockResolvedValue([
        makeComment({ id: 'c1', resolved: false }),
        makeComment({ id: 'c2', resolved: true }),
        makeComment({ id: 'c3', resolved: false }),
      ])
      await commentStore.load('task-10')

      expect(commentStore.unresolvedCount('task-10')).toBe(2)
    })

    it('returns 0 for task with no comments', () => {
      expect(commentStore.unresolvedCount('nonexistent')).toBe(0)
    })

    it('returns 0 when all comments resolved', async () => {
      mockListReviewComments.mockResolvedValue([
        makeComment({ id: 'c1', resolved: true }),
      ])
      await commentStore.load('task-11')

      expect(commentStore.unresolvedCount('task-11')).toBe(0)
    })
  })
})
