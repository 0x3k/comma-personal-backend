#!/usr/bin/env bash
set -euo pipefail

# upgrade.sh -- Update a projd-based project to the latest template version.
#
# Compares each template file against a stored manifest of checksums to detect
# user modifications. Unmodified files are overwritten silently. Modified files
# prompt the user to choose: overwrite, diff, or keep.
#
# Usage:
#   ./.projd/scripts/upgrade.sh                 # upgrade from remote
#   ./.projd/scripts/upgrade.sh --local <path>  # upgrade from a local template copy
#   ./.projd/scripts/upgrade.sh --dry-run       # show what would change without applying
#   ./.projd/scripts/upgrade.sh --manifest      # regenerate manifest from current files

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

MANIFEST_DIR="$PROJECT_DIR/.projd"
MANIFEST_FILE="$MANIFEST_DIR/manifest"
DRY_RUN=false
MANIFEST_ONLY=false
LOCAL_PATH=""

cd "$PROJECT_DIR"
load_template_files

# --- Parse flags ---
while [[ $# -gt 0 ]]; do
    case $1 in
        --dry-run)  DRY_RUN=true; shift ;;
        --manifest) MANIFEST_ONLY=true; shift ;;
        --local)    LOCAL_PATH="$2"; shift 2 ;;
        --help|-h)
            echo "Usage: ./.projd/scripts/upgrade.sh [--dry-run] [--manifest] [--local <path>]"
            echo ""
            echo "  --dry-run    Show what would change without applying"
            echo "  --manifest   Regenerate manifest from current files (no upgrade)"
            echo "  --local      Use a local template directory instead of fetching remote"
            exit 0
            ;;
        *) echo "Unknown flag: $1 (try --help)"; exit 1 ;;
    esac
done

# --- Manifest operations ---

manifest_read() {
    local file="$1"
    if [ -f "$MANIFEST_FILE" ]; then
        grep "^${file}	" "$MANIFEST_FILE" 2>/dev/null | cut -f2 || true
    fi
}

manifest_write() {
    mkdir -p "$MANIFEST_DIR"
    # Write all checksums as tab-separated: filepath<TAB>checksum
    local tmpfile
    tmpfile=$(mktemp)
    for f in "${TEMPLATE_FILES[@]}"; do
        local cs
        cs=$(file_checksum "$PROJECT_DIR/$f")
        printf '%s\t%s\n' "$f" "$cs" >> "$tmpfile"
    done
    mv "$tmpfile" "$MANIFEST_FILE"
}

# --- Manifest-only mode ---
if [ "$MANIFEST_ONLY" = true ]; then
    echo -e "${BLD}Regenerating manifest...${R}"
    manifest_write
    echo -e "${GRN}Manifest written to ${MANIFEST_FILE}${R}"
    echo -e "${DIM}$(wc -l < "$MANIFEST_FILE") files tracked${R}"
    exit 0
fi

# --- Fetch latest template ---
TEMPLATE_DIR=""
CLEANUP_TEMPLATE=false

if [ -n "$LOCAL_PATH" ]; then
    if [ ! -d "$LOCAL_PATH" ]; then
        echo -e "${RED}Local path does not exist: ${LOCAL_PATH}${R}"
        exit 1
    fi
    TEMPLATE_DIR="$LOCAL_PATH"
else
    # Try to find the remote URL from the projd-create skill or git
    REMOTE_URL=""

    # Check if .projd/source exists (written at scaffold time)
    if [ -f "$MANIFEST_DIR/source" ]; then
        REMOTE_URL=$(cat "$MANIFEST_DIR/source")
    fi

    # Fallback: check for a git remote named 'projd'
    if [ -z "$REMOTE_URL" ]; then
        REMOTE_URL=$(git -C "$PROJECT_DIR" config --get remote.projd.url 2>/dev/null || true)
    fi

    # Fallback: hardcoded template URL
    if [ -z "$REMOTE_URL" ]; then
        REMOTE_URL="https://github.com/0spoon/projd.git"
    fi

    echo -e "${DIM}Fetching template from ${REMOTE_URL}...${R}"
    TEMPLATE_DIR=$(mktemp -d)
    CLEANUP_TEMPLATE=true
    if ! git clone --depth 1 "$REMOTE_URL" "$TEMPLATE_DIR" 2>/dev/null; then
        rm -rf "$TEMPLATE_DIR"
        echo -e "${RED}Failed to clone template from ${REMOTE_URL}${R}"
        exit 1
    fi
fi

# Cleanup on exit
if [ "$CLEANUP_TEMPLATE" = true ]; then
    trap 'rm -rf "$TEMPLATE_DIR"' EXIT
fi

# Reload template file list from the fetched template (the project may not
# have template-files.txt yet if this is the first upgrade).
if [ -f "$TEMPLATE_DIR/.projd/template-files.txt" ]; then
    TEMPLATE_FILES=()
    while IFS= read -r _line || [ -n "$_line" ]; do
        _line="${_line%%#*}"
        _line="${_line#"${_line%%[![:space:]]*}"}"
        _line="${_line%"${_line##*[![:space:]]}"}"
        [ -n "$_line" ] && TEMPLATE_FILES+=("$_line")
    done < "$TEMPLATE_DIR/.projd/template-files.txt"
fi

if [ ${#TEMPLATE_FILES[@]} -eq 0 ]; then
    echo -e "${RED}No template files to upgrade (template-files.txt is empty).${R}"
    exit 1
fi

# --- Check manifest exists ---
FIRST_RUN=false
if [ ! -f "$MANIFEST_FILE" ]; then
    FIRST_RUN=true
    echo -e "${YLW}No manifest found. This is the first upgrade.${R}"
    echo -e "${DIM}Files that differ from the template will be flagged for review.${R}"
    echo ""
fi

# --- Compare and update ---
updated=0
skipped=0
kept=0
added=0
unchanged=0

for f in "${TEMPLATE_FILES[@]}"; do
    template_file="$TEMPLATE_DIR/$f"
    project_file="$PROJECT_DIR/$f"

    # Skip files that don't exist in the new template
    if [ ! -f "$template_file" ]; then
        continue
    fi

    template_cs=$(file_checksum "$template_file")
    current_cs=$(file_checksum "$project_file")

    # File is identical to template -- nothing to do
    if [ "$template_cs" = "$current_cs" ]; then
        unchanged=$((unchanged + 1))
        continue
    fi

    # New file (doesn't exist in project)
    if [ "$current_cs" = "MISSING" ]; then
        if [ "$DRY_RUN" = true ]; then
            echo -e "  ${GRN}+ ${f}${R}  ${DIM}(new file)${R}"
        else
            mkdir -p "$(dirname "$project_file")"
            cp "$template_file" "$project_file"
            # Preserve executable bit
            if [ -x "$template_file" ]; then
                chmod +x "$project_file"
            fi
            echo -e "  ${GRN}+ ${f}${R}  ${DIM}(new file)${R}"
        fi
        added=$((added + 1))
        continue
    fi

    # File exists and differs from template -- check if user modified it
    manifest_cs=$(manifest_read "$f")
    user_modified=false

    if [ "$FIRST_RUN" = true ]; then
        # No manifest: assume modified if it differs from the template
        user_modified=true
    elif [ -z "$manifest_cs" ]; then
        # File not in manifest (new tracked file): assume modified
        user_modified=true
    elif [ "$current_cs" != "$manifest_cs" ]; then
        # Current differs from what we installed last time: user modified
        user_modified=true
    fi

    if [ "$user_modified" = false ]; then
        # User did not modify -- safe to overwrite
        if [ "$DRY_RUN" = true ]; then
            echo -e "  ${CYN}~ ${f}${R}  ${DIM}(updated)${R}"
        else
            cp "$template_file" "$project_file"
            if [ -x "$template_file" ]; then
                chmod +x "$project_file"
            fi
            echo -e "  ${CYN}~ ${f}${R}  ${DIM}(updated)${R}"
        fi
        updated=$((updated + 1))
    else
        # User modified -- prompt
        if [ "$DRY_RUN" = true ]; then
            echo -e "  ${YLW}! ${f}${R}  ${DIM}(modified locally, needs review)${R}"
            skipped=$((skipped + 1))
            continue
        fi

        echo ""
        echo -e "  ${YLW}! ${f}${R}  ${DIM}(you have local changes)${R}"
        echo ""

        while true; do
            echo -e "    ${DIM}[d]iff  [o]verwrite  [k]eep mine  [m]erge (save .orig)${R}"
            echo -ne "    > "
            read -r choice

            case "${choice:-}" in
                d|diff)
                    echo ""
                    diff --color=always "$project_file" "$template_file" || true
                    echo ""
                    ;;
                o|overwrite)
                    cp "$template_file" "$project_file"
                    if [ -x "$template_file" ]; then
                        chmod +x "$project_file"
                    fi
                    echo -e "    ${GRN}Overwritten${R}"
                    updated=$((updated + 1))
                    break
                    ;;
                k|keep)
                    echo -e "    ${DIM}Kept your version${R}"
                    kept=$((kept + 1))
                    break
                    ;;
                m|merge)
                    # Save user's version as .orig, install template version
                    cp "$project_file" "${project_file}.orig"
                    cp "$template_file" "$project_file"
                    if [ -x "$template_file" ]; then
                        chmod +x "$project_file"
                    fi
                    echo -e "    ${GRN}Template installed. Your version saved as ${f}.orig${R}"
                    echo -e "    ${DIM}Merge your changes from the .orig file, then delete it.${R}"
                    updated=$((updated + 1))
                    break
                    ;;
                *)
                    echo -e "    ${DIM}Choose: d, o, k, or m${R}"
                    ;;
            esac
        done
    fi
done

# --- Update manifest ---
if [ "$DRY_RUN" = false ]; then
    manifest_write
fi

# --- Summary ---
echo ""
if [ "$DRY_RUN" = true ]; then
    echo -e "${BLD}Dry run complete${R} (no changes applied)"
else
    echo -e "${BLD}Upgrade complete${R}"
fi

echo -e "  ${GRN}${updated} updated${R}  ${GRN}${added} added${R}  ${unchanged} unchanged  ${YLW}${kept} kept${R}  ${YLW}${skipped} need review${R}"

if [ "$DRY_RUN" = false ] && [ "$FIRST_RUN" = true ]; then
    echo ""
    echo -e "${DIM}Manifest created at ${MANIFEST_FILE}${R}"
    echo -e "${DIM}Future upgrades will detect your changes automatically.${R}"
fi

# --- Migration checks ---
if [ "$DRY_RUN" = false ]; then
    # Detect pre-split CLAUDE.md (still has workflow sections in root file)
    if grep -q '## Agent Controls' CLAUDE.md 2>/dev/null && [ -f .claude/CLAUDE.md ]; then
        echo ""
        echo -e "${YLW}Migration needed: CLAUDE.md still contains projd workflow sections.${R}"
        echo -e "${DIM}The workflow sections have moved to .claude/CLAUDE.md.${R}"
        echo -e "${DIM}Remove everything from '## Agent Controls' onward in your root CLAUDE.md.${R}"
    fi

    # Warn if solo mode and .claude/CLAUDE.md is not gitignored
    if [ -f .projd/mode ] && [ "$(cat .projd/mode)" = "solo" ]; then
        if ! grep -q '\.claude/CLAUDE\.md' .gitignore 2>/dev/null; then
            echo ""
            echo -e "${YLW}Solo mode: .claude/CLAUDE.md is not in .gitignore.${R}"
            echo -e "${DIM}Add it to keep projd infrastructure out of version control.${R}"
        fi
    fi
fi
