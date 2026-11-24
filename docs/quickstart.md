---
title: "Quickstart"
description: "Install Meldra, connect a provider, and run a first request in a few minutes."
---

You need macOS or Linux on `amd64` or `arm64`, `curl`, `tar`, and either
`sha256sum` or `shasum`. You also need an OpenAI API key, or an endpoint that
implements the Responses API.

## 1. Install

```sh wrap
curl -fsSL https://raw.githubusercontent.com/ruhuang2001/meldra/main/scripts/install.sh | sh
```

The installer picks your platform, verifies the SHA-256 checksum, and installs to
`~/.local/bin` without `sudo`.

If `meldra` is not found afterwards, add that directory to your `PATH` for the
current terminal, then check the version:

```sh wrap
export PATH="$HOME/.local/bin:$PATH"
meldra version
```

Add the same export to `~/.zshrc` or `~/.bashrc` for future terminals; the
installer does not edit your shell files. You can also download a platform archive
from [GitHub Releases](https://github.com/ruhuang2001/meldra/releases).

## 2. Configure a provider

```sh wrap
meldra config init
```

This writes `~/.meldra/config.toml` and `~/.meldra/credentials.env`. Put your key
in the credentials file:

```sh wrap
OPENAI_API_KEY=your-api-key
```

Never commit that file. `OPENAI_API_KEY`, `OPENAI_MODEL`, and `OPENAI_BASE_URL`
override file values for a single process, and `MELDRA_HOME` selects a different
state directory. Check the effective configuration without printing the key:

```sh wrap
meldra config
```

<Note>
Using a local model server? See [Providers](./reference/providers.md) for the
Responses endpoint requirement, loopback HTTP rules, and a private-home setup
that keeps a local model separate from your main configuration.
</Note>

## 3. Start a session

```sh wrap
cd /path/to/project
meldra
```

A supported interactive terminal opens the full-screen TUI; other terminals and
non-interactive runs use a line interface. Press `Ctrl-C` to exit.

Start with a read-only request if this is your first time:

```sh wrap
meldra --mode plan --prompt 'Explain the project structure and suggest a small improvement.'
```

Plan mode blocks project writes and command execution. When you are ready to
implement, start Build mode and review each approval prompt. Automatic approval is
not a sandbox — use it only in an isolated environment.

## 4. Point it at the right code

Meldra reads `AGENTS.md` from the workspace root, plus any `AGENTS.md` in an
accessed file's parent directories. Reference specific files inline:

```text wrap
Review @src/main.go and compare it with @src/util.go:1-40
```

Add project guidance close to the code, and check what applies to a path without
calling the model:

```sh wrap
meldra context --path src/main.go
```

## Next steps

- [Plan and Build modes](./guides/modes.md): what Plan blocks and how permissions work.
- [Sessions and controls](./guides/controls.md): steer, queue, stop, and resume.
- [MCP servers](./integrations/mcp.md) and [Skills](./integrations/skills.md): external tools and reusable workflows.
- [Upgrade and downgrade](./reference/upgrading.md): self-update, backups, and rollback.
