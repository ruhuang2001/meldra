# Web tools failure matrix

**Unreleased.** Written before implementation. Verification uses a real Meldra
binary, isolated homes, a fake Responses provider, controlled HTTP/TLS origins,
and retained JSON reports, provider transcripts, task records and artifacts.
No unit tests are added. Production binaries have no SSRF bypass configuration.

| Failure boundary | Required evidence |
| --- | --- |
| Catalog/config | Fetch present; search absent by default; configured DeepSeek search needs its independent key; invalid backend/missing key fail before inference; show redacts both keys; save preserves credentials |
| Approval | Default deny without network; exact target/query/backend and disclosure shown; workspace-edit cannot bypass; auto-approval records decision; Plan allows approved external reads but no writes |
| URL | Reject non-HTTP(S), userinfo, fragments, missing host, malformed ports, local names, private/loopback/link-local/multicast/unspecified/reserved IPs and mapped IPv6 |
| DNS and dial | Resolve after approval; reject any forbidden answer including mixed answers; dial the checked numeric IP; revalidate connections and redirects; rebinding cannot alter connected address |
| Redirect | At most five; validate relative/absolute targets; reject private/credential redirects; report final URL/status; no cookies or Authorization forwarding |
| Response | HTML/text/JSON/+json only; reject binary/missing MIME; strip active HTML; 1 MiB plus detection byte; bounded artifact with digest reference; mark truncation; non-2xx failure metadata |
| Hosted API | Official fixed HTTPS endpoint; independent bearer key; exact schema; no proxies/redirect forwarding; disclosure; 401/429/5xx/invalid JSON/missing fields/oversize fail without secrets; target final URL/status unavailable |
| Cancellation/ledger | Intent and approval before request; lost/cancelled response after dispatch unknown; no automatic replay; durable artifacts and metadata; persistence failure stops work |
| MCP | Same catalog/config, classification, disclosure, argument binding, denial and effects across approval paths |

Positive-network scenarios build a real binary with a Go overlay supplying test
DNS and raw dial functions. The production address validator, redirect loop,
HTTP client, policy, CLI/MCP and ledger execute unchanged. The overlay maps
already validated public addresses to local fixtures. It is not linked into
release binaries and has no production environment activation. A separate
production-binary scenario proves loopback denial.
