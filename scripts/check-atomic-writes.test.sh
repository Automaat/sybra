#!/usr/bin/env bash
# A drift gate that quietly stops matching is worse than no gate: it reports
# green while the thing it guards rots. These cases are the spellings a
# line-oriented regex missed, which is why the gate is an AST check. The
# assert_accepted cases pin the other edge — an ordinary move must stay legal.
set -euo pipefail

cd "$(dirname "$0")/.."

fixture_dir="$(mktemp -d .atomic-writes-gate-test.XXXXXX)"
cleanup() {
  rm -rf -- "${fixture_dir}"
}
trap cleanup EXIT

assert_rejected() {
  local name="$1"
  local source="$2"
  local file="${fixture_dir}/${name}"
  case "${file}" in
    *.go) ;;
    *) file="${file}.go" ;;
  esac
  local output

  printf '%s\n' "${source}" > "${file}"
  if output="$(bash scripts/check-atomic-writes.sh 2>&1)"; then
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

assert_accepted() {
  local name="$1"
  local source="$2"
  local file="${fixture_dir}/${name}"
  case "${file}" in
    *.go) ;;
    *) file="${file}.go" ;;
  esac

  printf '%s\n' "${source}" > "${file}"
  if ! bash scripts/check-atomic-writes.sh >/dev/null 2>&1; then
    echo "ERROR: gate rejected ${name}, which is not a hand-rolled write" >&2
    bash scripts/check-atomic-writes.sh >&2 || true
    exit 1
  fi
  rm -f -- "${file}"
}

assert_rejected named_tmp $'package fixture\nimport "os"\nfunc f(dir, path string) error {\n\ttmp, err := os.CreateTemp(dir, "x-*")\n\tif err != nil {\n\t\treturn err\n\t}\n\ttmpName := tmp.Name()\n\treturn os.Rename(tmpName, path)\n}'
assert_rejected inline_name $'package fixture\nimport "os"\nfunc f(dir, path string) error {\n\tf, err := os.CreateTemp(dir, "x-*")\n\tif err != nil {\n\t\treturn err\n\t}\n\treturn os.Rename(f.Name(), path)\n}'
assert_rejected neutral_variable_name $'package fixture\nimport "os"\nfunc f(dir, path string) error {\n\tf, err := os.CreateTemp(dir, "x-*")\n\tif err != nil {\n\t\treturn err\n\t}\n\tstaging := f.Name()\n\treturn os.Rename(staging, path)\n}'
assert_rejected multiline $'package fixture\nimport "os"\nfunc f(dir, path string) error {\n\tf, err := os.CreateTemp(dir, "x-*")\n\tif err != nil {\n\t\treturn err\n\t}\n\treturn os.Rename(\n\t\tf.Name(),\n\t\tpath,\n\t)\n}'
assert_rejected aliased_import $'package fixture\nimport osfs "os"\nfunc f(dir, path string) error {\n\tf, err := osfs.CreateTemp(dir, "x-*")\n\tif err != nil {\n\t\treturn err\n\t}\n\treturn osfs.Rename(f.Name(), path)\n}'
assert_rejected fixture_test.go $'package fixture\nimport "os"\nfunc f(dir, path string) error {\n\tf, err := os.CreateTemp(dir, "x-*")\n\tif err != nil {\n\t\treturn err\n\t}\n\treturn os.Rename(f.Name(), path)\n}'

assert_accepted plain_rename $'package fixture\nimport "os"\nfunc f(from, to string) error {\n\treturn os.Rename(from, to)\n}'
assert_accepted rename_after_download $'package fixture\nimport "os"\nfunc f(dir, to string) error {\n\tstaged := dir + "/incoming"\n\treturn os.Rename(staged, to)\n}'

echo "check-atomic-writes.test: all tests passed"
