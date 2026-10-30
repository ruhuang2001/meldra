# Context compaction acceptance and failure matrix

This matrix was written before the compaction implementation. The repeatable
real-binary verifier is `scripts/compaction-e2e.py`; it uses private HOME/state/
workspace directories and a loopback Responses fixture, with update checks
disabled. No live model or credential is used.

| Scenario | Required observation |
| --- | --- |
| Manual line and PTY | `/compact [focus]` runs while idle, renders a notice and inspection command, and permits a subsequent turn. |
| Original requests | A request longer than the display snapshot limit remains complete in the artifact and next inference; repeated compaction does not duplicate it. |
| Folded tools | Every folded call ID, name, outcome, argument digest and artifact reference remains inspectable; original call records and artifacts do not change. |
| Repeated compaction/resume | The second summary replaces the first, the bounded recent tail survives, reference snapshots remain immutable, and resume sends no previous response ID. |
| Odd retained tail | A keep count of three may begin with an assistant reply; repeated compaction retains each request exactly once in the conversation evidence. |
| Folded images | A folded image reference still sends its original image bytes and matching declaration once on resume after the source file is deleted. |
| Steering | A correction received during inference keeps its control ID and attachment evidence through repeated compaction and resume. |
| Compaction controls | Early PTY compaction with no foldable history returns to usable input; steering automatic summary inference cancels it and continues the active turn. |
| Plan mode | Summary inference has no tools, does not change mode, preserves the plan, and creates no workspace side effect. |
| Busy command | Submitting `/compact` during inference reports the idle-only restriction without steering or cancelling the active turn. |
| Empty, oversized, incomplete or tool-bearing summary | Reject the response, preserve summary/messages/response ID, and dispatch no returned tool. |
| Provider error or cancellation | Preserve the pre-compaction session and permit explicit recovery. |
| Artifact write failure | Preserve the entire conversation snapshot and create no completion event. |
| Snapshot write failure | Retain artifact evidence, preserve the original snapshot, stop before further inference, and create no completion event. |
| Event write failure | Keep the already-durable compacted snapshot, stop, and do not fabricate a successful event. |
| Unknown call/process or pending approval | Refuse with a count and inspection command; do not summarize uncertain work or mutate existing records. |
| Corrupt summary artifact | Fail closed on repeated compaction/resume, without provider inference or replayed tools. |
| Threshold | Trigger at the configured UTF-8 byte percentage of the smaller applicable custom/native budget; zero disables; an unchanged projection skips. |
| Native continuation | A verified Go overlay changes only endpoint classification; the ordinary native chain uses the prior response ID, compaction clears it, and the next turn rebuilds context. |
| Summary wire placement | Subsequent native/custom inference carries the delimited summary in request instructions, subordinate to runtime policy. |
| Retained calls | Calls outside the fold retain bounded original arguments, argument hash, result status/output and outcome artifact references in the rebuilt request. |
| Event-gap resume | After snapshot success and event failure, explicit resume uses that snapshot without repeating summary inference or inventing a completion event. |
| SIGKILL before snapshot | Hold the existing session save lock, observe committed artifact evidence, kill the process, then verify the original snapshot and ordinary explicit recovery. Never unlink the lock. |
| Inspection completeness | JSON inspection resolves every folded ID to its original call record; corrupt compaction artifacts fail both human and JSON inspection. |
| Large ledger | More than 128 folded calls keep a bounded event list and complete artifact/inspection lists; a compaction beyond 1,000 events remains discoverable. |
| Credential reflection | A configured credential in summary output rejects the summary; a credential in compaction source rejects before summary inference. Preserve original context and create no compaction artifact. |
| Config boundaries | Show defaults and configured values; reject out-of-range keep/threshold/native-budget values before provider traffic. |

`report.json` records each result, the binary hash, source hashes, platform and
repeat command. Provider requests, terminal transcripts, before/after snapshots,
task records and digest-checked artifacts are retained alongside `checksums.json`.
SQLite triggers and deliberate isolated-state mutations inject persistence and
unresolved-record failures without changing application source. Native fixtures
record overlay provenance and prove the overlay is active before use.
