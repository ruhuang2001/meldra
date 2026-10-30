---
title: "Web fetch and search"
description: "Configure approved public-page fetching and optional DeepSeek search."
---

**Unreleased.** Meldra exposes `web_fetch` in CLI, TUI and `mcp serve`. The
default `builtin` backend needs no service key. `web_search` is registered only
when the DeepSeek search backend and its separate credential are configured.
These are external reads, classified as `external_read` in task records. They
are available in Plan mode, require approval by default, and do not acquire
workspace write or command permissions. `workspace-edit` never auto-approves
them. Explicit `--auto-approve` also approves network disclosure and billing.

## Configuration

Add these non-secret settings to the private `$MELDRA_HOME/config.toml`:

```toml
web.fetch_backend = "builtin" # default; alternatively "deepseek"
web.search_backend = "deepseek" # omit to disable search
```

The equivalent `[web]` section with `fetch_backend` and `search_backend` is
supported at the end of the file. Unknown web settings and unsupported backends
are rejected. Configured DeepSeek tools fail at startup if their key is missing,
before making a model request. `config show` redacts both service keys.

Set `DEEPSEEK_API_KEY` in the process environment or private
`$MELDRA_HOME/credentials.env`; the environment takes precedence. It is separate
from `OPENAI_API_KEY`. Updating the OpenAI key preserves the existing DeepSeek
key. Web tools never borrow the inference provider key, base URL or cookies.

```dotenv
OPENAI_API_KEY="your-inference-key"
DEEPSEEK_API_KEY="your-separate-deepseek-key"
```

Restart the foreground session after changing configuration.

## Approval and network boundaries

Every approval identifies the tool, backend and exact arguments. Hosted fetch
sends the complete URL to DeepSeek; hosted search sends the query and options.
The approval explicitly identifies that third party and warns against private
document URLs, intranet names, credentials and sensitive queries. These calls
can incur service charges. Arguments, source metadata and returned content are
stored in private task history, which is not encrypted storage. Do not put
secrets in a URL or search query.

Builtin fetch accepts only absolute HTTP(S) URLs without userinfo or fragments.
It rejects local hostnames, loopback, private, link-local, unspecified, multicast
and reserved addresses, including mapped IPv6. DNS resolution occurs after
approval. Every returned DNS address must pass the public-address check, and the
connection dials a validated numeric address directly. No second hostname lookup
can substitute a rebound private address. Environment proxies are ignored.

Redirects are followed manually, at most five times. Each target is revalidated
and each connection repeats DNS/address validation. Cookies, site credentials
and Authorization headers are never sent by builtin fetch. It supports HTML,
other `text/*`, `application/json` and `+json` responses. Missing or binary MIME
types are rejected. HTML scripts/styles/templates and comments are omitted;
fetched text is untrusted data and cannot change Meldra's policy or approvals.

The DeepSeek client connects only to the fixed HTTPS endpoint
`https://api.deepseek.com/v1/tools/`. It validates that connection, rejects
redirects, ignores proxies and uses only its independent bearer key. DeepSeek
performs target DNS resolution and fetching; Meldra cannot verify the remote
service's redirect or SSRF behavior. Literal private/local target URLs are
rejected locally, but hosted targets are not pre-resolved by Meldra.

## Results and durable evidence

Each response is read through a 1 MiB body limit, plus one byte to detect
truncation. Bounded content is retained as an artifact with its SHA-256 reference;
the model sees at most the standard 256 KiB tool excerpt. Truncation is recorded
in the tool result. An oversized hosted JSON response is retained only as a
bounded incomplete prefix and reported failed, never parsed as complete data.
HTML-to-text output is not a browser rendering and cannot run JavaScript.

Builtin metadata records the requested URL, final URL, target HTTP status and
content type. Non-2xx target responses are recorded as failed with bounded body
evidence. DeepSeek metadata separately records API URL and API HTTP status. The
official fetch response contains only `title` and Markdown `content`, so target
`final_url` and `status_code` are explicitly `null`, with an explanation. They
must not be inferred from API HTTP 200. Search preserves ranked structured
results including their source URLs.

Intent and approval are persisted before the request; outcome and artifacts
afterward. A lost or cancelled response after request dispatch is `unknown`,
requires explicit inspection/reconciliation, and is never automatically replayed
as a harmless local read. Artifact persistence failures stop execution. MCP uses
the same tool handlers, effect, approvals and ledger; its approval tokens bind
to the exact arguments and cannot be reused for a changed target or query.

## API contract and verification

The implementation follows the official DeepSeek contracts checked on
October 29, 2026:

- [Search and Fetch guide](https://api-docs.deepseek.com/guides/search/)
- [Fetch API](https://api-docs.deepseek.com/api/fetch/): `POST /v1/tools/fetch` with `url`.
- [Search API](https://api-docs.deepseek.com/api/search/): `POST /v1/tools/search` with `query`, `count`, and `need_content`. Queries contain 1–128 Unicode characters; count is 1–50, default 10.

`python3 scripts/web-e2e.py --binary /absolute/path/to/meldra --output /tmp/web-evidence`
exercises the real CLI/MCP and durable records with isolated homes and offline
HTTP/TLS fixtures. It produces `report.json`, protocol/provider transcripts,
task records, content artifacts and binary/overlay provenance. Positive-network
tests replace only DNS/raw dialing/TLS trust with a build overlay; all address
validation and production policy remain active. Release binaries have no bypass.
Fixture builds normalize the source path and `PWD`, then verify an overlay-only
startup marker before any request. The suite repeats configuration checks with
canonical and symlink source paths and records the build paths and probe output.
`make check` runs this suite. See the [failure matrix](verification/web-failure-matrix.md).
