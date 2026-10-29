# Stream JSON failure matrix (Unreleased)

The real-binary scripts/capability-e2e.py --stream-json suite retains transcripts,
ledger snapshots, request logs, a report and checksums. Before implementation:

- Human banners, previews, diagnostics, ANSI or update notices corrupt stdout.
- Envelope keys/types change, sequence regresses, reasoning/credentials leak.
- Streaming deltas lack a complete message or tool events lose correlation.
- Noninteractive mode consumes stdin, hangs for approval or runs later calls.
- Pending approval does not exit 3 or loses its durable waiting state.
- Wrong, replayed, cross-task or obsolete hashes permit execution.
- Resume grants changed arguments, files, command specs, policy or project rules.
- An approval grants a second operation or expands Plan permissions.
- Persistence/output failure permits effects or claims successful completion.
- Resume repeats completed effects, executes unknown work or bypasses leases.
- Usage errors exit 1; runtime errors omit closure; text behavior changes.
