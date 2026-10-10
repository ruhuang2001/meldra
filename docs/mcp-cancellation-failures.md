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
- Linux regular-file or /dev/null stdin must work without epoll registration;
  existing real-process shutdown/EOF checks cover this path.
