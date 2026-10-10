# MCP Authentication and Server Interactions

Updated 2026-10-10. Meldra reads MCP configuration from `~/.meldra/mcp.json` or
`$MELDRA_HOME/mcp.json`. OAuth and static `bearer_token_env` are mutually exclusive.
OAuth applies only to HTTP MCP servers.

```json
{
  "mcpServers": {
    "remote": {
      "url": "https://example.com/mcp",
      "oauth": {},
      "sampling": true
    }
  }
}
```

## OAuth

An empty `oauth` object uses SDK dynamic client registration. For a pre-registered
client, set `client_id`. `client_secret_env` specifies an environment-only secret
and requires `issuer` to bind it to the authorization server. HTTPS
`client_id_metadata_url` is also supported. Optional `scopes` selects requested
scopes; the default uses scopes advertised by the server. `callback_port` selects
a fixed loopback port; the default uses an ephemeral port.

```sh
meldra mcp login remote
meldra mcp logout remote
```

Login prints an authorization URL for the user to open. The callback listens on
`127.0.0.1` at `/callback` and waits up to five minutes. Each OAuth HTTP request
has a 30-second limit. The SDK performs PKCE S256, state, and issuer checks and
sends the resource parameter during authorization and token exchange. Meldra
cannot independently verify the audience of an opaque access token. Loopback HTTP
authorization is allowed for local development when the configured MCP endpoint
also uses loopback HTTP. External HTTP authorization and redirects are rejected.

Credentials are stored in private `mcp-oauth-<server>.json` files with mode `0600`,
bound to the server URL and OAuth configuration. Chat uses cached access tokens and
persists rotated refresh tokens. Environment-only client secrets are read again
from the current process and never persisted. Dynamically issued client secrets
remain in the private cache for refresh. Serialized caches are limited to 1 MiB
on both login and refresh; oversized responses are rejected before changing the
existing cache. Task artifacts and credential caches are not encrypted storage.

`logout` deletes local credentials but does not revoke service-side grants. It
works after the server configuration has been removed or damaged, allowing
orphaned caches to be deleted by their original server name. Re-login keeps the
previous session until authorization and the MCP connection succeed. Cancellation,
authorization errors, and connection errors preserve existing local credentials.
Concurrent logout or another successful login invalidates older pending attempts.
Refresh, login completion, and logout serialize through a cross-process lock;
refresh also respects the current request's cancellation. The adjacent `.lock`
and `.generation` files contain coordination state, not tokens. OAuth login,
refresh, and logout currently support macOS and Linux only.

## Sampling

Only servers with `sampling: true` receive sampling capability. Meldra supports
text messages and uses the user's configured model. Server model preferences are
hints and do not override Meldra's settings. Requests exclude the main conversation,
workspace content, other MCP content, and executable tools. Each request is capped
at 8192 output tokens and the server's `maxTokens`. Inference has a 60-second child
deadline; sharing approval uses the outer MCP request's remaining deadline.

Human approval is required before the model request and before sharing its result.
`--auto-approve` does not bypass these approvals. Sampling conversations are not
reused across requests. Sampling is deprecated in MCP `2026-07-28`; this feature
remains for compatibility. Multimodal sampling and sampling tools/context are not
supported.

## Elicitation

CLI and TUI support form and URL elicitation. Forms show the server, message, and
schema; users enter one JSON object, `decline`, or `cancel`. In the TUI, Enter
submits and Esc cancels. Fields are limited to flat strings, numbers, integers,
booleans, and string-enum arrays, with at most 32 fields and three attempts. Names,
descriptions, and sensitive formats such as `password` are rejected for forms.
This is an additional filter, not a general secret detector: use URL mode for
credentials. EOF cancels, and automatic approval never invents form answers.
Responses go directly to the requesting server, which may echo them in later results.

Integers within the `int64`/`uint64` range retain exact values; larger values and
fractional integers require retry. The SDK uses `float64` for `number` fields.
Meldra accepts those values only when their JSON round-trip preserves the supplied
value, rejecting high-precision inputs rather than silently rounding them.

URL mode displays the complete URL without opening or fetching it. The user
completes the external interaction in their browser and then accepts or declines.
Third-party pages and credentials do not pass through Meldra's model.

Meldra accepts interactions only during the corresponding outbound MCP call.
Unsolicited sampling and elicitation are rejected. SDK handling covers modern
multi round-trip requests and legacy callbacks. Unanswered interactions end at
the outer call deadline. Server cancellation leaves later CLI/TUI input usable
and does not leave a reader or stale approval behind.

## Verification

```sh
mkdir -p dist
go build -race -o dist/meldra-mcp-race .
python3 scripts/mcp-auth-interaction-e2e.py --binary dist/meldra-mcp-race --output dist/mcp-auth-interaction-repeat
```

Choose a fresh output directory to preserve evidence. Scripts use local protocol
and model fixtures without real accounts or model fees. Reports, checksums, wire
records, provider requests, and transcripts support reproduction. OAuth evidence
records grant and refresh types without printing access or refresh tokens. Real
third-party OAuth accounts and full conformance require separate validation.
