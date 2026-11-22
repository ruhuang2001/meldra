---
name: delete-comment
description: Audit Meldra source comments, delete redundant or stale prose, move design rationale into ADRs, and track workaround removals against the comment lint baseline.
---

Use the requested paths, defaulting to the whole repository. Run `make -s comment-inventory` from the repository root and keep the before inventory outside the source set. Inspect the working-tree version of each file, including tests and untracked project files. See the first audit in `docs/verification/comment-audit.md` for the reporting format.

Review each ordinary comment in context. Adjacent lines explaining one choice can share a disposition. Preserve functional directives, literal fixture contents, prompts and runtime help. A comment that both narrates an operation and explains a constraint can contain a decision worth keeping.

| Category | Action |
| --- | --- |
| Tautology | Delete prose that repeats the code. |
| Stale | Verify the inconsistency against the implementation or history, then delete it. |
| Excuse | Identify the workaround and the behavior a replacement must preserve. Find or file a repository issue and link the affected comments to it before removal. |
| Decision Record | Move the choice, its reason and its consequences into a topic under `docs/adr/`. Reuse an existing ADR when it already covers the choice. Delete the source prose. |

Preserve unrelated edits. Remove comment-only lines and just the comment on mixed lines. Check that executable tokens, directives and observable fixture data survive. For each removed legacy occurrence, remove its matching path/text entry from `scripts/source-comments-baseline.json`. The baseline can shrink.

Report the original locations, categories and ADR or issue destinations. Sum the inventory's ordinary `standalone_lines` for the source-line saving. Count inline removals separately. Run `make comment-lint-e2e` and the repository checks appropriate to the changed sources. Compare the final inventory to the initial one and state what remains.
