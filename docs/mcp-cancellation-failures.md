# Interaction cancellation failure cases

Recorded before implementing the input fix:
- A server requests a form while stdin stays open without a response: the tool
  timeout must return, record an unknown result and close its server process.
- The same cancellation must work for sampling approval and URL confirmation.
- A callback may outlive the tools/call context supplied by the SDK: link it to
  the active outbound call, reject unsolicited requests, and join it at cleanup.
- Cancellation must not leave a reader goroutine consuming a future chat input.
- Preserve already-buffered lines through the resume picker and MCP approvals.
- TUI cancellation joins its reader before closing descriptors; no race warning.
- Linux regular-file and verified /dev/null stdin must reach EOF without epoll
  registration; run real processes with both input sources.
- Reject /dev/zero and other nonterminal character devices before reading: they
  can produce an endless line or block beyond the interaction timeout.
- Record each MCP fixture PID and verify it is gone after Meldra exits, including
  callback timeout and cancellation scenarios.
