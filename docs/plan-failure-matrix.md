# Planned capability failure cases

These cases are specified before implementation. Real-binary E2E reports retain
provider requests, CLI transcripts, session snapshots, and ledger inspection.

## Regex search and session titles

- Omitting `regex` or setting it false must preserve literal matching.
- Invalid regular expressions must fail the tool without failing later calls.
- RE2 syntax, Unicode matches, empty results, and capped result counts must work.
- A matching long line must retain the existing bounded output and cancellation.
- Titles must use the first nonempty user message, remain stable across resume,
  and truncate by Unicode character, not bytes, at 60 characters.
- New metadata must be accepted by both full session loading and list loading.
- Legacy snapshots without a title must remain readable.

## Image file references

- Image bytes must be captured before inference and stored only as immutable
  artifact references in session JSON, never embedded base64.
- Resume and accepted controls must retain original image bytes after file edits.
- Symlinks, protected paths, oversized images, unsupported formats, corrupt image
  headers, and line ranges on images must fail before inference.
- Both initial and subsequent tool requests must include images where required
  by provider history semantics; text attachments must retain existing behavior.

## Context compaction

- Cancellation, provider errors, empty summaries, oversize summaries, and storage
  failures must preserve the uncompressed conversation.
- Compaction must preserve original requests verbatim and recent call evidence;
  no admitted side effects or outcomes may be rewritten or replayed.
- Both Plan and Build may compact metadata. Native continuation IDs must reset.
- Ledger evidence must identify folded call IDs and retained artifacts.
