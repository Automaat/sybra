#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

fixture_dir="$(mktemp -d .git-exec-gate-test.XXXXXX)"
cleanup() {
  rm -rf -- "${fixture_dir}"
}
trap cleanup EXIT

assert_rejected() {
  local name="$1"
  local source="$2"
  local file="${fixture_dir}/${name}.go"
  local output

  printf '%s\n' "${source}" > "${file}"
  if output="$(bash scripts/check-git-exec-gate.sh 2>&1)"; then
    echo "ERROR: gate accepted ${name}" >&2
    exit 1
  fi
  if ! grep -Fq "${file}" <<<"${output}"; then
    echo "ERROR: gate rejected ${name} without identifying ${file}" >&2
    echo "${output}" >&2
    exit 1
  fi
  rm -f -- "${file}"
}

assert_rejected command 'package fixture; import "os/exec"; func f() { _ = exec.Command("git", "status") }'
assert_rejected command_context 'package fixture; import ("context"; "os/exec"); func f(ctx context.Context) { _ = exec.CommandContext(ctx, "git", "status") }'
assert_rejected command_background 'package fixture; import ("context"; "os/exec"); func f() { _ = exec.CommandContext(context.Background(), "git", "status") }'
assert_rejected multiline_command $'package fixture\nimport "os/exec"\nfunc f() {\n\t_ = exec.Command(\n\t\t"git",\n\t\t"status",\n\t)\n}'
assert_rejected multiline_command_context $'package fixture\nimport ("context"; "os/exec")\nfunc f(ctx context.Context) {\n\t_ = exec.CommandContext(\n\t\tctx,\n\t\t"git",\n\t\t"status",\n\t)\n}'
assert_rejected aliased_import 'package fixture; import command "os/exec"; func f() { _ = command.Command("git", "status") }'

# Direct Git in test fixtures is deliberately allowed.
printf '%s\n' 'package fixture; import "os/exec"; func f() { _ = exec.Command("git", "status") }' > "${fixture_dir}/fixture_test.go"
bash scripts/check-git-exec-gate.sh >/dev/null

echo "check-git-exec-gate.test: all tests passed"
