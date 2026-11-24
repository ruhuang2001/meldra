---
title: "First comment audit"
description: "The November 2026 audit that cleared ordinary source comments."
---

The November 23, 2026 audit covered 204 first-party source and configuration files at commit `12b2070`. The original 1,008 ordinary comment tokens occupied 985 complete lines and shared 23 lines with code. All were removed. The 81 functional directives were 28 interpreter lines, 23 Go build constraints and 30 Go module dependency markers.

Adjacent comment lines that shared a disposition were reviewed together. The inventory counts each lexical token separately, including each line of a Go `//` paragraph.

| Disposition | Reviewed groups | Comment tokens | Action |
| --- | ---: | ---: | --- |
| Tautology | 54 | 59 | Deleted |
| Stale | 6 | 10 | Deleted |
| Excuse | 3 | 7 | Deleted and linked to two issues |
| Decision Record | 473 | 932 | Moved into nine ADRs |

## Decisions

- [ADR 0001 · Build provenance and verification isolation](../../adr/0001-build-and-verification.md)
- [ADR 0002 · Foreground ownership and tool outcomes](../../adr/0002-foreground-ownership.md)
- [ADR 0003 · Responses streaming and continuation](../../adr/0003-provider-streams.md)
- [ADR 0004 · Context measurement and replay](../../adr/0004-context-and-history.md)
- [ADR 0005 · Workspace policy and approval scope](../../adr/0005-workspace-policy.md)
- [ADR 0006 · MCP transport and evidence](../../adr/0006-mcp-boundaries.md)
- [ADR 0007 · Durable execution and recovery](../../adr/0007-durable-execution.md)
- [ADR 0008 · Terminal ownership and bounded output](../../adr/0008-terminal-io.md)
- [ADR 0009 · Configuration, credentials and network input](../../adr/0009-private-configuration.md)

## Workarounds

MCP form validation rewrites numeric field schemas to preserve exact integers. [Issue #86](https://github.com/ruhuang2001/meldra/issues/86)

Generating near-limit SQLite fixtures led to race-build exclusions in both WAL recovery and index migration tests. [Issue #87](https://github.com/ruhuang2001/meldra/issues/87)

## Original source ranges

Ranges refer to the files as they existed before deletion. The source can be inspected at [commit 12b2070](https://github.com/ruhuang2001/meldra/tree/12b2070e3ca2f1ce023c54fbe5ddcddfd9b4fdcb). Each comma-separated range denotes one reviewed group.

| Source | Disposition | Original lines |
| --- | --- | --- |
| `.github/workflows/ci.yml` | Decision Record · ADR 0001 | 21, 40, 54, 60–61, 82–83, 106–107 |
| `.github/workflows/release-please.yml` | Decision Record · ADR 0001 | 33–35, 59–60, 93–94, 99–100, 110, 114–116 |
| `.gitignore` | Stale | 10–11 |
| `.gitignore` | Tautology | 17, 22 |
| `.goreleaser.yaml` | Decision Record · ADR 0001 | 61 |
| `Makefile` | Decision Record · ADR 0001 | 10–11 |
| `config/examples/local-responses.toml` | Decision Record · ADR 0001 | 1 |
| `config/examples/openai-compatible.toml` | Decision Record · ADR 0001 | 1–2 |
| `internal/app/agent.go` | Decision Record · ADR 0003 | 86–87, 569–572, 602–603, 639–641 |
| `internal/app/agent.go` | Stale | 89–90 |
| `internal/app/agent.go` | Decision Record · ADR 0004 | 96–98, 100–102, 105–106, 446–449 |
| `internal/app/agent.go` | Decision Record · ADR 0002 | 244–246, 275–277 |
| `internal/app/agent.go` | Tautology | 272 |
| `internal/app/agent.go` | Decision Record · ADR 0007 | 728–729 |
| `internal/app/agent_boundary_test.go` | Decision Record · ADR 0003 | 122 |
| `internal/app/agent_control_boundaries_test.go` | Decision Record · ADR 0007 | 55 |
| `internal/app/cli.go` | Decision Record · ADR 0001 | 24 |
| `internal/app/cli.go` | Decision Record · ADR 0008 | 27, 32–34, 134–135 |
| `internal/app/cli.go` | Decision Record · ADR 0006 | 243 |
| `internal/app/cli.go` | Decision Record · ADR 0007 | 769, 868–869 |
| `internal/app/command_presentation.go` | Decision Record · ADR 0008 | 11–12, 24–25 |
| `internal/app/command_presentation_test.go` | Decision Record · ADR 0008 | 188–189, 444–445 |
| `internal/app/command_preview_test.go` | Decision Record · ADR 0008 | 20–21 |
| `internal/app/command_runtime.go` | Decision Record · ADR 0008 | 32–33, 51–52, 189–191, 276–278, 290–291, 297–298 |
| `internal/app/command_unix.go` | Decision Record · ADR 0008 | 13, 34–35 |
| `internal/app/commands.go` | Decision Record · ADR 0005 | 53–54 |
| `internal/app/commands.go` | Decision Record · ADR 0002 | 254 |
| `internal/app/compaction.go` | Decision Record · ADR 0004 | 20–21, 33–34, 80–81, 87–88, 106, 129–130, 230–231, 259–260 |
| `internal/app/compaction_context.go` | Decision Record · ADR 0004 | 76, 105, 115–116, 148–149, 291–294, 306–307, 319–320, 328–329 |
| `internal/app/compaction_history.go` | Decision Record · ADR 0004 | 314–315, 318–319, 358–360, 440–441, 490–492 |
| `internal/app/compaction_test.go` | Tautology | 25, 30 |
| `internal/app/compaction_test.go` | Decision Record · ADR 0004 | 42 |
| `internal/app/config.go` | Decision Record · ADR 0009 | 18–19, 26–27, 67–69, 97–99, 116, 136–137, 150–152, 169–171, 185–186, 210–211, 226–227, 230–231, 237–239, 266–268, 270–271, 339–341, 365–367, 376–377, 422–424, 838–840 |
| `internal/app/config.go` | Decision Record · ADR 0004 | 39–40 |
| `internal/app/config.go` | Tautology | 76, 91, 399, 483–484 |
| `internal/app/control.go` | Decision Record · ADR 0002 | 37–38, 72–73, 221–222, 229, 289–290, 316, 331–332, 346, 429, 433–434, 472–473 |
| `internal/app/durability.go` | Decision Record · ADR 0007 | 13–15, 34–37, 43–46 |
| `internal/app/evaluation_test.go` | Decision Record · ADR 0002 | 33–35 |
| `internal/app/inference.go` | Decision Record · ADR 0004 | 35–36 |
| `internal/app/jsonstream.go` | Decision Record · ADR 0008 | 33 |
| `internal/app/line_control.go` | Decision Record · ADR 0008 | 83–84, 91–92, 110–112, 132–133, 138, 150–152, 176–177, 201–202, 235–236 |
| `internal/app/mcp.go` | Decision Record · ADR 0006 | 131, 172, 251–253, 292, 318, 342, 398, 419–420, 464 |
| `internal/app/mcp.go` | Tautology | 156 |
| `internal/app/mcp_binding.go` | Decision Record · ADR 0006 | 17–18, 29–30, 104–105, 166 |
| `internal/app/mcp_catalog.go` | Decision Record · ADR 0006 | 12 |
| `internal/app/mcp_cli.go` | Tautology | 3–4 |
| `internal/app/mcp_cli.go` | Decision Record · ADR 0006 | 140, 155–156 |
| `internal/app/mcp_context_approval.go` | Decision Record · ADR 0006 | 9–11 |
| `internal/app/mcp_interaction.go` | Decision Record · ADR 0006 | 82, 223–225, 237–238, 260, 397, 409, 437, 476 |
| `internal/app/mcp_interaction.go` | Excuse · #86 | 159–161 |
| `internal/app/mcp_oauth.go` | Decision Record · ADR 0006 | 126–127, 161, 175, 186–188, 270–271, 522 |
| `internal/app/mcp_oauth.go` | Tautology | 235 |
| `internal/app/mcp_review_test.go` | Decision Record · ADR 0006 | 25–26 |
| `internal/app/mcp_server.go` | Decision Record · ADR 0006 | 25, 83–84, 108, 195–196, 207–208, 291 |
| `internal/app/output_linux.go` | Decision Record · ADR 0008 | 18–19 |
| `internal/app/output_unix.go` | Decision Record · ADR 0008 | 15–16, 34–35, 80–84, 144–145, 168–169 |
| `internal/app/policy.go` | Decision Record · ADR 0005 | 23–24, 161, 174 |
| `internal/app/policy_control_test.go` | Decision Record · ADR 0005 | 156 |
| `internal/app/process_cli_test.go` | Decision Record · ADR 0007 | 33–34 |
| `internal/app/process_manager.go` | Decision Record · ADR 0007 | 18–20, 31–32, 39, 121–122 |
| `internal/app/process_manager_test.go` | Decision Record · ADR 0007 | 258–261 |
| `internal/app/process_persistence_fault_test.go` | Decision Record · ADR 0007 | 60–61, 122 |
| `internal/app/process_recovery.go` | Decision Record · ADR 0007 | 42–43 |
| `internal/app/project_context.go` | Decision Record · ADR 0005 | 28–29, 49–51, 130–132, 187–188, 207–209, 260–261, 266–267, 361, 410–412 |
| `internal/app/project_context.go` | Tautology | 149 |
| `internal/app/recovery_boundaries_test.go` | Decision Record · ADR 0008 | 319–320 |
| `internal/app/reference_images.go` | Decision Record · ADR 0004 | 80–81, 144–145 |
| `internal/app/references.go` | Decision Record · ADR 0004 | 29–30, 44–45, 303, 319–320, 365–367, 405–406, 494–495 |
| `internal/app/request_history.go` | Decision Record · ADR 0004 | 13–14 |
| `internal/app/session.go` | Decision Record · ADR 0007 | 77–78, 85, 89–90, 99–101, 297–299, 339–340, 454–455, 610, 632, 752–753, 798–799 |
| `internal/app/session_commands.go` | Decision Record · ADR 0007 | 24, 70, 109, 142, 153, 170 |
| `internal/app/session_commands.go` | Decision Record · ADR 0004 | 56 |
| `internal/app/session_commands_test.go` | Tautology | 193 |
| `internal/app/session_persistence_test.go` | Decision Record · ADR 0007 | 38–39, 104–105 |
| `internal/app/skills.go` | Decision Record · ADR 0005 | 51–52, 108, 233–234, 242–243, 252, 296–297 |
| `internal/app/skills_other.go` | Decision Record · ADR 0005 | 10–11 |
| `internal/app/skills_unix.go` | Decision Record · ADR 0005 | 11–12 |
| `internal/app/steering_batch_test.go` | Decision Record · ADR 0003 | 18–19, 93–94 |
| `internal/app/steering_context.go` | Decision Record · ADR 0003 | 9–11 |
| `internal/app/stream_test.go` | Decision Record · ADR 0003 | 612–613, 647, 804–807, 966 |
| `internal/app/task_approval.go` | Decision Record · ADR 0007 | 12–13, 55–56, 111–112 |
| `internal/app/task_cli.go` | Decision Record · ADR 0007 | 394, 412–414 |
| `internal/app/task_cli_test.go` | Decision Record · ADR 0007 | 259–260 |
| `internal/app/task_execution.go` | Decision Record · ADR 0007 | 22–23, 65, 107, 147–148, 207–208, 211, 282–283, 370, 381–382, 557–558, 571–573, 589–590 |
| `internal/app/task_execution_test.go` | Decision Record · ADR 0007 | 215, 220 |
| `internal/app/task_fingerprints.go` | Decision Record · ADR 0007 | 15–16, 80, 111–112 |
| `internal/app/task_preflight_test.go` | Decision Record · ADR 0007 | 151 |
| `internal/app/task_recovery_fault_test.go` | Decision Record · ADR 0007 | 30–32, 307–308 |
| `internal/app/task_request_recovery_test.go` | Decision Record · ADR 0007 | 43–44, 145, 169 |
| `internal/app/task_shutdown_test.go` | Decision Record · ADR 0007 | 27–28, 261, 294 |
| `internal/app/task_shutdown_test.go` | Decision Record · ADR 0008 | 357–358, 446–448 |
| `internal/app/task_undo.go` | Decision Record · ADR 0007 | 14–15, 93–95 |
| `internal/app/terminal_presentation.go` | Decision Record · ADR 0008 | 8–9, 35, 95 |
| `internal/app/terminal_stream.go` | Decision Record · ADR 0008 | 8–9 |
| `internal/app/text_limits.go` | Decision Record · ADR 0008 | 8–11 |
| `internal/app/text_limits.go` | Stale | 23–24 |
| `internal/app/tui.go` | Decision Record · ADR 0008 | 40–41, 46–49, 85–87, 138–140, 167–169, 261–262, 351–353, 373, 474–475, 497–499, 507–509, 700–704, 894–896, 963–965, 1147–1148, 1159–1161, 1175–1176, 1184–1186, 1251–1252, 1284–1285 |
| `internal/app/tui_command_output.go` | Decision Record · ADR 0008 | 11–12, 21, 27 |
| `internal/app/tui_mode_test.go` | Decision Record · ADR 0008 | 102–103, 128–129, 151–152 |
| `internal/app/tui_references.go` | Decision Record · ADR 0008 | 41–42, 183–184 |
| `internal/app/tui_test.go` | Decision Record · ADR 0008 | 308 |
| `internal/app/ui_events.go` | Decision Record · ADR 0008 | 8–9, 29–31, 56–57, 76–77, 91–92 |
| `internal/app/ui_events.go` | Tautology | 24 |
| `internal/app/update.go` | Decision Record · ADR 0009 | 45, 275–276, 299, 349–350 |
| `internal/app/web.go` | Decision Record · ADR 0009 | 72–73, 184, 414 |
| `internal/app/web_network.go` | Decision Record · ADR 0009 | 11–12 |
| `internal/app/workspace.go` | Decision Record · ADR 0005 | 21–22, 43–44, 49, 74–76, 89–90, 95–96, 260–261, 280–282 |
| `internal/app/workspace.go` | Tautology | 252 |
| `internal/app/workspace_bench_test.go` | Decision Record · ADR 0005 | 11–12 |
| `internal/app/workspace_bench_test.go` | Tautology | 14, 28, 30, 58, 61, 77, 79, 98, 114, 116, 130, 135, 152, 180, 184, 187, 190 |
| `internal/app/workspace_bench_test.go` | Stale | 44–45 |
| `internal/app/workspace_change.go` | Decision Record · ADR 0005 | 126–128, 287–288, 350, 452–453, 555–557, 588–589 |
| `internal/app/workspace_command.go` | Tautology | 77 |
| `internal/app/workspace_command.go` | Decision Record · ADR 0005 | 92–93, 102–104, 139–141, 248–251, 484–485, 527–529 |
| `internal/app/workspace_files.go` | Decision Record · ADR 0005 | 103–105, 184–186, 353 |
| `internal/app/workspace_git_commit.go` | Decision Record · ADR 0005 | 24, 83, 95–96, 107, 133–134, 148–149, 217–218 |
| `internal/app/workspace_test.go` | Decision Record · ADR 0005 | 363 |
| `internal/app/workspace_undo.go` | Decision Record · ADR 0005 | 124–125, 137–138, 346–348 |
| `internal/app/workspace_undo.go` | Tautology | 330, 338 |
| `internal/app/workspace_verify.go` | Decision Record · ADR 0005 | 68–69, 328–330, 369–370 |
| `internal/provider/conversation.go` | Decision Record · ADR 0003 | 37–39, 84–85, 96–97, 114–116, 121–122, 150–152, 176–177, 222–223 |
| `internal/provider/conversation.go` | Tautology | 144, 146–147, 245 |
| `internal/provider/conversation_test.go` | Decision Record · ADR 0003 | 159, 238–239 |
| `internal/provider/responses.go` | Tautology | 31 |
| `internal/provider/responses.go` | Decision Record · ADR 0003 | 40, 49, 58–59, 78, 85–86, 93–95, 103–104, 187–188, 219–221, 245–246, 335–337, 355–356, 410–411, 436–438, 451–452, 487–489, 511–512, 540–541, 567–568, 601–602, 647, 664–666, 721–723, 770–773, 791–795, 819–823, 829–834, 856–858, 892–894 |
| `internal/store/artifacts.go` | Decision Record · ADR 0007 | 17–18, 50–51, 65–66 |
| `internal/store/capacity_wal_test.go` | Excuse · #87 | 13–14 |
| `internal/store/capacity_wal_test.go` | Decision Record · ADR 0007 | 39 |
| `internal/store/identity_test.go` | Decision Record · ADR 0007 | 29–30 |
| `internal/store/index_capacity_test.go` | Excuse · #87 | 3–4 |
| `internal/store/index_capacity_test.go` | Decision Record · ADR 0007 | 33 |
| `internal/store/lease.go` | Decision Record · ADR 0007 | 15–17, 121–123, 132–133 |
| `internal/store/lock_other.go` | Decision Record · ADR 0007 | 11–12 |
| `internal/store/lock_unix.go` | Decision Record · ADR 0007 | 14–15, 32 |
| `internal/store/processes.go` | Decision Record · ADR 0007 | 60–61, 118–119 |
| `internal/store/recovery_limits_test.go` | Decision Record · ADR 0007 | 36, 97, 153 |
| `internal/store/store.go` | Decision Record · ADR 0007 | 1–2, 40–41, 45–46, 87–88, 120–121, 174–175, 318, 349, 364–365, 436–437 |
| `internal/store/store_bench_test.go` | Tautology | 13–14, 68, 112, 147, 166, 190, 201, 224–225, 250, 281 |
| `internal/store/store_bench_test.go` | Stale | 269 |
| `internal/store/store_test.go` | Decision Record · ADR 0007 | 573 |
| `internal/store/tasks.go` | Decision Record · ADR 0007 | 77–79, 97, 134, 148–149, 331–332, 348–350 |
| `internal/store/tools.go` | Decision Record · ADR 0007 | 154–155 |
| `internal/task/process.go` | Decision Record · ADR 0007 | 17–18 |
| `internal/task/process.go` | Tautology | 31 |
| `internal/task/task.go` | Decision Record · ADR 0007 | 1–2, 14, 79–80 |
| `internal/testenv/environment.go` | Decision Record · ADR 0009 | 1–2, 13 |
| `internal/tool/result.go` | Decision Record · ADR 0002 | 17, 24–25, 37–39 |
| `internal/tool/tool.go` | Decision Record · ADR 0002 | 1–2, 12–13, 22–23, 54–55 |
| `internal/tool/tool.go` | Tautology | 18 |
| `main.go` | Decision Record · ADR 0001 | 5 |
| `scripts/capability-e2e.py` | Decision Record · ADR 0001 | 73, 498–499, 580–581 |
| `scripts/check-local-ci.py` | Decision Record · ADR 0001 | 64–65, 116–117 |
| `scripts/check-scripts-test.py` | Stale | 67 |
| `scripts/mcp-auth-interaction-e2e.py` | Decision Record · ADR 0001 | 388 |
| `scripts/mcp-check.py` | Decision Record · ADR 0001 | 150, 161–162, 181–182, 189–190, 194–195, 216, 230–231 |
| `scripts/mcp-e2e.py` | Decision Record · ADR 0001 | 24, 404 |
| `scripts/mcp-input-e2e.py` | Decision Record · ADR 0001 | 135, 220 |
| `scripts/mcp-oauth-regression-e2e.py` | Decision Record · ADR 0001 | 175–176, 271 |
| `scripts/skills-e2e.py` | Tautology | 351 |
| `scripts/swe-bench-adapter-e2e.py` | Decision Record · ADR 0001 | 307, 379 |
| `scripts/swe-bench-lite.py` | Decision Record · ADR 0001 | 46–47, 175, 229, 241–242, 258–259, 312, 331–333, 441 |
| `scripts/swe-bench-lite.py` | Tautology | 341 |
| `scripts/web-e2e.py` | Decision Record · ADR 0001 | 155, 171, 371 |

Run `make -s comment-inventory` for a fresh inventory of the current sources. [Comment policy and lint commands](../development.md#source-comments)
