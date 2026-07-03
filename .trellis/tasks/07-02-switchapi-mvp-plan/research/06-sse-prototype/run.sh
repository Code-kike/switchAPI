#!/usr/bin/env bash
# run.sh — research item #06 prototype runner.
# Requires a Go toolchain (>= 1.22). If `go` is not on PATH, point GO_BIN at one,
# e.g. GO_BIN=/tmp/gotoolchain/go/bin/go ./run.sh
set -euo pipefail
cd "$(dirname "$0")"

GO_BIN="${GO_BIN:-go}"
"$GO_BIN" version

echo "== go vet =="
"$GO_BIN" vet ./...

echo "== tests (timing/rewrite/tee/disconnect assertions) =="
"$GO_BIN" test -v -count=1 ./...

echo "== demo run: proxy on 127.0.0.1:19527 -> fake upstream on 127.0.0.1:19528 =="
"$GO_BIN" run .
