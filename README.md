# Meldra

<p align="center">
  <img src="docs/assets/icon.png" alt="Meldra" width="128">
</p>

<p align="center">
  <strong>A local, foreground coding agent for your terminal.</strong>
</p>

<p align="center">
  <a href="https://github.com/ruhuang2001/meldra/actions/workflows/ci.yml"><img src="https://github.com/ruhuang2001/meldra/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://github.com/ruhuang2001/meldra/releases/latest"><img src="https://img.shields.io/github/v/release/ruhuang2001/meldra" alt="Latest Release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/github/license/ruhuang2001/meldra" alt="License"></a>
</p>

Meldra works in the foreground, asks before sensitive actions, and keeps a local
record of task execution for inspection and explicit recovery. It is experimental:
review its changes and use it carefully around sensitive or irreplaceable files.
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

The [documentation](docs/index.md) has the [Quickstart](docs/quickstart.md),
[capabilities and controls](docs/capabilities.md), [MCP](docs/mcp-support.md),
[Skills](docs/skills.md), [task storage and recovery](docs/task-storage.md), and
[architecture](docs/architecture.md). These pages also power the Mintlify site. Release highlights are in the
[site changelog](docs/changelog.mdx); generated technical history is in
[CHANGELOG.md](CHANGELOG.md).

## Development

Requires Go 1.26.10 and Python 3 for PTY integration tests. See the
[development guide](docs/development.md) for CI, hooks, and release workflows.

```sh
make check-fast       # local Go checks and capability E2E
make check            # full race, E2E, coverage, and build gate
make check E2E_WORKERS=1  # run independent E2E suites serially
```

`make check-fast` skips race, coverage, and MCP-specific E2E suites. Run
`make check` before submitting changes. See
[Maintaining the documentation](docs/documentation.md) for Mintlify preview and
validation.
