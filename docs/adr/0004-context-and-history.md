# Context measurement and replay

The byte budget remains a hard trigger. Token-based compaction and the footer estimate require a configured model window and a real prompt measurement. Pair local text bytes with the first provider input-token count in a turn, before appended tool results distort the ratio. Calibration lives in memory and resets across models and resumes. Attachments use a separate budget. Measuring a request must leave its instruction-presentation state intact.

Manual compaction acquires the same leases as a turn and clears the prior request watermark. Automatic compaction excludes the newly journaled incoming request. Validate existing evidence even on an empty fold and skip a summary request when an empty summary could not reduce the context. Steering interrupts inference so the foreground owner can journal and apply it. Once snapshot replacement starts, finish the snapshot and event commit despite cancellation. Record any later audit gap. Protocol clients consume the ledger event and foreground interfaces can show a notice.

Digest-checked evidence restores full recent requests and tool outcomes when compaction replaces the provider chain. A bounded display preview cannot reconstruct the complete input. Preserve repeated requests using ordered content and control identity for old snapshots, and recover the matching turn when the retained tail starts with an assistant. Identify inherited fork messages only from the matching leading snapshot and recorded fork. Summaries enter provider instructions once as quoted historical data. Runtime credentials can detect reflected secrets during compaction and stay out of persisted history.

File references preserve submitted bytes, structured paths and evidence identities. Quoting prevents file contents from forging attachment headers. Select the whole history before declaring current and historical images. Resolve queued references within their control lifetime and keep their scoped instructions separate until applied.

[Compaction scenarios](../engineering/verification/compaction-failures.md) · [Image scenarios](../engineering/verification/image-failures.md)
