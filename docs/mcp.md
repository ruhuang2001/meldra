# MCP Support

Updated 2026-10-10. Meldra connects to external MCP servers and exposes bounded
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
1 MiB of encoded tool definitions. The combined external catalog is capped at 112
tools and 1 MiB, counting fully encoded JSON including escaped descriptions and
metadata. stdio frames and HTTP responses are capped at 16 MiB.

Configuration grants startup trust: stdio processes run with the current OS user's
permissions and are not sandboxed. Configure only trusted servers and keep secrets
out of command arguments and URLs.

Only private user configuration is read; repository MCP configuration does not
start servers automatically. Tool arguments and results may enter private task
history and artifacts, which are not encrypted or automatically redacted. On
macOS/Linux, connection cleanup terminates the server's process group. Processes
that deliberately detach from the group can survive; SIGKILL or power loss cannot
guarantee cleanup. Restart chat after changing configuration.

See [authentication and server interactions](mcp-auth-interactions.md) for OAuth,
sampling, and elicitation configuration and limits.

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
meldra mcp serve --workspace /path/to/project
```

The command reserves stdin/stdout for JSON-RPC and does not require a model API key.
It exposes `meldra://workspace`, `meldra://workspace/{path}`, and `review_workspace`.
Workspace protections, command allowlists, task records, and approvals remain active.
Modern approval tokens are bound to the operation, arguments, and undo target's change
generation; a later change invalidates a pending undo approval. Validation and
execution share a workspace lock.

`--auto-approve` skips workspace-operation approval for explicitly trusted isolated
environments. It does not make server-initiated sampling or form answers automatic.

## Verification

Run `make check` for Go race tests, five real-binary MCP E2E suites, and the 75%
coverage gate. The suites use local fixtures and write a fresh `.artifacts/checks/mcp-check-*`
evidence directory. Override it with `make check CHECK_DIR=/path/to/checks` or
`scripts/mcp-check.py --output-dir /path/to/checks`. Keep check evidence outside
`dist/`, which GoReleaser requires to be empty after its before hooks. CI also
runs `govulncheck`.

The tests cover implemented local integration paths. They do not certify every
third-party server, real OAuth provider, HTTP server hosting, subscriptions,
completion, dynamic notifications, or multimodal sampling.
