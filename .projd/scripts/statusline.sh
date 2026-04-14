#!/usr/bin/env bash
set -euo pipefail

# statusline.sh -- Claude Code status line provider.
#
# Receives session JSON on stdin. Reads progress/ and git worktrees
# from disk to show feature progress, agent activity, and token usage.
#
# Output (single line, ANSI colored):
#   Opus 4.6  main  42%  |  3/7  2 wip  |  2 agents  |  15.2k/4.8k tok  +156/-23  12m
#
# Installed via settings.json:
#   "statusLine": { "type": "command", "command": ".projd/scripts/statusline.sh" }

input=$(cat)

# --- Helpers ---
# Format a token count for display: 0-999 as-is, 1k-999k with one decimal, 1M+ with one decimal.
fmt_tok() {
    local n="${1:-0}"
    n=${n%%.*}
    if [ "$n" -ge 1000000 ]; then
        awk "BEGIN { printf \"%.1fM\", $n / 1000000 }"
    elif [ "$n" -ge 1000 ]; then
        awk "BEGIN { printf \"%.1fk\", $n / 1000 }"
    else
        echo "$n"
    fi
}

# --- Session data from stdin ---
model=$(echo "$input" | jq -r '.model.display_name // "?"' 2>/dev/null)
ctx_pct=$(echo "$input" | jq -r '.context_window.used_percentage // 0' 2>/dev/null)
ctx_pct=${ctx_pct%%.*}  # truncate to integer

total_in=$(echo "$input" | jq -r '.context_window.total_input_tokens // 0' 2>/dev/null)
total_out=$(echo "$input" | jq -r '.context_window.total_output_tokens // 0' 2>/dev/null)

duration_ms=$(echo "$input" | jq -r '.cost.total_duration_ms // 0' 2>/dev/null)
lines_added=$(echo "$input" | jq -r '.cost.total_lines_added // 0' 2>/dev/null)
lines_removed=$(echo "$input" | jq -r '.cost.total_lines_removed // 0' 2>/dev/null)

# Rate limits (Pro/Max only, may be absent)
rl5_pct=$(echo "$input" | jq -r '.rate_limits.five_hour.used_percentage // 0' 2>/dev/null)
rl5_pct=${rl5_pct%%.*}

# --- Git branch ---
branch=$(git branch --show-current 2>/dev/null || echo "?")

# --- Feature progress ---
done_n=0
wip_n=0
pending_n=0
total=0

shopt -s nullglob
for f in .projd/progress/*.json; do
    [ -f "$f" ] || continue
    total=$((total + 1))
    s=$(jq -r '.status // "pending"' "$f" 2>/dev/null)
    case "$s" in
        complete)    done_n=$((done_n + 1)) ;;
        in_progress) wip_n=$((wip_n + 1)) ;;
        *)           pending_n=$((pending_n + 1)) ;;
    esac
done

# --- Active worktrees (exclude main) ---
wt_n=0
if git rev-parse --is-inside-work-tree &>/dev/null; then
    wt_n=$(( $(git worktree list 2>/dev/null | wc -l | tr -d ' ') - 1 ))
    [ "$wt_n" -lt 0 ] && wt_n=0
fi

# --- Colors (from lib.sh if available, inline fallback for resilience) ---
_lib="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"
if [ -f "$_lib" ]; then
    source "$_lib"
else
    R='\033[0m'; DIM='\033[2m'; GRN='\033[32m'; YLW='\033[33m'
    RED='\033[31m'; CYN='\033[36m'; BLD='\033[1m'
fi

# Context color by usage
if [ "$ctx_pct" -ge 90 ]; then CC="$RED"
elif [ "$ctx_pct" -ge 70 ]; then CC="$YLW"
else CC="$GRN"; fi

# --- Build output ---
out=""

# Model (short)
out+="${DIM}${model}${R}"

# Branch
out+="  ${CYN}${branch}${R}"

# Context %
out+="  ${CC}${ctx_pct}%${R}"

# Features
if [ "$total" -gt 0 ]; then
    out+="  ${DIM}|${R}  "
    out+="${GRN}${done_n}${R}${DIM}/${R}${total}"
    if [ "$wip_n" -gt 0 ]; then
        out+=" ${YLW}${wip_n} wip${R}"
    fi
fi

# Agents
if [ "$wt_n" -gt 0 ]; then
    suffix="s"
    [ "$wt_n" -eq 1 ] && suffix=""
    out+="  ${DIM}|${R}  ${BLD}${wt_n} agent${suffix}${R}"
fi

# Token usage (cumulative session totals across all agents)
total_in=${total_in%%.*}
total_out=${total_out%%.*}
if [ "$total_in" -gt 0 ] || [ "$total_out" -gt 0 ]; then
    in_fmt=$(fmt_tok "$total_in")
    out_fmt=$(fmt_tok "$total_out")
    out+="  ${DIM}|${R}  ${DIM}${in_fmt}/${out_fmt} tok${R}"
fi

# Lines changed
lines_added=${lines_added%%.*}
lines_removed=${lines_removed%%.*}
if [ "$lines_added" -gt 0 ] || [ "$lines_removed" -gt 0 ]; then
    out+="  ${GRN}+${lines_added}${R}${DIM}/${R}${RED}-${lines_removed}${R}"
fi

# Session duration
duration_ms=${duration_ms%%.*}
if [ "$duration_ms" -gt 0 ]; then
    total_sec=$((duration_ms / 1000))
    if [ "$total_sec" -ge 3600 ]; then
        h=$((total_sec / 3600))
        m=$(( (total_sec % 3600) / 60 ))
        dur="${h}h${m}m"
    elif [ "$total_sec" -ge 60 ]; then
        dur="$((total_sec / 60))m"
    else
        dur="${total_sec}s"
    fi
    out+="  ${DIM}${dur}${R}"
fi

# Rate limit warning (show only when getting close)
if [ "$rl5_pct" -ge 90 ]; then
    out+="  ${RED}${BLD}rate ${rl5_pct}%${R}"
elif [ "$rl5_pct" -ge 70 ]; then
    out+="  ${YLW}rate ${rl5_pct}%${R}"
fi

printf '%b\n' "$out"
