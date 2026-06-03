#!/usr/bin/env bash
# CI drift check: fail if the committed Go types don't match openapi.json.
# Run from anywhere; operates on this directory.
set -euo pipefail
cd "$(dirname "$0")"

go generate ./...

if ! git diff --quiet -- types.gen.go; then
  echo "::error:: internal/beamdapi/types.gen.go is stale." >&2
  echo "The OpenAPI spec changed without regenerating. Run 'make api-gen' and commit." >&2
  git --no-pager diff -- types.gen.go || true
  exit 1
fi

echo "beamdapi types are in sync with openapi.json."
