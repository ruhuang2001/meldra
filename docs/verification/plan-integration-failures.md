---
title: "Feature integration failure matrix"
description: "Cross-feature protocol, approval and image verification."
---

# Plan integration failure matrix

Specified before cross-feature integration changes. This verification tree
combines independently reviewable feature branches; it is not a release.

- JSON prompts must use the same command expansion as line/TUI requests, once.
  A command expanding to another slash command or builtin stays ordinary text.
- JSON command discovery warnings must remain JSON notices, with no plain text
  on stdout and no API credentials or provider reasoning in the transcript.
- Command-expanded image references must produce provider image content on the
  initial request and custom-provider tool replay; Plan must still reject edits.
- JSON web_fetch must record waiting approval and stop before network dispatch;
  a wrong hash cannot execute, and the exact hash executes only that operation.
- Hosted web responses must redact the separate key from stream output, provider
  history and durable tool artifacts; model keys must never enter web headers.
- Resume must preserve effect classification and auditable approval identity.
- Source navigation must include new guide pages and their .md redirects.

This is a manual all-features acceptance suite. It requires the custom command,
image reference, web tools and stream-json branches combined in one checkout.
Do not run it against an independently reviewed feature branch alone. It is not
part of that branch's default check runner.

Build and run with a fresh output directory outside the source tree:

    CGO_ENABLED=0 go build -o /tmp/meldra-integrated .
    python3 scripts/plan-integration-e2e.py --binary /tmp/meldra-integrated --output /tmp/meldra-integration-evidence

Only DNS/raw dial/TLS trust and the app's custom-provider classification are
replaced in separate fixture builds; the production provider transport, policy,
ledger and approval handlers remain unchanged. Native coverage verifies initial
image input, previous_response_id tool continuation and artifact-based resume.

Run scripts/plan-integration-e2e.py against the combined binary. Evidence includes
isolated fixtures, provider requests, JSONL, ledger records, artifact bytes,
checksums, source/binary hashes and DNS/dial/TLS overlay provenance. No paid
provider or non-local service is contacted.
