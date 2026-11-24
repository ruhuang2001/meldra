---
title: "Context compaction"
description: "Keep long sessions inside the model's context window with manual /compact and the automatic threshold."
---

Every model request rebuilds text context from the saved conversation: recent
messages, the plan, the summary, and attachment evidence. When that rebuilt
context grows past a threshold, Meldra can replace older messages with a summary
written by the configured model.

Compaction is available from v0.5.0 and is on by default.

## Compact on demand

```text wrap
/compact
/compact the flaky integration test
```

`/compact [focus]` summarizes older messages while keeping the most recent ones
verbatim (8 by default). The optional focus steers the summary toward the work you
name. Run it while idle, in the TUI or the line interface, in Plan or Build mode;
submitting it during a turn is rejected. It changes conversation metadata only —
no tools run, and mode, permissions, and approvals are untouched.

## Automatic compaction

Automatic compaction runs the same path before inference when locally rebuilt text
context reaches the configured byte threshold (80% by default). A summary request
carries no tools and may produce at most 1024 output tokens. Cancelled, failed,
empty, oversized, or truncated summaries are not committed, and automatic
compaction that cannot reduce context is skipped.

Automatic compaction makes an extra request to the configured model, which can add
latency and provider charges. Set `compact_threshold_percent = 0` in
`config.toml` to disable the trigger; `/compact` stays available. See the
[configuration reference](../reference/configuration.md#context-compaction) for
the threshold, retention, and byte limits.

The threshold counts UTF-8 bytes of the locally rebuilt text context, not model
tokens. Native Responses keeps server-side history that Meldra cannot measure, so
the threshold does not guarantee the provider's window has not been exceeded. Use
`/compact` deliberately when a long native conversation matters.

## What compaction preserves

A successful compaction keeps the plan, the recent messages, and their attachment
references. Its immutable artifact retains the original user requests verbatim,
plus the folded tool-call identities, statuses, argument digests, and outcome
artifact references. The next turn rebuilds context from that evidence and the
summary, without continuing the old provider response chain.

The summary also appears as quoted historical data in the developer instructions.
It is subordinate to your current request and the runtime policy: a summary can
never authorize an operation.

Compaction refuses source text or a generated summary that reflects a configured
model or search credential, without printing the credential. Unknown secrets you
typed into ordinary task data are not automatically identifiable.

## When compaction is blocked

Unknown tool outcomes, unknown process effects, and pending approvals block manual
compaction, and skip automatic compaction with a notice. Resolve them first:
summarization cannot turn uncertainty into success.

A successful notice names the source turn range and gives you
`meldra task show TASK_ID`. That command, including `--json`, exposes every folded
call ID and the original execution records.

Persistence failures stop the turn. A failure before the snapshot commit leaves
the old summary and response chain unchanged; a failure after commit leaves a
consistent snapshot with an audit gap. Inspection and recovery never rerun the
summarization request.
