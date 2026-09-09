#!/usr/bin/env bash
# Generate Claude Code bridges and skill mirrors from Cursor guidance.

set -euo pipefail

# shellcheck disable=SC2034 # Read by the organization coverage audit.
AGENT_GUIDANCE_TOOLING_VERSION=2
DRY_RUN=false
ROOT=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) DRY_RUN=true; shift ;;
    -h | --help)
      echo "Usage: gen-agent-guidance.sh [--dry-run] [REPO_ROOT]"
      exit 0
      ;;
    -*)
      echo "Unknown option: $1" >&2
      exit 2
      ;;
    *)
      ROOT="$1"
      shift
      ;;
  esac
done

[[ -n "$ROOT" ]] || ROOT="$(pwd)"
ROOT="$(cd "$ROOT" && pwd)"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if $DRY_RUN; then
  "$SCRIPT_DIR/gen-claude-bridges.sh" --dry-run "$ROOT"
else
  "$SCRIPT_DIR/gen-claude-bridges.sh" "$ROOT"
fi

validate_skill() {
  local skill_dir="$1"
  local skill_file="$skill_dir/SKILL.md"
  local expected_name
  expected_name="$(basename "$skill_dir")"

  if [[ ! -f "$skill_file" ]]; then
    echo "Invalid Cursor skill: $skill_dir has no SKILL.md" >&2
    return 1
  fi

  awk -v expected="$expected_name" '
    BEGIN { in_fm=0; closed=0; name=""; description="" }
    NR==1 && /^---[[:space:]]*$/ { in_fm=1; next }
    in_fm && /^---[[:space:]]*$/ { closed=1; exit }
    in_fm && /^name:[[:space:]]*/ {
      name=$0
      sub(/^name:[[:space:]]*/, "", name)
      gsub(/^["'\'']|["'\'']$/, "", name)
    }
    in_fm && /^description:[[:space:]]*/ {
      description=$0
      sub(/^description:[[:space:]]*/, "", description)
    }
    END {
      if (!closed || name != expected || description == "") exit 1
    }
  ' "$skill_file" || {
    echo "Invalid Cursor skill metadata: $skill_file (name must be '$expected_name'; description is required)" >&2
    return 1
  }
}

is_nested_checkout() {
  local dir="$1"
  while [[ "$dir" != "$ROOT" && "$dir" != "/" ]]; do
    [[ -e "$dir/.git" ]] && return 0
    dir="$(dirname "$dir")"
  done
  return 1
}

collect_skill_roots() {
  local cursor_dir parent manifest
  while IFS= read -r -d '' cursor_dir; do
    parent="$(dirname "$(dirname "$cursor_dir")")"
    if is_nested_checkout "$parent"; then
      continue
    fi
    printf '%s\0' "$parent"
  done < <(
    find "$ROOT" \
      \( -type d \( -name .git -o -name node_modules -o -name .venv -o -name dist -o -name .pnpm-store \) -prune \) -o \
      -type d -path '*/.cursor/skills' -print0 2>/dev/null
  )

  while IFS= read -r -d '' manifest; do
    parent="$(dirname "$(dirname "$manifest")")"
    if is_nested_checkout "$parent"; then
      continue
    fi
    printf '%s\0' "$parent"
  done < <(
    find "$ROOT" \
      \( -type d \( -name .git -o -name node_modules -o -name .venv -o -name dist -o -name .pnpm-store \) -prune \) -o \
      -type f -path '*/.claude/.cursor-skill-sync-manifest' -print0 2>/dev/null
  )
}

sync_skill_root() {
  local parent="$1"
  local source_root="$parent/.cursor/skills"
  local target_root="$parent/.claude/skills"
  local manifest="$parent/.claude/.cursor-skill-sync-manifest"
  local new_manifest old_entry source_file rel target_file skill_dir
  new_manifest="$(mktemp)"
  trap 'rm -f "$new_manifest"' RETURN

  if [[ -d "$source_root" ]]; then
    while IFS= read -r -d '' skill_dir; do
      validate_skill "$skill_dir"
    done < <(find "$source_root" -mindepth 1 -maxdepth 1 -type d -print0 | sort -z)

    while IFS= read -r -d '' source_file; do
      rel="${source_file#"$source_root"/}"
      printf 'skills/%s\n' "$rel" >>"$new_manifest"
    done < <(find "$source_root" -type f -print0 | sort -z)
  fi

  if [[ -f "$manifest" ]]; then
    while IFS= read -r old_entry; do
      [[ -n "$old_entry" ]] || continue
      case "$old_entry" in
        skills/*)
          case "/$old_entry/" in
            *"/../"* | *"/./"* | *"//"*)
              echo "Unsafe generated skill manifest entry: $old_entry" >&2
              return 1
              ;;
          esac
          [[ "$old_entry" != */ ]] || {
            echo "Unsafe generated skill manifest entry: $old_entry" >&2
            return 1
          }
          if ! grep -qxF "$old_entry" "$new_manifest"; then
            target_file="$parent/.claude/$old_entry"
            if $DRY_RUN; then
              echo "  - ${target_file#"$ROOT"/} (would remove)"
            else
              rm -f "$target_file"
            fi
          fi
          ;;
        *)
          echo "Unsafe generated skill manifest entry: $old_entry" >&2
          return 1
          ;;
      esac
    done <"$manifest"
  fi

  while IFS= read -r old_entry; do
    [[ -n "$old_entry" ]] || continue
    rel="${old_entry#skills/}"
    source_file="$source_root/$rel"
    target_file="$target_root/$rel"
    if [[ -f "$target_file" ]] && cmp -s "$source_file" "$target_file"; then
      continue
    fi
    if $DRY_RUN; then
      echo "  ~ ${target_file#"$ROOT"/} (would write)"
    else
      mkdir -p "$(dirname "$target_file")"
      [[ ! -L "$target_file" ]] || rm -f "$target_file"
      cp "$source_file" "$target_file"
      echo "  + ${target_file#"$ROOT"/}"
    fi
  done <"$new_manifest"

  if [[ ! -s "$new_manifest" ]]; then
    if $DRY_RUN; then
      [[ ! -f "$manifest" ]] || echo "  - ${manifest#"$ROOT"/} (would remove)"
    else
      rm -f "$manifest"
      find "$target_root" -depth -type d -empty -delete 2>/dev/null || true
      rmdir "$parent/.claude" 2>/dev/null || true
    fi
  elif $DRY_RUN; then
    if [[ ! -f "$manifest" ]] || ! cmp -s "$new_manifest" "$manifest"; then
      echo "  ~ ${manifest#"$ROOT"/} (would write)"
    fi
  else
    mkdir -p "$(dirname "$manifest")"
    cp "$new_manifest" "$manifest"
    find "$target_root" -depth -type d -empty -delete 2>/dev/null || true
  fi

  rm -f "$new_manifest"
  trap - RETURN
}

echo "Generating Claude skill mirrors under $ROOT"
while IFS= read -r -d '' parent; do
  sync_skill_root "$parent"
done < <(collect_skill_roots | sort -zu)

echo "Agent guidance generation complete."
