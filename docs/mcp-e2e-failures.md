# MCP end-to-end failure matrix

Written before the executable fixture. Use the real built meldra binary with isolated configuration/workspace, and preserve requests, CLI transcript, task inspection/events, and checksummed report.

| Failure | Independent observable check |
| --- | --- |
| stdio or HTTP transport support | Real wire handshake, discovered schema, invocation log and succeeded durable call. |
| Optional schemas forced strict | Provider sees optional property and strict:false. |
| Pagination ignored | Second-page tool is advertised and called. |
| Names collide | Two servers with echo produce distinct advertised names and invocation logs. |
| Denial executes | n or EOF creates no invocation and a declined durable result. |
| Acceptance lost | y creates one invocation and an approved decision. |
| MCP isError falsely succeeds | Error text survives in failed durable result and model continuation. |
| Drop or timeout retries side effect | One invocation, unknown durable result, halted model loop. |
| Resume repeats unresolved call | Resume fails before new model or tool invocation. |
| Provider call identity crosses response scopes | Same ID in separate Responses scopes is treated as two calls; resume/replay remains scoped. |
| Missing server blocks healthy server | Visible startup warning and healthy call succeeds. |
| Disabled server launches | No startup marker for disabled command. |
| Invalid config accepted | Bad JSON, command plus URL, invalid timeout fail before inference. |
| Huge output floods model | Bounded continuation and preserved full-result artifact. |
| Process leaks | All recorded fixture PIDs are gone after CLI exit. |

The protocol fixture implements only the methods needed by these scenarios; focused Go
tests cover numeric elicitation precision, sensitive formats, and sampling deadlines.

HTTP startup regression: a two-second initialize must succeed with a ten-second
startup allowance even when the separate tool-call deadline is one second.

Additional security/lifecycle cases, specified before fixture changes:
- Invalid argument type: schema rejects before remote dispatch or approval.
- Bearer credentials: HTTP endpoint receives configured token; provider, transcript and durable ledger never do.
- Process environment: explicitly allowed/overridden variables arrive; OpenAI key and unrelated secret do not.
- Structured content: nested JSON survives in durable output/artifact.
- Startup timeout: unavailable warning, healthy server still runs, delayed child exits.
- Successful resume: recorded completed effect is not repeated; new run completes.
- SIGTERM during external call: durable unknown outcome, no retries, child cleaned up.
- TUI approval bridge: real PTY displays approval; y/n controls dispatch and durable decision; Ctrl-C restores/exits.
- Descendant process ownership: fixture spawns a child inheriting stdio; both PIDs disappear on normal exit, startup timeout, and SIGTERM during a call.

## Runtime review regressions (before fixes)

- A workspace command that exits nonzero can return a structured failed result without
  a Go error; MCP must preserve its output and send isError=true. Declines are errors too.
- Waiting 121 seconds for legacy approval must not exhaust the subsequent workspace
  operation; approval has its own bounded deadline, parent cancellation still applies.
- Cancelling a resource request before dispatch must persist cancelled, not unknown,
  and send no resource request on the wire.
- Opaque cursors beginning with "-" must be accepted after a "--" option terminator.
- Individually valid servers whose combined catalogs exceed the available external-tool allowance or
  1 MiB schema/description must not overflow the model request; offending servers
  are skipped with a warning and closed while accepted servers remain usable.
