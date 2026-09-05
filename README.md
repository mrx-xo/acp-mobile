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
- **Auth** — random 256-bit authkey (generated on first run, stored in `~/.acp-mobile/authkey`)
- **Security hardening** — CSRF protection, DNS rebinding protection, CSP headers, XSS-safe markdown

## Setup

```bash
go build -o acp-mobile .
./acp-mobile [port]  # default 8090
```

The server binds to `127.0.0.1` only. On first run it generates an authkey and prints a URL with the key embedded — open that URL to authenticate.

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
