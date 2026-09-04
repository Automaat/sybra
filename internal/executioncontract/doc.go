// Package executioncontract defines the versioned, transport-neutral messages
// exchanged between a Sybra leader and an execution worker.
//
// The contract intentionally contains no agent.RunConfig, callbacks, provider
// credentials, node credentials, or leader filesystem paths. Paths are always
// relative to one of the declared LogicalRoot values. Credentials are acquired
// by a worker from a scoped SecretRef; their values never cross this boundary.
//
// Confidentiality rules:
//   - Prompt.Text, command/event payloads, terminal error text, and artifact
//     names may contain task or provider content and must be encrypted in
//     transit, access-controlled at rest, and excluded from routine logs.
//   - EnvironmentBinding.Value is for explicitly public configuration only.
//     Sensitive values use SecretRef. Provider and node master credentials are
//     forbidden even as secret references; workers obtain those locally.
//   - Artifact entries carry a sensitivity classification. A secret artifact
//     must not be copied into public task, issue, PR, or audit text.
package executioncontract
