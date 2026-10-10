# Meldra Skills Research and Validation Prototype

The initial prototype was researched in October 2026 on `codex/skills-support`.
This document describes the discovery rules, integration boundaries, and validation approach.

## Design

Skills provide discoverable workflow guidance and resources loaded on demand. Meldra reuses its existing executor, workspace command tools, approvals, task records, and artifact storage. No separate script runtime is introduced.

This implements the application flow rather than full compatibility with other agents' permission extensions, plugin installers, or slash commands. `$name` is a prompt convention that asks the model to read a skill, not a frontend-enforced command.

## Reference implementations

| Reference | Discovery and duplicates | Loading and invocation | Implication for Meldra |
| --- | --- | --- | --- |
| Agent Skills specification [1] | Defines package format, not directories or precedence | YAML metadata, Markdown body, optional scripts and resources; progressive disclosure | Adopt the format and layered loading, with explicit Meldra discovery policy |
| Codex documentation [2] | Workspace `.agents/skills`, user and managed/system directories; same-name skills may coexist | Initial names, descriptions, and paths; bodies loaded for explicit mentions or matching tasks | Include metadata in context and read bodies and references on demand |
| Claude Code documentation [3] | `.claude/skills` from project, personal, and enterprise sources; documented precedence is enterprise > personal > project | Skill commands, argument substitution, invocation controls, and permission extensions | Do not interpret agent-specific extensions as portable Meldra authorization |
| OpenCode documentation and source [4] | Native and `.claude`/`.agents` directories, recursive discovery, additional sources | Dedicated skill reader with a base directory and permission checks; command-name conflict handling | Use a dedicated audited reader and document discovery and duplicate behavior |

OpenCode source was examined at commit `907b3bc518fa48e90e8ec24dd327d13eee71c36c`, including `skill/index.ts`, `skill/discovery.ts`, `tool/skill.ts`, and `command/index.ts`. Its concurrent `Effect.forEach` loading overwrites duplicate map entries after warning, which does not establish a deterministic last-scanner-wins rule. The reader calls `ctx.ask`; command registration avoids existing command names.

Format, discovery, and execution policy are separate compatibility questions. The integration guide's project-over-user summary differs from Claude Code's documented precedence. Meldra's precedence is its own policy, not a universal interoperability rule.

## Discovery and metadata

Scan direct `<name>/SKILL.md` children in this order:

1. Workspace `.meldra/skills`.
2. Workspace `.agents/skills`.
3. `$MELDRA_HOME/skills`, defaulting to `~/.meldra/skills`.
4. `$HOME/.agents/skills`.

The first valid package admitted to the catalog wins a duplicate name. Shadowed sources produce warnings; the final catalog is sorted by name. Missing directories are skipped, and invalid packages do not block valid ones. Discovery does not recurse, search parent repositories, or read private `.claude` and `.codex` sources.

`SKILL.md` must be UTF-8 text beginning with YAML frontmatter:

- `name`: 1–64 lowercase ASCII letters, digits, or single hyphens, matching the directory name; no leading, trailing, or consecutive hyphens.
- `description`: a nonempty string of at most 1,024 Unicode characters.
- Quoted values, folded scalars, and CRLF are supported. Duplicate keys and invalid field types are rejected.
- Other fields do not affect policy. `allowed-tools`, `disable-model-invocation`, `context`, argument substitution, and `agents/openai.yaml` are not implemented. They cannot grant permissions or disable implicit loading.

The implementation uses `go.yaml.in/yaml/v3` v3.0.5 because neither the Go standard library nor the project's existing dependencies provided a YAML parser.

| Limit | Behavior |
| --- | --- |
| One file: 128 KiB | Oversized instructions and resources are rejected rather than silently truncated |
| YAML header: 16 KiB | Oversized or unterminated headers are rejected |
| Direct entries: 256 total | An overfull root and subsequent sources are skipped with a warning instead of choosing a filesystem-order subset |
| Initial catalog JSON: 64 KiB | Entries that do not fit are skipped with warnings; bodies are excluded |

The catalog is a process-lifetime snapshot, refreshed on restart or resume. Each `read_skill` call reads the current resource. Body edits take effect immediately; new packages, renames, and changed descriptions require a refresh.

## Integration and security boundaries

`newChatRuntime` discovers skills after protecting runtime paths and registers `read_skill` when valid packages exist. `runInference` adds names, descriptions, and paths to every request without preloading bodies. Empty catalogs retain explicit missing-skill guidance.

Example tool input:

    {"name":"review-checklist","path":null}

`path=null` reads `SKILL.md`; `path=references/checklist.md` reads a resource relative to its package. Output includes the name, base directory, and resource path. Successful reads use digest-addressed task artifacts and the `read` effect. Recovery reuses existing task, session, and audit mechanisms without adding tables.

Configuration, task storage, and execution locks are protected across all sources, including packages outside the workspace. The sole configuration-directory exception permits `read_skill` to access native `$MELDRA_HOME/skills` packages. Protected descendants inside those packages remain blocked; ordinary file tools retain their configuration-directory restrictions.

Absolute paths, traversal, `.git`, symlinks, hard-linked files, non-regular files, binary data, and oversized resources are rejected. Configured home prefixes are canonicalized; package components never follow symlinks. Directory identities and file link counts are checked during opening. Darwin/Linux use `O_NOFOLLOW` and `O_NONBLOCK` to prevent final-file symlink and FIFO swaps. Other platforms reject skill reads before opening files.

Skill guidance remains subordinate to the user's request and runtime restrictions. Reading does not execute scripts, grant permissions, or add tools. Workspace scripts use the existing allowlist and approvals; external scripts must first be copied into the workspace with approval. Approved commands run as the user, and Skills do not add an operating-system sandbox.

Compaction may discard previously loaded guidance. The prompt requires rereading it when needed. Tests cover fresh discovery and resource loading after cross-process recovery, but do not prove that a real model reliably rereads instructions after compaction.

## Try it

Create `.agents/skills/review-checklist/SKILL.md` in a test project:

    ---
    name: review-checklist
    description: Check correctness, error handling, and verification evidence during code review.
    ---
    Inspect changes and their call sites. Prefer reproducible findings with file locations.
    Verify with existing tools and follow Meldra's command approval requirements.

Build and inspect the test project:

    mkdir -p dist && go build -o dist/meldra-skills .
    ./dist/meldra-skills skills --workspace /path/to/test-project --json
    ./dist/meldra-skills --workspace /path/to/test-project --prompt 'Use $review-checklist to review the current changes'

Listing requires no API credentials. Chat uses the existing model and credential configuration.

## Verification and evidence

The failure matrix [5] and executable harness preceded the initial implementation. Subsequent review fixes also have focused Go regression tests. `scripts/skills-e2e.py` builds the real binary, isolates `HOME`, `MELDRA_HOME`, and the workspace, and drives the CLI, tools, approvals, SQLite records, and session recovery through a deterministic loopback Responses SSE server. It uses fake credentials and no paid model.

    python3 scripts/skills-e2e.py --output dist/skills-e2e-new-run
    make check-local-ci

The output directory must be new or empty. Stage changes before `make check-local-ci`, which checks an isolated copy of the index. Keep a separate output directory for each E2E run.

The final PR evidence in `dist/pr42-merge-review/skills-e2e-final/assertions.json` records 121 passing checks, source and binary SHA-256 hashes, HTTP requests/responses, stdout/stderr, task/event JSON, fixtures, generated files, and audit artifacts. `dist/pr42-merge-review/check-local-ci.log` records successful race tests, vet, formatting, dependency checks, build, and 81.2% coverage. These local artifacts are not tracked in Git and are historical evidence for the tested source; rerun verification for later changes.

The suite covers external packages containing protected configuration, global reads, CLI/runtime cache protection consistency, static FIFO rejection, hard-linked metadata and credential rejection, approval decline and acceptance, multiline presentation, help, and recovery with fresh bodies. Earlier local validation also executed a cross-built Linux arm64 binary against the same 121 checks; cross-compilation alone does not establish native runtime behavior.

The scripted provider validates application behavior, not live-model skill selection or instruction compliance. Arbitrary concurrent filesystem replacements, mounts, and malicious-instruction resistance require separate security review.

## Future trade-offs

Add a frontend selector when deterministic selection is needed. Explicit-only invocation requires enforceable policy filtering rather than prompt text alone. Consider pagination, hot reload, and ancestor discovery when catalog scale or update frequency justifies them. Marketplaces, installers, remote downloads, new executors, and new permission systems are outside this prototype.

## Primary sources

1. Agent Skills: [specification](https://agentskills.io/specification) and [integration guide](https://agentskills.io/integrate-skills).
2. OpenAI: [Codex Skills](https://developers.openai.com/codex/skills/) and [Markdown](https://developers.openai.com/codex/skills.md).
3. Claude Code: [Skills](https://code.claude.com/docs/en/skills) and [duplicate resolution](https://code.claude.com/docs/en/skills#resolve-skills-that-share-a-name).
4. OpenCode: [documentation](https://opencode.ai/docs/skills/) and pinned [loader](https://github.com/anomalyco/opencode/blob/907b3bc518fa48e90e8ec24dd327d13eee71c36c/packages/opencode/src/skill/index.ts), [discovery](https://github.com/anomalyco/opencode/blob/907b3bc518fa48e90e8ec24dd327d13eee71c36c/packages/opencode/src/skill/discovery.ts), [skill tool](https://github.com/anomalyco/opencode/blob/907b3bc518fa48e90e8ec24dd327d13eee71c36c/packages/opencode/src/tool/skill.ts), and [commands](https://github.com/anomalyco/opencode/blob/907b3bc518fa48e90e8ec24dd327d13eee71c36c/packages/opencode/src/command/index.ts).
5. This repository: [failure matrix](skills-failure-matrix.md), [E2E harness](../scripts/skills-e2e.py), and [implementation](../internal/app/skills.go).
