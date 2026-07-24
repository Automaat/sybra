import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/svelte'

const mockListAttachments = vi.fn()
const mockUploadAttachment = vi.fn()
const mockDeleteAttachment = vi.fn()
const mockGetAttachmentURL = vi.fn()

vi.mock('$lib/api', () => ({
  ListAttachments: (...args: unknown[]) => mockListAttachments(...args),
  UploadAttachment: (...args: unknown[]) => mockUploadAttachment(...args),
  DeleteAttachment: (...args: unknown[]) => mockDeleteAttachment(...args),
  GetAttachmentURL: (...args: unknown[]) => mockGetAttachmentURL(...args),
}))

const TaskAttachmentsPanel = (await import('./TaskAttachmentsPanel.svelte')).default

const baseTask = {
  id: 'task-1',
  title: 'Attachment task',
  attachments: [],
}

describe('TaskAttachmentsPanel', () => {
  beforeEach(() => {
    mockListAttachments.mockReset()
    mockUploadAttachment.mockReset()
    mockDeleteAttachment.mockReset()
    mockGetAttachmentURL.mockReset()
  })

  afterEach(() => {
    cleanup()
  })

  it('uploads a file and refreshes the list', async () => {
    mockListAttachments
      .mockResolvedValueOnce([])
      .mockResolvedValueOnce([
        {
          id: 'att-1',
          fileName: 'notes.txt',
          contentType: 'text/plain',
          sizeBytes: 5,
          path: '/tmp/task-1/att-1/notes.txt',
          createdAt: '2026-07-19T00:00:00Z',
        },
      ])
    mockUploadAttachment.mockResolvedValue({
      id: 'att-1',
      fileName: 'notes.txt',
      contentType: 'text/plain',
      sizeBytes: 5,
      path: '/tmp/task-1/att-1/notes.txt',
      createdAt: '2026-07-19T00:00:00Z',
    })

    render(TaskAttachmentsPanel, { props: { task: baseTask } })

    const input = screen.getByTestId('attachment-input') as HTMLInputElement
    const file = new File(['hello'], 'notes.txt', { type: 'text/plain' })
    await fireEvent.change(input, { target: { files: [file] } })

    await vi.waitFor(() => {
      expect(mockUploadAttachment).toHaveBeenCalledWith('task-1', 'notes.txt', [104, 101, 108, 108, 111])
    })
    await vi.waitFor(() => {
      expect(screen.getByText('notes.txt')).toBeDefined()
    })
  })

  it('renders inline previews for images', async () => {
    mockListAttachments.mockResolvedValue([
      {
        id: 'att-1',
        fileName: 'diagram.png',
        contentType: 'image/png',
        sizeBytes: 8,
        path: '/tmp/task-1/att-1/diagram.png',
        createdAt: '2026-07-19T00:00:00Z',
      },
    ])
    mockGetAttachmentURL.mockResolvedValue('data:image/png;base64,ZmFrZQ==')

    render(TaskAttachmentsPanel, { props: { task: baseTask } })

    await vi.waitFor(() => {
      expect(screen.getByAltText('Preview of diagram.png')).toBeDefined()
    })
  })

  it('deletes an attachment and refreshes the list', async () => {
    mockListAttachments
      .mockResolvedValueOnce([
        {
          id: 'att-1',
          fileName: 'cleanup.txt',
          contentType: 'text/plain',
          sizeBytes: 7,
          path: '/tmp/task-1/att-1/cleanup.txt',
          createdAt: '2026-07-19T00:00:00Z',
        },
      ])
      .mockResolvedValueOnce([])
    mockDeleteAttachment.mockResolvedValue(undefined)

    render(TaskAttachmentsPanel, { props: { task: baseTask } })

    await vi.waitFor(() => {
      expect(screen.getByText('cleanup.txt')).toBeDefined()
    })
    await fireEvent.click(screen.getByText('Delete'))

    await vi.waitFor(() => {
      expect(mockDeleteAttachment).toHaveBeenCalledWith('task-1', 'att-1')
    })
    await vi.waitFor(() => {
      expect(screen.getByTestId('attachment-empty')).toBeDefined()
    })
  })
})
