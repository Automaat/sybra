---
name: fix-review-auto
description: Non-interactive PR review fix — fetch unresolved review threads, auto-apply valid fixes, reply on every thread (applied/questionable/invalid). Use when given a PR URL and asked to address review feedback without prompts.
allowed-tools: Read, Edit, Grep, Glob, Bash
user-invocable: true
---

# Fix PR Review Comments (Non-Interactive)

**Role**: Senior software engineer autonomously addressing PR review feedback.

**Task**: Process unresolved review comments from the given PR URL. Research validity. Auto-apply valid fixes. Reply to every processed thread — applied, questionable, or invalid. Never silently drop a comment.

**IMPORTANT**: Work directly — no plan mode, no clarifying questions. Apply fixes immediately after research.

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
gh api graphql -f owner="$OWNER" -f repo="$REPO" -F pr="$PR" -F query=@- << 'EOF'
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
}
EOF
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

**Valid** (ALL must be true):
- References specific code in PR diff
- Technically correct
- Doesn't break existing functionality
- Aligns with codebase patterns
- Sound reasoning from reviewer

**Invalid** (ANY true):
- References code not in this PR
- Already implemented
- Conflicts with existing patterns
- Technically incorrect
- Misunderstands the code's purpose

**Questionable** (ANY doubt):
- Ambiguous request
- Multiple valid interpretations
- Trade-offs not clear from the diff alone

## Phase 2: Apply Fixes

### Process Order
1. Critical (bugs, security, correctness)
2. Major (refactoring, performance)
3. Minor (style, naming)

### For Valid Fixes
- Use Edit to apply the change.
- Record `threadId` + one-line note describing the fix.

### For Questionable Comments
- **SKIP the fix** — do NOT attempt in non-interactive mode.
- Record `threadId` + the tradeoff and the open question for the reviewer.

### For Invalid Comments
- **SKIP the fix**.
- Record `threadId` + evidence (e.g. "already at file.go:42", "outside PR diff", "conflicts with pattern in pkg/x").

## Phase 3: Commit and Push

```bash
git add .
git commit -s -S -m "fix(<scope>): address PR review comments"
git push
```

Push **before** Phase 4 — replies reference the new short SHA so reviewers can navigate to the applied changes.

If you are on a fork, push to the fork remote (see project conventions in CLAUDE.md).

## Phase 4: Reply to Every Unresolved Thread

For **every** thread processed in Phase 1 — applied, questionable, or invalid — post one reply. Silent skips are the failure mode this skill exists to eliminate.

### Reply templates

```
**Applied** — <one-line description of the change> (<short-sha>).
```

```
**Skipped (questionable)** — <one-line reasoning>. <Concrete question for reviewer, or tradeoff statement>. Leaving the thread open for your call.
```

```
**Skipped (invalid)** — <one-line evidence>. Happy to revisit if I'm reading this wrong.
```

### Posting

GraphQL (preferred):

```bash
gh api graphql -f threadId="$THREAD_ID" -f body="$REPLY_BODY" -F query=@- << 'EOF'
mutation($threadId: ID!, $body: String!) {
  addPullRequestReviewThreadReply(input: { pullRequestReviewThreadId: $threadId, body: $body }) {
    comment { id url }
  }
}
EOF
```

REST fallback (if the mutation is unavailable on the host):

```bash
gh api -X POST "/repos/$OWNER/$REPO/pulls/$PR/comments/$FIRST_COMMENT_DBID/replies" \
  -f body="$REPLY_BODY"
```

### Reply rules

- One reply per thread. Never spam.
- Match the reviewer's terseness. No apologies, no filler, no AI attribution.
- Reference the fix commit's short SHA on applied replies.
- **Never mark threads as resolved** — the reviewer decides.

## Phase 5: Summary

```text
Summary: Fixed N/M unresolved review comments

Applied (replied + pushed in <sha>):
✓ <path:line by @author>: <one-liner>
✓ ...

Skipped (replied with reasoning):
? <path:line by @author>: <one-liner> — questionable, awaiting reviewer
✗ <path:line by @author>: <one-liner> — invalid, <evidence>

Threads replied: <count> / <total processed>
```

## Key Rules

**DO:**
- Research each comment before acting
- Search codebase for patterns
- Apply only clearly valid fixes
- Commit with `-s -S` flags
- Push before replying
- **Reply to every processed thread** — applied, questionable, invalid
- Reference fix SHA in applied replies

**DON'T:**
- Enter plan mode
- Ask for user input
- Apply questionable fixes
- Use linter skip/disable directives
- Mark review threads as resolved
- Silently drop a comment — if you read it, you reply to it
