# Meldra

Meldra is a command-line coding agent.

Inspired by Amp's article [How to Build an Agent](https://ampcode.com/notes/how-to-build-an-agent).

> **Early stage:** Meldra is experimental. Review every change it makes and avoid running it in directories with sensitive or irreplaceable files.

## Requirements

- macOS or Linux (amd64 / arm64)
- An OpenAI API key

## Quick start

1. Download the archive for your platform from [GitHub Releases](https://github.com/ruhuang2001/meldra/releases):

   ```bash
   # example: macOS arm64
   curl -fsSL -o meldra.tar.gz https://github.com/ruhuang2001/meldra/releases/latest/download/meldra_Darwin_arm64.tar.gz
   tar -xzf meldra.tar.gz
   cp meldra ~/.local/bin/
   ```

2. Configure and run:

   ```bash
   meldra config init
   $EDITOR ~/.meldra/credentials.env   # add your OpenAI API key
   cd /path/to/project
   meldra
   ```

Press `Ctrl-C` to exit.

In a supported interactive terminal, Meldra opens a full-screen TUI and streams assistant responses. Non-interactive and unsupported terminals retain the line-based interface.

## Configuration

```text
~/.meldra/config.toml       # model, provider URL, and limits
~/.meldra/credentials.env   # OPENAI_API_KEY
~/.meldra/sessions/         # resumable session state
```

Set `MELDRA_HOME` to use a different directory. Configuration precedence:

1. `OPENAI_API_KEY`, `OPENAI_MODEL`, `OPENAI_BASE_URL` environment variables
2. Files under `~/.meldra`
3. Built-in defaults

Provider responses are capped at 32 MiB by default. Set
`max_provider_response_bytes` in `config.toml`, or
`MELDRA_MAX_PROVIDER_RESPONSE_BYTES` for a process override, to choose a
positive limit up to 256 MiB.

Custom provider URLs must use HTTPS. For local development only, an HTTP URL
on `localhost` or another loopback address can be enabled explicitly:

```toml
base_url = "http://localhost:8080/v1"
allow_insecure_base_url = true
```

Meldra warns before a configured API key is sent to a custom provider host.

```bash
meldra config init   # create starter config files
meldra config        # show effective values and config paths (hides API key)
meldra version
```

## Safety

- File tools are constrained to the workspace root (resolves `..`, symlinks, absolute paths) and block access to `.git` and Meldra's own config/session directory.
- Every edit prints a diff and waits for confirmation before writing.
- Command execution is allowlisted. Commands that compile or execute workspace code require explicit approval; restricted read-only Git commands and `gofmt -d` do not. Approved commands run as your OS user and may access the filesystem and network; environment filtering is not a sandbox. Use `--yes` only inside an isolated container or VM.
- The `verify` tool detects root project markers and selects bounded presets for Make, Go, Python/pytest, Node/npm or pnpm, and Rust/Cargo projects. A Makefile's explicit `check` or `test` target takes priority.
- Each agent turn is bounded to 20 model steps and 50 function calls. Custom providers also have a 4 MiB replay-context budget; older tool results are compacted first when needed.
- Session files are bounded and validated while loading. The TUI keeps at most 200 rendered history entries in memory without applying that display limit to session persistence.
- Repository contents and tool output are treated as untrusted data, not instructions.

## Sessions

```bash
meldra sessions
meldra resume                 # choose a saved session
meldra resume SESSION_ID      # resume a specific session
meldra resume latest          # resume the most recently saved session
```

`Ctrl-C` / `SIGTERM` cancel in-flight work, save session state, and print the resume command.

## Development

Requires Go 1.26+.

```bash
make check       # formatting, vet, modules, race tests, coverage, and build
make benchmark   # local performance baseline; not a noisy CI gate
```

The test suite must maintain at least 75% statement coverage.

## PR test binaries

PR checks do not build downloadable artifacts by default. To request one from a
PR, an owner or collaborator comments exactly `/meldra package`. GitHub Actions
acknowledges the request, builds the PR's merge result, then replies with a link
to a seven-day artifact containing the same Darwin/Linux `amd64` and `arm64`
archives and checksums used for releases. It never creates a GitHub Release.

The **Build PR test artifacts** workflow remains available under **Actions** as
a manual fallback: select **Run workflow** and enter the PR number.
The workflow must first be present on the default `main` branch before either
trigger is available.

Meldra follows Semantic Versioning. While the project is below 1.0, the
automated release flow publishes `alpha` prereleases (`v0.1.0-alpha.N`).
Publishing a stable release requires an explicit update to
`release-please-config.json`.

## License

[MIT](LICENSE)
