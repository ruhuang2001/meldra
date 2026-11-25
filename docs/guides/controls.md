---
title: "Sessions and controls"
description: "Steer a running turn, queue the next request, stop cleanly, and resume a conversation later."
---

A session is one saved conversation. Closing the terminal ends execution; nothing
keeps working in the background. Pick a conversation up later with `meldra
resume`.

```sh wrap
meldra sessions            # list saved conversations
meldra resume latest       # open the most recent one in this workspace
meldra resume SESSION_ID
```

Each session gets a stable title from its first non-empty message: whitespace is
collapsed and the first 60 characters are kept. The title does not change in
later turns, so the session list stays readable.

## Commands

All of these work in the TUI and the line interface.

| Input | Action |
| --- | --- |
| `/plan` | Enter Plan once incompatible active work has settled. |
| `/build` | Enter Build within the current permission profile. |
| `/stop` | Stop the current turn and keep the session open. |
| `/steer correction` | Apply a correction to the active task. |
| `/queue request` | Save a separate follow-up request for after this turn. |
| `/continue-queue` | Apply queued requests recovered after a restart. |
| `/context [path]` | Show applicable `AGENTS.md` sources while idle. |
| `/compact [focus]` | Summarize older context while idle. |

<Note>
**Unreleased:** `/model`, `/fork`, and `/init` exist on the development branch
but are not in the latest published release.
</Note>

`Ctrl-C` exits the application; `/stop` keeps the conversation open for another
request. In the TUI, `Ctrl-X` also requests a turn stop, including while an
approval or form owns the input.

## Steer a turn in progress

Ordinary text submitted while the TUI is busy acts as steering. In the line
interface, use `/steer` explicitly; piped lines keep their FIFO order.

A correction cancels active inference, prevents unstarted calls from the old
response, and starts a new response carrying the correction and the results
already known. An admitted atomic file write may still finish before the
correction applies. A correction waits for a running command's safe boundary; use
`/stop` to cancel the command itself.

Press `Ctrl-G` in the TUI to compose a correction while an approval or MCP form is
pending. Escape returns to that interaction without submitting a correction.

## Queue work for after the turn

`/queue` keeps a follow-up request separate instead of folding it into the current
task. Queued requests keep their own identities even when their text is
identical, and are recorded durably.

After a crash, pending requests are shown on resume but are never executed
automatically. `/continue-queue` applies them; opening history or sending an
ordinary resume prompt leaves that recovered queue pending.

Control commands are acknowledged only after their record is saved. Received does
not mean applied: stopping owned processes and reconciling outcomes can take a
moment, and an invalidated approval prompt cannot authorize a replacement
operation.

<Accordion title="Unreleased: model switching, session forks, and AGENTS.md drafting">
  **`/model [name]`** shows or switches the model on the current provider. The
  choice is saved with the session and restored on resume, taking precedence over
  `config.toml` and `OPENAI_MODEL`. A switch keeps the conversation but clears the
  old provider response chain and resets token calibration, so compaction falls
  back to its byte budget until the next response recalibrates. It never edits
  configuration, changes the provider URL or key, or queries a model-list
  endpoint; the provider validates the name on the next request. Handle pending
  requests, approvals, and unknown outcomes first.

  **`/fork`** copies the retained messages, references, plan, and summary into a
  new saved session and continues there, recording the parent session ID. It is
  conversation branching, not a Git branch, worktree, or filesystem rewind:
  project files stay shared. It does not copy execution records, approvals,
  queued requests, provider response IDs, or the in-memory undo stack. The child
  inherits the compacted summary, not the parent's recorded history.

  **`/init`** expands into a repository-inspection request. In Build it uses the
  same edit approval path as any request; in Plan it proposes text without
  writing. Existing guidance is preserved and improved rather than replaced
  wholesale. Review the generated `AGENTS.md` yourself — Meldra does not certify
  the model's recommendations.
</Accordion>

## Scroll and select text

Scroll chat history with the mouse wheel or trackpad; Page Up and Page Down also
work (Fn–Up/Down on compact Mac keyboards). To select text, use your terminal's
mouse-reporting override — often Shift or Option while dragging.

## Reading the activity timeline

Consecutive file reads, directory listings, and searches collapse into a single
line that counts them, such as `Exploring ×3` while work continues and
`Explored ×12` once it finishes. Only successful exploration steps fold this way:
a failed call stays on its own line with its name, status, and reason, so an
error never disappears into a counter. Everything else keeps a separate entry.
