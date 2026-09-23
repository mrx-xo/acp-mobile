# Chat header and pull-down toolbar

Date: 2026-09-23
Status: approved in chat

Figma: `SYZYGY` file, page `Chat Study`, frames `D toolbar closed` and
`D toolbar open`. The header text described below supersedes the row 2
shown in those frames.

## Problem

The chat header shows raw data: the Emacs buffer name as the title
(`Claude Agent @ acp-mobile<4>`) and the raw mode id as a pill
(`bypassPermissions`). The kebab menu holds eight items of very different
frequency. The `Before commit: review repository` bar takes a permanent row
above the composer.

## Goals

- Title and second row read as designed text, not data.
- Agent shown by provider icon, not name.
- Connection and busy state shown without the status dot.
- Git branch and dirty state in the header. One tap to the review.
- Frequent actions on a pull-down toolbar. The kebab keeps the rest.
- The bottom review bar goes away.

## Non-goals

- Changes to the review screen itself, the mode picker, or the model picker.
- Server-side session status. Busy state stays client-side.

## Header

Two rows in `#header`, same padding as today.

### Row 1

- Back arrow, title, bell, kebab, unchanged in order.
- The kebab becomes a bare 28px icon button like the bell. No border.
- Title rule: the user label when set, in orange. Otherwise the project
  name from the session in fg, followed by the buffer ordinal in fg-mute
  when the buffer name ends in `<N>`. `acp-mobile 4`. Falls back to the
  buffer name, then the agent title, then `ACP`.

### Row 2

One line, 11px mono, segments separated by ` · ` in fg-mute:

1. Provider icon 14px from `providerIcon()`, then the current model name in
   fg-dim. The model name comes from `/api/models` fetched once per chat
   open and refreshed after the picker changes it. While unknown, only the
   icon shows.
2. Mode as a colored word, no pill, no background. Color from
   `modeColor()`. Label from a short-name map: `bypassPermissions` ->
   `bypass`, `acceptEdits` -> `accept edits`, `plan` -> `plan`, `default`
   -> `ask`; unknown ids fall back to the agent's `name` lowercased. Tap
   opens the mode picker, same as the pill today.
3. Branch name in fg, prefixed by a 12px branch icon. An orange 6px dot on
   the icon when the tree is dirty. Tap opens the review on the repository
   scope. Hidden when the session has no git status.

### Status

- The status dot is removed. `#status-text` stays as a visually hidden
  live region so the socket code and its node tests are untouched.
- The provider icon shows steady state: full color when the socket is
  connected, 40% opacity when disconnected or waiting for a session.
- The existing `.load-line` under the header shows transitions: yellow
  sweep while connecting, reconnecting, or while the agent is replying;
  red static line when disconnected. Hidden when connected and idle.
- Error text such as `Reconnecting...` moves into the line's `aria-label`
  and stays announced through `role=status`.

### Grabber and toolbar

- A 36x4 pill in bg2 centered under row 2, inside the header, in a 12px
  zone that is a 44px tap target through padding.
- Tap or drag down opens the toolbar. Tap or drag up closes it. Open
  state persists in localStorage key `acp-toolbar-open` across chats.
- The toolbar is a 62px row under the header, bg0, 1px bg2 bottom border.
  Six items, each an icon over a 10px label, 56px wide, spaced evenly:
  1. Git: branch icon with the dirty dot, label is the branch name. Opens
     the review on the repository scope. Disabled with fg-mute when there
     is no git status.
  2. Model: chip icon, label is the model name or `model`. Opens the model
     picker.
  3. Pinned: pin icon. Opens pinned messages.
  4. Fork, 5. Clone: existing actions.
  6. Catalogue: bookmark icon, label flips to `Catalogued` and the icon
     fills when the chat is catalogued. Toggles catalogue state.
- The kebab keeps `Show turn nav`, `Pin chat`, `Kill session`, and
  `Uncatalogue` shown only while the chat is catalogued, since the toolbar
  button only saves or edits.
- Opening the toolbar does not scroll the message list; the list shrinks.

### Removed

- `#review-bar` and `#review-repo-btn`, their CSS, and the enable logic.
- `#mode-btn` pill styling. The element stays as the mode segment.
- `#status-text`, `#header-buf`.
- Kebab items `Pinned`, `Clone`, `Fork`, `Model`, `Catalogue`.

## Server

### `GET /api/git-status?pid=PID`

Read-only. Resolves the pid to its cwd like the repository review does and
returns:

```json
{"branch":"syzygy","staged":3,"unstaged":2,"untracked":1}
```

- `branch` is the short symbolic ref, or `detached at <short>`.
- Counts come from `git status --porcelain=v1 -z` with
  `GIT_OPTIONAL_LOCKS=0`. A path that is both staged and unstaged counts
  once in each.
- 404 when the pid has no live socket, 200 with `{"branch":""}` when the
  cwd is not a git repository. Same 2 MiB output cap as the diff reads.

### Client fetch policy

Fetched when a chat opens, after each `turn_complete`, and when the
toolbar opens. Never polled on a timer. A failed fetch keeps the last
value and clears it on the next chat switch.

## Testing

Go: `TestGitStatusHandler` in `diff_test.go` covering a clean repo, a
partially staged file, an untracked file, a non-repo cwd, and an unknown
pid.

Chrome, in a new `header_ui_test.go` at 393x852:

- Title shows `acp-mobile` plus `4` from buffer name
  `Claude Agent @ acp-mobile<4>`, and the label when one is set.
- Row 2 shows the anthropic icon, the model name after `/api/models`
  resolves, `bypass` in red for `bypassPermissions`, and the branch from a
  stubbed `/api/git-status`.
- Disconnecting the socket dims the icon and shows the red line;
  reconnecting shows the sweep.
- Grabber tap opens the toolbar with six items, the message list height
  shrinks by the toolbar height, the state survives a reload, and the git
  item opens the review on the repository scope.
- The kebab lists exactly three visible items for an uncatalogued chat.

Existing tests that click `#review-repo-btn` or read `#status-text`
change to the new elements. Update `README.md` where it names the review
bar entry point.
