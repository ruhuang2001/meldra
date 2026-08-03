# Changelog

All notable changes to Meldra are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/). This file is updated automatically
by Release Please from Conventional Commits.

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
