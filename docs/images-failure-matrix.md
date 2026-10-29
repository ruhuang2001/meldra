# Image reference failure matrix

**Unreleased.** Specified before implementation. Use the real-binary image E2E
suite with a new output directory. All provider traffic uses a loopback fixture.

| Boundary | Failure to prevent | Required evidence |
| --- | --- | --- |
| Submission | Image becomes binary text instead of provider image content | Initial image data URL decodes to the exact file digest; ordinary text attachments unchanged |
| Tool replay | Compatible provider loses initial images on tool follow-up | Both requests contain the same bytes; tool output remains present |
| Persistence | Base64 or raw image bytes enter session JSON or ledger | Only MIME/path/digest/artifact metadata; artifact bytes match submitted file |
| Resume | Changed/deleted source replaces the accepted image | Resumed provider request uses original artifact bytes |
| Controls | Queued/steered images are reread after acknowledgment | Original accepted bytes survive source replacement, with durable control identity |
| Security | Symlink, hardlink, protected-state or traversal source is read | Rejected before inference; no secret content in provider requests |
| Format | Unsupported image or corrupt header/body enters inference | PNG/JPEG accepted; unsupported formats and damaged images rejected |
| Limits | Image/file count, bytes or decompressed dimensions consume unbounded resources | 1 MiB/file, 2 MiB/current image request, 16 million decoded pixels; reject overflow |
| Ranges | Image line ranges silently select different bytes | Any image range rejected |
| Artifacts | Missing/corrupt historical image silently disappears | Resume fails before model request |
| History | Older attachments exceed reconstruction budget | Most recent images retained within 2 MiB; omitted-image count explicitly reported |
| Evidence | Passing checks cannot be reproduced | Reports include source/binary hashes, requests, transcript, ledger and artifact checksums |
