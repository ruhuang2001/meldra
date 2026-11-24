---
title: "Custom slash commands"
description: "Turn repeated prompts into Markdown templates that expand into ordinary user messages."
---

A custom command is a Markdown file whose body becomes a normal user message.
Use it for prompts you retype often: reviewing a diff, writing a migration, or
checking a release.

## Create a command

Save `.meldra/commands/review.md` in the workspace, or `~/.meldra/commands/review.md`
for every project:

```markdown wrap
---
description: Review the current diff for correctness bugs
argument-hint: "[focus area]"
---
Review the uncommitted diff. Pay special attention to: $ARGUMENTS
Report findings with file locations and a one-line explanation.
```

Then invoke it like any other slash command:

```text wrap
/review error handling
```

Or pass it to a one-shot prompt:

```sh wrap
meldra --prompt '/review error handling'
meldra commands --workspace /path/to/project          # list without credentials
meldra commands --workspace /path/to/project --json
```

Only workspace `.meldra/commands` and `$MELDRA_HOME/commands` are searched. The
project file wins a duplicate name, and built-in commands always win. Invalid or
shadowed files produce a warning and do not replace a valid entry.

Listing is offline: JSON contains `commands` and `warnings`, with each command's
`name`, `description`, `argument_hint`, and source path — never the body. Restart
or resume to pick up edits; a running process uses an immutable snapshot.

## Arguments

`$ARGUMENTS` receives the full trimmed argument string, preserving interior
whitespace. `$1` through `$9` receive whitespace-separated positions; a missing
position becomes empty text. Quoting has no shell meaning.

Substitution happens once. Dollar placeholders inside the arguments stay literal,
and expanded text never invokes another command or a built-in control. An unknown
slash name remains ordinary text. Templates never run a shell.

Commands go through the normal reference, policy, approval, session, and ledger
path, so Plan restrictions and approval rules are unchanged. `/steer /review focus`
and `/queue /review focus` expand before their control request is recorded, and a
recovered request keeps the saved expansion. A remote MCP prompt never expands a
local command.

## Completion and file rules

In the TUI, typing `/` lists built-in and custom commands. Type a prefix to
filter, use Up/Down to select, Tab to insert, then Enter to send. Escape dismisses
the suggestions. Completion is disabled while an approval, MCP form, or the
correction composer owns input.

Filenames use 1–64 lowercase ASCII letters, digits, or single hyphens, without
leading, trailing, or repeated hyphens. Frontmatter needs a non-empty
`description` of at most 512 characters; `argument-hint` is optional. Quoted and
folded YAML, UTF-8, BOM, and CRLF are supported. Duplicate keys, wrong types, empty
bodies, binary data, and symbolic or hard links are rejected.

| Limit | Value |
| --- | --- |
| Template file | 32 KiB |
| Candidate files across both sources | 128 |
| Expanded message | 16 KiB (rejected, never truncated) |

Runtime state and execution-lock paths stay protected; only the native
`$MELDRA_HOME/commands` directory is allowed for discovery.
