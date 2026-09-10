#!/usr/bin/env bash
# Reproducibly generate and verify the Linux/amd64 helper binaries embedded in
# the provider. This script never reads Databricks configuration or credentials.
set -euo pipefail

readonly PINNED_GO_VERSION="1.26.8"
readonly ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly GO_BIN="${GO:-go}"

usage() {
  echo "usage: $0 {generate|check|smoke} [bzmux|bzhttpmcp|all]" >&2
  exit 2
}

[[ $# -ge 1 && $# -le 2 ]] || usage
action=$1
helper=${2:-all}
case "$action" in generate|check|smoke) ;; *) usage ;; esac
case "$helper" in bzmux|bzhttpmcp|all) ;; *) usage ;; esac

cd "$ROOT"

verify_toolchain() {
  local actual
  actual="$($GO_BIN env GOVERSION)"
  if [[ "$actual" != "go${PINNED_GO_VERSION}" ]]; then
    echo "embedded helpers require Go ${PINNED_GO_VERSION}; selected toolchain is ${actual}" >&2
    exit 1
  fi
}

selected_helpers() {
  if [[ "$helper" == "all" ]]; then
    printf '%s\n' bzmux bzhttpmcp
  else
    printf '%s\n' "$helper"
  fi
}

artifact_for() {
  case "$1" in
    bzmux) echo "internal/muxbin/bzmux.linux-amd64" ;;
    bzhttpmcp) echo "internal/httpmcpbin/bzhttpmcp.linux-amd64" ;;
  esac
}

hashfile_for() {
  case "$1" in
    bzmux) echo "internal/muxbin/bzmux.srchash" ;;
    bzhttpmcp) echo "internal/httpmcpbin/bzhttpmcp.srchash" ;;
  esac
}

build_helper() {
  local name=$1 output=$2
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1 \
    "$GO_BIN" build -mod=readonly -trimpath -buildvcs=false \
      -ldflags='-s -w -buildid=' -o "$output" "./cmd/${name}"
}

write_source_hash() {
  local name=$1 output=$2
  python3 - "$name" "$output" <<'PY'
import hashlib
import pathlib
import sys

name, output = sys.argv[1:]
directories = {
    "bzmux": ("cmd/bzmux", "internal/muxcfg"),
    "bzhttpmcp": ("cmd/bzhttpmcp", "internal/httpmcpbin"),
}[name]
paths = sorted(
    path
    for directory in directories
    for path in pathlib.Path(directory).iterdir()
    if path.is_file() and path.suffix == ".go" and not path.name.endswith("_test.go")
)
digest = hashlib.sha256()
for path in paths:
    digest.update(path.as_posix().encode())
    digest.update(b"\n")
    digest.update(path.read_bytes())
pathlib.Path(output).write_text(digest.hexdigest() + "\n")
PY
}

compare_file() {
  local generated=$1 committed=$2 label=$3
  if ! cmp -s "$generated" "$committed"; then
    echo "${label} differs from its reproducible Go ${PINNED_GO_VERSION} output." >&2
    echo "Run 'make embedded-generate' with Go ${PINNED_GO_VERSION} and commit the result." >&2
    return 1
  fi
}

generate() {
  verify_toolchain
  local tmp name artifact hashfile
  tmp=$(mktemp -d)
  trap "rm -rf '$tmp'" EXIT
  while IFS= read -r name; do
    artifact=$(artifact_for "$name")
    hashfile=$(hashfile_for "$name")
    build_helper "$name" "$tmp/$name"
    write_source_hash "$name" "$tmp/$name.srchash"
    install -m 0755 "$tmp/$name" "$artifact"
    install -m 0644 "$tmp/$name.srchash" "$hashfile"
    echo "generated $artifact and $hashfile with Go ${PINNED_GO_VERSION}"
  done < <(selected_helpers)
}

check() {
  verify_toolchain
  local tmp name artifact hashfile
  tmp=$(mktemp -d)
  trap "rm -rf '$tmp'" EXIT
  while IFS= read -r name; do
    artifact=$(artifact_for "$name")
    hashfile=$(hashfile_for "$name")
    build_helper "$name" "$tmp/$name.first"
    build_helper "$name" "$tmp/$name.second"
    write_source_hash "$name" "$tmp/$name.srchash"
    compare_file "$tmp/$name.first" "$tmp/$name.second" "$name repeated build" || exit 1
    compare_file "$tmp/$name.first" "$artifact" "$artifact" || exit 1
    compare_file "$tmp/$name.srchash" "$hashfile" "$hashfile" || exit 1
    echo "verified deterministic bytes and source hash for $name"
  done < <(selected_helpers)
}

smoke() {
  if [[ "$(uname -s)" != Linux || "$(uname -m)" != x86_64 ]]; then
    echo "embedded helper execution smoke tests require a Linux/x86_64 host; skipping" >&2
    return 0
  fi

  local output status=0
  output=$(mktemp)
  trap "rm -f '$output'" EXIT

  if [[ "$helper" == all || "$helper" == bzmux ]]; then
    "$(artifact_for bzmux)" -h >"$output" 2>&1
    grep -q 'Usage of' "$output"
    echo "smoke-tested bzmux"
  fi

  if [[ "$helper" == all || "$helper" == bzhttpmcp ]]; then
    set +e
    env -u DATABRICKS_HOST -u DATABRICKS_TOKEN -u DATABRICKS_CONFIG_PROFILE \
      "$(artifact_for bzhttpmcp)" -h >"$output" 2>&1
    status=$?
    set -e
    [[ $status -eq 1 ]] || {
      echo "bzhttpmcp -h exited $status; expected its documented help status 1" >&2
      cat "$output" >&2
      exit 1
    }
    grep -q 'Usage of bzhttpmcp' "$output"
    grep -q 'flag: help requested' "$output"
    echo "smoke-tested bzhttpmcp without credentials or a network request"
  fi
}

case "$action" in
  generate) generate ;;
  check) check ;;
  smoke) smoke ;;
esac
