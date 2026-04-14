#!/usr/bin/env bash
set -euo pipefail

# monitor.sh -- Interactive live dashboard for parallel agent sessions.
#
# Shows feature status, dispatch waves, and per-feature token usage
# (input/output) from Claude Code session logs. Spinner in the header
# confirms the dashboard is alive.
#
# Navigate features with arrow keys or j/k, act on the selected feature
# with single-key commands. Auto-refreshes in the background.
#
# Usage:
#   ./.projd/scripts/monitor.sh            # interactive dashboard (~1s tick, 5s full refresh)
#   ./.projd/scripts/monitor.sh --watch    # same as above (kept for compat)
#   ./.projd/scripts/monitor.sh --watch 3  # custom full refresh interval
#   ./.projd/scripts/monitor.sh --once     # print snapshot and exit (non-interactive)
#
# Navigation:
#   Up/k      Move selection up
#   Down/j    Move selection down
#   d         Feature details (JSON, commits, diff stats)
#   l         Recent commits on the feature branch
#   p         Open the feature's PR in browser
#   r         Reset feature to pending
#   c         Mark feature complete
#   x         Remove the feature's worktree
#   m         Merge the feature's PR
#   /         Refresh now
#   q         Quit

# Re-exec under zsh for sub-second read -t support (macOS ships bash 3.2)
if [ -z "${ZSH_VERSION:-}" ] && [ "${BASH_VERSINFO[0]:-0}" -lt 4 ]; then
    if command -v zsh &>/dev/null; then
        exec zsh "$0" "$@"
    fi
fi

# Compat shims: zsh uses -k where bash uses -n for character reads,
# and setopt for glob options. Abstract the differences here.
if [ -n "${ZSH_VERSION:-}" ]; then
    setopt nullglob KSH_ARRAYS BASH_REMATCH NO_NOMATCH TYPESET_SILENT 2>/dev/null
    _read_key() { read -rsk1 "$@"; }
else
    _read_key() { read -rsn1 "$@"; }
fi


source "$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)/lib.sh"
cd "$PROJECT_DIR"

INTERVAL=5
ONCE=false

case "${1:-}" in
    --once)    ONCE=true ;;
    --watch)
        if [ -n "${2:-}" ] && [ "$2" -gt 0 ] 2>/dev/null; then
            INTERVAL="$2"
        fi
        ;;
    --help|-h)
        sed -n '3,/^$/p' "$0" | sed 's/^# \?//'
        exit 0
        ;;
esac

# Render tick: 250ms when fractional read -t works, 1s fallback
if [ -n "${ZSH_VERSION:-}" ] || [ "${BASH_VERSINFO[0]:-0}" -ge 4 ]; then
    TICK=0.25
else
    TICK=1
fi
FEATURES_CADENCE=4  # reload feature JSONs every N ticks (~1s at 250ms tick)

# Extra colors not in lib.sh
BLU='\033[34m'
MAG='\033[35m'
INV='\033[7m'

# Wave colors -- cycle through for visual grouping
WAVE_COLORS=("$CYN" "$MAG" "$BLU" "$YLW" "$GRN")

# --- Project info (parsed from CLAUDE.md) ---
PROJECT_NAME=""
PROJECT_DESC=""
if [ -f "$PROJECT_DIR/CLAUDE.md" ]; then
    PROJECT_NAME=$(sed -n 's/^\*\*Name\*\*:[[:space:]]*//p' "$PROJECT_DIR/CLAUDE.md" | head -1)
    PROJECT_DESC=$(sed -n 's/^\*\*Purpose\*\*:[[:space:]]*//p' "$PROJECT_DIR/CLAUDE.md" | head -1)
    # Strip HTML comment placeholders
    [[ "$PROJECT_DESC" == "<!--"* ]] && PROJECT_DESC=""
fi

# --- State ---
SELECTED=0
FEATURE_IDS=()
FEATURE_COUNT=0
OVERLAY=""
CONFIRM_ACTION=""
CONFIRM_ID=""
STATUS_MSG=""
SPIN_FRAME=0
SPINNER=('⠋' '⠙' '⠹' '⠸' '⠼' '⠴' '⠦' '⠧' '⠇' '⠏')

# --- Terminal setup ---
if [ "$ONCE" = false ]; then
    ORIG_STTY=$(stty -g 2>/dev/null || echo "sane")

    cleanup() {
        stty "$ORIG_STTY" 2>/dev/null
        printf '\033[?25h'   # show cursor
        printf '\033[?1049l' # restore main screen
    }

    trap cleanup EXIT INT TERM
    printf '\033[?1049h'  # alternate screen
    printf '\033[?25l'    # hide cursor
fi

# --- Helpers ---

feature_file() {
    local id="$1"
    echo ".projd/progress/${id}.json"
}

feature_field() {
    local file="$1" field="$2"
    jq -r ".${field} // empty" "$file" 2>/dev/null
}

worktree_for_branch() {
    local branch="$1"
    git worktree list --porcelain 2>/dev/null | awk -v b="$branch" '
        /^worktree / { wt=$2 }
        /^branch /   { if ($2 == "refs/heads/" b) print wt }
    '
}

# Resolve the branch for a feature: check feature file, then try prefix+id.
resolve_branch() {
    local id="$1"
    local f
    f=$(feature_file "$id")
    local branch=""
    [ -f "$f" ] && branch=$(feature_field "$f" "branch")
    if [ -z "$branch" ]; then
        # Try to infer from agent.json prefix
        local prefix
        prefix=$(jq -r '.git.branch_prefix // "agent/"' .projd/agent.json 2>/dev/null)
        local candidate="${prefix}${id}"
        if git rev-parse --verify "$candidate" &>/dev/null; then
            branch="$candidate"
        fi
    fi
    echo "$branch"
}

pr_for_branch() {
    local branch="$1"
    if command -v gh &>/dev/null; then
        gh pr list --head "$branch" --state open --json number --jq '.[0].number' 2>/dev/null || true
    fi
}

# Sets _FMT to formatted token count (no subshell, no awk)
format_tokens() {
    local n="$1"
    if [ "$n" -ge 1000000 ]; then
        _FMT="$((n / 1000000)).$((n % 1000000 / 100000))M"
    elif [ "$n" -ge 1000 ]; then
        _FMT="$((n / 1000)).$((n % 1000 / 100))k"
    elif [ "$n" -gt 0 ]; then
        _FMT="$n"
    else
        _FMT="--"
    fi
}

# Sets _FMT to formatted duration (no subshell)
format_duration() {
    local secs="$1"
    if [ "$secs" -ge 86400 ]; then
        _FMT="$((secs / 86400))d$((secs % 86400 / 3600))h"
    elif [ "$secs" -ge 3600 ]; then
        _FMT="$((secs / 3600))h$((secs % 3600 / 60))m"
    elif [ "$secs" -ge 60 ]; then
        _FMT="$((secs / 60))m"
    else
        _FMT="${secs}s"
    fi
}

# --- Cached data arrays (populated by load_features / load_slow_data) ---
# Feature data -- parallel arrays indexed by position
F_STATUS=()    # "pending" | "in_progress" | "complete"
F_BRANCH=()
F_NOTES=()
F_BLOCKED=()
F_WAVE=()      # dispatch wave number (1 = no blockers, 2 = blocked by wave 1, ...)
F_AC=()        # acceptance criteria count per feature

# Slow data -- refreshed on interval only
CACHED_WT=""       # pre-formatted worktree lines
CACHED_WT_COUNT=0
CACHED_PR=""       # pre-formatted PR lines
CACHED_PR_COUNT=0

# Agent activity -- parallel array indexed same as FEATURE_IDS
F_AGENT=()   # populated by load_slow_data

# Token usage -- parallel arrays indexed same as FEATURE_IDS
F_TOKENS_IN=()    # input + cache_creation tokens per feature
F_TOKENS_OUT=()   # output tokens per feature
CACHED_TOKEN_DATA=""  # pre-computed branch->token data from JSONL files

# Elapsed time and liveness -- parallel arrays indexed same as FEATURE_IDS
F_ELAPSED=()            # formatted elapsed time for WIP features
F_ALIVE=()              # "live", "stale", or ""
CACHED_JSONL_MTIMES=""  # branch<TAB>mtime for stale detection

# Merged PR tracking
CACHED_MERGED_COUNT=0
CACHED_LAST_MERGE=""    # e.g. "12m ago"

# Process resource stats (refreshed with slow data)
CACHED_SELF_CPU="--"
CACHED_SELF_MEM="--"
CACHED_LOAD="--"

# --- Load features into arrays (sorted: wip, ready, blocked, complete) ---
# Reads each JSON file once with a single jq call per file.
load_features() {
    local wip_ids=() ready_ids=() block_ids=() done_ids=()
    local wip_s=()   ready_s=()   block_s=()   done_s=()
    local wip_br=()  ready_br=()  block_br=()  done_br=()
    local wip_no=()  ready_no=()  block_no=()  done_no=()
    local wip_bl=()  ready_bl=()  block_bl=()  done_bl=()
    local wip_ac=()  ready_ac=()  block_ac=()  done_ac=()

    [ -z "${ZSH_VERSION:-}" ] && shopt -s nullglob
    for f in .projd/progress/*.json; do
        [ -f "$f" ] || continue
        # Single jq call: emit tab-separated fields
        local row
        row=$(jq -r '[
            (.id // ""),
            (.status // "pending"),
            (.branch // ""),
            (.notes // ""),
            ((.blocked_by // []) | join(", ")),
            ((.acceptance_criteria // []) | length | tostring)
        ] | join("\u001f")' "$f" 2>/dev/null) || continue

        IFS=$'\x1f' read -r _id _st _br _no _bl _ac <<< "$row"
        [ -z "$_id" ] && continue

        [ -z "$_ac" ] && _ac=1

        case "$_st" in
            in_progress)
                wip_ids+=("$_id"); wip_s+=("$_st"); wip_br+=("$_br"); wip_no+=("$_no"); wip_bl+=("$_bl"); wip_ac+=("$_ac") ;;
            complete)
                done_ids+=("$_id"); done_s+=("$_st"); done_br+=("$_br"); done_no+=("$_no"); done_bl+=("$_bl"); done_ac+=("$_ac") ;;
            *)
                if [ -n "$_bl" ]; then
                    block_ids+=("$_id"); block_s+=("$_st"); block_br+=("$_br"); block_no+=("$_no"); block_bl+=("$_bl"); block_ac+=("$_ac")
                else
                    ready_ids+=("$_id"); ready_s+=("$_st"); ready_br+=("$_br"); ready_no+=("$_no"); ready_bl+=("$_bl"); ready_ac+=("$_ac")
                fi
                ;;
        esac
    done

    # Cross-reference active worktrees: if a worktree branch matches
    # {prefix}{feature-id}, that feature is in-progress even if the main
    # repo's progress file hasn't been updated yet (the agent writes to
    # its worktree copy, not the main repo).
    local prefix
    prefix=$(jq -r '.git.branch_prefix // "agent/"' .projd/agent.json 2>/dev/null)
    local wt_branches=""
    wt_branches=$(git worktree list --porcelain 2>/dev/null | awk '/^branch / { sub("refs/heads/", "", $2); print $2 }')

    # Build lookup of active worktree branches
    local _all_ids=()
    [ ${#wip_ids[@]} -gt 0 ] && _all_ids+=("${wip_ids[@]}")
    [ ${#ready_ids[@]} -gt 0 ] && _all_ids+=("${ready_ids[@]}")
    [ ${#block_ids[@]} -gt 0 ] && _all_ids+=("${block_ids[@]}")
    [ ${#done_ids[@]} -gt 0 ] && _all_ids+=("${done_ids[@]}")

    if [ -n "$wt_branches" ]; then
        while IFS= read -r wt_br; do
            [ -z "$wt_br" ] && continue
            # Extract feature id from branch: strip prefix
            local fid="${wt_br#"$prefix"}"
            [ "$fid" = "$wt_br" ] && continue  # didn't match prefix

            # Find this feature in the non-wip buckets and move it to wip
            local found=false
            for ((i=0; i<${#ready_ids[@]}; i++)); do
                if [ "${ready_ids[$i]}" = "$fid" ]; then
                    wip_ids+=("$fid"); wip_s+=("in_progress"); wip_br+=("$wt_br"); wip_no+=("${ready_no[$i]}"); wip_bl+=("${ready_bl[$i]}"); wip_ac+=("${ready_ac[$i]}")
                    # Remove element and reindex (splice around i)
                    ready_ids=("${ready_ids[@]:0:$i}" "${ready_ids[@]:$((i+1))}")
                    ready_s=("${ready_s[@]:0:$i}" "${ready_s[@]:$((i+1))}")
                    ready_br=("${ready_br[@]:0:$i}" "${ready_br[@]:$((i+1))}")
                    ready_no=("${ready_no[@]:0:$i}" "${ready_no[@]:$((i+1))}")
                    ready_bl=("${ready_bl[@]:0:$i}" "${ready_bl[@]:$((i+1))}")
                    ready_ac=("${ready_ac[@]:0:$i}" "${ready_ac[@]:$((i+1))}")
                    found=true; break
                fi
            done
            if [ "$found" = false ]; then
                for ((i=0; i<${#block_ids[@]}; i++)); do
                    if [ "${block_ids[$i]}" = "$fid" ]; then
                        wip_ids+=("$fid"); wip_s+=("in_progress"); wip_br+=("$wt_br"); wip_no+=("${block_no[$i]}"); wip_bl+=("${block_bl[$i]}"); wip_ac+=("${block_ac[$i]}")
                        # Remove element and reindex (splice around i)
                        block_ids=("${block_ids[@]:0:$i}" "${block_ids[@]:$((i+1))}")
                        block_s=("${block_s[@]:0:$i}" "${block_s[@]:$((i+1))}")
                        block_br=("${block_br[@]:0:$i}" "${block_br[@]:$((i+1))}")
                        block_no=("${block_no[@]:0:$i}" "${block_no[@]:$((i+1))}")
                        block_bl=("${block_bl[@]:0:$i}" "${block_bl[@]:$((i+1))}")
                        block_ac=("${block_ac[@]:0:$i}" "${block_ac[@]:$((i+1))}")
                        found=true; break
                    fi
                done
            fi
            # Also patch existing wip entries that have no branch
            if [ "$found" = false ]; then
                for ((i=0; i<${#wip_ids[@]}; i++)); do
                    if [ "${wip_ids[$i]}" = "$fid" ] && [ -z "${wip_br[$i]}" ]; then
                        wip_br[$i]="$wt_br"
                        break
                    fi
                done
            fi
        done <<< "$wt_branches"
    fi

    FEATURE_IDS=(); F_STATUS=(); F_BRANCH=(); F_NOTES=(); F_BLOCKED=(); F_AC=()
    [ ${#wip_ids[@]} -gt 0 ] && { FEATURE_IDS+=("${wip_ids[@]}"); F_STATUS+=("${wip_s[@]}"); F_BRANCH+=("${wip_br[@]}"); F_NOTES+=("${wip_no[@]}"); F_BLOCKED+=("${wip_bl[@]}"); F_AC+=("${wip_ac[@]}"); }
    [ ${#ready_ids[@]} -gt 0 ] && { FEATURE_IDS+=("${ready_ids[@]}"); F_STATUS+=("${ready_s[@]}"); F_BRANCH+=("${ready_br[@]}"); F_NOTES+=("${ready_no[@]}"); F_BLOCKED+=("${ready_bl[@]}"); F_AC+=("${ready_ac[@]}"); }
    [ ${#block_ids[@]} -gt 0 ] && { FEATURE_IDS+=("${block_ids[@]}"); F_STATUS+=("${block_s[@]}"); F_BRANCH+=("${block_br[@]}"); F_NOTES+=("${block_no[@]}"); F_BLOCKED+=("${block_bl[@]}"); F_AC+=("${block_ac[@]}"); }
    [ ${#done_ids[@]} -gt 0 ] && { FEATURE_IDS+=("${done_ids[@]}"); F_STATUS+=("${done_s[@]}"); F_BRANCH+=("${done_br[@]}"); F_NOTES+=("${done_no[@]}"); F_BLOCKED+=("${done_bl[@]}"); F_AC+=("${done_ac[@]}"); }

    FEATURE_COUNT=${#FEATURE_IDS[@]}
    if [ "$SELECTED" -ge "$FEATURE_COUNT" ] && [ "$FEATURE_COUNT" -gt 0 ]; then
        SELECTED=$((FEATURE_COUNT - 1))
    fi

    # Compute dispatch waves from the dependency graph.
    # Wave 1 = no blockers (or all blockers complete), wave 2 = blocked only by
    # wave-1 features, etc.  Complete features get wave 0 (already done).
    # Uses parallel arrays (no associative arrays -- bash 3.2 compat).
    F_WAVE=()
    local _waves=()
    for ((i=0; i<FEATURE_COUNT; i++)); do
        _waves+=(0)
    done

    # Helper: look up wave for a feature id by scanning FEATURE_IDS
    _wave_of_id() {
        local target="$1"
        for ((wi=0; wi<FEATURE_COUNT; wi++)); do
            if [ "${FEATURE_IDS[$wi]}" = "$target" ]; then
                echo "${_waves[$wi]}"
                return
            fi
        done
        echo 0
    }
    _status_of_id() {
        local target="$1"
        for ((wi=0; wi<FEATURE_COUNT; wi++)); do
            if [ "${FEATURE_IDS[$wi]}" = "$target" ]; then
                echo "${F_STATUS[$wi]}"
                return
            fi
        done
        echo ""
    }

    # Iteratively assign waves until stable.
    # Two-phase per iteration: first collect eligible indices, then assign.
    # This prevents a feature from being assigned the same wave as its blocker
    # within a single pass.
    local changed=true wave_num=1
    while [ "$changed" = true ] && [ "$wave_num" -le "$FEATURE_COUNT" ]; do
        changed=false
        local _eligible=()
        for ((i=0; i<FEATURE_COUNT; i++)); do
            [ "${_waves[$i]}" -gt 0 ] && continue
            [ "${F_STATUS[$i]}" = "complete" ] && continue

            local bl="${F_BLOCKED[$i]}"
            if [ -z "$bl" ]; then
                _eligible+=("$i")
                continue
            fi

            # Check if all blockers were assigned in a previous wave (or are complete)
            local all_resolved=true
            local _old_ifs="${IFS}"; IFS=', '; set -- $bl; IFS="${_old_ifs}"
            for dep in "$@"; do
                dep="${dep// /}"
                [ -z "$dep" ] && continue
                local dep_status
                dep_status=$(_status_of_id "$dep")
                if [ "$dep_status" = "complete" ]; then
                    continue
                fi
                local dep_wave
                dep_wave=$(_wave_of_id "$dep")
                if [ "$dep_wave" -eq 0 ] || [ "$dep_wave" -ge "$wave_num" ]; then
                    all_resolved=false
                    break
                fi
            done

            if [ "$all_resolved" = true ]; then
                _eligible+=("$i")
            fi
        done

        # Assign wave to all eligible features at once
        if [ ${#_eligible[@]} -gt 0 ]; then
            for ei in "${_eligible[@]}"; do
                _waves[$ei]=$wave_num
                changed=true
            done
        fi
        wave_num=$((wave_num + 1))
    done

    for ((i=0; i<FEATURE_COUNT; i++)); do
        F_WAVE+=("${_waves[$i]}")
    done
}

# --- Load slow data (worktrees, PRs) -- called on timed refresh only ---
load_slow_data() {
    # Worktrees
    CACHED_WT=""
    CACHED_WT_COUNT=0
    local wt_raw
    wt_raw=$(git worktree list 2>/dev/null | tail -n +2 || true)
    if [ -n "$wt_raw" ]; then
        CACHED_WT_COUNT=$(echo "$wt_raw" | wc -l | tr -d ' ')
        CACHED_WT=""
        while IFS= read -r line; do
            local wt_path wt_branch
            wt_path=$(echo "$line" | awk '{print $1}')
            wt_branch=$(echo "$line" | grep -o '\[.*\]' | tr -d '[]')
            CACHED_WT+="$(printf "    ${CYN}%-25s${R}  ${DIM}%s${R}" "$wt_branch" "$wt_path")\n"
        done <<< "$wt_raw"
    fi

    # Agent activity for in-progress features (parallel array)
    F_AGENT=()
    for ((i=0; i<FEATURE_COUNT; i++)); do
        local _st="${F_STATUS[$i]}"
        local _br="${F_BRANCH[$i]}"

        if [ "$_st" != "in_progress" ] || [ -z "$_br" ]; then
            F_AGENT+=("")
            continue
        fi

        local info_parts=""

        # Worktree path for this branch
        local _wt
        _wt=$(worktree_for_branch "$_br")
        if [ -n "$_wt" ]; then
            info_parts+="worktree active"
        fi

        # Commit count and last commit message on this branch (vs main)
        if git rev-parse --verify "$_br" &>/dev/null; then
            local base="main"
            git rev-parse --verify main &>/dev/null || base=$(git rev-parse --abbrev-ref HEAD 2>/dev/null)
            local commits
            commits=$(git rev-list --count "${base}..${_br}" 2>/dev/null || echo 0)
            if [ "$commits" -gt 0 ]; then
                local last_msg
                last_msg=$(git log "$_br" -1 --format='%s' 2>/dev/null)
                [ ${#last_msg} -gt 40 ] && last_msg="${last_msg:0:37}..."
                local diff_stat
                diff_stat=$(git diff --shortstat "${base}...${_br}" 2>/dev/null | sed 's/ file.*/f/' | sed 's/.*changed, *//' | sed 's/ insertion.*/+/' | sed 's/ deletion.*/-/' | tr -d '\n' | tr ',' ' ')
                [ -n "$info_parts" ] && info_parts+="  "
                info_parts+="${commits} commits"
                [ -n "$diff_stat" ] && info_parts+="  ${diff_stat}"
                info_parts+="  last: ${last_msg}"
            fi
        fi

        F_AGENT+=("$info_parts")
    done

    # Elapsed time for WIP features
    F_ELAPSED=()
    local _now
    _now=$(date +%s)
    for ((i=0; i<FEATURE_COUNT; i++)); do
        local _est="${F_STATUS[$i]}"
        local _ebr="${F_BRANCH[$i]}"
        if [ "$_est" != "in_progress" ] || [ -z "$_ebr" ]; then
            F_ELAPSED+=("")
            continue
        fi

        local start_ts=""
        # Try first commit time on the branch
        if git rev-parse --verify "$_ebr" &>/dev/null; then
            local base="main"
            git rev-parse --verify main &>/dev/null || base=$(git rev-parse --abbrev-ref HEAD 2>/dev/null)
            start_ts=$(git log "${base}..${_ebr}" --reverse --format='%ct' 2>/dev/null | head -1)
        fi
        # Fallback: worktree directory mtime
        if [ -z "$start_ts" ]; then
            local _ewt
            _ewt=$(worktree_for_branch "$_ebr")
            if [ -n "$_ewt" ] && [ -d "$_ewt" ]; then
                start_ts=$(stat -f '%m' "$_ewt" 2>/dev/null || true)
            fi
        fi

        if [ -n "$start_ts" ] && [ "$start_ts" -gt 0 ] 2>/dev/null; then
            local elapsed_secs=$((_now - start_ts))
            [ "$elapsed_secs" -lt 0 ] && elapsed_secs=0
            format_duration "$elapsed_secs"
            F_ELAPSED+=("$_FMT")
        else
            F_ELAPSED+=("--")
        fi
    done

    # Stale detection for WIP features
    F_ALIVE=()
    CACHED_JSONL_MTIMES=""

    # Pre-scan Claude sessions: collect pid<TAB>cwd for running sessions
    local _session_pids=""
    local claude_sessions_dir="$HOME/.claude/sessions"
    if [ -d "$claude_sessions_dir" ]; then
        [ -z "${ZSH_VERSION:-}" ] && shopt -s nullglob
        for sf in "$claude_sessions_dir"/*.json; do
            local _spid _scwd
            _spid=$(jq -r '.pid // empty' "$sf" 2>/dev/null) || continue
            _scwd=$(jq -r '.cwd // empty' "$sf" 2>/dev/null) || continue
            [ -n "$_spid" ] && [ -n "$_scwd" ] && _session_pids+="${_spid}\t${_scwd}\n"
        done
    fi

    # Pre-scan JSONL files for branch -> mtime mapping
    local _jsonl_dir="$HOME/.claude"
    local _proj_slug
    _proj_slug=$(echo "$PROJECT_DIR" | sed 's|/|-|g')
    local _jsonl_path="$_jsonl_dir/projects/$_proj_slug"
    if [ -d "$_jsonl_path" ]; then
        [ -z "${ZSH_VERSION:-}" ] && shopt -s nullglob
        for jf in "$_jsonl_path"/*.jsonl; do
            local _jmtime _jbranch
            _jmtime=$(stat -f '%m' "$jf" 2>/dev/null || true)
            [ -z "$_jmtime" ] && continue
            _jbranch=$(tail -1 "$jf" 2>/dev/null | jq -r '.gitBranch // empty' 2>/dev/null || true)
            [ -n "$_jbranch" ] && CACHED_JSONL_MTIMES+="${_jbranch}\t${_jmtime}\n"
        done
    fi

    local _stale_threshold=300  # 5 minutes in seconds

    for ((i=0; i<FEATURE_COUNT; i++)); do
        local _ast="${F_STATUS[$i]}"
        local _abr="${F_BRANCH[$i]}"
        if [ "$_ast" != "in_progress" ] || [ -z "$_abr" ]; then
            F_ALIVE+=("")
            continue
        fi

        local _is_live=false

        # Check if a Claude session is running in this feature's worktree
        local _awt
        _awt=$(worktree_for_branch "$_abr")
        if [ -n "$_awt" ] && [ -n "$_session_pids" ]; then
            while IFS=$'\t' read -r _cpid _ccwd; do
                [ -z "$_cpid" ] && continue
                if [ "$_ccwd" = "$_awt" ] && kill -0 "$_cpid" 2>/dev/null; then
                    _is_live=true
                    break
                fi
            done < <(printf '%b' "$_session_pids")
        fi

        # Also check JSONL mtime for this branch
        if [ "$_is_live" = false ] && [ -n "$CACHED_JSONL_MTIMES" ]; then
            local _best_mtime=0
            while IFS=$'\t' read -r _mbranch _mtime; do
                if [ "$_mbranch" = "$_abr" ] && [ "${_mtime:-0}" -gt "$_best_mtime" ] 2>/dev/null; then
                    _best_mtime="$_mtime"
                fi
            done < <(printf '%b' "$CACHED_JSONL_MTIMES")
            if [ "$_best_mtime" -gt 0 ] && [ $((_now - _best_mtime)) -lt "$_stale_threshold" ]; then
                _is_live=true
            fi
        fi

        if [ "$_is_live" = true ]; then
            F_ALIVE+=("live")
        else
            F_ALIVE+=("stale")
        fi
    done

    # Token usage from Claude Code session JSONL files
    F_TOKENS_IN=()
    F_TOKENS_OUT=()
    local claude_dir="$HOME/.claude"
    local project_slug
    project_slug=$(echo "$PROJECT_DIR" | sed 's|/|-|g')
    local jsonl_dir="$claude_dir/projects/$project_slug"

    if [ -d "$jsonl_dir" ]; then
        # Single pass: extract branch + token sums from all JSONL files.
        # grep for "usage" lines first (fast filter), then jq parses only those.
        CACHED_TOKEN_DATA=$(grep -h '"usage"' "$jsonl_dir"/*.jsonl 2>/dev/null | \
            jq -r 'select(.message.usage != null and .gitBranch != null) |
                .gitBranch + "\t" +
                ((.message.usage.input_tokens // 0) + (.message.usage.cache_creation_input_tokens // 0) + (.message.usage.cache_read_input_tokens // 0) | tostring) + "\t" +
                (.message.usage.output_tokens // 0 | tostring)' 2>/dev/null || true)
    else
        CACHED_TOKEN_DATA=""
    fi

    for ((i=0; i<FEATURE_COUNT; i++)); do
        local branch="${F_BRANCH[$i]}"
        if [ -z "$branch" ] || [ -z "$CACHED_TOKEN_DATA" ]; then
            F_TOKENS_IN+=(0); F_TOKENS_OUT+=(0)
            continue
        fi

        local sums
        sums=$(echo "$CACHED_TOKEN_DATA" | awk -F'\t' -v b="$branch" \
            '$1 == b {in_t += $2; out_t += $3} END {printf "%d %d", in_t+0, out_t+0}')
        local tin tout
        read -r tin tout <<< "$sums"
        F_TOKENS_IN+=("${tin:-0}")
        F_TOKENS_OUT+=("${tout:-0}")
    done

    # PRs (skip if gh not available)
    CACHED_PR=""
    CACHED_PR_COUNT=0
    if command -v gh &>/dev/null && gh auth status &>/dev/null 2>&1; then
        local prefix_val
        prefix_val=$(jq -r '.git.branch_prefix // "agent/"' .projd/agent.json 2>/dev/null)
        local prs
        prs=$(gh pr list --state open --json number,title,headRefName --jq ".[] | select(.headRefName | startswith(\"$prefix_val\"))" 2>/dev/null || true)
        if [ -n "$prs" ]; then
            CACHED_PR_COUNT=$(echo "$prs" | jq -s 'length' 2>/dev/null || echo 0)
            CACHED_PR=""
            while IFS= read -r pr_line; do
                CACHED_PR+="$(printf "  ${GRN}  %s${R}" "$pr_line")\n"
            done < <(echo "$prs" | jq -r '"#\(.number)  \(.title)"' 2>/dev/null)
        fi

        # Merged PRs
        CACHED_MERGED_COUNT=0
        CACHED_LAST_MERGE=""
        local merged_prs
        merged_prs=$(gh pr list --state merged --json number,headRefName,mergedAt \
            --jq ".[] | select(.headRefName | startswith(\"$prefix_val\"))" 2>/dev/null || true)
        if [ -n "$merged_prs" ]; then
            CACHED_MERGED_COUNT=$(echo "$merged_prs" | jq -s 'length' 2>/dev/null || echo 0)
            # Find the most recent merge time
            local latest_merge_iso
            latest_merge_iso=$(echo "$merged_prs" | jq -rs '[.[].mergedAt] | sort | last // empty' 2>/dev/null || true)
            if [ -n "$latest_merge_iso" ]; then
                # Strip trailing Z and fractional seconds for macOS date parsing
                local clean_iso="${latest_merge_iso%%.*}"
                clean_iso="${clean_iso%Z}"
                local merge_epoch
                merge_epoch=$(date -j -f '%Y-%m-%dT%H:%M:%S' "$clean_iso" '+%s' 2>/dev/null || true)
                if [ -n "$merge_epoch" ] && [ "$merge_epoch" -gt 0 ] 2>/dev/null; then
                    local merge_ago=$((_now - merge_epoch))
                    [ "$merge_ago" -lt 0 ] && merge_ago=0
                    format_duration "$merge_ago"
                    CACHED_LAST_MERGE="${_FMT} ago"
                fi
            fi
        fi
    fi

    # Process resource stats (lightweight)
    local _ps_out
    _ps_out=$(ps -o %cpu=,rss= -p $$ 2>/dev/null || true)
    if [ -n "$_ps_out" ]; then
        local _cpu _rss
        read -r _cpu _rss <<< "$_ps_out"
        CACHED_SELF_CPU="${_cpu}%"
        if [ "${_rss:-0}" -ge 1024 ] 2>/dev/null; then
            CACHED_SELF_MEM="$((_rss / 1024))M"
        else
            CACHED_SELF_MEM="${_rss}K"
        fi
    fi
    CACHED_LOAD=$(uptime 2>/dev/null | awk -F'load averages?: ' '{print $2}' | awk '{print $1}' | tr -d ',')
    [ -z "$CACHED_LOAD" ] && CACHED_LOAD="--"
    true
}

# --- Render (pure output from cached data -- no external calls) ---

render() {
    local lines=""

    # Header with spinner animation
    local spin_char="${SPINNER[$((SPIN_FRAME % ${#SPINNER[@]}))]}"
    SPIN_FRAME=$((SPIN_FRAME + 1))

    local header="${CYN}${spin_char}${R} ${BLD}projd monitor${R}"
    if [ -n "$PROJECT_NAME" ]; then
        header+="  ${BLD}${CYN}${PROJECT_NAME}${R}"
    fi
    if [ -n "$PROJECT_DESC" ]; then
        header+="  ${DIM}-- ${PROJECT_DESC}${R}"
    fi
    local _ts
    _ts=$(date '+%H:%M:%S')
    header+="  ${DIM}${_ts}${R}"
    header+="  ${DIM}cpu ${CACHED_SELF_CPU}  mem ${CACHED_SELF_MEM}  load ${CACHED_LOAD}${R}"
    lines+="${header}\n\n"

    # Progress summary -- weighted by acceptance criteria count per feature.
    # Complete features get full credit, wip features get half credit.
    if [ "$FEATURE_COUNT" -gt 0 ]; then
        local done_n=0 wip_n=0 pending_n=0
        local total_weight=0 earned_weight=0
        for ((i=0; i<FEATURE_COUNT; i++)); do
            local ac="${F_AC[$i]:-1}"
            [ "$ac" -lt 1 ] && ac=1
            total_weight=$((total_weight + ac))
            case "${F_STATUS[$i]}" in
                complete)
                    done_n=$((done_n + 1))
                    earned_weight=$((earned_weight + ac))
                    ;;
                in_progress)
                    wip_n=$((wip_n + 1))
                    earned_weight=$((earned_weight + ac / 2))
                    ;;
                *)
                    pending_n=$((pending_n + 1))
                    ;;
            esac
        done

        local pct=$((earned_weight * 100 / total_weight))
        local filled=$((earned_weight * 20 / total_weight))
        local partial=$(( (earned_weight * 20 % total_weight > 0 && filled < 20) ? 1 : 0 ))
        local empty=$((20 - filled - partial))
        local bar=""
        for ((i=0; i<filled; i++)); do bar+="█"; done
        if [ "$partial" -gt 0 ]; then bar+="▓"; fi
        for ((i=0; i<empty; i++)); do bar+="░"; done

        # Total tokens across all features
        local total_tok=0
        for ((i=0; i<FEATURE_COUNT; i++)); do
            total_tok=$((total_tok + ${F_TOKENS_IN[$i]:-0} + ${F_TOKENS_OUT[$i]:-0}))
        done
        local tok_summary=""
        if [ "$total_tok" -gt 0 ]; then
            format_tokens "$total_tok"
            tok_summary="  ${DIM}tokens: ${_FMT}${R}"
        fi

        printf -v _line "  ${GRN}%s${R}  ${BLD}%d%%${R}  ${GRN}%d${R} done  ${YLW}%d${R} wip  ${DIM}%d pending${R}  ${DIM}(%d total)${R}%b" "$bar" "$pct" "$done_n" "$wip_n" "$pending_n" "$FEATURE_COUNT" "$tok_summary"
        lines+="${_line}\n"

        # Wave summary line
        local max_wave=0
        for ((i=0; i<FEATURE_COUNT; i++)); do
            local _wv="${F_WAVE[$i]:-0}"
            [ "$_wv" -gt "$max_wave" ] && max_wave="$_wv"
        done
        if [ "$max_wave" -gt 0 ]; then
            local wave_line="  "
            for ((w=1; w<=max_wave; w++)); do
                local w_done=0 w_wip=0 w_pending=0 w_total=0
                for ((i=0; i<FEATURE_COUNT; i++)); do
                    [ "${F_WAVE[$i]:-0}" -ne "$w" ] && continue
                    w_total=$((w_total + 1))
                    case "${F_STATUS[$i]}" in
                        complete)    w_done=$((w_done + 1)) ;;
                        in_progress) w_wip=$((w_wip + 1)) ;;
                        *)           w_pending=$((w_pending + 1)) ;;
                    esac
                done
                [ "$w_total" -eq 0 ] && continue
                local wc_idx=$(( (w - 1) % ${#WAVE_COLORS[@]} ))
                local wc="${WAVE_COLORS[$wc_idx]}"
                local w_seg="${wc}w${w}:${R} ${w_done}/${w_total} done"
                [ "$w_wip" -gt 0 ] && w_seg+=" ${YLW}${w_wip} wip${R}"
                [ "$w_pending" -gt 0 ] && w_seg+=" ${DIM}${w_pending} waiting${R}"
                [ "$w" -gt 1 ] && wave_line+="  ${DIM}|${R}  "
                wave_line+="$w_seg"
            done
            lines+="${wave_line}\n"
        fi
        lines+="\n"

        # Table header (aligned with data rows: 2-char prefix + 22-char id + state + wave + tokens + details)
        printf -v _line "${DIM}  %-22s %-5s %-4s %-10s %s${R}" "FEATURE" "STATE" "WAVE" "TOKENS" "DETAILS"
        lines+="${_line}\n"

        # Feature rows from cached arrays
        for ((idx=0; idx<FEATURE_COUNT; idx++)); do
            local id="${FEATURE_IDS[$idx]}"
            local fstatus="${F_STATUS[$idx]}"
            local branch="${F_BRANCH[$idx]}"
            local notes="${F_NOTES[$idx]}"
            local blocked_by="${F_BLOCKED[$idx]}"
            local wave="${F_WAVE[$idx]:-0}"
            local detail

            # Wave: color-coded by wave number, dimmed dash for complete
            local wave_text wave_color
            if [ "$fstatus" = "complete" ]; then
                wave_text="--"
                wave_color="$DIM"
            elif [ "$wave" -gt 0 ]; then
                local wc_idx=$(( (wave - 1) % ${#WAVE_COLORS[@]} ))
                wave_text="w${wave}"
                wave_color="${WAVE_COLORS[$wc_idx]}"
            else
                wave_text="?"
                wave_color="$DIM"
            fi

            # Combine elapsed, branch, agent info, blocked_by, and notes into one detail string
            detail=""

            # Elapsed+alive indicator for WIP features
            local _elapsed="${F_ELAPSED[$idx]:-}"
            local _alive="${F_ALIVE[$idx]:-}"
            if [ -n "$_elapsed" ] && [ "$_elapsed" != "--" ]; then
                if [ "$_alive" = "stale" ]; then
                    detail+="${RED}${_elapsed}!${R} "
                elif [ "$_alive" = "live" ]; then
                    detail+="${GRN}${_elapsed}${R} "
                else
                    detail+="${DIM}${_elapsed}${R} "
                fi
            elif [ "$_elapsed" = "--" ] && [ "$fstatus" = "in_progress" ]; then
                detail+="${DIM}--${R} "
            fi

            if [ -n "$branch" ]; then
                detail+="${CYN}${branch}${R}"
                # Show agent activity for in-progress features
                local agent_detail="${F_AGENT[$idx]:-}"
                if [ -n "$agent_detail" ]; then
                    detail+="  ${DIM}${agent_detail}${R}"
                elif [ -n "$notes" ]; then
                    detail+="  ${DIM}${notes}${R}"
                fi
            fi

            # Show enriched blockers for non-complete features
            if [ -n "$blocked_by" ] && [ "$fstatus" != "complete" ]; then
                local enriched_blockers=""
                local _old_ifs="${IFS}"; IFS=', '; set -- $blocked_by; IFS="${_old_ifs}"
                for _bdep in "$@"; do
                    _bdep="${_bdep// /}"
                    [ -z "$_bdep" ] && continue
                    local _bst="" _bagent="" _belapsed=""
                    # Look up blocker status, agent info, elapsed
                    for ((bi=0; bi<FEATURE_COUNT; bi++)); do
                        if [ "${FEATURE_IDS[$bi]}" = "$_bdep" ]; then
                            _bst="${F_STATUS[$bi]}"
                            _bagent="${F_AGENT[$bi]:-}"
                            _belapsed="${F_ELAPSED[$bi]:-}"
                            break
                        fi
                    done
                    local _btag=""
                    case "$_bst" in
                        complete) _btag="${GRN}done${R}" ;;
                        in_progress)
                            _btag="wip"
                            # Extract commit count from agent info (e.g. "5 commits")
                            if [[ "$_bagent" =~ ([0-9]+)\ commit ]]; then
                                _btag+=" ${BASH_REMATCH[1]}c"
                            fi
                            [ -n "$_belapsed" ] && [ "$_belapsed" != "--" ] && _btag+=" $_belapsed"
                            _btag="${YLW}${_btag}${R}"
                            ;;
                        *) _btag="${DIM}pending${R}" ;;
                    esac
                    [ -n "$enriched_blockers" ] && enriched_blockers+=", "
                    enriched_blockers+="${_bdep} [${_btag}]"
                done
                if [ -n "$detail" ]; then
                    detail+="  ${RED}blocked:${R} ${enriched_blockers}"
                else
                    detail="${RED}blocked:${R} ${enriched_blockers}"
                fi
            elif [ -z "$detail" ] && [ -n "$notes" ]; then
                detail="${DIM}${notes}${R}"
            fi

            local display_id="$id"
            [ ${#display_id} -gt 22 ] && display_id="${display_id:0:19}..."

            # Token display for this feature
            local tok_in="${F_TOKENS_IN[$idx]:-0}"
            local tok_out="${F_TOKENS_OUT[$idx]:-0}"
            local tok_total=$((tok_in + tok_out))
            local token_text
            if [ "$tok_total" -gt 0 ]; then
                format_tokens "$tok_in"; local _ti="$_FMT"
                format_tokens "$tok_out"
                token_text="${_ti}/${_FMT}"
            else
                token_text="--"
            fi

            # Build row: prefix(2) + id(24) + state(6) + wave(5) + tokens(11) + detail
            # Use printf %-Ns on plain text for alignment, wrap ANSI around the result
            local state_text
            case "$fstatus" in
                complete)    state_text="done" ;;
                in_progress) state_text="wip" ;;
                *)
                    if [ -n "$blocked_by" ]; then
                        state_text="block"
                    else
                        state_text="ready"
                    fi
                    ;;
            esac

            local state_color
            case "$fstatus" in
                complete)    state_color="$GRN" ;;
                in_progress) state_color="$YLW" ;;
                *)
                    if [ -n "$blocked_by" ]; then
                        state_color="$RED"
                    else
                        state_color="$DIM"
                    fi
                    ;;
            esac

            local token_color="$DIM"
            [ "$tok_total" -gt 0 ] && token_color="$YLW"

            if [ "$idx" -eq "$SELECTED" ]; then
                printf -v _line "${INV}${BLD}> %-22s${R} ${state_color}%-5s${R} ${wave_color}%-4s${R} ${token_color}%-10s${R} %b" "$display_id" "$state_text" "$wave_text" "$token_text" "$detail"
            else
                printf -v _line "  %-22s ${state_color}%-5s${R} ${wave_color}%-4s${R} ${token_color}%-10s${R} %b" "$display_id" "$state_text" "$wave_text" "$token_text" "$detail"
            fi
            lines+="${_line}\n"
        done
    else
        lines+="  ${DIM}No features in .projd/progress/${R}\n"
    fi

    # Worktrees (from cache)
    lines+="\n"
    if [ "$CACHED_WT_COUNT" -gt 0 ]; then
        local _s=""; [ "$CACHED_WT_COUNT" -ne 1 ] && _s="s"
        lines+="  ${BLD}${CACHED_WT_COUNT} active worktree${_s}${R}\n"
        lines+="$CACHED_WT"
    else
        lines+="  ${DIM}No active worktrees${R}\n"
    fi

    # PRs (from cache)
    if [ "$CACHED_PR_COUNT" -gt 0 ]; then
        lines+="\n"
        _s=""; [ "$CACHED_PR_COUNT" -ne 1 ] && _s="s"
        lines+="  ${BLD}${CACHED_PR_COUNT} open PR${_s}${R}\n"
        lines+="$CACHED_PR"
    fi

    # Merged PRs
    if [ "$CACHED_MERGED_COUNT" -gt 0 ]; then
        local merged_text
        _s=""; [ "$CACHED_MERGED_COUNT" -ne 1 ] && _s="s"
        merged_text="${DIM}${CACHED_MERGED_COUNT} merged PR${_s}"
        [ -n "$CACHED_LAST_MERGE" ] && merged_text+="  |  last ${CACHED_LAST_MERGE}"
        merged_text+="${R}"
        [ "$CACHED_PR_COUNT" -eq 0 ] && lines+="\n"
        lines+="  ${merged_text}\n"
    fi

    # Footer
    lines+="\n"
    if [ -n "$STATUS_MSG" ]; then
        lines+="  ${GRN}${STATUS_MSG}${R}\n\n"
    fi
    if [ -n "$CONFIRM_ACTION" ]; then
        lines+="  ${YLW}${CONFIRM_ACTION} '${CONFIRM_ID}'? [y/N]${R}\n"
    else
        lines+="  ${DIM}j/k${R} navigate  ${DIM}d${R} detail  ${DIM}l${R} log  ${DIM}p${R} pr  ${DIM}r${R} reset  ${DIM}c${R} complete  ${DIM}x${R} kill  ${DIM}m${R} merge  ${DIM}q${R} quit\n"
    fi

    # Write to screen: cursor home, erase trailing chars per line, clear below
    printf '\033[H'
    printf '%b' "${lines//\\n/\\033[K\\n}"
    printf '\033[J'
}

# --- Overlay display (pauses refresh, shows detail, waits for key) ---

show_overlay() {
    local content="$1"
    printf '\033[2J\033[H'
    printf '%b\n' "$content"
    printf '\n'
    printf "  ${DIM}Press any key to return...${R}"
    _read_key _ 2>/dev/null || true
}

detail_overlay() {
    local id="$1"
    local f
    f=$(feature_file "$id")
    [ ! -f "$f" ] && return

    local out=""
    out+="$(printf "${BLD}Feature: %s${R}" "$id")\n"
    out+="$(printf "${DIM}%s${R}" "$(feature_field "$f" "name")")\n"
    out+="\n"

    # JSON
    out+="$(printf "${DIM}--- feature file ---${R}")\n"
    out+="$(jq '.' "$f" 2>/dev/null)\n"
    out+="\n"

    # Acceptance criteria
    local ac_count
    ac_count=$(jq '.acceptance_criteria | length' "$f" 2>/dev/null || echo 0)
    if [ "$ac_count" -gt 0 ]; then
        out+="$(printf "${BLD}Acceptance criteria:${R}")\n"
        while IFS= read -r line; do
            out+="  $line\n"
        done < <(jq -r '.acceptance_criteria[] // empty' "$f" 2>/dev/null | nl -ba -s '. ')
        out+="\n"
    fi

    # Branch log
    local branch
    branch=$(resolve_branch "$id")
    if [ -n "$branch" ] && git rev-parse --verify "$branch" &>/dev/null; then
        out+="$(printf "${BLD}Recent commits on ${CYN}%s${R}:" "$branch")\n"
        while IFS= read -r line; do
            out+="  $line\n"
        done < <(git log "$branch" --oneline -10 2>/dev/null)
        out+="\n"

        local base="main"
        git rev-parse --verify main &>/dev/null || base=$(git rev-parse --abbrev-ref HEAD 2>/dev/null)
        out+="$(printf "${BLD}Diff vs %s:${R}" "$base")\n"
        while IFS= read -r line; do
            out+="  $line\n"
        done < <(git diff --stat "${base}...${branch}" 2>/dev/null)
        out+="\n"
    fi

    # Worktree
    if [ -n "$branch" ]; then
        local wt
        wt=$(worktree_for_branch "$branch")
        if [ -n "$wt" ]; then
            out+="$(printf "${BLD}Worktree:${R} ${DIM}%s${R}" "$wt")\n"
        fi
    fi

    # Token usage breakdown
    if [ -n "$branch" ] && [ -n "$CACHED_TOKEN_DATA" ]; then
        local tok_sums
        tok_sums=$(echo "$CACHED_TOKEN_DATA" | awk -F'\t' -v b="$branch" \
            '$1 == b {in_t += $2; out_t += $3} END {printf "%d %d", in_t+0, out_t+0}')
        local d_tin d_tout
        read -r d_tin d_tout <<< "$tok_sums"
        if [ "$((d_tin + d_tout))" -gt 0 ]; then
            out+="\n"
            out+="$(printf "${BLD}Token usage:${R}")\n"
            format_tokens "$d_tin"; out+="  Input:  ${_FMT}  ($d_tin)\n"
            format_tokens "$d_tout"; out+="  Output: ${_FMT}  ($d_tout)\n"
            format_tokens "$((d_tin + d_tout))"; out+="  Total:  ${_FMT}\n"
        fi
    fi

    # PR
    if [ -n "$branch" ] && command -v gh &>/dev/null; then
        local pr_num
        pr_num=$(pr_for_branch "$branch")
        if [ -n "$pr_num" ]; then
            out+="$(printf "${BLD}PR:${R} ${GRN}#%s${R}" "$pr_num")\n"
            out+="$(gh pr view "$pr_num" --json title,state,additions,deletions,reviewDecision,url \
                --jq '"  \(.url)\n  +\(.additions) -\(.deletions)  \(.state)  review: \(.reviewDecision // "none")"' 2>/dev/null || true)\n"
        fi
    fi

    show_overlay "$out"
}

log_overlay() {
    local id="$1"
    local f
    f=$(feature_file "$id")
    [ ! -f "$f" ] && return

    local branch
    branch=$(resolve_branch "$id")
    if [ -z "$branch" ] || ! git rev-parse --verify "$branch" &>/dev/null; then
        show_overlay "$(printf "${DIM}No branch for %s${R}" "$id")"
        return
    fi

    local out=""
    out+="$(printf "${BLD}Commits on ${CYN}%s${R}:" "$branch")\n\n"
    while IFS= read -r line; do
        out+="  $line\n"
    done < <(git log "$branch" --oneline -25 2>/dev/null)

    show_overlay "$out"
}

# --- Actions ---

selected_id() {
    if [ "$FEATURE_COUNT" -gt 0 ] && [ "$SELECTED" -lt "$FEATURE_COUNT" ]; then
        echo "${FEATURE_IDS[$SELECTED]}"
    fi
}

action_reset() {
    local id="$1"
    local f
    f=$(feature_file "$id")
    [ ! -f "$f" ] && return

    local branch
    branch=$(resolve_branch "$id")
    if [ -n "$branch" ]; then
        local wt
        wt=$(worktree_for_branch "$branch")
        if [ -n "$wt" ]; then
            git worktree remove --force "$wt" 2>/dev/null || true
        fi
    fi

    local tmp
    tmp=$(mktemp)
    jq '.status = "pending" | .branch = "" | .notes = ""' "$f" > "$tmp" && mv "$tmp" "$f"
    STATUS_MSG="Reset '$id' to pending"
}

action_complete() {
    local id="$1"
    local f
    f=$(feature_file "$id")
    [ ! -f "$f" ] && return

    local branch
    branch=$(resolve_branch "$id")
    if [ -n "$branch" ]; then
        local wt
        wt=$(worktree_for_branch "$branch")
        if [ -n "$wt" ]; then
            git worktree remove --force "$wt" 2>/dev/null || true
        fi
    fi

    local tmp
    tmp=$(mktemp)
    jq '.status = "complete"' "$f" > "$tmp" && mv "$tmp" "$f"
    STATUS_MSG="Marked '$id' complete"
}

action_kill() {
    local id="$1"
    local f
    f=$(feature_file "$id")
    [ ! -f "$f" ] && return

    local branch
    branch=$(resolve_branch "$id")
    [ -z "$branch" ] && { STATUS_MSG="No branch for '$id'"; return; }

    local wt
    wt=$(worktree_for_branch "$branch")
    [ -z "$wt" ] && { STATUS_MSG="No worktree for '$id'"; return; }

    git worktree remove --force "$wt" 2>/dev/null && \
        STATUS_MSG="Removed worktree for '$id'" || \
        STATUS_MSG="Failed to remove worktree"
}

action_pr_open() {
    local id="$1"
    local f
    f=$(feature_file "$id")
    [ ! -f "$f" ] && return

    local branch
    branch=$(resolve_branch "$id")
    [ -z "$branch" ] && { STATUS_MSG="No branch for '$id'"; return; }

    command -v gh &>/dev/null || { STATUS_MSG="gh CLI not available"; return; }

    local pr_num
    pr_num=$(pr_for_branch "$branch")
    [ -z "$pr_num" ] && { STATUS_MSG="No open PR for '$id'"; return; }

    gh pr view "$pr_num" --web 2>/dev/null
    STATUS_MSG="Opened PR #${pr_num}"
}

action_merge() {
    local id="$1"
    local f
    f=$(feature_file "$id")
    [ ! -f "$f" ] && return

    local branch
    branch=$(resolve_branch "$id")
    [ -z "$branch" ] && { STATUS_MSG="No branch for '$id'"; return; }

    command -v gh &>/dev/null || { STATUS_MSG="gh CLI not available"; return; }

    local pr_num
    pr_num=$(pr_for_branch "$branch")
    [ -z "$pr_num" ] && { STATUS_MSG="No open PR for '$id'"; return; }

    if gh pr merge "$pr_num" --merge 2>/dev/null; then
        local wt
        wt=$(worktree_for_branch "$branch")
        if [ -n "$wt" ]; then
            git worktree remove --force "$wt" 2>/dev/null || true
        fi

        local tmp
        tmp=$(mktemp)
        jq '.status = "complete"' "$f" > "$tmp" && mv "$tmp" "$f"
        STATUS_MSG="Merged PR #${pr_num}, marked '$id' complete"
    else
        STATUS_MSG="Failed to merge PR #${pr_num}"
    fi
}

# --- Non-interactive mode ---

if [ "$ONCE" = true ]; then
    load_features
    load_slow_data
    # Simple non-interactive output (no alternate screen, no key reading)
    SELECTED=-1  # no selection highlight

    once_header="${BLD}projd monitor${R}"
    [ -n "$PROJECT_NAME" ] && once_header+="  ${BLD}${CYN}${PROJECT_NAME}${R}"
    [ -n "$PROJECT_DESC" ] && once_header+="  ${DIM}-- ${PROJECT_DESC}${R}"
    once_header+="  ${DIM}$(date '+%H:%M:%S')${R}"
    once_header+="  ${DIM}cpu ${CACHED_SELF_CPU}  mem ${CACHED_SELF_MEM}  load ${CACHED_LOAD}${R}"
    printf '%b\n\n' "$once_header"

    if [ "$FEATURE_COUNT" -gt 0 ]; then
        done_n=0; wip_n=0; pending_n=0
        total_weight=0; earned_weight=0
        for ((i=0; i<FEATURE_COUNT; i++)); do
            ac="${F_AC[$i]:-1}"
            [ "$ac" -lt 1 ] && ac=1
            total_weight=$((total_weight + ac))
            case "${F_STATUS[$i]}" in
                complete)
                    done_n=$((done_n + 1))
                    earned_weight=$((earned_weight + ac))
                    ;;
                in_progress)
                    wip_n=$((wip_n + 1))
                    earned_weight=$((earned_weight + ac / 2))
                    ;;
                *)
                    pending_n=$((pending_n + 1))
                    ;;
            esac
        done

        pct=$((earned_weight * 100 / total_weight))
        filled=$((earned_weight * 20 / total_weight))
        partial=$(( (earned_weight * 20 % total_weight > 0 && filled < 20) ? 1 : 0 ))
        empty_b=$((20 - filled - partial))
        bar=""
        for ((i=0; i<filled; i++)); do bar+="█"; done
        if [ "$partial" -gt 0 ]; then bar+="▓"; fi
        for ((i=0; i<empty_b; i++)); do bar+="░"; done
        printf "  ${GRN}%s${R}  ${BLD}%d%%${R}  ${GRN}%d${R} done  ${YLW}%d${R} wip  ${DIM}%d pending${R}  ${DIM}(%d total)${R}\n" \
            "$bar" "$pct" "$done_n" "$wip_n" "$pending_n" "$FEATURE_COUNT"

        # Wave summary
        max_wave=0
        for ((i=0; i<FEATURE_COUNT; i++)); do
            _wv="${F_WAVE[$i]:-0}"
            [ "$_wv" -gt "$max_wave" ] && max_wave="$_wv"
        done
        if [ "$max_wave" -gt 0 ]; then
            wave_line="  "
            for ((w=1; w<=max_wave; w++)); do
                w_done=0; w_wip=0; w_pending=0; w_total=0
                for ((i=0; i<FEATURE_COUNT; i++)); do
                    [ "${F_WAVE[$i]:-0}" -ne "$w" ] && continue
                    w_total=$((w_total + 1))
                    case "${F_STATUS[$i]}" in
                        complete)    w_done=$((w_done + 1)) ;;
                        in_progress) w_wip=$((w_wip + 1)) ;;
                        *)           w_pending=$((w_pending + 1)) ;;
                    esac
                done
                [ "$w_total" -eq 0 ] && continue
                wc_idx=$(( (w - 1) % ${#WAVE_COLORS[@]} ))
                wc="${WAVE_COLORS[$wc_idx]}"
                w_seg="${wc}w${w}:${R} ${w_done}/${w_total} done"
                [ "$w_wip" -gt 0 ] && w_seg+=" ${YLW}${w_wip} wip${R}"
                [ "$w_pending" -gt 0 ] && w_seg+=" ${DIM}${w_pending} waiting${R}"
                [ "$w" -gt 1 ] && wave_line+="  ${DIM}|${R}  "
                wave_line+="$w_seg"
            done
            printf '%b\n' "$wave_line"
        fi
        echo ""

        printf "  ${DIM}%-20s  %-6s  %-4s  %-10s  %-25s  %s${R}\n" "FEATURE" "STATUS" "WAVE" "TOKENS" "BRANCH" "INFO"
        for ((idx=0; idx<FEATURE_COUNT; idx++)); do
            id="${FEATURE_IDS[$idx]}"
            fstatus="${F_STATUS[$idx]}"
            branch="${F_BRANCH[$idx]}"
            blocked_by="${F_BLOCKED[$idx]}"
            wave="${F_WAVE[$idx]:-0}"
            icon="" wave_str="" info=""

            case "$fstatus" in
                complete)    icon="done " ;;
                in_progress) icon="wip  " ;;
                *)           icon="pend " ;;
            esac

            if [ "$fstatus" = "complete" ]; then
                wave_str="--"
            elif [ "$wave" -gt 0 ]; then
                wave_str="w${wave}"
            else
                wave_str="?"
            fi

            # Elapsed+alive for INFO column
            _elapsed="${F_ELAPSED[$idx]:-}"
            _alive="${F_ALIVE[$idx]:-}"
            elapsed_prefix=""
            if [ -n "$_elapsed" ] && [ "$_elapsed" != "--" ]; then
                if [ "$_alive" = "stale" ]; then
                    elapsed_prefix="${_elapsed}! "
                elif [ "$_alive" = "live" ]; then
                    elapsed_prefix="${_elapsed} "
                else
                    elapsed_prefix="${_elapsed} "
                fi
            elif [ "$_elapsed" = "--" ] && [ "$fstatus" = "in_progress" ]; then
                elapsed_prefix="-- "
            fi

            # Enriched blockers
            if [ -n "$blocked_by" ] && [ "$fstatus" != "complete" ]; then
                enriched=""
                local _old_ifs="${IFS}"; IFS=', '; set -- $blocked_by; IFS="${_old_ifs}"
                for _od in "$@"; do
                    _od="${_od// /}"
                    [ -z "$_od" ] && continue
                    _ost="" _oagent="" _oelapsed=""
                    for ((bi=0; bi<FEATURE_COUNT; bi++)); do
                        if [ "${FEATURE_IDS[$bi]}" = "$_od" ]; then
                            _ost="${F_STATUS[$bi]}"
                            _oagent="${F_AGENT[$bi]:-}"
                            _oelapsed="${F_ELAPSED[$bi]:-}"
                            break
                        fi
                    done
                    _otag=""
                    case "$_ost" in
                        complete) _otag="done" ;;
                        in_progress)
                            _otag="wip"
                            if [[ "$_oagent" =~ ([0-9]+)\ commit ]]; then
                                _otag+=" ${BASH_REMATCH[1]}c"
                            fi
                            [ -n "$_oelapsed" ] && [ "$_oelapsed" != "--" ] && _otag+=" $_oelapsed"
                            ;;
                        *) _otag="pending" ;;
                    esac
                    [ -n "$enriched" ] && enriched+=", "
                    enriched+="${_od} [${_otag}]"
                done
                info="blocked: ${enriched}"
            fi

            tok_in="${F_TOKENS_IN[$idx]:-0}"
            tok_out="${F_TOKENS_OUT[$idx]:-0}"
            tok_total=$((tok_in + tok_out))
            tok_str="--"
            if [ "$tok_total" -gt 0 ]; then
                format_tokens "$tok_in"; _ti="$_FMT"
                format_tokens "$tok_out"
                tok_str="${_ti}/${_FMT}"
            fi

            printf "  %-20s  %-6s  %-4s  %-10s  %-25s  %s\n" "$id" "$icon" "$wave_str" "$tok_str" "$branch" "${elapsed_prefix}${info}"
        done

        # Merged PRs
        if [ "$CACHED_MERGED_COUNT" -gt 0 ]; then
            echo ""
            merged_line="${CACHED_MERGED_COUNT} merged PR"
            [ "$CACHED_MERGED_COUNT" -ne 1 ] && merged_line+="s"
            [ -n "$CACHED_LAST_MERGE" ] && merged_line+="  |  last ${CACHED_LAST_MERGE}"
            printf "  ${DIM}%s${R}\n" "$merged_line"
        fi
    else
        printf "  ${DIM}No features in .projd/progress/${R}\n"
    fi
    echo ""
    exit 0
fi

# --- Main loop ---

# Disable echo so keypresses don't litter the screen.
# Use bash read -t for timeouts (portable, no stty timing conflicts).
stty -echo 2>/dev/null

load_features
load_slow_data
render

LAST_REFRESH=$(date +%s)
TICKS_SINCE_FEATURES=0

while true; do
    # Read one byte with TICK timeout (~250ms). Keypresses return immediately;
    # timeout returns empty (exit code > 128) and drives the render loop.
    KEY=""
    IFS= _read_key -t "$TICK" KEY 2>/dev/null || true

    # Arrow keys send 3 bytes: ESC [ A/B. Read the remaining bytes.
    # Bytes are already buffered so read returns instantly despite the timeout.
    if [[ "$KEY" == $'\033' ]]; then
        SEQ1="" SEQ2=""
        IFS= _read_key -t "$TICK" SEQ1 2>/dev/null || true
        IFS= _read_key -t "$TICK" SEQ2 2>/dev/null || true
        if [[ "$SEQ1" == "[" ]]; then
            case "$SEQ2" in
                A) KEY="UP" ;;
                B) KEY="DOWN" ;;
                *) KEY="" ;;
            esac
        else
            KEY=""
        fi
    fi

    NEEDS_RENDER=false

    # Handle confirmation mode
    if [ -n "$CONFIRM_ACTION" ]; then
        case "$KEY" in
            y|Y)
                case "$CONFIRM_ACTION" in
                    "Reset")    action_reset "$CONFIRM_ID" ;;
                    "Complete") action_complete "$CONFIRM_ID" ;;
                    "Kill worktree for") action_kill "$CONFIRM_ID" ;;
                    "Merge PR for")     action_merge "$CONFIRM_ID" ;;
                esac
                CONFIRM_ACTION=""
                CONFIRM_ID=""
                load_features
                TICKS_SINCE_FEATURES=0
                NEEDS_RENDER=true
                ;;
            n|N|"")
                if [ -n "$KEY" ]; then
                    CONFIRM_ACTION=""
                    CONFIRM_ID=""
                    STATUS_MSG="Cancelled"
                    NEEDS_RENDER=true
                fi
                ;;
            *)
                CONFIRM_ACTION=""
                CONFIRM_ID=""
                STATUS_MSG="Cancelled"
                NEEDS_RENDER=true
                ;;
        esac
    elif [ -n "$KEY" ]; then
        STATUS_MSG=""
        id=$(selected_id)

        case "$KEY" in
            q) break ;;
            k|UP)
                if [ "$SELECTED" -gt 0 ]; then
                    SELECTED=$((SELECTED - 1))
                    NEEDS_RENDER=true
                fi
                ;;
            j|DOWN)
                if [ "$SELECTED" -lt $((FEATURE_COUNT - 1)) ]; then
                    SELECTED=$((SELECTED + 1))
                    NEEDS_RENDER=true
                fi
                ;;
            d)
                [ -n "$id" ] && detail_overlay "$id"
                NEEDS_RENDER=true
                ;;
            l)
                [ -n "$id" ] && log_overlay "$id"
                NEEDS_RENDER=true
                ;;
            p)
                [ -n "$id" ] && action_pr_open "$id"
                NEEDS_RENDER=true
                ;;
            r)
                if [ -n "$id" ]; then
                    CONFIRM_ACTION="Reset"
                    CONFIRM_ID="$id"
                    NEEDS_RENDER=true
                fi
                ;;
            c)
                if [ -n "$id" ]; then
                    CONFIRM_ACTION="Complete"
                    CONFIRM_ID="$id"
                    NEEDS_RENDER=true
                fi
                ;;
            x)
                if [ -n "$id" ]; then
                    CONFIRM_ACTION="Kill worktree for"
                    CONFIRM_ID="$id"
                    NEEDS_RENDER=true
                fi
                ;;
            m)
                if [ -n "$id" ]; then
                    CONFIRM_ACTION="Merge PR for"
                    CONFIRM_ID="$id"
                    NEEDS_RENDER=true
                fi
                ;;
            /)
                load_features
                TICKS_SINCE_FEATURES=0
                NEEDS_RENDER=true
                ;;
        esac
    fi

    # Throttled refresh: features every FEATURES_CADENCE ticks, slow data on INTERVAL seconds
    TICKS_SINCE_FEATURES=$((TICKS_SINCE_FEATURES + 1))
    if [ "$TICKS_SINCE_FEATURES" -ge "$FEATURES_CADENCE" ]; then
        load_features
        TICKS_SINCE_FEATURES=0
    fi

    NOW=$(date +%s)
    if [ $((NOW - LAST_REFRESH)) -ge "$INTERVAL" ]; then
        load_slow_data
        LAST_REFRESH=$NOW
    fi

    # Always render -- spinner animation needs continuous updates
    render
done
