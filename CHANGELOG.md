# Changelog

All notable changes to Meldra are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/). This file is updated automatically
by Release Please from Conventional Commits.

## [0.3.1](https://github.com/ruhuang2001/meldra/compare/v0.3.0...v0.3.1) (2026-10-10)


### Bug Fixes

* **release:** keep legacy tag checks outside the checkout ([d633788](https://github.com/ruhuang2001/meldra/commit/d63378887c133fad5a37d76751ea483f50019060))
* **release:** separate check evidence from release artifacts ([e8f841a](https://github.com/ruhuang2001/meldra/commit/e8f841a6e8821805605876e906689e30ace07434))

## [0.3.0](https://github.com/ruhuang2001/meldra/compare/v0.2.0...v0.3.0) (2026-10-10)


### Features

* **agent:** add bounded skills discovery and loading ([91a71e4](https://github.com/ruhuang2001/meldra/commit/91a71e4c30fbef7f076c0eb7f6d9c5af141ece33))
* **agent:** add MCP client and server workflows ([0d2a825](https://github.com/ruhuang2001/meldra/commit/0d2a8259d7870625f534ed6134f473da3cd8eecf))


### Bug Fixes

* **agent:** address skills and runtime review findings ([ac350bb](https://github.com/ruhuang2001/meldra/commit/ac350bbc24e3c62efd91b3d553366547eda6d48e))
* **agent:** close remaining MCP validation and dispatch gaps ([81eefa6](https://github.com/ruhuang2001/meldra/commit/81eefa6da5ca8de970e54330f3bf2fa048940f8e))
* **agent:** enforce MCP numeric and credential limits ([eaa77de](https://github.com/ruhuang2001/meldra/commit/eaa77de90dc30e866970234b02ae7a190b6d8419))
* **agent:** finish OAuth callback responses before shutdown ([2f13483](https://github.com/ruhuang2001/meldra/commit/2f134836622a7a7fc179121395d702280d7f4a43))
* **agent:** harden MCP authorization and runtime boundaries ([ec3d58d](https://github.com/ruhuang2001/meldra/commit/ec3d58da0dbee58a39fec42c0c58eb3205a46827))
* **agent:** harden MCP review boundaries and record verification ([18f1794](https://github.com/ruhuang2001/meldra/commit/18f1794f67924b14923b1db48a34d6cbe033463f))
* **cli:** preserve unpollable stdin on Linux ([fbc96d1](https://github.com/ruhuang2001/meldra/commit/fbc96d1735dbc0bba92dc206f05647d5a47a3c56))
* **session:** preserve legacy provider replay identities ([e89f90b](https://github.com/ruhuang2001/meldra/commit/e89f90b246d09c8e1fd21d1fac6c4c0e87a96b3d))
* **stream:** adapt tool output and events to updated SDK ([b097886](https://github.com/ruhuang2001/meldra/commit/b097886d9778234c41ad6ed03a472ef770687de6))
* **stream:** preserve recovered assistant output positions ([a12b1f3](https://github.com/ruhuang2001/meldra/commit/a12b1f30b191a6f733eefde387163412b47c8737))

## [0.2.0](https://github.com/ruhuang2001/meldra/compare/v0.1.0...v0.2.0) (2026-10-09)


### Features

* **agent:** integrate foreground recovery and task controls ([c2fbb33](https://github.com/ruhuang2001/meldra/commit/c2fbb33216c1c3394e483601667efd69dc77c430))
* **session:** persist task records and execution ownership ([0bb40ea](https://github.com/ruhuang2001/meldra/commit/0bb40ea6a3b3284200fcdfc276058d563bdc4e3a))


### Bug Fixes

* **agent:** harden execution recovery and provider replay ([f8ab8ae](https://github.com/ruhuang2001/meldra/commit/f8ab8aef2d5109790e4ba69f403dc08566f2b509))
* **agent:** preserve recovery evidence across failure boundaries ([0c40e43](https://github.com/ruhuang2001/meldra/commit/0c40e4388f92cceff2fca46ddad54c9397fb0ff4))
* **agent:** resolve remaining runtime and recovery issues ([4be7c2e](https://github.com/ruhuang2001/meldra/commit/4be7c2e3efb6a842ade7b0a91837789209816639))
* **ci:** update Go toolchain for security fixes ([9d28c09](https://github.com/ruhuang2001/meldra/commit/9d28c0910b81e857ca2270987c8750debafc7daf))
* **cli:** remove custom provider startup warning ([8dd71d2](https://github.com/ruhuang2001/meldra/commit/8dd71d2044162a3b65c3c7dd183d8034d2a4d62c))
* **session:** stop execution after snapshot persistence failures ([7fcd435](https://github.com/ruhuang2001/meldra/commit/7fcd435648ad5416605e74fcf7ce6062d9c7a4bb))
* **stream:** preserve terminal tool identity during merge ([744547f](https://github.com/ruhuang2001/meldra/commit/744547f7f145a29a5f2a59a4552d2991f3a95120))
* **tui:** simplify session identification at startup ([35b9507](https://github.com/ruhuang2001/meldra/commit/35b95074390b29509c83f8f45a6075dee701a12a))


### Performance

* **stream:** assemble completed text only when needed ([170fe97](https://github.com/ruhuang2001/meldra/commit/170fe979db83ff81bb9bcf6914c60cbb19436d70))

## [0.1.0](https://github.com/ruhuang2001/meldra/compare/v0.1.0-alpha.4...v0.1.0) (2026-09-15)


### Features

* **agent:** remove turn execution limits ([5b511d8](https://github.com/ruhuang2001/meldra/commit/5b511d8f326e2e301b8b8cc9ee2475898d893cd9))


### Bug Fixes

* **tui:** preserve native copy and valid file output ([fbc5d9f](https://github.com/ruhuang2001/meldra/commit/fbc5d9fa5eecb7e94285c65df1dcf6a7c8ec3c23))

## [0.1.0-alpha.4](https://github.com/ruhuang2001/meldra/compare/v0.1.0-alpha.3...v0.1.0-alpha.4) (2026-08-28)


### Features

* **agent:** improve runtime safety and efficiency ([f78cd18](https://github.com/ruhuang2001/meldra/commit/f78cd18bb8dbe62bcdf8df5dc99148b8f43d1931))
* **agent:** improve verification and runtime safeguards ([141bd6f](https://github.com/ruhuang2001/meldra/commit/141bd6f0c9b3895d3d75d5c46de14f8de812bb3f))
* **benchmarks:** record sanityharness baselines ([428d6d8](https://github.com/ruhuang2001/meldra/commit/428d6d89c7aaa29e9a2de39dd5cc0edec0881fe0))
* **ci:** acknowledge package requests with a reaction ([ce95bc3](https://github.com/ruhuang2001/meldra/commit/ce95bc3d214c5d5044f18bb8825df58157774917))
* **ci:** add PR comment packaging workflow ([e963c58](https://github.com/ruhuang2001/meldra/commit/e963c58ecf0a3bde710c72192c324188fb1a4574))
* **session:** improve resume and TUI experience ([7d4fc9d](https://github.com/ruhuang2001/meldra/commit/7d4fc9d05c332f36657f16715ef7c85cc0cfc2ff))


### Bug Fixes

* **agent:** limit provider responses ([aa78c7b](https://github.com/ruhuang2001/meldra/commit/aa78c7b10fce1655eb3fee9d4bf56faaf186404a))
* **ci:** grant PR comment notification permissions ([e329277](https://github.com/ruhuang2001/meldra/commit/e3292779e006c5492e830d5a7463b63d1fcd7180))
* **ci:** identify PR package runs by marker ([2d4d67b](https://github.com/ruhuang2001/meldra/commit/2d4d67ba76e046c479372c18eb53847a4aa643b1))
* **ci:** keep PR package comments in report job ([8e12e44](https://github.com/ruhuang2001/meldra/commit/8e12e44e9a7093481f61cf8d0f368cd177d3f790))
* **ci:** keep PR packaging running without comment permissions ([9876d26](https://github.com/ruhuang2001/meldra/commit/9876d26f3244d879aed946906c135e0421adec82))
* **ci:** preserve PR package notifications ([fea2ce8](https://github.com/ruhuang2001/meldra/commit/fea2ce87d3277657690e7285462652fe780b3675))
* **ci:** report PR package status reliably ([664f2ce](https://github.com/ruhuang2001/meldra/commit/664f2ce7f3cf63dce6c3c96cb84d8afb52fb082c))
* **ci:** streamline PR package comments ([253690d](https://github.com/ruhuang2001/meldra/commit/253690d6da1ee94b4ba030027261efa64e55cb91))
* **cli:** resume explicit sessions from saved workspace ([bbba07c](https://github.com/ruhuang2001/meldra/commit/bbba07c8b06094f4282499b23f4809b4d7202319))
* **session:** preserve saved sessions durably ([7cdfe60](https://github.com/ruhuang2001/meldra/commit/7cdfe6010107abb4fdcd1dafd218963522706d99))
* **workspace:** block pytest argument and path expansion ([94bde93](https://github.com/ruhuang2001/meldra/commit/94bde93fada6a2b1768f426ab6d00d06cce4397f))
* **workspace:** bound edits and isolate commands ([e7906eb](https://github.com/ruhuang2001/meldra/commit/e7906eb4703eca38c81e4b9ccfe855f8642544ff))
* **workspace:** restore created directories during rollback ([5424a53](https://github.com/ruhuang2001/meldra/commit/5424a5337a9eca53a7bb48c377218c8d2bbdf554))

## [0.1.0-alpha.3](https://github.com/ruhuang2001/meldra/compare/v0.1.0-alpha.2...v0.1.0-alpha.3) (2026-08-03)


### Features

* **benchmarks:** add sanityharness and swe-bench lite evaluation harnesses ([c8bdafd](https://github.com/ruhuang2001/meldra/commit/c8bdafdbb3f0ef080ed24f486bb96ec81327370c))
* **cli:** add --prompt option for non-interactive agent invocation ([16876e6](https://github.com/ruhuang2001/meldra/commit/16876e6abb86c39882a8ed1418f2792a135e5735))
* **workspace:** allow python3 -m pytest for benchmark self-verification ([30a2c11](https://github.com/ruhuang2001/meldra/commit/30a2c11f5165c111d38ad14baefb85e481077acc))


### Bug Fixes

* **agent:** harden benchmark execution ([95e0946](https://github.com/ruhuang2001/meldra/commit/95e0946941d0d61f209eaf4a67721723212e204e))
* **ci:** upgrade release checks to go 1.26 ([9d79795](https://github.com/ruhuang2001/meldra/commit/9d7979561be33c0267a71990bb70eb52cd52584b))
* **ci:** use goreleaser v2.13.3 for go 1.25.12 compatibility ([f22237f](https://github.com/ruhuang2001/meldra/commit/f22237fc952ab6a9afdd6b2b459ccefa53934e90))
* **release:** harden artifact recovery ([6098681](https://github.com/ruhuang2001/meldra/commit/60986816675e680e71107a76a0ecd8cc750cd87e))
* **release:** make validation and artifact publishing repeatable ([7cd4bf7](https://github.com/ruhuang2001/meldra/commit/7cd4bf7b7a6c4a80771e21c4563e1f2def6821a9))

## [0.1.0-alpha.2](https://github.com/ruhuang2001/meldra/compare/v0.1.0-alpha.1...v0.1.0-alpha.2) (2026-08-02)


### Features

* **tui:** add streaming agent interface ([cfb2779](https://github.com/ruhuang2001/meldra/commit/cfb27799ee76e09b0ab3bd4fdddaf5606a12373f))


### Bug Fixes

* **agent:** preserve custom gateway tool context ([dcaa0cf](https://github.com/ruhuang2001/meldra/commit/dcaa0cfd2a77daf4935d8896825157045682699c))
* **tui:** harden interactive fallback behavior ([d4a01f1](https://github.com/ruhuang2001/meldra/commit/d4a01f1b1bc4cab0ba369ed44efb9dcf1e18adf0))
* **tui:** preserve unicode summaries ([b2e556f](https://github.com/ruhuang2001/meldra/commit/b2e556f07ab4aac7c49cbd1c39136aba07636feb))
* **tui:** surface streaming response state ([971ad95](https://github.com/ruhuang2001/meldra/commit/971ad95c69ec8b1646be5330d79a8608275884e5))

## 0.1.0-alpha.1 (2026-08-01)


### Features

* **agent:** add OpenAI file-editing tool loop ([7b49a4f](https://github.com/ruhuang2001/meldra/commit/7b49a4f9b4ae482ebcc0de025ea0956021d095d4))
* **agent:** add safe workspace workflow ([5fea352](https://github.com/ruhuang2001/meldra/commit/5fea352e9dce5113a56c7bed0973bad81791bd35))


### Bug Fixes

* **agent:** support custom OpenAI-compatible base URLs ([2c13af3](https://github.com/ruhuang2001/meldra/commit/2c13af373892fbfe3d57f0a0cad8d44a7cb06192))
