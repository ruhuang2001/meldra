# Configuration, credentials and network input

Resolve explicit paths under the configured Meldra home so CI, personal and work configurations can stay independent. Preserve existing credentials during initialization and tighten private permissions. Read only the explicit credentials file. Suppress parser errors that might quote malformed secret input. Keep secrets outside Config, redact the complete credential in display data, preserve independently stored keys when replacing one, and avoid shell arguments that expose keys through history.

Callers apply process overrides after file loading. A meaningful zero threshold survives saves, while older Config literals retain defaults for positive-only settings. Ignore unknown top-level settings so newer configuration remains readable without changing authorization. Test helpers retain explicitly scoped fixtures inside isolated homes. Stored provider identifiers exclude credentials and URL query strings.

Compare validated semantic versions without integer overflow. Run eligible update checks outside the interactive path. Bound total archive expansion and extracted binary size and read the gzip trailer so truncated or corrupted data fails verification. Web tools separate DNS resolution from raw dialing to bind the actual connection to the validated address. Exclude reflected secrets and transport diagnostics from persistent results and match the durable JSON representation when computing evidence.

Resolve configuration paths to absolute paths at startup so a later working-directory change cannot redirect credential reads. An absent config uses compaction defaults. Displaying effective settings is a separate caller choice after environment overrides.

[Configuration](../reference/configuration.md) · [Web tools](../integrations/web-tools.md) · [Upgrading](../reference/upgrading.md)
