#!/usr/bin/env bash
# Calls every route the hub forwards on ONE node, directly, the way the hub
# would (X-Acp-Peer: 1), and prints "route status first-80-bytes" per call.
#
#   fleet-node-sweep.sh <machine-id>            read-only sweep
#   fleet-node-sweep.sh <machine-id> --launch   also launch one idle chat,
#                                               probe it, then kill it
#
# The node address is read from ~/fleet/syzygy.json at run time and is never
# printed. Run from the hub machine.
set -u

id="${1:?usage: fleet-node-sweep.sh <machine-id> [--launch]}"
mode="${2:-}"
reg="$HOME/fleet/syzygy.json"
ip="$(jq -r --arg id "$id" '.machines[] | select(.id==$id) | .ip' "$reg")"
if [ -z "$ip" ] || [ "$ip" = "null" ]; then
  echo "no ip for machine $id in registry" >&2
  exit 2
fi
base="http://$ip:8090"

# scrub replaces the node address with <node> in anything we print.
scrub() { sed "s/$ip/<node>/g"; }

# call METHOD PATH [JSON-BODY] [MAX-TIME]; sets $status and $body, prints a line.
call() {
  local method="$1" path="$2" data="${3:-}" max="${4:-20}" out
  local args=(-sS --max-time "$max" -X "$method" -H 'X-Acp-Peer: 1' -o - -w '\n%{http_code}')
  if [ -n "$data" ]; then
    args+=(-H 'Content-Type: application/json' --data "$data")
  fi
  out="$(curl "${args[@]}" "$base$path" 2>&1)"
  rc=$?
  if [ $rc -ne 0 ]; then
    status="curl-$rc"
    body="$(printf '%s' "$out" | scrub)"
  else
    status="${out##*$'\n'}"
    body="${out%$'\n'*}"
  fi
  printf '%s %s %s\n' "$method $path" "$status" \
    "$(printf '%s' "$body" | tr '\n\r' '  ' | cut -c1-80 | scrub)"
}

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

# probe_chat BUFFER PID CWD: the five calls that need a live chat.
probe_chat() {
  local buf="$1" pid="$2" cwd="$3" enc
  # A chat with no cwd of its own falls back to the node's first project.
  [ -n "$cwd" ] || cwd="$(printf '%s' "$projects" | jq -r '.projects[0].path // "."')"
  enc="$(jq -rn --arg b "$buf" '$b|@uri')"
  call POST /api/models "$(jq -nc --arg b "$buf" '{bufferName:$b}')"
  call GET "/api/preview?pid=$pid"
  call GET "/api/git-status?pid=$pid"
  call GET "/api/fork?bufferName=$enc"
  call POST /files/list "$(jq -nc --arg p "$cwd" '{path:$p}')"
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
cwd="$(printf '%s' "$projects" | jq -r '.projects[0].path // empty')"
# A node whose emacsclient mangles long replies answers /api/launch-options with
# a 500 whose error text still carries the base64 payload split by a protocol
# error line. Recover it so the launch can still be built from the node's own data.
if ! printf '%s' "$launch_opts" | jq -e '.agents' >/dev/null 2>&1; then
  call POST /api/launch-options '{}' >/dev/null
  launch_opts="$(printf '%s' "$body" | jq -r '.error? // empty' \
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
     else {} end)')"
if [ -z "$cwd" ] || [ -z "$settings" ]; then
  echo "launch: cannot build spawn body (cwd or settings missing)"
  exit 4
fi
echo "launch settings: $settings"
start=$(date +%s)
call POST /api/spawn "$(jq -nc --arg c "$cwd" --argjson s "$settings" '{cwd:$c,settings:$s}')" 200
echo "launch took $(( $(date +%s) - start ))s"
buf="$(printf '%s' "$body" | jq -r '.bufferName // empty' 2>/dev/null)"
if [ -z "$buf" ]; then
  echo "launch: no bufferName returned, nothing to kill"
  exit 5
fi
echo "launched buffer: $buf"

found=""
for _ in $(seq 1 15); do
  call POST /api/sessions '{}' >/dev/null
  entry="$(printf '%s' "$body" | jq -c --arg b "$buf" '[.sessions[]? | select(.bufferName==$b)][0] // empty')"
  if [ -n "$entry" ]; then found="$entry"; break; fi
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

call POST /api/kill "$(jq -nc --arg b "$buf" '{bufferName:$b}')"
gone=no
for _ in $(seq 1 10); do
  call POST /api/sessions '{}' >/dev/null
  if [ -z "$(printf '%s' "$body" | jq -c --arg b "$buf" '[.sessions[]? | select(.bufferName==$b)][0] // empty')" ]; then
    gone=yes; break
  fi
  sleep 2
done
echo "kill confirmed gone: $gone ($buf)"
