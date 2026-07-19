<script lang="ts">
  import { Download, ExternalLink, LoaderCircle, Paperclip, Trash2, Upload } from '@lucide/svelte'
  import type { Task } from '../../../bindings/github.com/Automaat/sybra/internal/task/models.js'
  import { DeleteAttachment, GetAttachmentURL, ListAttachments, UploadAttachment } from '$lib/api'

  type TaskPanelTask = Pick<Task, 'id' | 'attachments'>

  interface Props {
    task: TaskPanelTask
  }

  const { task }: Props = $props()

  let attachments = $state<NonNullable<TaskPanelTask['attachments']>>([])
  let previewURLs = $state<Record<string, string>>({})
  let urlCache = $state<Record<string, string>>({})
  let loading = $state(false)
  let error = $state('')
  let dragOver = $state(false)
  let fileInput = $state<HTMLInputElement | null>(null)

  function isImage(contentType: string): boolean {
    return contentType.startsWith('image/')
  }

  function formatSize(size: number): string {
    if (size < 1024) return `${size} B`
    if (size < 1024*1024) return `${(size / 1024).toFixed(1)} KB`
    return `${(size / (1024 * 1024)).toFixed(1)} MB`
  }

  async function refreshAttachments() {
    loading = true
    try {
      attachments = (await ListAttachments(task.id)) ?? []
      error = ''
      await refreshPreviews()
    } catch (e) {
      error = String(e)
    } finally {
      loading = false
    }
  }

  async function refreshPreviews() {
    const nextPreviews: Record<string, string> = {}
    for (const attachment of attachments) {
      if (!isImage(attachment.contentType)) continue
      try {
        const url = urlCache[attachment.id] ?? await GetAttachmentURL(task.id, attachment.id)
        urlCache = { ...urlCache, [attachment.id]: url }
        nextPreviews[attachment.id] = url
      } catch {
        // Keep the list usable even when preview loading fails.
      }
    }
    previewURLs = nextPreviews
  }

  async function ensureURL(attachmentID: string): Promise<string> {
    const cached = urlCache[attachmentID]
    if (cached) return cached
    const url = await GetAttachmentURL(task.id, attachmentID)
    urlCache = { ...urlCache, [attachmentID]: url }
    return url
  }

  function clickURL(url: string, fileName: string, download: boolean) {
    const anchor = document.createElement('a')
    anchor.href = url
    if (download) anchor.download = fileName
    else anchor.target = '_blank'
    anchor.rel = 'noreferrer'
    document.body.appendChild(anchor)
    anchor.click()
    anchor.remove()
  }

  async function openAttachment(attachmentID: string, fileName: string, download: boolean) {
    try {
      const url = await ensureURL(attachmentID)
      clickURL(url, fileName, download)
      error = ''
    } catch (e) {
      error = String(e)
    }
  }

  async function uploadFiles(files: FileList | File[] | null | undefined) {
    if (!files || files.length === 0) return
    loading = true
    try {
      for (const file of Array.from(files)) {
        const data = Array.from(new Uint8Array(await file.arrayBuffer()))
        await UploadAttachment(task.id, file.name, data)
      }
      error = ''
      if (fileInput) fileInput.value = ''
      await refreshAttachments()
    } catch (e) {
      error = String(e)
      loading = false
    }
  }

  async function removeAttachment(attachmentID: string) {
    loading = true
    try {
      await DeleteAttachment(task.id, attachmentID)
      const nextCache = { ...urlCache }
      const nextPreviews = { ...previewURLs }
      delete nextCache[attachmentID]
      delete nextPreviews[attachmentID]
      urlCache = nextCache
      previewURLs = nextPreviews
      error = ''
      await refreshAttachments()
    } catch (e) {
      error = String(e)
      loading = false
    }
  }

  function handleDrop(event: DragEvent) {
    event.preventDefault()
    dragOver = false
    void uploadFiles(event.dataTransfer?.files)
  }

  function handleDropzoneKey(event: KeyboardEvent) {
    if (event.key !== 'Enter' && event.key !== ' ') return
    event.preventDefault()
    fileInput?.click()
  }

  $effect(() => {
    task.id
    attachments = [...(task.attachments ?? [])]
    void refreshAttachments()
  })
</script>

<section class="flex flex-col gap-4 rounded-xl border border-surface-300 bg-surface-50 p-4 dark:border-surface-700 dark:bg-surface-900" data-testid="task-attachments-panel">
  <div class="flex items-start justify-between gap-3">
    <div class="flex items-center gap-2">
      <Paperclip size={16} class="shrink-0 text-surface-500" />
      <div>
        <h3 class="text-sm font-semibold">Attachments</h3>
        <p class="text-xs text-surface-500">Local task files available to the UI and implementation agents by path.</p>
      </div>
    </div>

    <div class="flex items-center gap-2">
      {#if loading}
        <span class="inline-flex items-center gap-1 text-xs text-surface-500">
          <LoaderCircle size={14} class="animate-spin" />
          Syncing
        </span>
      {/if}
      <input
        bind:this={fileInput}
        class="hidden"
        data-testid="attachment-input"
        multiple
        type="file"
        onchange={(event) => void uploadFiles((event.currentTarget as HTMLInputElement).files)}
      />
      <button
        type="button"
        class="inline-flex items-center gap-1 rounded-lg border border-surface-300 px-3 py-1.5 text-sm transition-colors hover:bg-surface-100 dark:border-surface-600 dark:hover:bg-surface-800"
        onclick={() => fileInput?.click()}
      >
        <Upload size={14} />
        Add files
      </button>
    </div>
  </div>

  <div
    class={`rounded-xl border border-dashed p-4 text-sm transition-colors ${dragOver ? 'border-primary-500 bg-primary-500/8' : 'border-surface-300 bg-surface-100/70 dark:border-surface-700 dark:bg-surface-800/70'}`}
    data-testid="attachment-dropzone"
    role="button"
    tabindex="0"
    ondragenter={(event) => {
      event.preventDefault()
      dragOver = true
    }}
    ondragover={(event) => {
      event.preventDefault()
      dragOver = true
    }}
    ondragleave={() => { dragOver = false }}
    ondrop={handleDrop}
    onkeydown={handleDropzoneKey}
  >
    Drag files here or use <span class="font-medium">Add files</span>.
  </div>

  {#if error}
    <p class="text-sm text-error-500" data-testid="attachment-error">{error}</p>
  {/if}

  {#if attachments.length === 0}
    <p class="text-sm text-surface-500" data-testid="attachment-empty">No attachments yet.</p>
  {:else}
    <div class="flex flex-col gap-3">
      {#each attachments as attachment (attachment.id)}
        <article class="rounded-lg border border-surface-200 bg-white p-3 dark:border-surface-700 dark:bg-surface-950/60" data-testid="attachment-row">
          <div class="flex flex-col gap-3 md:flex-row md:items-start md:justify-between">
            <div class="min-w-0">
              <p class="truncate text-sm font-medium">{attachment.fileName}</p>
              <p class="text-xs text-surface-500">{attachment.contentType} · {formatSize(attachment.sizeBytes)}</p>
            </div>
            <div class="flex flex-wrap items-center gap-2">
              <button
                type="button"
                class="inline-flex items-center gap-1 rounded px-2 py-1 text-xs text-surface-600 transition-colors hover:bg-surface-100 dark:text-surface-300 dark:hover:bg-surface-800"
                onclick={() => void openAttachment(attachment.id, attachment.fileName, false)}
              >
                <ExternalLink size={12} />
                Open
              </button>
              <button
                type="button"
                class="inline-flex items-center gap-1 rounded px-2 py-1 text-xs text-surface-600 transition-colors hover:bg-surface-100 dark:text-surface-300 dark:hover:bg-surface-800"
                onclick={() => void openAttachment(attachment.id, attachment.fileName, true)}
              >
                <Download size={12} />
                Download
              </button>
              <button
                type="button"
                class="inline-flex items-center gap-1 rounded px-2 py-1 text-xs text-error-600 transition-colors hover:bg-error-500/10 dark:text-error-400"
                onclick={() => void removeAttachment(attachment.id)}
              >
                <Trash2 size={12} />
                Delete
              </button>
            </div>
          </div>

          {#if previewURLs[attachment.id]}
            <img
              alt={`Preview of ${attachment.fileName}`}
              class="mt-3 max-h-72 rounded-lg border border-surface-200 object-contain dark:border-surface-700"
              src={previewURLs[attachment.id]}
            />
          {/if}
        </article>
      {/each}
    </div>
  {/if}
</section>
