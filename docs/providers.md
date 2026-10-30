# Responses-compatible providers

**Unreleased guide.** Meldra already supports a configurable Responses endpoint.
The examples below describe the current source; they do not certify a particular
model or server release. The endpoint must implement `POST /v1/responses`,
including function calls and function-call outputs. A server advertising only
`/v1/chat/completions` is insufficient. There is no Chat Completions or Anthropic
Messages adapter in this version.

## Configure a local endpoint

Use a separate private home for the local model. Run `meldra config init`, then
edit that home's `config.toml`:

```toml
model = "your-loaded-model"
base_url = "http://127.0.0.1:1234/v1"
allow_insecure_base_url = true
max_provider_response_bytes = 33554432
max_custom_turn_input_bytes = 4194304
```

Set `OPENAI_API_KEY` in that home's private `credentials.env`. Meldra requires a
nonempty key; for an unauthenticated loopback server only, use an explicit dummy
value such as `local-only`. For an authenticated server use its actual key.
Never reuse a paid provider credential as the dummy value.

```sh
MELDRA_HOME="$HOME/.meldra-local" meldra config init
# Edit ~/.meldra-local/config.toml and credentials.env first.
MELDRA_HOME="$HOME/.meldra-local" meldra config show
MELDRA_HOME="$HOME/.meldra-local" meldra --mode plan --prompt 'Read README.md and summarize it.'
```

HTTP is restricted to loopback and requires the explicit insecure flag. Remote
endpoints require HTTPS without URL credentials, query strings, or fragments.
The server and model must support the requested tool schemas. Model context and
image support are independent of endpoint reachability.

## Server examples and compatibility checks

| Server | Typical base URL | Required check |
| --- | --- | --- |
| LM Studio | `http://127.0.0.1:1234/v1` | Load a tool-capable model and enable the local Responses server. |
| Ollama | `http://127.0.0.1:11434/v1` | Check the installed version's Responses and function-tool support. |
| vLLM | `http://127.0.0.1:8000/v1` | Confirm Responses support and the model-specific tool parser/template. |
| llama.cpp | `http://127.0.0.1:8080/v1` | Confirm the installed server actually provides Responses; Chat Completions alone does not work. |

Ports are examples, not discovery rules. Configuration templates are in the
[repository's config/examples directory](https://github.com/ruhuang2001/meldra/tree/main/config/examples).
Consult the server's own documentation and verify your installed build:
[LM Studio](https://lmstudio.ai/docs/developer/openai-compat/responses),
[Ollama](https://docs.ollama.com/api/openai-compatibility),
[vLLM](https://docs.vllm.ai/en/latest/serving/openai_compatible_server/), and
[llama.cpp](https://github.com/ggml-org/llama.cpp/tree/master/tools/server).
No live server/model compatibility benchmark is claimed by the offline suite.

## Continuation and streaming

Custom endpoints receive complete current-turn history, including tool calls and
results, without `previous_response_id`. Follow-up turns reconstruct saved recent
conversation, plan, summary, attachment evidence and task context. Input is
bounded by `max_custom_turn_input_bytes`; older tool outputs may be replaced by
explicit omission markers, and an irreducible over-budget turn pauses for an
explicit continuation. This is not model-generated compaction.

**Unreleased:** `/compact [focus]` and automatic threshold compaction add a
separate model-generated summary of older conversation. They use the configured
model with no tools and a 1024-output-token limit. A valid summary is saved with
an immutable evidence artifact and recent messages; subsequent requests rebuild
context without the old `previous_response_id`. Provider failure, cancellation,
and invalid or truncated summaries leave the prior snapshot unchanged.

The threshold measures locally rebuilt UTF-8 text bytes. For a custom endpoint,
its denominator is the smaller of `max_custom_turn_input_bytes` and
`max_turn_context_bytes`; native Responses uses `max_turn_context_bytes`. Native
server history remains invisible locally, so the threshold is a floor on measured
context, not a claim about the model's full context window. Omission-marker
trimming still bounds custom-provider tool replay after compaction. See
[configuration](configuration.md#context-compaction) for settings and
[foreground controls](capabilities.md#context-compaction) for preserved evidence.

Before the first compaction, local conversation reconstruction measures its
bounded recent-message window, not all server-retained turns. The default
threshold therefore does not guarantee automatic compaction when only the
native server-side chain grows. Use the idle command to compact deliberately.

Streaming is preferred. Recognized unsupported-stream errors can fall back to a
nonstreaming request only before any output event; an interrupted partial stream
must not be retried as though it never happened. JSON Responses returned to a
streaming request are normalized. Malformed responses and missing tool identities
fail closed. Responses replay can carry opaque protocol reasoning transiently,
but credentials and raw reasoning do not enter durable session/ledger records.
Cross-protocol reasoning preservation is not promised.

The second-phase [Anthropic adapter issue](https://github.com/ruhuang2001/meldra/issues/66)
defines explicit capability queries and provider-switch history rebuilding.
The [context compaction design](proposals/context-compaction.md) records the
summary and evidence contract.

## Repeatable offline verification

`make check` exercises custom Responses SSE/JSON handling, tool replay, limits,
errors and cancellation through Go regressions and real-binary loopback E2E.
Run `python3 scripts/provider-compat-e2e.py --binary dist/meldra --output /tmp/meldra-provider-check`
for the dedicated compatibility scenarios and retained wire/ledger evidence.
