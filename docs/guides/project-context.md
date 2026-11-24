---
title: "Project context and file references"
description: "Give Meldra the right files: @ references for exact code, image attachments, and scoped AGENTS.md instructions."
---

Meldra sees a file only when a tool reads it or you point at it. There are two
explicit ways to point: `@` references in a message, and `AGENTS.md` guidance
that applies to a directory tree.

## Reference a file

```text wrap
Review @src/main.go
Compare @src/main.go:10-40 with @src/util.go:1-20
Read @"path with spaces/example.txt":2-5
```

In the TUI, press `Ctrl-F` to pick a file by name. Type to filter, use the arrow
keys to select, and press Enter to insert a quoted reference, or Escape to return
to your draft. The picker reads filenames only and is unavailable while an
approval or MCP form owns input.

Ranges are one-based and inclusive. Quote paths containing spaces; Unicode paths
work as written. Escape a literal reference with a backslash: `\@example.txt`.
Email addresses and references inside inline code or fenced blocks stay ordinary
text.

A reference is quoted file data. It cannot grant instructions or permissions, and
only the `AGENTS.md` loader assigns project-instruction meaning. References inside
a remote MCP prompt do not attach local files.

Meldra saves the path, range, digest, and a content snapshot in local task
artifacts, so a resumed task can show what was submitted even after the file
changes. Own limits apply per reference and per request; truncation is marked,
so use a narrower range for a large file.

Missing files, invalid ranges, protected paths, symlinks, hard links, special
files, and unsupported binary references are rejected before the model sees them.

### Attach an image

Use the same syntax with a PNG or JPEG:

```text wrap
Describe @screenshot.png
Compare @before.jpg and @after.png
```

Supported image signatures are recognized even without a filename extension.
Other raster and vector formats, plus corrupt files, are rejected. Images cannot
carry a line range, and clipboard paste is not implemented — save the image to
the workspace first.

Image bytes are stored as immutable, digest-addressed artifacts. The session keeps
only a reference; base64 is built for the individual model request. Resume and
accepted steering requests read the original artifact, so changing or deleting the
source file afterwards does not change what was sent. A missing or corrupt
artifact fails the request before inference instead of silently dropping an image.

Size limits, and the number of images kept in history, are listed in the
[configuration reference](../reference/configuration.md#reference-limits).

## Project instructions with `AGENTS.md`

Meldra reads `AGENTS.md` from the workspace root before requesting a model
response. When a file is accessed, it also discovers `AGENTS.md` files in that
file's ancestor directories inside the workspace. Root instructions apply
throughout the workspace; nested instructions apply only to their directory and
its descendants, and take precedence within that scope.

```sh wrap
meldra context --workspace /path/to/project --path src/main.go
meldra context --workspace /path/to/project --path src/main.go --json
```

The context inspector shows the source path, scope, contents, and digest without
calling the model. Inside an idle session, `/context src/main.go` shows the same
sources without submitting another request.

If an edit discovers instructions the model has not seen yet, Meldra rejects that
operation and asks the model to reconsider with the new context. A changed
instruction digest invalidates a pending operation. This also covers files the
model creates itself. Instructions never grant runtime permissions or authorize
their own edits.

Instruction files must be regular UTF-8 text without symbolic links. Meldra reads
at most 32 KiB per file across 1024 directories, and keeps the active payload
under 128 KiB including paths and digests. Invalid or oversized instructions
produce a visible error before anything is acknowledged or sent.

Parent directories outside the workspace, recursive includes, and alternative
instruction filenames are not loaded. Per-file discovery also cannot bound what a
program writes, because an approved program may touch files outside its working
directory.

## Reference limits

| Item | Limit |
| --- | --- |
| Source file | 1 MiB read per reference |
| Retained text | 64 KiB per reference, 128 KiB per request |
| References per request | 32 |
| Image | 1 MiB encoded, 16 million decoded pixels |
| Images per request | 2 MiB total |

Older images beyond the history budget are dropped and identified as omitted; the
most recent distinct images are kept. Compatible providers also apply their own
context limit to encoded multimodal input, so a very long text history may need a
smaller image or a fresh session.
