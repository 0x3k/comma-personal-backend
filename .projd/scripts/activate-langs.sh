#!/usr/bin/env bash
set -euo pipefail

# activate-langs.sh -- Activate language blocks in template files.
#
# Files use markers like:
#   # [typescript]
#   # commented code...
#   # [/typescript]
#
# For selected languages: uncomment the block (remove "# " prefix).
# For other languages: delete the block entirely.
# Consecutive blank lines are collapsed.
#
# If the target file is lefthook.yml, also removes the placeholder command
# and setup comments.
#
# Usage:
#   ./.projd/scripts/activate-langs.sh <file> <lang1> [lang2 ...]

if [ $# -lt 2 ]; then
    echo "Usage: $0 <file> <lang1> [lang2 ...]"
    exit 1
fi

FILE="$1"
shift
LANGS="$*"

if [ ! -f "$FILE" ]; then
    echo "ERROR: File not found: $FILE"
    exit 1
fi

tmpfile=$(mktemp)

awk -v langs="$LANGS" '
BEGIN {
    split(langs, arr, " ")
    for (i in arr) selected[arr[i]] = 1
    skip = 0; uncomment = 0; blank = 0
}
{
    # Opening tag: # [langname]
    if ($0 ~ /# \[[a-z]+\]$/ && $0 !~ /\//) {
        tag = $0
        gsub(/.*\[/, "", tag)
        gsub(/\].*/, "", tag)
        if (tag in selected) {
            uncomment = 1
        } else {
            skip = 1
        }
        next
    }
    # Closing tag: # [/langname]
    if ($0 ~ /# \[\/[a-z]+\]/) {
        skip = 0
        uncomment = 0
        next
    }
    if (skip) next
    if (uncomment) {
        sub(/# /, "")
    }
    # Collapse consecutive blank lines
    if ($0 ~ /^[[:space:]]*$/) {
        if (!blank) { print; blank = 1 }
        next
    }
    blank = 0
    print
}
' "$FILE" > "$tmpfile"

# If this is lefthook.yml, also remove the placeholder command and setup comments
BASENAME="$(basename "$FILE")"
if [ "$BASENAME" = "lefthook.yml" ]; then
    tmpfile2=$(mktemp)
    awk '
    /placeholder:/ { skip = 1; next }
    skip && /run:/ { skip = 0; next }
    skip { next }
    /activated by \.\/setup\.sh/ { next }
    /delete the placeholder/ { next }
    { print }
    ' "$tmpfile" > "$tmpfile2"
    mv "$tmpfile2" "$tmpfile"
fi

# Preserve executable bit
if [ -x "$FILE" ]; then
    chmod +x "$tmpfile"
fi
mv "$tmpfile" "$FILE"
