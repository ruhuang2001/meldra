---
title: "Skills"
description: "Pack reusable review checklists, project conventions, and reference files that Meldra loads only when a task needs them."
---

A skill is a folder with a `SKILL.md` file. Meldra sees its name and description
at the start of a conversation and reads the body only when the task calls for
it, so long checklists and reference files cost context only when used.

Use one when you repeat the same instructions — a review checklist, a migration
recipe, house style for a package. Leave `AGENTS.md` for facts that always apply
to a directory tree; see [Project context](../guides/project-context.md).

## Write a skill

Create the package next to the code it governs:

```text
.agents/skills/review-checklist/SKILL.md
.agents/skills/review-checklist/references/severity.md
```

```markdown
---
name: review-checklist
description: Check correctness, error handling, and verification evidence during code review.
---
Inspect changes and their call sites. Prefer reproducible findings with file
locations. Verify with existing tools and follow Meldra's command approval
requirements.

Read `references/severity.md` before ranking findings.
```

Frontmatter has two fields:

| Field | Rule |
| --- | --- |
| `name` | 1–64 lowercase ASCII letters, digits, or single hyphens; must match the directory name; no leading, trailing, or repeated hyphens |
| `description` | Nonempty, at most 1,024 characters; this is what the model uses to decide whether to load the skill |

Quoted values, folded scalars, and CRLF are accepted. Duplicate keys, wrong
types, and empty descriptions are rejected. Fields from other agents'
implementations — `allowed-tools`, `disable-model-invocation`, `context`,
argument substitution, `agents/openai.yaml` — are ignored and cannot grant
permissions or prevent loading.

Check the package without model credentials:

```sh wrap
meldra skills --workspace /path/to/project
meldra skills --workspace /path/to/project --json
```

## Use a skill

Name it in the request with `$` when you want a specific one:

```text wrap
Use $review-checklist to review the current changes.
```

Otherwise the model decides from the description. Reading a skill is an ordinary
`read_skill` call recorded with the task. It cannot run scripts, grant
permissions, or reach outside its package.

Pass a resource path to load supporting files relative to the package:

```json wrap
{"name":"review-checklist","path":"references/severity.md"}
```

## Where Meldra looks

Direct `<name>/SKILL.md` children of these directories, in order:

1. Workspace `.meldra/skills`
2. Workspace `.agents/skills`
3. `$MELDRA_HOME/skills`, default `~/.meldra/skills`
4. `$HOME/.agents/skills`

The first valid package wins a duplicate name; later copies are shadowed with a
warning. The catalog is sorted by name. Missing directories are skipped, and one
invalid package does not hide the others. Discovery does not recurse, read parent
repositories, or load `.claude` and `.codex` directories.

Both workspace sources ship with the repository and are visible to everyone
working in it. The two home sources are private to your machine — put anything
project-specific in the workspace copies.

## Limits

| Item | Limit |
| --- | --- |
| One `SKILL.md` or resource | 128 KiB |
| YAML frontmatter | 16 KiB |
| Direct entries across all sources | 256 |
| Initial catalog sent to the model | 64 KiB |

Oversized files are rejected rather than truncated, and entries that do not fit
the catalog are skipped with a warning.

The catalog is a snapshot for the life of the process: restart or resume after
adding, renaming, or redesigning a skill. Editing a body takes effect on the next
read, so a correction does not need a restart.

## Warnings and troubleshooting

| Symptom | Cause |
| --- | --- |
| `No skills found.` | No `.agents/skills/<name>/SKILL.md` in the workspace or home sources |
| Skill missing from the catalog | Invalid frontmatter, a name that differs from its directory, a symlinked package, or a filled catalog |
| Old description after an edit | The catalog is stale; restart or resume the session |
| A warning repeats | It appeared once per home; a changed message is shown again |

Identical discovery warnings are shown once per Meldra home, including across
restarts, with fingerprints cached privately in `skill-warnings.json`. Failing to
read or save that cache leaves the warnings visible.

## Security boundaries

Reading a skill does not execute anything. Workspace scripts it points to still
need the normal command allowlist and approval; a script outside the workspace
must first be copied in with approval. Skills do not add an operating-system
sandbox.

Packages outside the workspace are read through the same protected-path checks as
configuration and task storage. Traversal, symlinks, hard links, binary files, and
oversized resources are rejected, and a package cannot open configuration or
credential files by a relative path.

Compaction can discard guidance loaded earlier, so the instructions require
rereading a skill when it is still needed. That is a prompt-level rule, not proof
that a model always complies.

