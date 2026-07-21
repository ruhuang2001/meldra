# Meldra

Meldra is an early-stage command-line coding agent. It uses the OpenAI Responses API to chat with you and can list, read, create, and edit files in the directory where it runs.

Meldra was inspired by Amp's article, [How to Build an Agent](https://ampcode.com/notes/how-to-build-an-agent).

> **Early stage:** Meldra is experimental. Review every change it makes and avoid running it in directories containing sensitive or irreplaceable files.

## Requirements

- macOS or Linux on amd64 or arm64
- An OpenAI API key

## Quick start

```bash
curl -fsSL https://raw.githubusercontent.com/ruhuang2001/meldra/main/scripts/install.sh | sh
meldra config init
$EDITOR ~/.meldra/credentials.env
meldra config show
cd /path/to/project
meldra
```

The installer downloads the matching binary from GitHub Releases, verifies its
SHA-256 checksum, and installs it to `~/.local/bin`. Set `INSTALL_DIR` to choose
another directory, or set `VERSION` (for example, `VERSION=v0.1.0`) to install a
specific release.

Run Meldra from the project you want to work on so relative file paths refer to
that project. Type a request such as:

```text
Read main.go and explain how the agent loop works.
```

Press `Ctrl-C` to exit.

## Configuration

Meldra stores configuration outside your projects:

```text
~/.meldra/config.toml       # model and base_url
~/.meldra/credentials.env   # OPENAI_API_KEY
~/.meldra/sessions/         # private resumable session state
```

Set `MELDRA_HOME` to use a different configuration directory. Effective values
use this precedence:

1. `OPENAI_API_KEY`, `OPENAI_MODEL`, and `OPENAI_BASE_URL` environment variables
2. Files under `~/.meldra`
3. Built-in defaults

Meldra **never automatically reads a `.env` file from the current project**. If
you used the old `.env` setup, move its API key to
`~/.meldra/credentials.env` and its model/base URL to `~/.meldra/config.toml`.

Useful commands:

```bash
meldra config init   # create private starter files without overwriting existing files
meldra config show   # show paths and effective values without revealing the API key
meldra config path   # print configuration paths
meldra version
```

## Workspace safety

Meldra treats the current directory as its workspace by default. Choose another
root explicitly when needed:

```bash
meldra --workspace /path/to/project
```

File tools reject paths outside that root after resolving `..`, absolute paths,
and symbolic links. They also refuse direct access to `.git` and Meldra's own
configuration/session directory. Every edit or multi-file patch prints a diff,
rechecks the original contents, and waits for confirmation before writing.
These pathname checks assume another hostile local process is not concurrently
replacing workspace directories while Meldra is operating; they are not an
operating-system sandbox.

Command execution does not use a shell. It is limited to workspace-local Python
files and Go programs, Go test/build/vet, `gofmt -d`, selected Make targets, and
read-only Git commands, with bounded output and a timeout. Before Python, Go, or
Make can execute workspace code, Meldra displays its exact argument list and
waits for confirmation. Programs, tests, and Make targets are still code from
the workspace and run with your user account rather than in an operating-system
sandbox, so review an unfamiliar repository before approving them. Use `--yes`
only in automation where you intentionally want to skip both file-write and
executable-command confirmations.

Meldra sends a fixed safety and workflow instruction with every model request.
It tells the model to treat repository contents and tool output as untrusted
data rather than instructions. Each user turn is also bounded to 20 model steps
and 50 function calls. If a turn reaches either limit, Meldra saves the session,
breaks the unfinished remote response chain, and lets you send `continue` after
reviewing the workspace.

Sessions are saved privately and can be resumed later:

```bash
meldra sessions
meldra resume                 # latest session
meldra resume SESSION_ID
# Equivalent: meldra --resume SESSION_ID
```

`Ctrl-C` and `SIGTERM` cancel an in-flight API request or workspace command,
save resumable session context, and print the exact resume command. Because an
approved tool may have completed before cancellation arrived, inspect the
workspace before continuing an interrupted turn.

## Current capabilities

- List and search files recursively without entering `.git`
- Read bounded, line-numbered file segments
- Preview and confirm exact edits or unified multi-file patches
- Undo the most recent Meldra edit without resetting unrelated Git changes
- Run allowlisted tests, builds, formatting checks, and diffs
- Review Git status, staged/unstaged diffs, and recent commits
- Persist plans, summaries, and resumable sessions
- Bound each agent turn and recover cleanly from interruption

## Development

Development requires Go 1.25 or newer.

```bash
make check
```

## License

[MIT](LICENSE)
