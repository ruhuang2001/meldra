---
title: MCP
sidebarTitle: MCP
description: Connect MCP servers, configure authentication and interactions, and expose Meldra workspace tools.
---

Meldra connects to external MCP servers and exposes bounded
workspace tools through `meldra mcp serve`. It uses the official Go SDK v1.8.0 and
Meldra's existing approvals, task records, and recovery runtime.

**Available since v0.5.0:** [Stream JSON approval resume](stream-json.md) binds each external
operation to its MCP connection configuration and OAuth login generation.
Changing the selected server, executable, environment or authentication settings
requires a new approval before the replacement transport is started.

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
  is capped at 256 KiB. Inline images use the separate artifacts described below.

## Inline tool images (unreleased)

External tool results can contain ordered text and inline PNG/JPEG images. Meldra
validates actual image bytes and sends accepted images as `input_image` parts
inside their original Responses `function_call_output`, preserving `call_id`.
It does not fabricate a user message or send the image as a JSON/base64 text blob.
The configured model and gateway must support image-valued function outputs;
provider errors remain visible, without a text-only retry that pretends to see images.

Each image is limited to 1 MiB and 16 million decoded pixels; one tool result
allows at most 32 images and 2 MiB of image bytes. Unsupported, invalid, or
oversized images become explicit omission notices and mark the result truncated.
Text truncation does not discard valid images. A tool's `isError` remains an
error even when its result contains an image. Text-only tool results keep their
existing representation. Resource links and server-returned file paths are never
opened automatically; this feature handles inline tool images, not binary resources
or multimodal prompts/sampling.

Images are saved as private, digest-checked binary task artifacts before the next
inference. The text ledger and `mcp-result.json` contain artifact metadata rather
than image base64; `mcp-content-v1.json` retains ordered text and image references.
Duplicate provider call IDs restore that evidence without calling the server
again. Missing or corrupt evidence stops execution, including when the original
save exhausted storage. The known external tool outcome remains recorded.
Screenshots may contain secrets: private file permissions are not encryption.

Phase 1 does **not** restore a screenshot gallery into ordinary new turns or
restart/resume prompts. Custom-provider context trimming may explicitly omit
older image results while retaining their artifacts. Capture again when checking
current page state. Terminal output and stream JSON do not render images.

### Chrome DevTools MCP

Use an existing browser server; Meldra has no browser engine or special browser
approval bypass. A tested configuration uses Chrome DevTools MCP 1.13.0, Node.js
and an installed Chrome. Add it to your private `mcp.json`:

```json
{
  "mcpServers": {
    "chrome": {
      "command": "npx",
      "args": ["-y", "chrome-devtools-mcp@1.13.0", "--headless", "--isolated",
               "--no-usage-statistics", "--no-performance-crux",
               "--viewport=1280x1000", "--screenshot-format=png",
               "--no-file-navigations"],
      "allow_during_process": [
        "list_pages", "new_page", "select_page", "navigate_page", "close_page",
        "resize_page", "take_snapshot", "take_screenshot", "fill", "fill_form",
        "click", "press_key", "type_text", "wait_for", "evaluate_script",
        "list_console_messages", "get_console_message",
        "list_network_requests", "get_network_request"
      ],
      "startup_timeout_sec": 60,
      "tool_timeout_sec": 60
    }
  }
}
```

This downloads and executes third-party code as your OS user. The isolated
browser profile avoids reusing personal login state; it is not an OS sandbox.
Use Build mode and review external calls normally. Plan restrictions remain
unchanged. Request `take_screenshot` without `filePath` to receive inline pixels;
if the server returns only a path, request a smaller viewport/element capture.

Built-in DeepSeek search and web fetch are unchanged. They remain useful for
finding and reading information without launching a browser; browser MCP adds
interactive page observation, not a replacement search backend.

### Browser interaction during managed processes (unreleased)

By default, external MCP tools are blocked while a managed command is running.
The optional `allow_during_process` list grants named tools on that particular
server permission to run alongside it. This lets Chrome operate a local app
started with `start_process`, without detaching the app or bypassing task records.
The list accepts up to 128 exact advertised tool names, each at most 256 bytes;
wildcards, whitespace, and missing tools are rejected. Names are the server's
original names, not Meldra's `mcp__...` wire names. Restart the session after editing
private configuration. Changing the list invalidates pending connection approvals.

This is a **trust grant**, not a read-only classification or an origin/filesystem
sandbox. Browser clicks and JavaScript can mutate application state, send network
requests, and invoke server behavior. Neither server annotations nor a tool's name
grant this permission. Remove any tools you do not trust to use alongside the app;
the example includes `evaluate_script` for the demo's application-level shutdown.

Normal external-call approval and Build-mode requirements still apply. Unlisted
tools and other servers remain blocked, and the grant cannot override unresolved
process effects or persistence failures. Those conditions and the current policy
are checked again after approval, before dispatch. Tool calls remain serial.

Built-in edits and other commands still cannot run alongside the managed process.
Use the application's supported graceful shutdown, await a known exit, edit,
restart, and capture fresh evidence. `stop_process`, cancellation, or a timeout
may leave unknown effects that require explicit reconciliation; this feature does
not treat termination as proof of a clean shutdown. Apps without a clean shutdown
path need that reconciliation, or a separately user-managed server. There is no
automatic browser workflow, hot-edit exception, or generic shutdown endpoint.

### Reproduce the browser check

The opt-in driver uses a real Meldra binary, real Chrome MCP, and a synthetic
image-only board. An offline Responses fixture selects navigation and capture,
then checks the actual wire payload and persisted PNG. It does not require
browser installation or network inference in ordinary `make check` runs.

```sh
go build -o /tmp/meldra-images .
python3 scripts/mcp-browser-check.py --binary /tmp/meldra-images \
  --server-script /absolute/path/to/installed/chrome-devtools-mcp.js \
  --output .artifacts/checks/browser-new-run
```

Use the entrypoint from the installed package's `bin.chrome-devtools-mcp` field
and a fresh output directory. The driver requires Python 3.11+ and accepts
`--node /absolute/path/to/node`. `--expect-text-only` records the pre-feature
baseline. With explicit `--live-config /path/to/private/MELDRA_HOME`, the driver
forwards the final image-bearing request to that configured model, incurs model
usage, and checks the fresh visual code and shapes in its answer. It does not
copy credentials into evidence. This verifies visual interpretation, not
autonomous tool selection or a page-repair workflow. Reports, screenshots,
provider requests, and task records are retained for inspection.

### Reproduce autonomous page repair (unreleased)

`scripts/mcp-browser-repair.py` runs the configured live model for the entire task
against a disposable copy of `scripts/fixtures/browser-repair`. Unlike the
screenshot check above, it does not script model responses or tool selections.
The intentionally broken demo has a failing reservation action and horizontal
overflow on phones. The model must reproduce them through Chrome, inspect
screenshots/console/network, shut down cleanly, repair only the frontend, restart,
and operate the form again. No real reservations or external sites are involved.

```sh
go build -o /tmp/meldra-browser .
python3 -B scripts/mcp-browser-repair.py --binary /tmp/meldra-browser \
  --server-script /absolute/path/to/installed/chrome-devtools-mcp.js \
  --node /absolute/path/to/node --chrome /absolute/path/to/chrome \
  --live-config /path/to/private/MELDRA_HOME \
  --output .artifacts/checks/browser-repair-new-run
```

This opts in to paid inference and automatic approvals for the disposable task.
It isolates HOME, state, browser profile, and workspace, but is **not an OS
sandbox**: the configured model and MCP server are trusted. Credentials go only
to the Meldra process, not the MCP environment or evidence files. The driver
requires Python 3.11+, Node.js, Chrome, and the tested MCP package. Its independent
verifier uses that package's bundled Puppeteer; pass `--puppeteer-module` if the
installed package has a different layout.

An independent browser, whose assertions are not supplied to the repairing model,
checks real HTTP results and calculated prices, required/invalid fields and seat
limits, reload, recovery after an injected HTTP 503, and control geometry at
320/390/768/1280 pixels. The task audit checks before/after interaction, screenshots,
console/network observations, artifact digests, clean managed-process exits, and
the permitted file-change scope. Reports, diffs, screenshots, and task records are
retained even for failed attempts when available. Use a fresh output directory
per run; model outcomes vary, so repeat and report failures as well as successes.
The default model-run bound is 900 seconds (`--timeout`). These opt-in live checks
are outside ordinary `make check` and do not establish reliability on arbitrary
sites, providers, or browsers.

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

**Updated for v0.5.0.** Meldra's built-in [web tools](web-tools.md) work
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

Run `make check` for Go race tests, real-binary MCP and application E2E suites,
and the 75% coverage gate. The [verification driver](https://github.com/ruhuang2001/meldra/blob/main/scripts/mcp-check.py)
defines suite selection; each report records `suite_order` and omitted checks. The suites use local fixtures
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
