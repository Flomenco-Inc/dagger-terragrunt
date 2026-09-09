#!/usr/bin/env bash
# Read-only parity check for Cursor rules/skills and generated Claude guidance.

set -euo pipefail

ROOT="${1:-$(pwd)}"
ROOT="$(cd "$ROOT" && pwd)"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
output="$(mktemp)"
trap 'rm -f "$output"' EXIT

"$SCRIPT_DIR/check-claude-bridges.sh" "$ROOT"

if ! "$SCRIPT_DIR/gen-agent-guidance.sh" --dry-run "$ROOT" >"$output"; then
  cat "$output"
  exit 1
fi

if grep -qE '\(would (write|remove)\)' "$output"; then
  echo "Claude guidance is stale:" >&2
  grep -E '\(would (write|remove)\)' "$output" >&2
  echo "Fix: run scripts/gen-agent-guidance.sh and commit the generated files." >&2
  exit 1
fi

echo "✓ Cursor–Claude agent guidance is in sync"
