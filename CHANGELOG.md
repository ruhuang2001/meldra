# Changelog

All notable changes to Meldra are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/). This file is updated automatically
by Release Please from Conventional Commits.

## [0.1.0-alpha.5](https://github.com/ruhuang2001/meldra/compare/v0.1.0-alpha.4...v0.1.0-alpha.5) (2026-08-28)


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
