# New chat stepper

Date: 2026-09-19
Status: approved in chat, pending spec review

## Problem

The New chat sheet packs the project row, a horizontally scrolling preset
chip strip, four setting rows, the first-message box and two icon tools into
one screen, with ten sub-views hanging off it. On a phone it is cramped, the
chip strip is hard to use, and a repeat launch such as "Fable in dotfiles"
takes as many taps as a brand new one.

Comfort matters more than tap count. Each decision gets a full screen.

## Goals

- One decision per screen, tall lists, big tap targets, no horizontal strip.
- A repeat launch is: tap a remembered combo, type the prompt, start.
- Combos can be pinned.
- No server changes. Everything new lives in `index.html` and localStorage.

## Non-goals

- Popularity counters. Recency is enough.
- Custom labels for pins. Labels derive from the preset and project.
- Changes to rig presets, the spawn API, or session polling.

## Screens

All screens live inside the existing `#spawn-sheet`, one full-height view at
a time, selected by `spView`. The header keeps the close and back icons and
the centred title. The footer holds one full-width action per screen.

Back goes Prompt to Agent, Agent to Project, Project to Home. When Home
would be empty, Project shows the close icon instead of back. Escape follows
the same path.

### Home (`New chat`)

- Two lists, `Pinned` then `Recent`. A heading is omitted when its list is
  empty.
- Each row: preset label as the title, project name plus short path as the
  detail, and a pin icon on the right that toggles pinned state. A pinned
  combo leaves `Recent` and appears under `Pinned`; unpinning moves it back
  to the top of `Recent`.
- Tapping a row copies the combo into the draft and opens Prompt.
- Footer button `Start from scratch` opens Project.
- When both lists are empty the sheet opens directly on Project. The close
  icon still closes the sheet from there.

### Project (step 1 of 3)

- The existing project picker becomes the whole screen: search box, then
  `Pinned`, `Recent`, `Projects` groups, each row with a pin icon. Project
  pins and recents keep their current localStorage keys.
- Footer button with the folder icon opens the typed-path screen, unchanged.
- Picking a project stores it in the draft and advances to Agent.
- Title shows `Project` with `1 / 3` beneath it.

### Agent (step 2 of 3)

- A tall list of presets: rig presets in rig order, then custom presets, each
  showing label and `agent / model / mode / effort`. Custom presets keep
  their delete icon. The selected preset is highlighted.
- Tapping a preset applies it and advances to Prompt.
- Below the presets a `Customize` row with a chevron. Tapping it expands the
  four existing rows for agent, model, permissions and effort in place. Each
  opens its existing sub-picker and returns to Agent with the section still
  expanded. The selected preset highlight clears once any field differs from
  it.
- `Customize` starts expanded when the draft settings match no preset.
- When the section is expanded and the settings are complete, a `Save as
  preset` row appears under the four fields and opens the existing save
  screen.
- Footer button `Next` is enabled only when settings are complete. It is the
  only way forward after customizing.
- Title shows `Agent` with `2 / 3` beneath it.

### Prompt (step 3 of 3)

- A summary card at the top: first line is the preset label, or the agent
  name followed by `Custom` when no preset matches; second line is the
  project name and short path. Tapping the card opens Agent. The back icon
  also opens Agent.
- The textarea fills the remaining height. Placeholder stays `What are we
  working on?`. The first message stays optional.
- Under the textarea a collapsed row `Chat name / optional`. Tapping it
  reveals the existing name input in place. The row stays open while the
  draft has a name.
- Footer button `Start` launches. Behaviour on success, partial failure and
  recovery is unchanged, except that the recent combo list is updated on
  success.
- Recovery messages and the return-to-draft icon appear on this screen.
- Title shows `Prompt` with `3 / 3` beneath it.

### Entry points

- The `+` button opens Home, or Project when Home would be empty.
- The per-project `+` in the chat list opens Agent with that project already
  in the draft.
- Reopening the sheet resumes the saved step. A draft saved on Prompt with
  an empty project or incomplete settings resumes on the first incomplete
  step instead.

## Data

### New localStorage key `syzygy.launch.combos`

```json
{"pins":[{"cwd":"/src/dotfiles","preset":"f","settings":{"agent":"claude","model":"fable","mode":"bypass","effort":""}}],
 "recent":[...]}
```

- A combo is `cwd` plus the four settings plus the preset key at launch time.
  The preset key is informational; the label is recomputed from settings on
  every render so a renamed rig preset shows its new name.
- Two combos are the same when `cwd` and all four settings match.
- On a successful launch the combo moves to the front of `recent`, unless
  it is pinned. `recent` is capped at 8.
- Malformed entries are dropped on read, matching how pins and presets are
  validated today.

### Label rule

Preset label when the combo settings equal a preset from the rig or the
custom list, otherwise the agent name followed by ` / Custom`. Used on Home
rows and the Prompt summary card.

### Draft

`syzygy.launch.draft` gains a `step` field holding the last open view. The
existing fields, the pending buffer and the keeps-task flag are unchanged.

### Removed

- The preset chip strip, its CSS, and the horizontal swipe handling.
- The `Preset / Modified` label and the reset icon. Customize replaces both.
- The bottom tools row on the old main view. Its two actions move into
  Agent (save) and Prompt (chat name).

## Error handling

- Presets or catalog failing to load shows the existing status message on
  whichever step is open. Agent still lists custom presets and the
  Customize rows.
- A combo whose settings are no longer valid for the catalog still lands on
  Prompt; `Start` stays disabled and the summary card reads `Custom` so the
  user taps through and fixes it on Agent.
- Launch failures keep today's recovery flow.

## Accessibility

- Step titles are announced through the existing `aria-labelledby`.
- Each row is a button with a full label. Pin icons keep `aria-pressed`.
- Focus moves to the back icon when a step opens and returns to the row that
  opened a sub-picker, matching the current picker behaviour.

## Testing

Chrome-driven tests in `new_chat_ui_test.go` and the spawn cases in
`ui_test.go`.

- Update existing tests to walk Project, Agent, Prompt instead of the main
  view. Delete the chip swipe test.
- New: sheet opens on Project when nothing is pinned or recent.
- New: launching writes a recent combo; reopening shows it on Home; tapping
  it lands on Prompt with the draft prefilled and `Start` enabled.
- New: pinning a combo moves it to `Pinned` and survives a reload.
- New: Customize clears the preset highlight, `Next` gates on completeness,
  and the summary card reads `Custom`.
- New: reopening resumes the saved step.
- Visual review screenshots for the four screens at phone width.
