---
title: "Introduction"
description: "Meldra is a local coding agent for your terminal: it asks before sensitive actions and keeps a readable record of what it did."
---

Meldra reads your project, proposes changes, and runs commands you approve — all
inside one terminal session that ends when you close it. It does not keep
working in the background after you leave.

```sh wrap
curl -fsSL https://raw.githubusercontent.com/ruhuang2001/meldra/main/scripts/install.sh | sh
```

## Start here

<CardGroup cols={2}>
  <Card title="Quickstart" icon="rocket" href="/quickstart">
    Install, configure a provider, and run your first session in a few minutes.
  </Card>
  <Card title="Plan before Build" icon="compass" href="/guides/modes">
    Ask for a proposal, then let Meldra implement it with the approvals you choose.
  </Card>
  <Card title="Project context" icon="file-code" href="/guides/project-context">
    Point Meldra at files with `@` references and scoped `AGENTS.md` guidance.
  </Card>
  <Card title="Sessions and controls" icon="sliders" href="/guides/controls">
    Steer, queue, stop, resume, and compact a running conversation.
  </Card>
</CardGroup>

## A first request

```sh wrap
cd /path/to/project
meldra --mode plan --prompt 'Explain the project structure and suggest one small improvement.'
```

Plan mode reads and searches files but cannot write, run commands, or reach the
network without approval. When the plan looks right, switch to Build mode and let
Meldra make the change.

## What it does

- **Reads before it writes.** Build mode starts with investigation, and Plan
  mode lets you ask for a proposal without touching a file.
- **Asks before it acts.** File edits, commands, and network reads go through an
  approval prompt unless you have granted that permission for the session.
- **Keeps a local record.** Every task, tool call, and approval is written to a
  task record you can inspect with `meldra task show` — after a crash too.
- **Stays on task.** One request, one workspace, one turn at a time. You can
  steer, queue, or stop a turn while it runs.

Approved commands run with your own user permissions. Meldra is careful about
approvals, but it is **not an operating-system sandbox**: review what it asks to
run, and avoid working in directories whose contents you cannot afford to lose.

## Go further

| Topic | Page |
| --- | --- |
| External tools, browsers, and data | [MCP servers](./integrations/mcp.md) |
| Reusable checklists and workflows | [Skills](./integrations/skills.md) |
| External memory, policy checks, and run observers | [Lifecycle hooks](./integrations/hooks.md) |
| Fetching and searching the web | [Web fetch and search](./integrations/web-tools.md) |
| Every configuration key | [Configuration reference](./reference/configuration.md) |

## Status

Meldra is experimental and pre-1.0. The published archives are tested on macOS
and Linux for `amd64` and `arm64`; behavior and file formats can change between
minor releases. Pages mark features that are not in the latest release as
**Unreleased**, and the [Changelog](./changelog.mdx) records what shipped.
