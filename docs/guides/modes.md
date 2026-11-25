---
title: "Plan and Build modes"
description: "Investigate in Plan before anything changes, then implement in Build with the approvals you choose."
---

By default a session starts in Build mode: Meldra can change files and run
programs, asking first according to your permission profile.

Plan mode is the read-only half of the same session. Ask a question, read the
answer, and decide before anything is touched.

```sh wrap
meldra --mode plan --prompt 'Explain the project structure and suggest one small improvement.'
```

Plan mode reads and searches files, inspects diffs, and can save the plan. It
cannot write files, run commands, or reach the network without approval. It is
not a no-network mode: approved web reads stay available, so you can ask Meldra
to look something up while it plans. It also does not connect configured MCP
servers; switching to Build connects them on demand.

## From a plan to a change

Switch inside the session with `/plan` and `/build`, or press `Shift-Tab` in the
TUI composer. A mode change always needs your action; Meldra never changes mode
on its own. `/plan` waits for incompatible work to settle before it applies.

Plan mode is strict about what a request needs. For something that needs no
project reading — "rename this function" with no file named — Meldra replies
that Plan cannot make the change, names Build, and ends the turn instead of
exploring.

None of these can lift the Plan boundary: `--auto-approve`, Skills, `AGENTS.md`
instructions, or MCP `readOnlyHint` annotations. Meldra's own file tools are
blocked regardless of what a project file or server claims.

## Mode and permissions are separate

The **mode** decides whether Meldra may change anything. The **permission
profile** decides which changes still need your approval.

| Profile | File changes | Project programs | External effects |
| --- | --- | --- | --- |
| `interactive` (default) | Ask | Ask | Ask |
| `workspace-edit` | Checked built-in file operations granted | Ask | Ask |

`workspace-edit` grants Meldra's own file tools inside the workspace. It still
asks before running programs that execute project code, and it never
auto-approves network reads. An automatic grant under this profile is recorded
differently from a human approval, so the task record always shows which one happened.

```sh wrap
meldra --mode build --permissions workspace-edit
```

### Automatic approval

`--auto-approve` skips operation approvals entirely, including network
disclosure and the provider charges a hosted search can incur. Use it only in an
isolated container or virtual machine. It never expands Plan permissions.

Approved programs run as your own OS user. Executable restrictions and filtered
environment variables are not an OS sandbox: plan approvals accordingly, and
review a command before you accept it.

## Working elsewhere

`meldra mcp serve --workspace /path/to/project --mode plan` applies the same
rules when another MCP client drives Meldra's workspace tools.

## Related

- [Project context](./project-context.md): scoped `AGENTS.md` files and `@` references.
- [Sessions and controls](./controls.md): steer, queue, and stop a running turn.
