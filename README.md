# Meldra

Meldra is an command-line coding agent.

	Inspired by Amp's article, [How to Build an Agent](https://ampcode.com/notes/how-to-build-an-agent).

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
~/.meldra/config.toml       # model and base_url
~/.meldra/credentials.env   # OPENAI_API_KEY
~/.meldra/sessions/         # resumable session state
```

Set `MELDRA_HOME` to use a different directory. Configuration precedence:

1. `OPENAI_API_KEY`, `OPENAI_MODEL`, `OPENAI_BASE_URL` environment variables
2. Files under `~/.meldra`
3. Built-in defaults

```bash
meldra config init   # create starter config files
meldra config show   # show effective values (hides API key)
meldra config path   # print config paths
meldra version
```

## Safety

- File tools are constrained to the workspace root (resolves `..`, symlinks, absolute paths) and block access to `.git` and Meldra's own config/session directory.
- Every edit prints a diff and waits for confirmation before writing.
- Command execution is allowlisted (Go test/build/vet, `gofmt -d`, selected Make targets, read-only Git) and requires explicit approval. Use `--yes` to skip confirmations in automation.
- Each agent turn is bounded to 20 model steps and 50 function calls.
- Repository contents and tool output are treated as untrusted data, not instructions.

## Sessions

```bash
meldra sessions
meldra resume                 # resume latest session
meldra resume SESSION_ID      # resume a specific session
```

`Ctrl-C` / `SIGTERM` cancel in-flight work, save session state, and print the resume command.

## Development

Requires Go 1.25+.

```bash
make check
```

## License

[MIT](LICENSE)
