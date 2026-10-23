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
are compacted first when needed, and if a turn still exceeds the budget, Meldra
saves the session and pauses so you can send `continue`. Set
`max_custom_turn_input_bytes` or `MELDRA_MAX_CUSTOM_TURN_INPUT_BYTES` to choose
a positive limit up to 64 MiB.

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
