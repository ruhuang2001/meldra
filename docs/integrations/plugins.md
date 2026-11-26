---
title: "Agent Plugins"
description: "Load portable Agent Plugins v1.0.0 packages containing skills and MCP servers with explicit local trust."
---

**Unreleased:** Meldra supports the portable [Agent Plugins v1.0.0](https://agent-plugins.org/specification)
package format for local plugin directories containing `plugin.json`, `skills/`,
and `mcp.json`.

A plugin is a self-contained directory:

```text
my-plugin/
├── plugin.json
├── skills/
│   └── summarize/
│       └── SKILL.md
└── mcp.json
```

Nothing in a plugin runs automatically. Inspection is read-only, enabling a
plugin activates its skills only, and starting its MCP servers requires a
separate explicit approval bound to the plugin's MCP configuration and bundled
executables.

## Discover and enable a plugin

Place each unpacked package directly under `$MELDRA_HOME/plugins/` (default:
`~/.meldra/plugins/`). `list` shows both available and enabled packages;
use the `name` from `plugin.json`, not the folder name:

```sh wrap
meldra plugins list
meldra plugins inspect my-plugin
meldra plugins enable my-plugin
```

Listing and inspecting do not enable skills, write a registry, create plugin
data, or start MCP servers. Discovery does not recurse, follow symlinked package
directories, or scan the current project. If multiple packages declare the same
name, use the path shown by `list` to select one. Explicit paths also work for
packages outside the discovery directory:

```sh wrap
meldra plugins inspect ./my-plugin --json
meldra plugins enable ./my-plugin
```

Enabling records the package location and activates its skills across sessions.
MCP requires a separate step. After reviewing the plugin's `mcp.json`, approve
or revoke its servers by registered name:

```sh wrap
meldra plugins allow-mcp my-plugin
meldra plugins revoke-mcp my-plugin
```

Disable a plugin while preserving its persistent `PLUGIN_DATA` directory across
updates, or remove both the registration and `PLUGIN_DATA`. A package still in
the discovery directory returns to the available state, without MCP approval:

```sh wrap
meldra plugins disable my-plugin
meldra plugins disable my-plugin --purge-data
```

## Conformance and package boundaries

Meldra follows the Agent Plugins 1.0.0 specification:

- **Manifest (`plugin.json`):** Selects local validation from the canonical
  `$schema` identifier (`https://agent-plugins.org/schemas/1.0.0/plugin.schema.json`)
  without fetching schemas over the network. Unknown top-level fields and a
  non-object `extensions` field are reported and ignored; unimplemented
  extension namespaces are ignored without validating their values. Other
  manifest schema violations reject the plugin before component discovery.
- **Package containment:** Every package file, skill, bundled executable, and
  plugin-relative `cwd` must stay inside the filesystem-resolved plugin root.
  Internal symlinks resolving inside the plugin root are allowed; symlinks
  resolving outside it are rejected at the narrowest failure boundary.
- **Skills (`skills/`):** Immediate child directories with a regular `SKILL.md`
  are discovered without recursion. Invalid skills are skipped individually and
  do not block sibling skills or MCP servers. Plugin skills follow workspace and
  user skill directories in precedence, appear in `meldra skills`, and are read
  through `read_skill`. Plugin skills follow the portable Agent Skills frontmatter
  rules, including Unicode names and a string-to-string `metadata` map.
- **MCP servers (`mcp.json`):** Validated in two stages. Top-level errors or a
  mismatched `$schema` disable MCP for that plugin without affecting skills.
  Invalid server entries or runtime startup failures skip only that server.
  Meldra supports `stdio`, `streamable-http`, and legacy `sse` transports using
  each entry's declared `type`.
- **Plugin variables and stdio runtime:** stdio servers default their working
  directory to the plugin root and receive `PLUGIN_ROOT` and a persistent,
  writable `PLUGIN_DATA` directory (`$MELDRA_HOME/plugin-data/<name>`) created
  before launch. `${PLUGIN_ROOT}` and `${PLUGIN_DATA}` expand in a single
  non-recursive pass in `args`, `env` values, and `cwd` only. Configured `env`
  overlays Meldra's sanitized base environment; `OPENAI_API_KEY` is never
  inherited.
- **Remote headers and origins:** Configured `headers` are literal values sent
  only to the configured origin. Client-generated HTTP, MCP, and authorization
  headers take precedence. Redirects are disabled, and configured headers are
  never forwarded to another origin through redirects or legacy SSE endpoint
  events.

## Security and approval model

Plugin registration lives in private `$MELDRA_HOME/plugins.json` (`0600`).
Keep plugin directories separate from Meldra's private state: do not register
`MELDRA_HOME`, a directory containing it, or any of its subdirectories except
`$MELDRA_HOME/plugins/` and packages inside it.

Plugin MCP servers appear as `plugin/<plugin-name>/<server-name>` in approvals,
task records, and `meldra mcp` catalog commands. The last segment is URL-escaped;
copy the server ID from `meldra plugins inspect` for names containing slashes,
spaces, or non-ASCII characters. `meldra plugins allow-mcp`
records a digest of the effective server configuration and the contents of
bundled `./...` commands. Changing either blocks the next startup until you
inspect and re-approve the plugin. Whitespace-only edits do not invalidate
approval. A bundled command that cannot be read is skipped, not approved.

This is startup approval, not a sandbox or a signature over the package.
Commands run as your OS user. Scripts passed as arguments to an interpreter,
dependencies, and programs resolved through `PATH` are not pinned by this
approval. Enable MCP only for packages you trust; keep their files and runtime
dependencies out of directories writable by untrusted users. Revocation
prevents future connections; close an existing session to stop its servers.

Plan mode, external MCP call approvals, connection-binding checks, and task
records apply to plugin MCP servers identically to servers configured in
`$MELDRA_HOME/mcp.json`. See [Skills](./skills.md) and [MCP servers](./mcp.md).
