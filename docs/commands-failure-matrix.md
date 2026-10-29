---
title: "Custom command verification"
description: "Failure cases and repeatable E2E evidence for custom slash commands."
---

**Unreleased.** Written before implementation. Verify through the real binary
with `python3 scripts/commands-e2e.py --binary dist/meldra --output dist/commands-e2e`.
The output directory must be new. Keep fixtures, provider requests, terminal
transcripts, task records, and the checksummed report for repeatable review.

| Boundary | Failure to prevent | Expected evidence |
| --- | --- | --- |
| Offline discovery | Listing needs credentials or submits a model request | Empty, text, JSON, and help listings finish offline |
| Precedence | A user command shadows a project command or a builtin | Project winner; builtin and duplicate diagnostics |
| Metadata | Malformed YAML, duplicate keys, invalid names/types or empty body enters catalog | Invalid entries rejected; valid quoted/folded/Unicode metadata preserved |
| Limits | More than 128 commands, files over 32 KiB or descriptions over 512 Unicode characters enter catalog | Exact boundaries accepted; overflow rejected without truncation |
| Files | Symlinks, hard links, FIFOs or protected state disclose content or block discovery | Invalid sources rejected; no sentinel in requests |
| Expansion | Argument whitespace is lost, placeholders expand repeatedly, or missing positions remain | Exact message for `$ARGUMENTS`, `$1` through `$9`, missing and literal arguments |
| Ordinary messages | Expanded text recursively invokes controls or another command | Expanded slash text reaches model as a user message |
| Bounds | Repeated placeholders grow a request beyond the message limit | Expansion rejected before task or model submission |
| Policy | Custom command grants write/exec authority in Plan or bypasses approval | Denied/declined calls, unchanged workspace and durable records |
| Controls | Busy steering/queued requests expand twice or skip durable history | Expanded text retained in control events and provider input |
| TUI | Slash picker hides builtin/custom entries or changes form ownership | PTY completion, invocation, Escape and builtin operation |
| Recovery | New sessions retain stale command bodies | Restart/resume discovers current files; prior expanded history remains |
| Evidence | A report cannot identify its source/binary or failures exit zero | Source/binary hashes, scenario outcomes, retained traces, nonzero failures |
