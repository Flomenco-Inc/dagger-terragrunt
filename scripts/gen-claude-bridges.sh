#!/usr/bin/env bash
# gen-claude-bridges.sh — idempotent CLAUDE.md bridges for Claude Code.
#
# Claude Code reads CLAUDE.md (not AGENTS.md / .cursor/rules/*.mdc). This
# script writes thin CLAUDE.md files that:
#   1. @-import sibling AGENTS.md (when present)
#   2. @-import every alwaysApply:true rule under local .cursor/rules/
#   3. Index on-demand (globs / agent-requested) rules for manual Read
#
# Generated regions live between BEGIN/END markers so hand-written Claude-only
# notes outside those markers are preserved across re-runs.
#
# Usage:
#   ./scripts/gen-claude-bridges.sh              # cwd = repo root
#   ./scripts/gen-claude-bridges.sh /path/to/repo
#   ./scripts/gen-claude-bridges.sh --dry-run
#
# Exit 0 always on success (including "nothing to write").

set -euo pipefail

DRY_RUN=false
ROOT=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) DRY_RUN=true; shift ;;
    -h | --help)
      echo "Usage: gen-claude-bridges.sh [--dry-run] [REPO_ROOT]"
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

# Parse one .mdc: print "alwaysApply|<description>" (description may be empty).
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
      # strip optional surrounding quotes
      if (val ~ /^".*"$/) { sub(/^"/, "", val); sub(/"$/, "", val) }
      if (val ~ /^'\''.*'\''$/) { sub(/^'\''/, "", val); sub(/'\''$/, "", val) }
      desc=val
      next
    }
    END { print always "|" desc }
  ' "$file"
}

# Collect unique bridge directories: parents of AGENTS.md and parents of .cursor/rules.
collect_bridge_dirs() {
  local dir
  # AGENTS.md anywhere
  while IFS= read -r -d '' f; do
    dir="$(dirname "$f")"
    printf '%s\n' "$dir"
  done < <(find "$ROOT" \( -path '*/.git/*' -o -path '*/node_modules/*' -o -path '*/.venv/*' -o -path '*/dist/*' -o -path '*/.pnpm-store/*' \) -prune -o -type f -name 'AGENTS.md' -print0 2>/dev/null)

  # .cursor/rules directories (parent of .cursor is the bridge dir)
  while IFS= read -r -d '' d; do
    # d = .../.cursor/rules → parent of .cursor
    dir="$(dirname "$(dirname "$d")")"
    printf '%s\n' "$dir"
  done < <(find "$ROOT" \( -path '*/.git/*' -o -path '*/node_modules/*' -o -path '*/.venv/*' \) -prune -o -type d -path '*/.cursor/rules' -print0 2>/dev/null)
}

# Strip generated blocks and a leading @AGENTS.md import from existing content.
# Remaining text is preserved as hand-written Claude-only notes.
extract_handwritten() {
  local file="$1"
  [[ -f "$file" ]] || { echo ""; return; }
  awk -v ba="$BEGIN_ALWAYS" -v ea="$END_ALWAYS" -v bi="$BEGIN_INDEX" -v ei="$END_INDEX" '
    BEGIN { skip=0 }
    $0 == ba { skip=1; next }
    $0 == ea { skip=0; next }
    $0 == bi { skip=1; next }
    $0 == ei { skip=0; next }
    skip { next }
    # drop lone @AGENTS.md import lines (re-emitted)
    /^@AGENTS\.md[[:space:]]*$/ { next }
    # drop Visibility: lines copied from AGENTS.md (re-emitted)
    /^Visibility:[[:space:]]/ { next }
    # drop the stock Claude Code heading/blurbs we re-emit
    /^## Claude Code[[:space:]]*$/ { next }
    /^- No glob auto-attach here:/ { next }
    /^- Shared guidance lives in `AGENTS\.md`/ { next }
    /^- Cursor rules \(if any\) live under ancestor/ { next }
    { print }
  ' "$file" | awk '
    # trim leading/trailing blank lines
    { lines[NR]=$0 }
    END {
      start=1
      while (start<=NR && lines[start] ~ /^[[:space:]]*$/) start++
      end=NR
      while (end>=start && lines[end] ~ /^[[:space:]]*$/) end--
      for (i=start; i<=end; i++) print lines[i]
    }
  '
}

# First Visibility: line from AGENTS.md (flo-context corpus metadata), if any.
agents_visibility_line() {
  local agents="$1"
  [[ -f "$agents" ]] || return 0
  awk '/^Visibility:[[:space:]]/ { print; exit }' "$agents"
}

build_claude_for_dir() {
  local dir="$1"
  local claude="$dir/CLAUDE.md"
  local rules_dir="$dir/.cursor/rules"
  local agents="$dir/AGENTS.md"
  local rel_rules=".cursor/rules"

  local always_imports=()
  local index_lines=()
  local has_rules=false

  if [[ -d "$rules_dir" ]]; then
    has_rules=true
    local mdc meta always desc basename import_path
    # stable sort by filename
    while IFS= read -r -d '' mdc; do
      meta="$(parse_mdc_meta "$mdc")"
      always="${meta%%|*}"
      desc="${meta#*|}"
      basename="$(basename "$mdc")"
      import_path="${rel_rules}/${basename}"
      if [[ "$always" == "true" ]]; then
        always_imports+=("@${import_path}")
      else
        if [[ -z "$desc" ]]; then
          desc="$basename"
        fi
        index_lines+=("- ${desc} — \`${import_path}\`")
      fi
    done < <(find "$rules_dir" -maxdepth 1 -type f -name '*.mdc' -print0 | sort -z)
  fi

  local has_agents=false
  [[ -f "$agents" ]] && has_agents=true

  # Nothing to bridge in this dir
  if ! $has_agents && ! $has_rules; then
    return 0
  fi

  local handwritten
  handwritten="$(extract_handwritten "$claude")"

  local visibility=""
  if $has_agents; then
    visibility="$(agents_visibility_line "$agents")"
  fi

  local out=""
  if $has_agents; then
    out+="@AGENTS.md"$'\n'
    if [[ -n "$visibility" ]]; then
      out+="${visibility}"$'\n'
    fi
    out+=$'\n'
  fi

  if $has_rules; then
    out+="${BEGIN_ALWAYS}"$'\n'
    if [[ ${#always_imports[@]} -gt 0 ]]; then
      local line
      for line in "${always_imports[@]}"; do
        out+="${line}"$'\n'
      done
    else
      out+="<!-- (no alwaysApply:true rules in this directory) -->"$'\n'
    fi
    out+="${END_ALWAYS}"$'\n\n'

    out+="## Claude Code"$'\n'
    out+="- No glob auto-attach here: read the on-demand rule that matches your task."$'\n'
    out+="${BEGIN_INDEX}"$'\n'
    if [[ ${#index_lines[@]} -gt 0 ]]; then
      local line
      for line in "${index_lines[@]}"; do
        out+="${line}"$'\n'
      done
    else
      out+="<!-- (no on-demand rules in this directory) -->"$'\n'
    fi
    out+="${END_INDEX}"$'\n'
  elif $has_agents; then
    # AGENTS-only bridge: still add a short Claude Code note so the file isn't just @AGENTS.md
    out+="## Claude Code"$'\n'
    out+="- Shared guidance lives in \`AGENTS.md\` (imported above)."$'\n'
    out+="- Cursor rules (if any) live under ancestor \`.cursor/rules/\`; check the nearest \`CLAUDE.md\`."$'\n'
  fi

  if [[ -n "$handwritten" ]]; then
    out+=$'\n'"${handwritten}"$'\n'
  fi

  if [[ -f "$claude" ]]; then
    # Do not use $(cat) — command substitution strips trailing newlines and
    # makes the equality check flap, rewriting every run.
    if printf '%s' "$out" | cmp -s - "$claude"; then
      echo "  = $claude (unchanged)"
      return 0
    fi
  fi

  local rel="${claude#"$ROOT"/}"
  if $DRY_RUN; then
    echo "  ~ $rel (would write)"
  else
    # Replace symlinks (e.g. CLAUDE.md -> AGENTS.md) so we never overwrite AGENTS.md
    if [[ -L "$claude" ]]; then
      rm -f "$claude"
    fi
    printf '%s' "$out" >"$claude"
    echo "  + $rel"
  fi
}

echo "Generating CLAUDE.md bridges under $ROOT"
if $DRY_RUN; then
  echo "(dry-run)"
fi

BRIDGE_DIRS=()
while IFS= read -r d; do
  [[ -z "$d" ]] && continue
  BRIDGE_DIRS+=("$d")
done < <(collect_bridge_dirs | sort -u)

if [[ ${#BRIDGE_DIRS[@]} -eq 0 ]]; then
  echo "No AGENTS.md or .cursor/rules found — nothing to do."
  exit 0
fi

for d in "${BRIDGE_DIRS[@]}"; do
  [[ -z "$d" ]] && continue
  build_claude_for_dir "$d"
done

echo "Done."
