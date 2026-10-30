---
title: "Configuration"
---

## Configuration

```text
~/.meldra/config.toml       # model, provider URL, and limits
~/.meldra/credentials.env   # OPENAI_API_KEY
~/.meldra/mcp.json          # optional trusted MCP server configuration
~/.meldra/update-check.json # cached 24-hour stable release check metadata
~/.meldra/sessions/         # preserved legacy 0.1.x session snapshots
~/.meldra/tasks/tasks.db    # versioned task, run, tool, process, approval, and event records
~/.meldra/tasks/sessions/   # current conversation snapshots
~/.meldra/tasks/artifacts/  # bounded tool/verification output, named by digest
```

Set `MELDRA_HOME` to use a different directory. Configuration precedence:

1. `OPENAI_API_KEY`, `OPENAI_MODEL`, `OPENAI_BASE_URL` environment variables
2. Files under `~/.meldra`
3. Built-in defaults

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

**Unreleased.** Model-generated compaction summarizes older conversation messages
and retains a recent tail. These `config.toml` settings appear in `config show`:

| Key | Default | Accepted values |
| --- | --- | --- |
| `compact_keep_messages` | `8` | `2..64` messages, counting user and assistant messages together |
| `compact_threshold_percent` | `80` | `0..100`; `0` disables automatic compaction |
| `max_turn_context_bytes` | `4194304` (4 MiB) | `1..67108864` bytes |

The corresponding process overrides are `MELDRA_COMPACT_KEEP_MESSAGES`,
`MELDRA_COMPACT_THRESHOLD_PERCENT`, and `MELDRA_MAX_TURN_CONTEXT_BYTES`.

The threshold measures UTF-8 bytes of the locally rebuilt text context, not
tokens or model capacity. Native Responses uses `max_turn_context_bytes` as the
denominator. Custom endpoints use the smaller of that value and
`max_custom_turn_input_bytes`. Attachment payloads have separate limits.
Native server-side history is not measurable locally, so the threshold does
not guarantee that the provider's context window has not been exceeded.

Use `/compact [focus]` while idle to request compaction manually, even when the
automatic threshold is disabled. See [foreground controls](capabilities.md#context-compaction)
for preservation and failure behavior. Omission-marker trimming remains a
separate custom-provider budget safeguard.

## Provider transport

Custom provider URLs must use HTTPS. For local development only, an HTTP URL
on `localhost` or another loopback address can be enabled explicitly:

```toml
base_url = "http://localhost:8080/v1"
allow_insecure_base_url = true
```

```bash
meldra config init   # create starter config files
meldra config        # show effective values and config paths (hides API key)
meldra version
```
