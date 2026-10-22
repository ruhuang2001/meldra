---
title: "Quickstart"
description: "Install Meldra, configure a provider, and start a foreground coding session."
---

## Requirements

- macOS or Linux on `amd64` or `arm64`.
- `curl`, `tar`, and either `sha256sum` or `shasum` for the installer.
- An OpenAI API key or a compatible Responses API endpoint.

## Install

```sh wrap
curl -fsSL https://raw.githubusercontent.com/ruhuang2001/meldra/main/scripts/install.sh | sh
```

The installer selects your platform, downloads the latest release, verifies its
SHA-256 checksum, and installs to `~/.local/bin` without requiring `sudo`.

If that directory is not on your `PATH`, the installer prints instructions. For
the current terminal:

```sh
export PATH="$HOME/.local/bin:$PATH"
meldra version
```

For future terminals, add the export to your shell's startup configuration, such
as `~/.zshrc` for interactive Zsh or `~/.bashrc` for interactive Bash. The installer
does not edit these files automatically.

You can also download a platform archive from
[GitHub Releases](https://github.com/ruhuang2001/meldra/releases).

## Configure your provider

```sh
meldra config init
vi ~/.meldra/credentials.env
```

Add your API key to the generated credentials file:

```sh
OPENAI_API_KEY=your-api-key
```

Do not commit credentials. `OPENAI_API_KEY`, `OPENAI_MODEL`, and `OPENAI_BASE_URL`
environment variables override file configuration. Set `MELDRA_HOME` to select a
different state/configuration directory. Inspect effective configuration without
printing the API key:

```sh
meldra config
```

See the [configuration reference](https://github.com/ruhuang2001/meldra#configuration)
for provider URL rules and response/context limits.

## Start a session

**Available since v0.4.0:** Plan/Build modes and foreground controls are
included in the latest release. If upgrading from v0.3.0 or earlier, follow the
[upgrade and rollback instructions](https://github.com/ruhuang2001/meldra#upgrade-and-downgrade)
before opening the new ledger.

```sh
cd /path/to/project
meldra
```

Supported interactive terminals use the full-screen TUI. Other terminals and
non-interactive invocations use the line-based interface. Press `Ctrl-C` to exit.

For a read-only first request:

```sh
meldra --mode plan --prompt 'Explain the project structure and suggest a small improvement.'
```

Plan mode blocks project writes and command execution. Use Build mode when you
are ready to implement, and review requested approvals. Automatic approval is
not a sandbox and should only be used in an isolated environment.

## Continue learning

- [Project instructions, references, modes, and controls](capabilities.md)
- [MCP configuration and workspace tools](mcp.md)
- [On-demand Skills](skills.md)
- [Upgrade, backup, and downgrade guidance](https://github.com/ruhuang2001/meldra#upgrade-and-downgrade)
