# acp-mobile

> **WIP** — Functional and in daily use, but still under active development.
>
> **Security warning:** This exposes ACP agent sessions over HTTP/WebSocket. It has hardening measures (authkey cookies, CSRF via Sec-Fetch-Site, DNS rebinding protection, CSP nonces, rate-limited auth, localhost-only binding) and is designed for use over Tailscale, but it hasn't had a dedicated security review. Be cautious about exposing machines with sensitive data or credentials.

Mobile web UI for [ACP](https://github.com/zed-industries/agent-client-protocol) sessions running through [acp-multiplex](https://github.com/ElleNajt/acp-multiplex).

Discovers all live acp-multiplex sockets on the machine, groups them by project, and lets you chat with any session from your phone.

## Features

- **Session discovery** — automatically finds all active acp-multiplex sockets, groups by project
- **Chat interface** — WebSocket bridge to any session with markdown rendering, streaming, tool call display
- **File browser** — browse and view files from session working directories
- **Diff review** — read-only, phone-first reader for what changed (see below)
- **Auth** — random 256-bit authkey (generated on first run, stored in `~/.acp-mobile/authkey`)
- **Security hardening** — CSRF protection, DNS rebinding protection, CSP headers, XSS-safe markdown

## Message spacing and copying

Sent and queued prompts retain their original line breaks, blank lines, tabs,
and spaces. Message bubbles preserve whitespace when displaying plain text;
agent replies still render Markdown. Whitespace-only prompts are not sent.

Swipe left on a message and choose `Copy text` to copy its original plain
text, including leading and trailing whitespace. Code-block copy buttons also
retain the source's whitespace. Native text selection follows browser rules
and may omit a final newline at a block boundary. Rich-text copying is not
provided. Whitespace already stripped from older sent messages cannot be
recovered.

## Diff review

A read-only diff reader with two scopes, opened from inside a chat. It
never touches the working tree, the index, or any ref.

- **This turn** shows the latest completed turn that was started from
  acp-mobile. The server stages the working tree into a temporary index
  and object directory before it forwards the prompt, does it again when
  the response arrives, and saves the diff of the two trees under
  `~/.acp-mobile/turn-diffs/<sessionId>.json`. That snapshot is
  immutable: later edits do not change it, and only the next completed
  phone turn replaces it. Turns started from Emacs or another client have
  no snapshot and the tab says `Snapshot unavailable`; the repository
  diff is never substituted. A cancelled or failed prompt, a disconnect
  mid-turn, or a server restart during a turn discards the capture.
- **Before commit** opens from the branch segment in the chat header or
  the git button on the pull-down toolbar, and is computed fresh from the
  session's repository on every open or `Refresh`: staged changes against `HEAD` (or the empty
  tree in a repository with no commits), unstaged changes against the
  index, and untracked files as additions from `/dev/null`. A partially
  staged file appears once under `Staged` and once under `Unstaged`.
  Directories that are not a Git repository get a labelled reason.

Binary files, renames without content changes, and output past the 2 MB
cap are labelled rather than rendered as code; a truncated response
says so in the summary. Rows carry one line-number column (old numbers
for removals, new numbers otherwise) and a `+` / `-` marker that stays
visible when `Hide numbers` is on; that preference is kept in
`localStorage`.

The endpoint is `GET /api/diff-review?scope=turn&sessionId=<id>` or
`GET /api/diff-review?scope=repository&pid=<multiplex pid>`; the pid
must belong to a live socket, an arbitrary path is never accepted.

## Setup

```bash
go build -o acp-mobile .
./acp-mobile [port]  # default 8090
```

The server binds to `127.0.0.1` only. On first run it generates an authkey and prints a URL with the key embedded — open that URL to authenticate.

Bundled Mermaid 11.12.2 lives in `assets/mermaid.min.js`, copied from
`~/.emacs.d/elpaca/repos/markdown-xwidget/resources/mermaid.min.js`.
Update this version note deliberately when replacing the bundle.

## Phone push (Web Push)

acp-mobile can send Web Push notifications to phones that installed it as a
home-screen web app. Requirements: an https origin (for example
`tailscale serve` in front of the port; when it proxies to this port the
link file switches to that https URL), the service worker at `/sw.js`, and
a tap on a chat's bell, which asks for notification permission and posts
the subscription to `/api/push-subscribe`. A local caller (Emacs) triggers
a push with:

```bash
curl -X POST -H "Cookie: authkey=$(cat ~/.acp-mobile/authkey)" \
  -H 'Content-Type: application/json' \
  -d '{"bufferName":"Claude Agent @ proj","title":"proj","message":"Finished"}' \
  http://127.0.0.1:8090/api/notify
```

State: `~/.acp-mobile/vapid.json` (signing keypair, generated once) and
`~/.acp-mobile/push-subscriptions.json`. Tapping a notification opens the
home-screen app on that chat.

## Session names

acp-mobile shows session names from acp-multiplex. To pass agent-shell buffer names through, set the `ACP_MULTIPLEX_NAME` environment variable when spawning the acp-multiplex process. In agent-shell, this requires injecting it into `:environment-variables` (not `process-environment`) because `acp.el` starts the process lazily:

```elisp
(advice-add 'agent-shell--make-acp-client :around
            (lambda (orig-fn &rest args)
              (let* ((buf (plist-get args :context-buffer))
                     (name-var (when buf
                                 (format "ACP_MULTIPLEX_NAME=%s" (buffer-name buf)))))
                (when name-var
                  (plist-put args :environment-variables
                             (cons name-var (plist-get args :environment-variables))))
                (apply orig-fn args))))
```

## Stopping acp-mobile

Don't use `pkill -f acp-mobile` — on macOS, `-f` can match unrelated processes (confirmed empirically: `pgrep -f acp-mobile` matches claude agent processes). Best guess is that node's `process.title` argv rewriting causes `pgrep -f` to search into environment variable data in `KERN_PROCARGS2`. Use `pkill -x acp-mobile` instead.

## Requirements

- Go 1.21+
- One or more [acp-multiplex](https://github.com/ElleNajt/acp-multiplex) proxies running
