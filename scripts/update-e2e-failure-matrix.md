# Update E2E failure matrix

These unreleased expectations are exercised by `scripts/update-e2e.py` using
real, versioned CLI binaries and loopback release/provider fixtures.

| Failure or regression | Observable evidence |
| --- | --- |
| Metadata lookup blocks without progress | Pipe and PTY transcripts contain the checking notice while the metadata response is still gated. |
| Checksums or archive download blocks without progress | Stage notices are visible before each gated HTTP response completes. |
| Successful replacement hides the version change | Ordered transcript states both old and new versions; installed binary reports the new version and matches its SHA-256. |
| Success still warns about existing sessions | Success output and update help omit the obsolete session warning. |
| Equal-version update downloads or replaces unnecessarily | Output identifies installed/latest versions; only metadata is requested and binary SHA-256 is unchanged. |
| Metadata lookup fails | CLI exits unsuccessfully, records the HTTP failure from the release redirect, and preserves the installed binary. |
| A `git describe` or `-dirty` build updates itself | CLI refuses with a development-build error and makes no release request at all. |
| Download checksum fails | CLI exits unsuccessfully, never announces installation/success, and preserves the installed binary. |
| Cancellation during a blocked download installs partial bytes | CLI exits unsuccessfully before the gate opens and preserves the installed binary. |
| Updating makes saved sessions unusable | A session created by the old binary resumes under the replacement binary with the same session ID and prior conversation included in the provider request. |

Each run writes `report.json`, command transcripts, gate observations, binary
hashes, release-fixture requests, build-overlay metadata, and a replay command
into a fresh `--output` directory. No real provider or release service is used.
