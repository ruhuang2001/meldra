---
title: "Configuration"
---

```text wrap
~/.meldra/config.toml       # model, provider URL, and limits
~/.meldra/credentials.env   # OPENAI_API_KEY
~/.meldra/mcp.json          # optional trusted MCP server configuration
~/.meldra/update-check.json # cached 24-hour stable release check metadata
~/.meldra/sessions/         # preserved legacy 0.1.x session snapshots
~/.meldra/tasks/tasks.db    # versioned task, run, tool, process, approval, and event records
~/.meldra/tasks/sessions/   # current conversation snapshots
~/.meldra/tasks/artifacts/  # bounded tool/verification output, named by digest
```

The local and OpenAI-compatible TOML examples in
[`config/examples/`](https://github.com/ruhuang2001/meldra/tree/main/config/examples)
use placeholder model names and provider URLs. Set both values for your Responses
API server, and store the remote provider's key in `credentials.env`.

Set `MELDRA_HOME` to use a different directory. Configuration precedence:

1. `OPENAI_API_KEY`, `OPENAI_MODEL`, `OPENAI_BASE_URL` environment variables
2. Files under `~/.meldra`
3. Built-in defaults

The precedence above is for global configuration. A saved session can override the
model; see [sessions and controls](../guides/controls.md). `config show` reports
global configuration, not the effective settings of an already saved session.

Provider responses are capped at 32 MiB by default. Set
`max_provider_response_bytes` in `config.toml`, or
`MELDRA_MAX_PROVIDER_RESPONSE_BYTES` for a process override, to choose a
positive limit up to 256 MiB.

Custom-provider replay context is capped at 4 MiB by default; older tool outputs
are replaced by omission markers first when needed. If a turn still exceeds the
budget, Meldra saves the session and pauses so you can send `continue`. Set
`max_custom_turn_input_bytes` or `MELDRA_MAX_CUSTOM_TURN_INPUT_BYTES` to choose
a positive limit up to 64 MiB.

### Context compaction

Model-generated compaction summarizes older conversation messages
and retains a recent tail. These `config.toml` settings appear in `config show`:

| Key | Default | Accepted values |
| --- | --- | --- |
| `compact_keep_messages` | `8` | `2..64` messages, counting user and assistant messages together |
| `compact_threshold_percent` | `80` | `0..100`; `0` disables automatic compaction |
| `max_turn_context_bytes` | `4194304` (4 MiB) | `1..67108864` bytes |
| `context_window_tokens` (unreleased) | `0` (unset) | `0..2000000` tokens |

Automatic compaction is enabled by default. When the locally rebuilt text
context reaches the 80% threshold, compaction makes an additional request to
the configured model, which can add latency and provider charges. To disable
the automatic trigger, set this in `config.toml`:

```toml wrap
compact_threshold_percent = 0
```

Manual `/compact [focus]` remains available.

The corresponding process overrides are `MELDRA_COMPACT_KEEP_MESSAGES`,
`MELDRA_COMPACT_THRESHOLD_PERCENT`, `MELDRA_MAX_TURN_CONTEXT_BYTES`, and
`MELDRA_CONTEXT_WINDOW_TOKENS` (unreleased).

The threshold measures UTF-8 bytes of the locally rebuilt text context, not
tokens or model capacity. Native Responses uses `max_turn_context_bytes` as the
denominator. Custom endpoints use the smaller of that value and
`max_custom_turn_input_bytes`. Attachment payloads have separate limits.
Native server-side history is not measurable locally, so the threshold does
not guarantee that the provider's context window has not been exceeded.

**Unreleased.** Setting `context_window_tokens` to the model's context window
adds a token trigger. After the first successful response, Meldra pairs the
provider-reported `input_tokens` with the byte count of the request it built,
then scales later byte measurements by that ratio. Compaction starts when the
estimate reaches `compact_threshold_percent` of the configured window. The byte
budget keeps applying as a hard ceiling in every case, and no token estimate is
made until a response has calibrated it, so an unset window changes nothing.
The TUI footer shows the current turn's usage and, once calibrated, the context
percentage.

Use `/compact [focus]` while idle to request compaction manually, even when the
automatic threshold is disabled. See [context compaction](../guides/context-compaction.md)
for preservation and failure behavior. Omission-marker trimming remains a
separate custom-provider budget safeguard.

## Provider transport

Custom provider URLs must use HTTPS. For local development only, an HTTP URL
on `localhost` or another loopback address can be enabled explicitly:

```toml wrap
base_url = "http://localhost:8080/v1"
allow_insecure_base_url = true
```

```bash wrap
meldra config init   # create starter config files
meldra config        # show effective values and config paths (hides API key)
meldra version
```

## Reference limits

These bounds apply to inline `@` references and image attachments. They are
fixed in the application, not configurable.

| Item | Limit |
| --- | --- |
| Source file read per reference | 1 MiB |
| Retained text per reference | 64 KiB |
| Retained text per request | 128 KiB |
| References per request | 32 |
| Image encoded size | 1 MiB |
| Image decoded pixels | 16 million |
| Image bytes per request | 2 MiB |
| Distinct images kept in history | 32, within 2 MiB |

Older images beyond the history budget are replaced by an explicit omission
notice; their artifacts remain in task history. See
[project context and file references](../guides/project-context.md).
