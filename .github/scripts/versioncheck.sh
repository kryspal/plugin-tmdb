#!/usr/bin/env bash
# Fail when the plugin binary differs from the one at the latest tag but VERSION was not bumped (a published version never changes).
# Without a previous tag (the first release) the check is skipped.
set -euo pipefail
id="${PLUGIN_ID:?PLUGIN_ID is not set}"
prev="$(git describe --tags --abbrev=0 --match 'v*' 2>/dev/null || true)"
[[ -n "$prev" ]] || { echo "no previous tag; skipping"; exit 0; }
build() { ( cd "$1" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -o "$2" . ); }
work="$(mktemp -d)"
git worktree add -q "$work/prev" "$prev"
build "$work/prev" "$work/prev.bin"
build . "$work/cur.bin"
git worktree remove --force "$work/prev"
if ! cmp -s "$work/prev.bin" "$work/cur.bin" && [[ "$(tr -d '[:space:]' < VERSION)" == "${prev#v}" ]]; then
  echo "error: the binary changed since $prev but VERSION is still ${prev#v}; bump VERSION" >&2
  exit 1
fi
echo "version check passed ($id, previous $prev)"
