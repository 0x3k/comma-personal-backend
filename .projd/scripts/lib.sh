#!/usr/bin/env bash
# lib.sh -- Shared utilities for projd scripts.
# Source with: source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

[ -n "${_PROJD_LIB_LOADED:-}" ] && return 0
_PROJD_LIB_LOADED=1

# --- Bootstrap ---
PROJECT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")/../.." && pwd)"

# --- Colors ---
R='\033[0m'
DIM='\033[2m'
GRN='\033[32m'
YLW='\033[33m'
RED='\033[31m'
CYN='\033[36m'
BLD='\033[1m'

# --- Logging ---
log_ok()   { printf '  [ok]   %s\n' "$*"; }
log_pass() { printf '  [PASS] %s\n' "$*"; }
log_fail() { printf '  [FAIL] %s\n' "$*" >&2; }
log_warn() { printf '  [WARN] %s\n' "$*" >&2; }

# --- Template files ---
load_template_files() {
    TEMPLATE_FILES=()
    local tf="${PROJECT_DIR}/.projd/template-files.txt"
    [ -f "$tf" ] || return 0
    while IFS= read -r line || [ -n "$line" ]; do
        line="${line%%#*}"
        line="${line#"${line%%[![:space:]]*}"}"
        line="${line%"${line##*[![:space:]]}"}"
        [ -n "$line" ] && TEMPLATE_FILES+=("$line")
    done < "$tf"
}

# --- Mode ---
projd_mode() {
    local mode_file="${PROJECT_DIR}/.projd/mode"
    if [ -f "$mode_file" ]; then
        cat "$mode_file"
    else
        echo "team"
    fi
}

projd_settings_file() {
    if [ "$(projd_mode)" = "solo" ]; then
        echo ".claude/settings.local.json"
    else
        echo ".claude/settings.json"
    fi
}

# --- Checksum ---
file_checksum() {
    local file="$1"
    if [ -f "$file" ]; then
        shasum -a 256 "$file" | awk '{print $1}'
    else
        echo "MISSING"
    fi
}
