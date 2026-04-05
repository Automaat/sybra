<script lang="ts">
  import { commentStore } from '../stores/comments.svelte.js'
  import { task } from '../../wailsjs/go/models.js'

  let { taskId, planBody } = $props<{ taskId: string; planBody: string }>()

  let addingLine = $state<number | null>(null)
  let newCommentBody = $state('')
  let submitting = $state(false)

  const lines = $derived(planBody ? planBody.split('\n') : [])

  async function submitComment() {
    if (!newCommentBody.trim() || addingLine === null) return
    submitting = true
    try {
      await commentStore.add(taskId, addingLine, newCommentBody.trim())
      newCommentBody = ''
      addingLine = null
    } finally {
      submitting = false
    }
  }

  function cancelComment() {
    addingLine = null
    newCommentBody = ''
  }

  async function resolveComment(commentId: string) {
    await commentStore.resolve(taskId, commentId)
  }

  async function deleteComment(commentId: string) {
    await commentStore.remove(taskId, commentId)
  }

  function formatTime(ts: any): string {
    if (!ts) return ''
    return new Date(ts).toLocaleString()
  }
</script>

<div class="font-mono text-sm">
  {#each lines as line, i}
    {@const lineNum = i + 1}
    {@const lineComments = commentStore.byLine(taskId, lineNum)}
    <div class="group">
      <div class="flex items-start hover:bg-surface-100 dark:hover:bg-surface-800/50">
        <button
          type="button"
          class="w-8 shrink-0 select-none text-right text-xs text-surface-400 pr-2 pt-0.5 opacity-0 group-hover:opacity-100 hover:text-primary-500"
          onclick={() => { addingLine = lineNum; newCommentBody = '' }}
          title="Add comment"
        >+</button>
        <span class="w-10 shrink-0 select-none text-right text-xs text-surface-400 pr-3 pt-0.5">{lineNum}</span>
        <pre class="flex-1 whitespace-pre-wrap break-all py-0.5">{line}</pre>
      </div>

      {#each lineComments as comment}
        <div class="ml-18 my-1 rounded border {comment.resolved ? 'border-surface-300 opacity-60 dark:border-surface-600' : 'border-warning-400 dark:border-warning-600'} bg-surface-50 dark:bg-surface-800 p-3">
          <div class="flex items-start justify-between gap-2">
            <pre class="flex-1 whitespace-pre-wrap text-sm">{comment.body}</pre>
            <div class="flex shrink-0 items-center gap-1">
              {#if !comment.resolved}
                <button
                  type="button"
                  class="rounded px-2 py-0.5 text-xs text-success-600 hover:bg-success-100 dark:text-success-400 dark:hover:bg-success-900/30"
                  onclick={() => resolveComment(comment.id)}
                >Resolve</button>
              {:else}
                <span class="text-xs text-surface-400 italic">Resolved</span>
              {/if}
              <button
                type="button"
                class="rounded px-2 py-0.5 text-xs text-error-600 hover:bg-error-100 dark:text-error-400 dark:hover:bg-error-900/30"
                onclick={() => deleteComment(comment.id)}
              >Delete</button>
            </div>
          </div>
          <div class="mt-1 text-xs text-surface-400">{formatTime(comment.createdAt)}</div>
        </div>
      {/each}

      {#if addingLine === lineNum}
        <div class="ml-18 my-1 rounded border border-primary-400 dark:border-primary-600 bg-surface-50 dark:bg-surface-800 p-3">
          <textarea
            class="w-full resize-y rounded border border-surface-300 bg-white p-2 text-sm dark:border-surface-600 dark:bg-surface-900"
            rows="3"
            placeholder="Add a comment..."
            bind:value={newCommentBody}
            onkeydown={(e) => { if (e.key === 'Escape') cancelComment() }}
            autofocus
          ></textarea>
          <div class="mt-2 flex gap-2">
            <button
              type="button"
              class="rounded bg-primary-500 px-3 py-1 text-xs font-medium text-white hover:bg-primary-600 disabled:opacity-50"
              onclick={submitComment}
              disabled={submitting || !newCommentBody.trim()}
            >Add Comment</button>
            <button
              type="button"
              class="rounded px-3 py-1 text-xs text-surface-600 hover:bg-surface-200 dark:text-surface-400 dark:hover:bg-surface-700"
              onclick={cancelComment}
            >Cancel</button>
          </div>
        </div>
      {/if}
    </div>
  {/each}
</div>
