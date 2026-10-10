# Skills E2E failure matrix

Written before the skills implementation and its executable verification. Run
`python3 scripts/skills-e2e.py --output dist/skills-e2e` from the repository.
The harness builds the real binary, uses temporary HOME/MELDRA_HOME/workspace
directories, and talks only to a deterministic loopback Responses SSE server.
It uses no real API credentials. It leaves its inputs, HTTP requests, command
stdout/stderr, task/event JSON, artifact digests, and assertions report in the
output directory; a failed assertion exits nonzero and retains the evidence.

| Boundary | Failure to prevent | E2E evidence |
| --- | --- | --- |
| Discovery | Missing source or precedence picks lower-priority duplicate | Four search roots, sorted catalog, duplicate winner and warning |
| Offline CLI | Skills listing requires a model, credentials, or HTTP | Catalog runs before config exists with API variables removed |
| Help and presentation | Help exits unsuccessfully, or untrusted metadata and path newlines forge listing rows or chat warnings | Both help flags exit successfully; multiline paths and terminal-control fixtures retain exact JSON while human listings and skill warnings stay on their designated lines |
| YAML | Hand parser drops quoted colon/hash, folded scalars, CRLF, optional fields | Exact parsed descriptions in catalog JSON |
| Metadata | Invalid/mismatched names, empty/oversized descriptions, duplicate keys or malformed YAML accepted | Adversarial fixtures excluded with diagnostics |
| Filesystem | Discovery follows linked directories or SKILL.md files | Linked fixtures absent from catalog |
| Hard links | A regular-looking resource aliases credentials, or linked metadata bypasses package confinement | Hardlinked SKILL.md excluded; actual read_skill of hardlinked credentials fails, with no secret in provider requests or audit |
| Trusted home aliases | Symlinked HOME or MELDRA_HOME hides legitimate user skills or permits package symlinks | Alias catalog matches canonical catalog while package links remain rejected; runtime retains its existing prohibition on a symlinked MELDRA_HOME |
| Discovery bounds | Oversized file/frontmatter, candidate flood, or metadata flood expands context | Separate capped catalogs with limit diagnostics |
| Progressive loading | Entire skill body or reference leaks into initial request | Unique markers absent initially; metadata and read_skill schema present |
| Activation | Explicit $name has no instruction to load skill | Initial instructions describe explicit read_skill activation |
| Read tool | Body/reference inaccessible despite valid catalog | SSE tool calls and subsequent request outputs contain unique markers |
| Read confinement | Unknown skill, absolute/traversal, .git, symlink, binary, oversized file, or protected state can be read | Failed audited calls and no secret marker in any provider request |
| Nested protected state | User skill outside workspace contains MELDRA_HOME and exposes credentials through a relative resource | Nested state/credentials.env read fails; sentinel absent from HTTP requests and audited output |
| Global skill exception | Protecting MELDRA_HOME blocks legitimate native state/skills packages | Actual read_skill calls load global source bodies with durable artifacts |
| Cache protection | CLI discovers a cache-protected source that runtime forbids, or nested skill resources expose execution-cache state | Isolated native cache layouts exercise matching CLI/runtime exclusion and failed resource reads |
| Special files | FIFO skill metadata or resource blocks a read waiting for a writer | Static FIFO fixtures are rejected without opening and bounded CLI/runtime finish |
| Existing tools | Skills bypass workspace/protected-state boundaries | Existing read_file cannot read outside/protected fixture |
| Execution | Reading a skill authorizes executable code | Declined run_command leaves no output; approved run creates exact output |
| Audit | Skill loading is treated as mutation or misses durable evidence | read effect, no read approvals, stored output artifact hashes |
| Resume | Cached catalog/body persists after source change | Same saved session resumes with fresh metadata and body marker |
| Reproducibility | A passing assertion cannot be traced to source/build | Report contains source and binary SHA-256 digests and complete traces |

Run the harness manually; it is not part of CI. Source digests cover the Go
application, module files, harness and failure matrix; unrelated local projects
and build artifacts are excluded.

This exercises the application boundary, not a nondeterministic live model's
judgment. The scripted provider follows the exposed protocol and requests both
valid and adversarial operations. Race resistance and arbitrary concurrent
filesystem replacement require separate security review.
