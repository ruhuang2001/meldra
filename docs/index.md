---
title: "Meldra"
description: "A local, foreground coding agent with explicit safety boundaries and durable execution records."
---

Meldra helps you inspect and change a project from your terminal. It runs locally
in the foreground, streams responses in an interactive TUI, and records task
execution so you can inspect outcomes and explicitly recover interrupted work.

Meldra is experimental. Review changes and avoid using it in directories with
sensitive or irreplaceable files. Approved commands run as your OS user; Meldra
is not an OS sandbox.

## Start with a project

[Install Meldra and run your first session](quickstart.md). You need macOS or
Linux and an OpenAI API key or a compatible Responses API provider.

## Work with explicit boundaries

- **Inspect before editing.** The unreleased [Plan and Build modes](capabilities.md)
  separate read-only investigation from implementation.
- **Keep project instructions close to the code.** Unreleased scoped `AGENTS.md`
  files and explicit file references provide context for the paths you work on.
- **Stay in control.** Unreleased foreground controls let you steer a turn,
  queue a follow-up, or stop the current turn without leaving the session.
- **Connect tools and guidance.** Use [MCP servers](mcp.md) for integrations and
  [Skills](skills.md) for on-demand workflow instructions.
- **Inspect execution evidence.** The [task ledger](task-storage.md) records runs,
  tool outcomes, approvals, and managed processes. Recovery is explicit, not an
  automatic replay of side effects.

## Understand the runtime

Read [Architecture and runtime boundaries](architecture.md) for package ownership,
execution lifetimes, persistence, and recovery guarantees. Meldra is a serial
foreground agent, not a daemon, scheduler, or multi-agent execution service.
