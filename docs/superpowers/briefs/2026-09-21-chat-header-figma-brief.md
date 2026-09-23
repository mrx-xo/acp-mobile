# Figma brief: acp-mobile chat header and composer

Handoff for an agent driving the Figma MCP. Output is a Figma file with
mockups of three header layouts for the phone chat screen, plus the current
layout for comparison. Nothing here touches the repo.

## How to work

1. Load the `figma-create-new-file` skill, then create one Design file named
   `acp-mobile chat header`.
2. Load the `figma-use` skill before every `use_figma` call. Follow it.
3. Build design tokens as Figma variables first (collection `gruvbox`), then
   frames. Bind fills and text colors to the variables, never hardcode hex
   in a node.
4. One page, four frames side by side, 393 x 852 each, named exactly
   `Current`, `A second line`, `B git strip`, `C git icon`. Add a fifth frame
   `States` showing the four state rows listed at the end.
5. When done, reply with the file URL, one screenshot of the page, and a
   list of anything in this brief you could not do. Do not add screens,
   flows, or components beyond what is asked.

## Tokens

Colors (name: hex):

- bg-hard: #1d2021 (page background)
- bg0: #282828 (header, composer, bars)
- bg1: #3c3836 (inputs, pills)
- bg2: #504945 (1px borders on bg0)
- fg: #ebdbb2 (primary text)
- fg-dim: #a89984 (secondary text)
- fg-mute: #928374 (tertiary text, disabled)
- orange: #fe8019 (actions, back arrow, labeled title)
- yellow: #fabd2f (busy)
- green: #b8bb26 (connected, send button)
- red: #fb4934 (bypass mode, stop, destructive)
- blue: #83a598 (paths)

Font: everything is monospace. Use `Iosevka Term Slab` if available, else
`SF Mono`, else `Menlo`. Sizes used: 15 semibold (title), 13 regular (bars),
12 regular (secondary), 11 regular (status line), 10 semibold (mode pill).

Phone: 393 wide. Top safe area 59px, bottom safe area 34px. Every tap target
is at least 44px tall.

## Sample content

Use these strings verbatim in every frame.

- Chat title: `dotfiles: fix keyboard dismiss` (orange, 15 semibold, single
  line, ellipsis)
- Buffer name: `*agent-shell: claude*` (11, fg-mute)
- Agent and model: `claude / fable`
- Mode pill: `Bypass` in red text on bg1, radius 999, padding 2 x 9, 10
  semibold
- Branch: `syzygy`
- Dirty counts: `3 staged  2 changed  1 new`
- Composer placeholder: `Message the agent...` (16, fg-mute)
- Messages area: fill with two placeholder bubbles, one user (right, bg1,
  radius 12) and one agent (left, no bubble, fg text), lorem is fine.

## Frame: Current

Reproduce what ships today.

Header, bg0, 1px bg2 bottom border, padding 10 x 12 plus safe area on top:

- Row 1: back arrow `<-` in orange, 18px. Title. Bell icon 18px in fg-mute.
  Kebab `...` in a 1px bg2 bordered box, radius 6, fg-dim.
- Row 2, 2px below: green dot 11px, buffer name, mode pill. Gap 8.

Bottom, stacked, all bg0:

- Review bar: full width, 44 tall, 1px bg2 top border, text 13 fg-dim with
  `Before commit:` in fg semibold followed by ` review repository`.
- Composer: 1px bg2 top border, padding 12, gap 10. `+` button 44 x 44
  fg-dim. Textarea bg1, radius 12, 44 tall, padding 10 x 16. Send button
  green, radius 12, 60 x 44, with an up arrow in bg-hard.

## Frame: A second line

Header only changes. No review bar at the bottom; composer sits directly on
the messages area.

- Row 1: unchanged from Current.
- Row 2: green dot, `claude / fable` (11, fg-dim), mode pill, then pushed to
  the right edge a git segment: branch icon 12px, `syzygy` (11, fg), then
  `+3 ~2 ?1` (11, fg-dim). The whole git segment is one tap target at least
  44 tall, overflowing the row's visual height is fine.
- When the tree is clean the counts disappear and the branch is fg-dim.

## Frame: B git strip

Header is Current's row 1 plus a row 2 of green dot, `claude / fable`, mode
pill. No review bar at the bottom.

Below the header, a strip 44 tall, bg0, 1px bg2 bottom border, padding 0 x
12:

- Left: branch icon 12px, `syzygy` (13, fg), then `3 staged  2 changed  1
  new` (12, fg-dim).
- Right: text button `View diff` (13, orange) and a close `x` 44 x 44
  fg-mute.

## Frame: C git icon

Header row 1 gains a git branch icon button 28 x 28 between the title and
the bell, fg-mute, with a 6px orange dot at its top right when dirty. Row 2
is green dot, `claude / fable`, mode pill. No review bar, no strip.

## Frame: States

Four header rows stacked with 24px between them, each labeled on the left in
fg-mute 11:

1. `connected, clean`: green dot, no counts, branch fg-dim.
2. `busy`: dot is yellow, title unchanged.
3. `reconnecting`: dot replaced by `Reconnecting...` in yellow 11.
4. `disconnected`: `Disconnected` in red 11, mode pill hidden, git segment
   hidden.

Build these on layout A. If time is short, skip the States frame and say so.
