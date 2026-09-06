# Go CI execution

`Go Tests` still runs the full untagged tree with the race detector. Its Go
module and compilation caches are restored after mise selects the pinned
toolchain. Cache keys separate the OS, architecture, Go version, dependency
inputs, and suite; a cold cache changes speed, never which tests run.

`Go E2E Tests` is the protected aggregate of four application shards and one
child-package job. Every dependency must succeed; missing, skipped, canceled,
and failed results cannot satisfy the gate. The other required checks and
branch protection are unchanged.

The application shards use `go test -race -tags e2e -list .` to discover the
current package's runnable top-level tests, examples, and fuzz seed tests.
Names are hashed into four disjoint buckets. Subtests stay with their parent,
and each runner has its own process, fixtures, and sandbox. No timing file or
test allowlist needs updating when a test is added. The child-package job
discovers every package under `internal/sybra`, excludes only that already
sharded root, and executes the rest with the same race/E2E flags.

Reproduce an application shard from the repository root:

```sh
mise exec -- go run ./scripts/gotestshard -index 0 -total 4
```

Indices are 0–3. Discovery failures, duplicate names, and empty selections
fail closed. Execution uses `-count=1`, preserving fresh behavioral evidence
while reusing compiled packages. Each job prints its exact test selection.

This trades additional isolated runners and repeated cold compilation for a
shorter critical path. Measure cold and warm CI runs before changing shard
count; one very slow top-level test still sets a lower bound. Keep the matrix,
command's `-total`, and partition contract test together when changing it.
