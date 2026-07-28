# GitHub comment-command webhooks

Sybra can create tasks from comments delivered by its GitHub App:

- `<prefix> ship` on an issue creates an implementation task.
- `<prefix> review` on a pull request creates an inbound PR-review task.

The prefix defaults to `/sybra` and is configurable. Commands must be the
entire comment apart from surrounding or repeated whitespace.

## Configuration

```yaml
integrations:
  github:
    enabled: true
    webhook:
      enabled: true
      port: 8081
      secret: replace-with-the-github-app-webhook-secret
      command_prefix: /sybra
    app:
      enabled: true
      app_id: 123456
      installation_id: 7891011
      private_key_path: /data/sybra/github-app.pem
```

`SYBRA_GITHUB_WEBHOOK_SECRET` overrides the YAML secret. The command prefix is
configured only in YAML. The optional `task_secret` setting enables and signs
the generic `POST /webhook/task` sibling route using `X-Sybra-Signature`;
`SYBRA_WEBHOOK_SECRET` can override that value. Set `task_enabled: true` only
when that route must remain available without a signature. Legacy top-level
`webhook.enabled: true` configurations migrate with `task_enabled` set so their
existing behavior is preserved.

In the GitHub App registration:

1. Set the webhook URL to the externally reachable
   `https://<host>/webhook/github` route.
2. Set the webhook secret to the same value as
   `integrations.github.webhook.secret`.
3. Grant at least read access to repository metadata, issues, and pull
   requests.
4. Subscribe to the **Issue comments** event.

The deployment must route `/webhook/github` to the webhook listener port. The
main control-plane bearer token does not apply to this separate listener;
GitHub's HMAC signature authenticates the request.

## Processing rules

Sybra verifies `X-Hub-Signature-256` against the unmodified request body before
parsing it. Missing or invalid signatures return `401`.

Only `issue_comment.created` deliveries authored by an `OWNER`, `MEMBER`, or
`COLLABORATOR` are eligible. Bot comments, outside contributors, other events,
unknown commands, and commands used on the wrong GitHub object return `200`
without creating a task.

The App installation ID is checked when `github.app.installation_id` is set.
The GitHub comment ID is stored as a task tag, so manual webhook redelivery
returns the existing task instead of creating a duplicate.
