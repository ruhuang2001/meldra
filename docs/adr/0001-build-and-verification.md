# 0001. Build provenance and verification isolation

## Builds and release repairs

Local binaries embed the Git revision so a reported failure can be traced to its source. Builders can set `VERSION` for a reproducible version string. Release Please owns release notes and GoReleaser uploads the archives. A release created with the workflow token cannot start a second workflow through the release event. Manual dispatch lets maintainers repair an interrupted upload.

Release verification takes its driver from the reviewed workflow revision and checks out application source and E2E fixtures at the immutable tag. Older tags can use their own Make entry point. Both historical output variables point outside the distribution directory so a repair preserves verification evidence. Release checks serialize package builds and tests to keep storage stress tests from starving deadline-sensitive children. Isolated helpers can discard Python’s bytecode environment setting, so the clean-checkout step removes generated, untracked bytecode before it checks the tag.

## CI and local checks

Each main-branch run gets its own concurrency group so a later push cannot evict a pending check. Complementary app test filters place expensive process and output tests on one shard and keep new tests, examples, and fuzz seeds covered. One shard writes the Go cache. Other shards can restore a compatible prefix after dependency changes because Go hashes its compilation inputs. Running a pinned govulncheck through Go reuses the compiler cache.

The staged check exports the index and creates an index in the exported tree for source fingerprinting. Cleanup signals the entire helper process group even after Make exits because descendants can remain alive. The full check runs race and coverage tests separately to reduce contention in timing-sensitive fixtures. E2E workers run independent suite processes with their own homes, workspaces, logs, ports and coverage. They resolve shared Go cache paths before changing HOME. Pass explicit harness controls into isolated environments and disable the race runtime’s one-second exit sleep in short-lived helpers.

Start expensive suites first within the selection for the current mode and source revision. Historical tags can lack newer suites or command-line flags. Probe for support before requesting opt-in scenarios that wait past real deadlines.

## E2E evidence

- For terminal teardown, verify enhanced input was enabled before accepting the reset sequence. The TUI exposes its mode in the composer badge. Line mode needs a transcript notice because it has no persistent badge.
- Keep stdin open when reproducing cancellation or teardown so EOF cannot satisfy the test. Pass an explicit prompt when testing the unsupported-device guard. Check for leaked processes before fixture cleanup removes them.
- OAuth cancellation tests keep the process alive long enough to detect an extra token refresh. Rotate the fixture’s token identity to simulate server-side revocation. Retain grant types and request counts as evidence while keeping credentials and authorization codes private.
- Browser repair tests keep acceptance code outside the model’s workspace. Vary products and quantities to expose fixed totals, and capture the phone viewport at its CSS width to preserve clipping evidence. Retain failed and interrupted attempts even when a secondary audit fails. The visual fixture embeds its challenge in an image so the screenshot provides the answer. Live inference reads the configured key and keeps credentials and request headers out of logs.
- Web tests probe the overlay with a harmless version request before sending traffic. Set the build working directory to its canonical spelling because Go can honor a PWD alias while overlay entries use canonical paths. Exercise both dotted TOML keys and the equivalent table form.

## Benchmark evidence

Choose SWE-bench datasets that supply the per-instance image used for local verification. Keep the hosted evaluator’s subset identifier alongside the dataset name. Run the local evaluator in its output directory to collect its logs and report. Protect prompt instructions from headings and unmatched fences in issue text. Preserve existing result rows before repairing old or damaged attempts.

Use file-backed child output so surviving descendants cannot keep a capture pipe open. On timeout, allow the agent to cancel its turn and reap owned processes. If the parent stalls, verify captured membership before signalling a child group and exclude the harness’s own group. Shutdown tests allow time for interpreter startup under load and accept a dead orphan awaiting OS reaping.

Stage new files when capturing patches. Failed attempts retain partial diffs as evidence and are excluded from predictions. A successful attempt may produce an empty patch.

## Provider examples

Examples use placeholders for the model and endpoint. The remote example assumes Responses API compatibility and keeps the API key in the credentials file. See [provider configuration](../configuration.md).
