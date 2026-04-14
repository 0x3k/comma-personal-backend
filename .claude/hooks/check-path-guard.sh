#!/usr/bin/env bash
set -euo pipefail

# check-path-guard.sh -- PreToolUse hook that blocks file operations outside
# the project directory. Catches absolute paths, ".." traversal, and symlink
# escapes. Intended for vibes mode where most tools are auto-approved.
#
# Handles:
#   Bash tools: rm, cp, mv, cat, head, tail, touch, chmod
#   Claude tools: Read, Write, Edit (via tool_input.file_path)

INPUT=$(cat)
TOOL=$(echo "$INPUT" | jq -r '.tool_name // empty')
CWD=$(echo "$INPUT" | jq -r '.cwd // empty')

[ -z "$CWD" ] && exit 0

# Resolve project root to a real path (no symlinks)
PROJECT_ROOT=$(cd "$CWD" && pwd -P)

deny() {
    local reason="$1"
    jq -n --arg reason "$reason" '{
        hookSpecificOutput: {
            hookEventName: "PreToolUse",
            permissionDecision: "deny",
            permissionDecisionReason: $reason
        }
    }'
    exit 0
}

# Resolve a path and check it stays within the project.
# Returns 0 if safe, 1 if it escapes.
is_inside_project() {
    local target="$1"

    # Make relative paths absolute from project root
    if [[ "$target" != /* ]]; then
        target="${PROJECT_ROOT}/${target}"
    fi

    # Resolve ".." and symlinks. If the path doesn't exist yet (e.g. new file),
    # walk up until we find an existing ancestor and resolve from there.
    local resolved="$target"
    if [ -e "$target" ]; then
        resolved=$(cd "$(dirname "$target")" && pwd -P)/$(basename "$target")
    else
        local dir="$target"
        while [ ! -d "$dir" ]; do
            dir=$(dirname "$dir")
        done
        resolved=$(cd "$dir" && pwd -P)/${target#"$dir"/}
    fi

    # Check prefix
    [[ "$resolved" == "${PROJECT_ROOT}/"* ]] || [[ "$resolved" == "${PROJECT_ROOT}" ]]
}

# --- Claude tools: Read, Write, Edit ---
if [[ "$TOOL" == "Read" || "$TOOL" == "Write" || "$TOOL" == "Edit" ]]; then
    FILE_PATH=$(echo "$INPUT" | jq -r '.tool_input.file_path // empty')
    [ -z "$FILE_PATH" ] && exit 0

    if ! is_inside_project "$FILE_PATH"; then
        deny "Blocked: '$FILE_PATH' is outside the project directory. File operations are restricted to ${PROJECT_ROOT}."
    fi
    exit 0
fi

# --- Bash tools ---
if [[ "$TOOL" == "Bash" ]]; then
    COMMAND=$(echo "$INPUT" | jq -r '.tool_input.command // empty')
    [ -z "$COMMAND" ] && exit 0

    # Check destructive/read commands for paths outside the project.
    # Split compound commands on shell operators and check each segment.
    DANGEROUS_PAT='(rm|cp|mv|cat|head|tail|touch|chmod)'
    if printf '%s' "$COMMAND" | grep -qE "${DANGEROUS_PAT}\\s"; then
        while IFS= read -r segment; do
            segment="${segment#"${segment%%[![:space:]]*}"}"
            [ -z "$segment" ] && continue

            CMD_NAME=$(printf '%s' "$segment" | grep -oE "$DANGEROUS_PAT" | head -1)
            [ -z "$CMD_NAME" ] && continue

            ARGS_STR=$(printf '%s' "$segment" | sed -E "s/.*${CMD_NAME}[[:space:]]+(.*)/\1/")
            [ -z "$ARGS_STR" ] && continue

            # Use xargs to split arguments respecting shell quoting
            while IFS= read -r arg; do
                [ -z "$arg" ] && continue
                [[ "$arg" == -* ]] && continue
                if ! is_inside_project "$arg"; then
                    deny "Blocked: '${CMD_NAME} ${arg}' targets a path outside the project directory. File operations are restricted to ${PROJECT_ROOT}."
                fi
            done < <(printf '%s' "$ARGS_STR" | xargs -n1 2>/dev/null)
        done < <(printf '%s' "$COMMAND" | awk '{gsub(/[[:space:]]*(&&|[|][|]|;|[|])[[:space:]]*/,"\n"); print}')
    fi
fi

# Default: allow
exit 0
