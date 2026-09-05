---
name: fix-review-auto
description: Non-interactive PR review fix — fetch unresolved review threads, fix every valid one now (no deferrals, reviewer wins ties), answer questions with the matching code change, and reply on every thread. Use when given a PR URL and asked to address review feedback without prompts.
allowed-tools: Read, Edit, Grep, Glob, Bash
user-invocable: true
---

# Fix PR Review Comments (Non-Interactive)

**Role**: Senior software engineer autonomously addressing PR review feedback.

**Task**: Process unresolved review comments from the given PR URL. Research each one, then fix it in this PR. Reply to every processed thread. Never silently drop a comment, and never defer one to a follow-up.

**IMPORTANT**: Work directly — no plan mode, no clarifying questions. Apply fixes immediately after research.

**The bar**: a reviewer's comment is work to do now, not a ticket to file. Skip
the code change only for a comment you can prove wrong. When in doubt, the
reviewer's version wins.

## Invocation

```bash
/fix-review-auto <PR-URL>
$fix-review-auto <PR-URL>
```

## Phase 1: Fetch & Analyze

### 1. Extract PR Info

```bash
OWNER=$(echo "$PR_URL" | sed 's|.*github.com/\([^/]*\)/.*|\1|')
REPO=$(echo "$PR_URL"  | sed 's|.*github.com/[^/]*/\([^/]*\)/.*|\1|')
PR=$(echo "$PR_URL"    | sed 's|.*/pull/\([0-9]*\).*|\1|')
```

### 2. Fetch Unresolved Review Threads

```bash
gh api graphql -f owner="$OWNER" -f repo="$REPO" -F pr="$PR" -f query='
query($owner: String!, $repo: String!, $pr: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $pr) {
      reviewThreads(first: 100) {
        nodes {
          id
          isResolved
          isOutdated
          path
          line
          comments(first: 100) {
            nodes {
              databaseId
              author { login }
              body
              createdAt
            }
          }
        }
      }
    }
  }
}'
```

Filter: `isResolved: false` AND `isOutdated: false`.

Capture per surviving thread:
- `threadId` — node id for `addPullRequestReviewThreadReply`
- first comment's `databaseId` — REST fallback id for `POST /repos/{O}/{R}/pulls/{N}/comments/{cid}/replies`

### 3. Research Each Comment

For each unresolved comment:

#### Context Gathering
- Read affected file at `path:line`.
- Grep the codebase for similar patterns.
- Check language/framework conventions used elsewhere in the repo.

#### Categorize

Every comment ends in a code change unless it is demonstrably wrong. There are
three outcomes, and only the last one leaves the code untouched.

**Fix** (the default):
- The reviewer names a change and the change is sound → apply it.

**Answer and fix**:
- The comment is a question ("why not X?", "does this handle Y?", "is this
  needed?"). Answer it in the reply **and** apply the change the honest answer
  implies — a question about a real gap is a fix request with a polite face.
- Only when the answer proves the code is already right does the reply stand
  alone, and then it must carry the evidence (file:line, test name, doc link).

**Invalid** (leave the code alone — only when you can prove it):
- The claim is demonstrably false, or the code it names is not in this PR, or
  the fix it asks for is already there — and you can cite where.
- "I disagree", "I prefer the current shape", and "the reviewer may have
  misread it" are not invalid. Those are the reviewer-wins case below.

### The reviewer wins ties

When you are not sure who is right, when the request is ambiguous, or when the
trade-off has no clear winner — **implement the reviewer's version**. They
gate the merge, matching their preference costs one small change, and arguing
costs a review round-trip. Say what the trade-off is in the reply if it
matters; make the change either way.

### Never defer

Do not reply that a fix is deferred, out of scope for this PR, left as-is for
now, better as a separate PR, or filed as a follow-up. Scope, breadth, or
having to touch a shared helper, a constructor signature, or another test's
setup is **not** a reason to skip — do that work here, in this PR.

Two escape hatches, and neither of them is a reply:

- The change needs a decision only a human can make (a product call, a missing
  credential, code you cannot reach from this worktree) → stop, leave the
  thread unanswered, and report `human-required` with the blocker named. A
  parked task keeps the feedback live; a deferral reply buries it.
- The change would break something you can demonstrate breaks → that is the
  invalid case; reply with the evidence, not with a promise.

The harness reads the replies you post. A thread whose latest reply promises a
follow-up, a separate PR, or "as-is for now" is counted as unanswered, and the
task parks as `human-required` — the same outcome as ignoring the thread.

## Phase 2: Apply Fixes

### Process Order
1. Critical (bugs, security, correctness)
2. Major (refactoring, performance)
3. Minor (style, naming)

### For Fixes
- Use Edit to apply the change.
- Record `threadId` + one-line note describing the fix.

### For Questions
- Work out the real answer from the code, then apply whatever change that
  answer calls for — same as any other fix.
- Record `threadId` + the answer + the one-line note describing the change.

### For Invalid Comments
- Leave the code alone.
- Record `threadId` + evidence (e.g. "already at file.go:42", "outside PR diff", "the assertion at foo_test.go:88 covers this").

### When the fix is bigger than the comment

Thread a new parameter through a constructor, widen a shared test helper,
rename across call sites — do it, then run the affected tests. Verify the
change the same way you would for the task's own implementation. A large but
mechanical change is still this PR's work.

## Phase 3: Commit and Push

```bash
git add .
git commit -s -S -m "fix(<scope>): address PR review comments"
git push
```

Push **before** Phase 4 — replies reference the new short SHA so reviewers can navigate to the applied changes.

If you are on a fork, push to the fork remote (see project conventions in CLAUDE.md).

## Phase 4: Reply to Every Unresolved Thread

For **every** thread processed in Phase 1 — fixed, answered, or invalid — post one reply. Silent skips are the failure mode this skill exists to eliminate.

### Idempotency guard

Before posting each individual reply:

1. Re-fetch that thread's comments.
2. Skip the thread if the authenticated user already posted a comment on that thread whose body contains `_Generated by Sybra harness_`.
3. Verify the reply body contains the final pushed short SHA when it is an Applied reply.
4. Refuse to post any body containing placeholders such as `__SHA__`, `<short-sha>`, `<sha>`, or `TODO`.
5. If any API call succeeds for a thread, record it as posted and do not retry that same thread through GraphQL, REST, or a rewritten command.

This guard runs per thread, immediately before the API call. A retry after a quoting error or fallback switch must re-fetch first, because earlier commands can partially succeed.

### Reply templates

Every reply ends with a blank line then `_Generated by Sybra harness_`.

```
**Applied** — <one-line description of the change> (<short-sha>).

_Generated by Sybra harness_
```

```
**Answered** — <the answer, one or two lines>. Applied <one-line description of the change> (<short-sha>).

_Generated by Sybra harness_
```

```
**Answered** — <the answer>. No change needed: <evidence, e.g. path/to/file.go:42 already does this>.

_Generated by Sybra harness_
```

```
**Skipped (invalid)** — <one-line evidence>. Happy to revisit if I'm reading this wrong.

_Generated by Sybra harness_
```

### Posting

GraphQL (preferred):

```bash
gh api graphql -f threadId="$THREAD_ID" -f body="$REPLY_BODY" -f query='
mutation($threadId: ID!, $body: String!) {
  addPullRequestReviewThreadReply(input: { pullRequestReviewThreadId: $threadId, body: $body }) {
    comment { id url }
  }
}'
```

Pass the query inline, as above. The gh wrapper on your PATH refuses a GraphQL call whose query it cannot read, so feeding the query from a file or a heredoc never reaches GitHub.

REST fallback (if the mutation is unavailable on the host):

```bash
gh api -X POST "/repos/$OWNER/$REPO/pulls/$PR/comments/$FIRST_COMMENT_DBID/replies" \
  -f body="$REPLY_BODY"
```

### Reply rules

- One reply per thread. Never spam.
- Match the reviewer's terseness. No apologies, no filler, no chatty AI persona.
- Reference the fix commit's short SHA on applied replies.
- Keep the body inline. If it has to come from a file, read it with `-F body=@file` — `-f`/`--raw-field` sends the value verbatim, so `-f body=@file` posts the path itself and GitHub accepts it with a 201.
- Do not use placeholders in posted replies. Compute the short SHA before building bodies, then inspect each body before calling `gh api`.
- **End every reply with a blank line then the harness attribution footer**, exactly: `_Generated by Sybra harness_`. This identifies the reply as machine-generated and is required on every thread reply and any PR comment you post.
- **Never mark threads as resolved** — the reviewer decides.

## Phase 5: Summary

```text
Summary: Fixed N/M unresolved review comments

Applied (replied + pushed in <sha>):
✓ <path:line by @author>: <one-liner>
✓ ...

Answered (replied, no change needed):
? <path:line by @author>: <answer> — <evidence>

Skipped (replied with reasoning):
✗ <path:line by @author>: <one-liner> — invalid, <evidence>

Threads replied: <count> / <total processed>
```

## Key Rules

**DO:**
- Research each comment before acting
- Search codebase for patterns
- Fix every comment you cannot prove wrong, in this PR
- Take the reviewer's version whenever the call is close
- Answer a question and apply the change that answer calls for
- Commit with `-s -S` flags
- Push before replying
- **Reply to every processed thread** — fixed, answered, invalid
- Re-fetch each thread before replying and skip ones that already have your harness reply
- Reference fix SHA in applied replies
- Report `human-required` instead of replying, when a change truly cannot be made here

**DON'T:**
- Enter plan mode
- Ask for user input
- Defer a fix: no "follow-up", "separate PR", "out of scope", "leaving as-is for now", "happy to pick this up separately"
- Skip a fix because it is large, touches shared helpers, or changes a signature
- Argue a trade-off instead of applying the reviewer's version
- Answer a question and stop there when the answer calls for a change
- Use linter skip/disable directives
- Mark review threads as resolved
- Retry a successful thread through another API path
- Post placeholders like `__SHA__` or `<short-sha>`
- Silently drop a comment — if you read it, you reply to it
