// Package workercontrol is the durable leader-side transport for remote
// executions.
//
// A worker establishes one leased session per stable worker ID. Registration
// negotiates the execution-contract version; an unsupported major is rejected.
// Heartbeats renew the lease. Expired sessions cannot poll or mutate delivery
// state. Registering a replacement fences the previous session atomically.
// Supplying its session ID and last acknowledged command cursor transfers only
// unacknowledged commands and active runs to the replacement.
//
// Commands are persisted before long-poll delivery and addressed by a monotonic
// per-session sequence. Their idempotency keys and fenced run effects make
// start, stop, steer, and approval delivery safe to repeat. Event batches are
// persisted in per-run sequence order; exact repeats are accepted, gaps and
// changed repeats are rejected. A leader restart therefore resumes from the
// database cursors rather than an in-memory stream. Events are retained after
// acknowledgement so a slow consumer never causes silent output loss.
//
// Draining sessions may finish polling, emitting, acknowledging, and uploading
// artifacts for accepted runs, but cannot accept new commands. Diagnostics
// intentionally expose only identities, protocol/build versions, leases, and
// counters—never command payloads, prompts, event bodies, artifact bytes, or
// credentials.
package workercontrol
