#!/usr/bin/env bash
# Calls every route the hub forwards on ONE node, directly, the way the hub
# would (X-Acp-Peer: 1), and prints "route status first-80-bytes" per call.
#
#   fleet-node-sweep.sh <machine-id>            read-only sweep
#   fleet-node-sweep.sh <machine-id> --launch   also launch one idle chat,
#                                               probe it, then kill it
#
# The node address is read from ~/fleet/syzygy.json at run time and is never
# printed: every printed body or curl error is scrubbed first, then cut to
# 80 bytes. Run from the hub machine.
#
# Test hook, only for exercising the cleanup path: SWEEP_FAKE_SESSIONS_FAIL=1
# makes the post-kill confirmation treat every /api/sessions reply as failed.
set -u

usage() { echo "usage: fleet-node-sweep.sh <machine-id> [--launch]" >&2; exit 2; }

[ $# -ge 1 ] && [ $# -le 2 ] || usage
id="$1"
mode="${2:-}"
if [ -n "$mode" ] && [ "$mode" != "--launch" ]; then usage; fi

reg="$HOME/fleet/syzygy.json"
ip="$(jq -r --arg id "$id" '.machines[] | select(.id==$id) | .ip // empty' "$reg" 2>/dev/null)"
if [ -z "$ip" ]; then
  echo "unknown machine $id" >&2
  exit 2
fi
base="http://$ip:8090"
ipre="${ip//./\\.}"

tmp="$(mktemp -d)"
buf=""            # buffer name of the chat this script launched
cleanup_done=1    # 0 while a launched chat still needs its kill confirmed

# scrub replaces the node address with <node> in anything we print.
scrub() { sed "s/$ipre/<node>/g"; }

# call METHOD PATH [JSON-BODY] [MAX-TIME]
# Sets $status and $body (full, internal use only) and prints one line unless
# QUIET=1. A LABEL=... prefix replaces the printed route.
call() {
  local method="$1" path="$2" data="${3:-}" max="${4:-20}" rc shown
  local args=(-sS --max-time "$max" -X "$method" -H 'X-Acp-Peer: 1' -o "$tmp/body" -w '%{http_code}')
  if [ -n "$data" ]; then
    args+=(-H 'Content-Type: application/json' --data "$data")
  fi
  rm -f "$tmp/body"
  status="$(curl "${args[@]}" "$base$path" 2>"$tmp/err")"
  rc=$?
  body="$(cat "$tmp/body" 2>/dev/null)"
  shown="$body"
  if [ $rc -ne 0 ]; then
    status="curl-$rc"
    shown="$(cat "$tmp/err")"
  fi
  if [ "${QUIET:-0}" != 1 ]; then
    printf '%s %s %s\n' "${LABEL:-$method $path}" "$status" \
      "$(printf '%s' "$shown" | tr '\n\r' '  ' | scrub | head -c 80)"
  fi
}

# in_sessions BUFFER [MAX-TIME]: sets $present to present, absent or unknown
# from a fresh /api/sessions reply. "absent" needs curl exit 0, HTTP 200, and a
# jq program that exits 0 after checking .sessions is an array; every other
# outcome, including any non-zero jq exit, is "unknown".
in_sessions() {
  local verdict
  QUIET=1 call POST /api/sessions '{}' "${2:-20}"
  present=unknown
  [ "$status" = 200 ] || return 0
  verdict="$(printf '%s' "$body" | jq -er --arg b "$1" '
    if (.sessions | type) == "array"
    then (if ([.sessions[] | select(.bufferName == $b)] | length) > 0 then "present" else "absent" end)
    else error("sessions is not an array") end' 2>/dev/null)" || return 0
  case "$verdict" in
    present|absent) present="$verdict" ;;
  esac
}

# confirm_gone BUFFER: wall-clock bounded to 15 seconds from the call; no
# request starts after the deadline and each --max-time is the time left.
# Returns 0 only on a positively confirmed "absent".
confirm_gone() {
  local deadline left
  deadline=$(( $(date +%s) + 15 ))
  while :; do
    left=$(( deadline - $(date +%s) ))
    [ "$left" -ge 1 ] || return 1
    in_sessions "$1" "$left"
    if [ -n "${SWEEP_FAKE_SESSIONS_FAIL:-}" ]; then present=unknown; fi
    [ "$present" = absent ] && return 0
    left=$(( deadline - $(date +%s) ))
    [ "$left" -ge 1 ] || return 1
    sleep 1
  done
}

# kill_buffer BUFFER: POST /api/kill, print its status, retry once on non-200.
kill_buffer() {
  local b="$1" payload
  payload="$(jq -nc --arg b "$b" '{bufferName:$b}')"
  call POST /api/kill "$payload"
  if [ "$status" != 200 ]; then
    sleep 2
    LABEL='POST /api/kill (retry)' call POST /api/kill "$payload"
  fi
}

on_exit() {
  local code=$?
  trap - EXIT   # no re-entry
  if [ "$cleanup_done" = 0 ] && [ -n "$buf" ]; then
    echo "exit before the kill was confirmed: one more kill attempt for $buf" | scrub
    kill_buffer "$buf"
    in_sessions "$buf" 5
    if [ "$present" = absent ]; then
      echo "after the extra kill: $buf confirmed gone" | scrub
    else
      echo "UNCONFIRMED: $buf may still be running on $id" | scrub
    fi
    [ "$code" -ne 0 ] || code=1
  fi
  rm -rf "$tmp"
  exit "$code"
}
trap on_exit EXIT

call POST /api/sessions '{}'
if [ "$status" != 200 ]; then
  echo "unreachable"
  exit 3
fi
sessions="$body"

call POST /api/presets '{}'
call POST /api/mode-words '{}'
call POST /api/launch-options '{}'
launch_opts="$body"
call POST /api/projects '{}'
projects="$body"
call POST /api/transcripts '{}'
call POST /api/transcript-search '{"query":"the"}'
call GET /api/pin

# probe_chat BUFFER PID CWD: the five calls that need a live chat. With no cwd
# the chat's own directory is unknown; say so, then probe the first project
# path under its own label so the two facts stay separate.
probe_chat() {
  local buf="$1" pid="$2" cwd="$3" enc ppath
  enc="$(jq -rn --arg b "$buf" '$b|@uri')"
  call POST /api/models "$(jq -nc --arg b "$buf" '{bufferName:$b}')"
  call GET "/api/preview?pid=$pid"
  call GET "/api/git-status?pid=$pid"
  call GET "/api/fork?bufferName=$enc"
  if [ -n "$cwd" ]; then
    call POST /files/list "$(jq -nc --arg p "$cwd" '{path:$p}')"
  else
    echo "POST /files/list skipped (session reports no cwd)"
    ppath="$(printf '%s' "$projects" | jq -r '.projects[0].path // empty' 2>/dev/null)"
    if [ -n "$ppath" ]; then
      LABEL='POST /files/list (first project path, not the chat cwd)' \
        call POST /files/list "$(jq -nc --arg p "$ppath" '{path:$p}')"
    else
      echo "POST /files/list (first project path, not the chat cwd) skipped (no project)"
    fi
  fi
}

first="$(printf '%s' "$sessions" | jq -c '[.sessions[]? | select(.bufferName and .pid)][0] // empty' 2>/dev/null)"
if [ -n "$first" ]; then
  probe_chat "$(jq -r .bufferName <<<"$first")" "$(jq -r .pid <<<"$first")" \
    "$(jq -r '.cwd // empty' <<<"$first")"
else
  for r in "POST /api/models" "GET /api/preview" "GET /api/git-status" "GET /api/fork" "POST /files/list"; do
    echo "$r skipped (no live chat)"
  done
fi

[ "$mode" = "--launch" ] || exit 0

# --- launch mode: one idle chat, probe it, kill it -------------------------
cwd="$(printf '%s' "$projects" | jq -r '.projects[0].path // empty' 2>/dev/null)"
# A node whose emacsclient mangles long replies answers /api/launch-options with
# a 500 whose error text still carries the base64 payload split by a protocol
# error line. Recover it so the launch can still be built from the node's own
# data. (Temporary: remove once the nodes run a fixed binary.)
if ! printf '%s' "$launch_opts" | jq -e '.agents' >/dev/null 2>&1; then
  QUIET=1 call POST /api/launch-options '{}'
  launch_opts="$(printf '%s' "$body" | jq -r '.error? // empty' 2>/dev/null \
    | sed 's/\*ERROR\*: Unknown message: //; s/&n$//' | tr -d '"\n' | base64 -d 2>/dev/null)"
  echo "launch-options recovered from the error text: $(printf '%s' "$launch_opts" | jq -e '.agents' >/dev/null 2>&1 && echo yes || echo no)"
fi
settings="$(printf '%s' "$launch_opts" | jq -c '
  . as $o | ($o.agents[] | select(.id == $o.defaultAgent)) as $a
  | {agent: $a.id,
     model: (if ($a.defaults.model // "") != "" then $a.defaults.model else $a.models[0].id end),
     mode:  (if ($a.defaults.mode  // "") != "" then $a.defaults.mode  else $a.modes[0].id  end)}
  + (if ($a.efforts | length) > 0
     then {effort: (if ($a.defaults.effort // "") != "" then $a.defaults.effort else $a.efforts[0].id end)}
     else {} end)' 2>/dev/null)"
if [ -z "$cwd" ] || [ -z "$settings" ]; then
  echo "launch: cannot build spawn body (cwd or settings missing)"
  exit 4
fi
echo "launch settings: $settings"
start=$(date +%s)
call POST /api/spawn "$(jq -nc --arg c "$cwd" --argjson s "$settings" '{cwd:$c,settings:$s}')" 200
echo "launch took $(( $(date +%s) - start ))s"
# Even an error reply can name a buffer; from here on it must be cleaned up.
buf="$(printf '%s' "$body" | jq -r '.bufferName? // empty' 2>/dev/null)"
if [ -z "$buf" ]; then
  echo "launch: no bufferName returned, nothing to kill"
  exit 5
fi
cleanup_done=0
echo "launched buffer: $buf" | scrub

found=""
for _ in $(seq 1 15); do
  in_sessions "$buf"
  if [ "$present" = present ]; then
    found="$(printf '%s' "$body" | jq -c --arg b "$buf" '[.sessions[] | select(.bufferName==$b)][0]')"
    break
  fi
  sleep 2
done
if [ -n "$found" ]; then
  echo "chat appeared in /api/sessions"
  if [ -z "$first" ]; then
    probe_chat "$buf" "$(jq -r .pid <<<"$found")" "$(jq -r '.cwd // empty' <<<"$found")"
  fi
else
  echo "chat did NOT appear in /api/sessions within 30s"
fi

kill_buffer "$buf"
confirm_start=$(date +%s)
if confirm_gone "$buf"; then
  echo "kill confirmed gone: yes ($buf), confirmation took $(( $(date +%s) - confirm_start ))s" | scrub
  cleanup_done=1
else
  echo "confirmation phase took $(( $(date +%s) - confirm_start ))s"
  echo "UNCONFIRMED: $buf may still be running on $id" | scrub
  exit 1   # cleanup_done stays 0, so the EXIT trap makes one more kill attempt
fi
