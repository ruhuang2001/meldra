---
title: "Plan and Build modes"
description: "Decide how much Meldra may change before it starts: investigate in Plan, implement in Build, and choose a permission profile."
---

A session has two independent settings. The **mode** decides whether Meldra may
change anything at all. The **permission profile** decides which changes still
need your approval.

| Mode | Allowed work |
| --- | --- |
| `plan` | Read and search workspace files, inspect diffs, and save the task's plan or summary. |
| `build` | Use any available tool within the selected permission profile. |

## Choose a mode

```sh wrap
meldra --mode plan --prompt 'Inspect the project and propose a change.'
meldra --mode build --permissions workspace-edit
meldra mcp serve --workspace /path/to/project --mode plan
```

Inside a session, switch with `/plan` and `/build`, or press `Shift-Tab` in the
TUI composer. A mode change always needs a user action; Meldra never enters Plan
or Build on its own. `/plan` waits for incompatible active work to settle before
it applies.

In Plan mode the agent reads the workspace only when the plan needs project
information. For a request that needs no project context — "rename this
function" with no file named — it replies that Plan cannot perform the change,
names Build as the required mode, and ends the turn instead of exploring.

### What Plan mode blocks

Plan mode denies workspace edits, undo, tests, builds, formatting, process
creation, and unclassified external effects at runtime. None of these can lift
that boundary:

- `--auto-approve`
- Skills
- `AGENTS.md` instructions
- MCP `readOnlyHint` annotations

Plan mode is not a no-network mode. Approved built-in web reads stay available,
so you can ask Meldra to look something up while it plans. Starting a Plan
session does not connect configured external MCP servers; switching to Build
connects them on demand.

## Choose a permission profile

| Profile | File changes | Project programs | External effects |
| --- | --- | --- | --- |
| `interactive` (default) | Ask | Ask | Ask |
| `workspace-edit` | Checked built-in file operations granted | Ask | Ask |

`workspace-edit` grants Meldra's own file tools inside the workspace. It still
asks before running programs that execute project code, and it never
auto-approves network reads. An automatic grant under this profile is recorded
differently from a human approval, so the ledger always shows which one happened.

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

## Related

- [Project context](./project-context.md): scoped `AGENTS.md` files and `@` references.
- [Sessions and controls](./controls.md): steer, queue, and stop a running turn.
- [Task records and recovery](../reference/task-records.md): what an approval leaves behind.
