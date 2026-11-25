# Meldra

<p align="center">
  <img src="docs/assets/icon.png" alt="Meldra" width="128">
</p>

<p align="center">
  <strong>A local coding agent that runs in your terminal.</strong>
</p>

<p align="center">
  <a href="https://github.com/ruhuang2001/meldra/actions/workflows/ci.yml"><img src="https://github.com/ruhuang2001/meldra/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://github.com/ruhuang2001/meldra/releases/latest"><img src="https://img.shields.io/github/v/release/ruhuang2001/meldra" alt="Latest Release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/github/license/ruhuang2001/meldra" alt="License"></a>
</p>

Meldra runs inside one terminal session, asks before sensitive actions, and
keeps a local record of task execution for inspection and explicit recovery. It
is experimental: review its changes and use it carefully around sensitive or
irreplaceable files.
Approved commands run as your user; Meldra is not an operating-system sandbox.

## Install

Requires macOS or Linux (`amd64` or `arm64`) and an OpenAI API key or compatible
Responses API provider.

```sh
curl -fsSL https://raw.githubusercontent.com/ruhuang2001/meldra/main/scripts/install.sh | sh
meldra config init
```

Add `OPENAI_API_KEY` to `~/.meldra/credentials.env`, then start a session in a
project directory:

```sh
cd /path/to/project
meldra
```

Use `Ctrl-C` to exit. The installer verifies the release checksum and installs
to `~/.local/bin` by default.

## Common commands

```sh
meldra --mode plan --prompt 'Review @src/main.go and suggest a change.'
meldra context --path src/main.go
meldra sessions
meldra resume SESSION_ID
meldra update
```

Plan mode is read-only. Approved commands run with your operating-system
permissions; use automatic approval only in an isolated environment.

## Documentation

The documentation site is organized by task:

- **Get started:** [Introduction](docs/index.md) and [Quickstart](docs/quickstart.md).
- **Guides:** [modes](docs/guides/modes.md), [project context](docs/guides/project-context.md),
  [sessions and controls](docs/guides/controls.md), [commands](docs/guides/commands.md),
  [context compaction](docs/guides/context-compaction.md), and
  [custom slash commands](docs/guides/slash-commands.md).
- **Integrations:** [MCP](docs/integrations/mcp.md), [Skills](docs/integrations/skills.md),
  and [web tools](docs/integrations/web-tools.md).
- **Reference:** [configuration](docs/reference/configuration.md),
  [providers](docs/reference/providers.md), and
  [upgrades](docs/reference/upgrading.md).

The same pages power the Mintlify site. Release highlights are in the
[site changelog](docs/changelog.mdx); generated technical history is in
[CHANGELOG.md](CHANGELOG.md).

Engineering records are not published. [engineering/](engineering/) holds the
runtime architecture, [task records](engineering/task-records.md), and the
[stream JSON](engineering/stream-json.md) protocol; contribution and verification
notes live under `docs/engineering/` and stay out of the public sidebar.

## Development

Building needs only Go 1.26.11. Local CI also requires Python 3.10+ and
[uv](https://docs.astral.sh/uv/getting-started/installation/). See the
[development guide](docs/engineering/development.md) for CI, hooks, and release workflows.

```sh
make check-fast       # local Go checks and selected non-MCP E2E suites
make check            # full race, E2E, coverage, and build gate
make check E2E_WORKERS=1  # run independent E2E suites serially
make check E2E_SLOW=1     # also run deliberately slow E2E scenarios
```

`make check-fast` skips race, coverage, and MCP-specific E2E suites. Run
`make check` before submitting changes. See
[Maintaining the documentation](docs/engineering/documentation.md) for Mintlify preview and
validation.
