# Figma brief: acp-mobile New chat stepper

Handoff for an agent driving the Figma MCP. Output is one new page in the
existing SYZYGY design file with six phone frames showing the New chat
stepper described in
`docs/superpowers/specs/2026-09-19-new-chat-stepper-design.md`. Read that
spec first; this brief only says how to draw it. Nothing here touches the
repo.

File: https://www.figma.com/design/iUV2tD2yHjWE1sU5eEjcPb/SYZYGY

## How to work

1. Load the `figma-use` skill before every `use_figma` call. Follow it.
2. Do not create a new file. Add one page to the SYZYGY file, named exactly
   `06 New Chat stepper`, placed after `05 New Chat exploration`.
3. Reuse what the file already has. Colors and text styles come from the
   file's existing variables and text styles (the Foundations collection
   the components are bound to). Never hardcode hex or font values in a
   node. Read the descriptions on the `02 Components` page before using a
   component.
4. Reuse these components from `02 Components`: `Bar / Chat header` is the
   pattern for the sheet header, `Control / Model row` for every list row,
   `Icon` variants `pin`, `search`, `plus`, `send`. Copy the header,
   status area, footer button, list row and section label styling from the
   frames on `05 New Chat exploration` so the two pages look like the same
   app. Where an icon is missing (back arrow, close, chevron right, chevron
   down, folder, save, check), draw a 20px stroke vector in the icon's
   color and name it `Icon / <name>`.
5. Every frame is 393 x 852 with the same status area, header, body and
   footer split as page 05: status area 44, header 56, body fills, footer
   98 with one full-width 48 tall button inside 16px margins. Frames sit in
   one row, 48px apart, in the order below. Above the row, copy page 05's
   title block and flow overview strip, one entry per frame, using the
   frame names and the one-line captions given below.
6. When done, reply with the page URL, one screenshot of the page, and a
   list of anything in this brief you could not do. Do not add screens,
   flows, or components beyond what is asked.

## Header pattern

- Left slot, 44 x 44: close icon on Home, back arrow on every other step.
- Centre: step title, 15 semibold, primary text color. On the three step
  screens a second line beneath it, 11 regular, tertiary text color, with
  the step counter: `1 / 3`, `2 / 3`, `3 / 3`.
- Right slot, 44 x 44, empty, to balance the title.

## Row pattern

Every list row is a button at least 56 tall, full width inside 16px
margins, bg1 surface, radius 12, 12 x 16 padding, 12px vertical gap between
rows. Title 15 regular in primary text, detail 12 regular in secondary text
on the line below. A selected row gets a 1px orange border and an orange
title, like the `Selected` rows on page 05. Section labels are 11 regular,
letterspaced, tertiary text, 16px above their first row.

Pin icon: 44 x 44 tap target at the row's right edge, `Icon / pin`,
tertiary color when unpinned, orange when pinned.

## Sample content

Use these strings verbatim.

Projects (name, short path):

- `acp-mobile`, `~/src/acp-mobile`
- `dotfiles`, `~/.dotfiles`
- `atlas`, `~/atlas`
- `docs`, `~/docs`
- `fleet`, `~/fleet`

Presets (label, detail line):

- `Sol · Full`, `codex / sol / full access / high`
- `Opus · Plan`, `claude / opus / plan / medium`
- `Fable · Bypass`, `claude / fable / bypass / agent default` (custom
  preset, shows a delete icon on the right)

Settings rows for Customize: `Agent` / `Codex`, `Model` / `Sol`,
`Permissions` / `Full access`, `Reasoning effort` / `High`.

Prompt placeholder: `What are we working on?`. Chat name placeholder:
`Chat name`.

## Frame 1: Home

Caption: `Tap a remembered combo, then type.`

Header: close icon, title `New chat`, no counter.

Body, top to bottom:

- Section label `PINNED`.
- One combo row: title `Sol · Full`, detail `acp-mobile  ~/src/acp-mobile`,
  pin icon orange.
- Section label `RECENT`.
- Three combo rows with pin icon tertiary:
  - `Opus · Plan`, `dotfiles  ~/.dotfiles`
  - `Fable · Bypass`, `acp-mobile  ~/src/acp-mobile`
  - `Sol · Full`, `atlas  ~/atlas`

Footer button: `Start from scratch`, secondary style (bg1 surface, primary
text, 1px bg2 border), radius 12.

## Frame 2: Project

Caption: `One decision per screen, 1 of 3.`

Header: back arrow, title `Project`, counter `1 / 3`.

Body:

- Search field 56 tall, bg1, radius 12, search icon left, placeholder
  `Search name or path` in tertiary text. Same as page 05.
- Section `PINNED`: `acp-mobile`, `dotfiles`, both with orange pin.
- Section `RECENT`: `atlas`, `docs`, tertiary pin.
- Section `PROJECTS`: `fleet`, tertiary pin.

Rows show name as title and short path as detail. No selected state on
this screen.

Footer button: folder icon plus `Enter a path`, secondary style.

## Frame 3: Agent

Caption: `Presets first, 2 of 3.`

Header: back arrow, title `Agent`, counter `2 / 3`.

Body:

- Section label `PRESETS`.
- Three preset rows from Sample content. `Sol · Full` is selected. The
  custom preset `Fable · Bypass` has a trash icon in a 44 x 44 target at
  the right edge, tertiary color.
- Below the presets, 24px gap, a row `Customize` with a chevron-right icon
  at the right edge. Same row style, no detail line.

Footer button: `Next`, primary style (green surface, dark text, like the
`Start chat` button on page 05), enabled.

## Frame 4: Agent / Customize

Caption: `Customize clears the preset.`

Same as Frame 3 with these changes:

- No preset row is selected.
- The `Customize` row's chevron points down.
- Beneath it, indented 0, the four settings rows from Sample content, each
  with a chevron-right at the right edge, except `Model` reads `Astra`
  instead of `Sol` to show the drift from the preset.
- Under those four, a row `Save as preset` with a save icon at the right
  edge.

Footer button: `Next`, primary style, enabled.

## Frame 5: Prompt

Caption: `Summary, then type, 3 of 3.`

Header: back arrow, title `Prompt`, counter `3 / 3`.

Body:

- Summary card: bg1, radius 12, 16 padding, full width. Line 1 `Sol · Full`
  15 semibold primary text. Line 2 `acp-mobile  ~/src/acp-mobile` 12
  secondary text. Chevron-right at the right edge, tertiary.
- Textarea: bg1, radius 12, 16 padding, fills the remaining body height
  down to the chat name row. Placeholder `What are we working on?` 16
  regular tertiary text at the top left.
- Chat name row, 44 tall, bottom of body: `Chat name` 13 secondary text,
  then `optional` 11 tertiary, chevron-right at the right edge.

Footer button: `Start`, primary style, enabled.

## Frame 6: Prompt / Custom

Caption: `Invalid combo lands here, Start disabled.`

Same as Frame 5 with these changes:

- Summary card line 1 reads `Codex / Custom`.
- Chat name row is expanded: chevron points down and a 44 tall input sits
  beneath it, bg1, radius 12, containing the text `fix keyboard dismiss` in
  primary text.
- Textarea is shorter to make room, at least 120 tall.
- Footer button `Start` is disabled: 40 percent opacity.

If time is short, skip Frame 6 and say so.
