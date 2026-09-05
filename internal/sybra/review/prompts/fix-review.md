## 1. List the threads

Every shell call starts a fresh shell, so re-set these three at the top of any
block that reads them — this one, and the reply fallback in step 6.

```sh
{{TARGET}}
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
          comments(first: 100) { nodes { databaseId author { login } body } }
        }
      }
    }
  }
}'
```

Process every thread with `isResolved: false` and `isOutdated: false`. Keep each one's `id` (for the reply mutation) and its first comment's `databaseId` (for the REST fallback).

## 2. Research each thread

Read the file at `path:line`. Grep for the same pattern elsewhere in the repo. Check what the surrounding code already does before you judge the comment.

## 3. Decide

{{POLICY}}

## 4. Apply the fixes

Order the work: correctness and security first, then design and performance, then naming and style. Run the tests that cover the code you touched.

Never weaken, skip, delete, or hardcode a test to satisfy a comment — fix the underlying code. Tampering is detected and blocks the task.

## 5. Commit and push

Use conventional commit format `fix(review): address PR review comments` (type(scope) is required by repo hooks). Sign the commit with `git commit {{COMMIT_FLAGS}}`.

{{PUSH}}

Push before you reply: an applied reply names the pushed short SHA, so the reviewer can navigate to the change.

## 6. Reply on every thread you processed

One reply per thread — fixed, answered, or invalid. A thread you read and never answered is the failure this procedure exists to prevent.

Before each reply, re-fetch that one thread and:

1. Skip it when your own account already replied with the harness footer on the current PR head.
2. Check the body carries the pushed short SHA when it claims a change.
3. Refuse any body still holding a placeholder such as `__SHA__`, `<short-sha>`, `<sha>`, or `TODO`.
4. Treat a successful call as final — never retry the same thread through the other API.

Templates. Every reply ends with a blank line then `{{FOOTER}}`:

```
**Applied** — <one-line description of the change> (<short-sha>).

{{FOOTER}}
```

```
**Answered** — <the answer, one or two lines>. Applied <one-line description of the change> (<short-sha>).

{{FOOTER}}
```

```
**Answered** — <the answer>. No change needed: <evidence, e.g. internal/x/y.go:42 already does this>.

{{FOOTER}}
```

```
**Skipped (invalid)** — <one-line evidence>. Happy to revisit if I am reading this wrong.

{{FOOTER}}
```

Post with the mutation, passing the query inline. The `gh` wrapper on your PATH refuses a GraphQL call whose query it cannot read, so a query fed from a file or a heredoc never reaches GitHub.

```sh
gh api graphql -f threadId="$THREAD_ID" -f body="$REPLY_BODY" -f query='
mutation($threadId: ID!, $body: String!) {
  addPullRequestReviewThreadReply(input: { pullRequestReviewThreadId: $threadId, body: $body }) {
    comment { id url }
  }
}'
```

REST fallback when the mutation is unavailable:

```sh
{{TARGET}}
gh api -X POST "/repos/$OWNER/$REPO/pulls/$PR/comments/$FIRST_COMMENT_DBID/replies" -f body="$REPLY_BODY"
```

Reply rules:

- Match the reviewer's terseness. No apologies, no filler, no chatty persona.
- Keep the body inline. If it must come from a file, read it with `-F body=@file` — `-f`/`--raw-field` sends the value verbatim, so `-f body=@file` posts the path itself and GitHub accepts it with a 201.
- Never mark a thread resolved. The reviewer decides that.

## 7. Report

End with a per-thread summary: which threads you fixed and in which commit, which you answered without a change and on what evidence, which you rejected and why, and the count of threads replied to against the count you processed.
