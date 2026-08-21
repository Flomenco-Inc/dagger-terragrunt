#!/usr/bin/env bash
# check-claude-bridges.sh — CI parity check for Claude Code CLAUDE.md bridges.
#
# Asserts:
#   1. Every AGENTS.md has a sibling CLAUDE.md whose first non-empty line is @AGENTS.md
#   2. Every bridge dir with .cursor/rules has BEGIN/END generated markers
#   3. Every alwaysApply:true .mdc is @-imported in that dir's CLAUDE.md
#   4. Every non-alwaysApply .mdc appears in the rules index
#
# Usage:
#   ./scripts/check-claude-bridges.sh              # cwd = repo root
#   ./scripts/check-claude-bridges.sh /path/to/repo
#
# Exit 0 on pass, 1 on fail. Suggests regenerating via gen-claude-bridges.sh.

set -euo pipefail

ROOT=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    -h | --help)
      echo "Usage: check-claude-bridges.sh [REPO_ROOT]"
      exit 0
      ;;
    -*)
      echo "Unknown option: $1" >&2
      exit 1
      ;;
    *)
      ROOT="$1"
      shift
      ;;
  esac
done

if [[ -z "$ROOT" ]]; then
  ROOT="$(pwd)"
fi
ROOT="$(cd "$ROOT" && pwd)"

BEGIN_ALWAYS='<!-- BEGIN generated: always-on rules (mirror of Cursor alwaysApply:true) -->'
END_ALWAYS='<!-- END generated: always-on rules (mirror of Cursor alwaysApply:true) -->'
BEGIN_INDEX='<!-- BEGIN generated: rules index -->'
END_INDEX='<!-- END generated: rules index -->'

failures=0
fail() {
  echo "✗ $*" >&2
  failures=$((failures + 1))
}

parse_mdc_meta() {
  local file="$1"
  awk '
    BEGIN { in_fm=0; always="false"; desc="" }
    NR==1 && /^---[[:space:]]*$/ { in_fm=1; next }
    in_fm && /^---[[:space:]]*$/ { exit }
    in_fm && /^alwaysApply:[[:space:]]*/ {
      val=$0; sub(/^alwaysApply:[[:space:]]*/, "", val)
      gsub(/[[:space:]]+$/, "", val)
      always=val
      next
    }
    in_fm && /^description:[[:space:]]*/ {
      val=$0; sub(/^description:[[:space:]]*/, "", val)
      if (val ~ /^".*"$/) { sub(/^"/, "", val); sub(/"$/, "", val) }
      if (val ~ /^'\''.*'\''$/) { sub(/^'\''/, "", val); sub(/'\''$/, "", val) }
      desc=val
      next
    }
    END { print always "|" desc }
  ' "$file"
}

collect_bridge_dirs() {
  local dir
  while IFS= read -r -d '' f; do
    dir="$(dirname "$f")"
    printf '%s\n' "$dir"
  done < <(find "$ROOT" \( -path '*/.git/*' -o -path '*/node_modules/*' -o -path '*/.venv/*' -o -path '*/dist/*' -o -path '*/.pnpm-store/*' \) -prune -o -type f -name 'AGENTS.md' -print0 2>/dev/null)

  while IFS= read -r -d '' d; do
    dir="$(dirname "$(dirname "$d")")"
    printf '%s\n' "$dir"
  done < <(find "$ROOT" \( -path '*/.git/*' -o -path '*/node_modules/*' -o -path '*/.venv/*' \) -prune -o -type d -path '*/.cursor/rules' -print0 2>/dev/null)
}

echo "Checking Claude Code bridges under $ROOT"

BRIDGE_DIRS=()
while IFS= read -r d; do
  [[ -z "$d" ]] && continue
  BRIDGE_DIRS+=("$d")
done < <(collect_bridge_dirs | sort -u)

if [[ ${#BRIDGE_DIRS[@]} -eq 0 ]]; then
  echo "No AGENTS.md or .cursor/rules found — nothing to check."
  exit 0
fi

for dir in "${BRIDGE_DIRS[@]}"; do
  [[ -z "$dir" ]] && continue
  local_rel="${dir#"$ROOT"/}"
  [[ "$local_rel" == "$dir" ]] && local_rel="."

  agents="$dir/AGENTS.md"
  claude="$dir/CLAUDE.md"
  rules_dir="$dir/.cursor/rules"

  if [[ -f "$agents" ]]; then
    if [[ ! -f "$claude" ]]; then
      fail "$local_rel/AGENTS.md missing sibling CLAUDE.md"
      continue
    fi
    first="$(awk 'NF { print; exit }' "$claude")"
    if [[ "$first" != "@AGENTS.md" ]]; then
      fail "$local_rel/CLAUDE.md first non-empty line must be @AGENTS.md (got: ${first:-<empty>})"
    fi
  fi

  if [[ ! -d "$rules_dir" ]]; then
    continue
  fi

  if [[ ! -f "$claude" ]]; then
    fail "$local_rel/.cursor/rules present but CLAUDE.md missing"
    continue
  fi

  if ! grep -qxF "$BEGIN_ALWAYS" "$claude" || ! grep -qxF "$END_ALWAYS" "$claude"; then
    fail "$local_rel/CLAUDE.md missing always-on generated markers"
  fi
  if ! grep -qxF "$BEGIN_INDEX" "$claude" || ! grep -qxF "$END_INDEX" "$claude"; then
    fail "$local_rel/CLAUDE.md missing rules-index generated markers"
  fi

  always_block="$(awk -v ba="$BEGIN_ALWAYS" -v ea="$END_ALWAYS" '
    $0==ba {p=1; next} $0==ea {p=0; next} p {print}
  ' "$claude")"
  index_block="$(awk -v bi="$BEGIN_INDEX" -v ei="$END_INDEX" '
    $0==bi {p=1; next} $0==ei {p=0; next} p {print}
  ' "$claude")"

  while IFS= read -r -d '' mdc; do
    meta="$(parse_mdc_meta "$mdc")"
    always="${meta%%|*}"
    basename="$(basename "$mdc")"
    import_line="@.cursor/rules/${basename}"
    index_needle="\`.cursor/rules/${basename}\`"

    if [[ "$always" == "true" ]]; then
      if ! printf '%s\n' "$always_block" | grep -qxF "$import_line"; then
        fail "$local_rel/CLAUDE.md missing always-on import $import_line"
      fi
    else
      if ! printf '%s\n' "$index_block" | grep -qF "$index_needle"; then
        fail "$local_rel/CLAUDE.md rules index missing $index_needle"
      fi
    fi
  done < <(find "$rules_dir" -maxdepth 1 -type f -name '*.mdc' -print0 | sort -z)
done

echo ""
if [[ $failures -gt 0 ]]; then
  echo "$failures Claude Code bridge check(s) failed."
  echo "Fix: run scripts/gen-claude-bridges.sh from the repo root and commit."
  exit 1
fi

echo "✓ Claude Code bridges OK"
exit 0
