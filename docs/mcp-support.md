---
title: MCP
sidebarTitle: MCP
description: Connect MCP servers, configure authentication and interactions, and expose Meldra workspace tools.
---

Meldra connects to external MCP servers and exposes bounded
workspace tools through `meldra mcp serve`. It uses the official Go SDK v1.8.0 and
Meldra's existing approvals, task records, and recovery runtime.

| Capability | Supported surface |
| --- | --- |
| Tools | stdio and Streamable HTTP; discovery, pagination, validation, calls, approvals |
| Resources | list, templates, and read with text and binary JSON content |
| Prompts | list and get with arguments; returned content remains untrusted |
| OAuth | explicit login/logout, dynamic and pre-registered clients, caching, refresh |
| Sampling | opt-in text sampling with independent requests and sharing approval |
| Elicitation | CLI/TUI form and URL interactions with validation and cancellation |
| Meldra server | stdio workspace tools, resources, and `review_workspace` |

This is the implemented product surface, not complete MCP certification. Sampling is
retained for compatibility even though it is deprecated in the `2026-07-28` spec.

## Design

`newChatRuntime -> connectMCP -> ToolDefinition -> Agent.runInference -> taskExecution -> Workspace.requestApproval -> ClientSession`.

- `mcp.go` handles configuration, connections, discovery, limits, and cleanup.
- `mcp_catalog.go` exposes on-demand resource and prompt operations.
- `mcp_oauth.go` and `mcp_interaction.go` handle OAuth and server interactions.
- CLI, TUI, and task resume share the runtime and close owned connections.
- External names normally use `mcp__SERVER__TOOL`, `mcp_resources__SERVER`, or
  `mcp_prompts__SERVER`; long names use a digest to avoid collisions.
- External schemas remain non-strict and are validated before dispatch; external
  references are never fetched.
- Calls that disconnect, time out, or cancel after dispatch are recorded as
  `unknown` and are never retried automatically.
- Complete result JSON is retained as an artifact up to 16 MiB; model-visible text
  is capped at 256 KiB.

## Configuration

Create `~/.meldra/mcp.json`, or use `$MELDRA_HOME/mcp.json`, after `meldra config init`:

```json
{
  "mcpServers": {
    "local": {
      "command": "node",
      "args": ["/absolute/path/to/server.js"],
      "env_vars": ["MY_SERVICE_TOKEN"],
      "env": {"LOG_LEVEL": "warn"},
      "startup_timeout_sec": 10,
      "tool_timeout_sec": 60
    },
    "remote": {
      "url": "https://mcp.example.com/mcp",
      "oauth": {},
      "sampling": false,
      "disabled": true
    }
  }
}
```

The configuration directory must be `0700` and the file `0600`. Each server must
specify exactly one of `command` or `url`. stdio servers do not use a shell and run
with the workspace as their working directory. Only a small default environment is
inherited; use `env_vars` and `env` explicitly. `OPENAI_API_KEY` is never inherited.

HTTP endpoints require HTTPS or loopback HTTP and reject credentials, queries,
fragments, and redirects. Use `bearer_token_env` for static authentication or
`oauth` for browser authorization; the modes are mutually exclusive.

Startup and discovery default to 10 seconds. An MCP call defaults to 60 seconds;
both values can be configured from 1 to 300 seconds. OAuth login waits up to five
minutes, while OAuth metadata and token requests have a 30-second limit. Configuration
is capped at 1 MiB and 16 servers. Each server may advertise at most 128 tools and
1 MiB of encoded tool definitions. For chat, the external tool allowance is computed from the actual built-in and
Skill tool count so the combined model catalog fits within 128 tools. The combined
external catalog also has a 1 MiB limit, counting fully encoded JSON including
escaped descriptions and metadata. stdio frames and HTTP responses are capped at 16 MiB.

Configuration grants startup trust: stdio processes run with the current OS user's
permissions and are not sandboxed. Configure only trusted servers and keep secrets
out of command arguments and URLs.

Only private user configuration is read; repository MCP configuration does not
start servers automatically. Tool arguments and results may enter private task
history and artifacts, which are not encrypted or automatically redacted. On
macOS/Linux, connection cleanup terminates the server's process group. Processes
that deliberately detach from the group can survive; SIGKILL or power loss cannot
guarantee cleanup. Restart chat after changing configuration.

## Optional fetch and Tavily servers

**Unreleased documentation.** Meldra's built-in [web tools](web-tools.md) work
without an MCP server. To use an external fetch implementation or Tavily search,
install the relevant runtime (`uv` for Fetch, Node.js/npm for Tavily) and add:

```json
{
  "mcpServers": {
    "fetch": {
      "command": "uvx",
      "args": ["mcp-server-fetch"]
    },
    "tavily": {
      "command": "npx",
      "args": ["-y", "tavily-mcp"],
      "env_vars": ["TAVILY_API_KEY"]
    }
  }
}
```

Set `TAVILY_API_KEY` in the environment before starting Meldra. These commands
download and execute third-party packages with OS-user privileges; pin reviewed
package versions for reproducible installations. Fetch/Tavily calls use normal
external MCP approvals and records, not the built-in web client's SSRF guards.
Queries and URLs are disclosed to the configured service. Follow the upstream
[Fetch server](https://github.com/modelcontextprotocol/servers/tree/main/src/fetch)
and [Tavily MCP](https://github.com/tavily-ai/tavily-mcp) documentation.

## Authentication

Meldra reads MCP configuration from `~/.meldra/mcp.json` or
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
text messages (up to 64 messages and 1 MiB request payload) and uses the user's
configured model. Server model preferences are hints and do not override Meldra's
settings. Requests exclude the main conversation, workspace content, other MCP
content, and executable tools. Each request is capped at 8192 output tokens, the
server's `maxTokens`, and a 1 MiB response body. Inference has a 60-second child
deadline; sharing approval uses the outer MCP request's remaining deadline.

Human approval is required before the model request and before sharing its result.
`--auto-approve` does not bypass these approvals. Sampling conversations are not
reused across requests. Sampling is deprecated in MCP `2026-07-28`; this feature
remains for compatibility. Multimodal sampling and sampling tools/context are not
supported.

## Elicitation

CLI and TUI support form and URL elicitation. Forms show the server, message, and
schema (message, schema, and JSON response are each capped at 64 KiB); users enter
one JSON object, `decline`, or `cancel`. In the TUI, Enter submits and Esc cancels.
Fields are limited to flat strings, numbers, integers, booleans, and string-enum
arrays, with at most 32 fields and three attempts. Names, titles, descriptions, and
formats containing sensitive terms (`password`, `secret`, `apikey`, `token`,
`credential`, `creditcard`) are rejected for forms. This is an additional filter,
not a general secret detector: use URL mode for credentials. EOF cancels, and
automatic approval never invents form answers. Responses go directly to the
requesting server, which may echo them in later results.

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

## Catalog CLI

These commands connect only to the selected server and can inspect catalogs
without model credentials. `--workspace PATH` selects their working directory.

```sh
meldra mcp resources SERVER [CURSOR]
meldra mcp templates SERVER [CURSOR]
meldra mcp read SERVER URI
meldra mcp prompts SERVER [CURSOR]
meldra mcp prompt SERVER NAME KEY=VALUE
meldra mcp prompt SERVER NAME KEY=VALUE --run
```

Use `--` before a cursor beginning with `-`, for example
`meldra mcp resources SERVER -- -opaque-cursor`. Prompt output is complete JSON;
terminal controls are escaped. `--run` confirms before sending text prompt content
to the model. Non-text prompts can be inspected but cannot be run by the text flow.
Prompt message roles remain quoted context rather than system instructions. List
returns one page; pass the returned cursor to continue. Resource templates must
be expanded to a resource URI before reading.

## Serving Meldra

```sh
meldra mcp serve --workspace /path/to/project [--mode plan|build] [--permissions interactive|workspace-edit] [--auto-approve]
```

The command reserves stdin/stdout for JSON-RPC and does not require a model API key.
It exposes Meldra's workspace tools (including `read_project_context`), `meldra://workspace`,
`meldra://workspace/{path}`, and `review_workspace`. Workspace protections, command
allowlists, execution mode/permission policy, task records, and approvals remain active.
Modern approval tokens are bound to the operation, arguments, applicable `AGENTS.md`
instruction digest, and undo target's change generation; a later change or instruction
update invalidates a pending approval. Validation and execution share a workspace lock.

`--auto-approve` skips workspace-operation approval for explicitly trusted isolated
environments. It does not make server-initiated sampling or form answers automatic.

## Verification

Run `make check` for Go race tests, six real-binary E2E suites (five MCP suites
plus `capability-e2e`), and the 75% coverage gate. The suites use local fixtures
and write a fresh `.artifacts/checks/mcp-check-*` evidence directory. Override
it with `make check CHECK_DIR=/path/to/checks` or
`scripts/mcp-check.py --output-dir /path/to/checks`. Keep check evidence outside
`dist/`, which GoReleaser requires to be empty after its before hooks. CI also
runs `govulncheck`.

The tests cover implemented local integration paths. They do not certify every
third-party server, real OAuth provider, HTTP server hosting, subscriptions,
completion, dynamic notifications, or multimodal sampling.

### Authentication and interaction checks


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
