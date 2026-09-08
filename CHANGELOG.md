# Changelog

## [v0.0.4](https://github.com/hiboma/esdump/compare/v0.0.3...v0.0.4) - 2026-09-08

- サプライチェーン対策: ビルドとリリース経路の hardening by @hiboma in https://github.com/hiboma/esdump/pull/13

## [v0.0.3](https://github.com/hiboma/esdump/compare/v0.0.2...v0.0.3) - 2026-09-08

- Test export against a real OpenSearch, and fix the four bugs it found by @hiboma in https://github.com/hiboma/esdump/pull/11
- 書き込みの失敗を捨てて exit 0 で終わるのを直す by @hiboma in https://github.com/hiboma/esdump/pull/10
- Document the Actions setting tagpr depends on by @hiboma in https://github.com/hiboma/esdump/pull/7

## [v0.0.2](https://github.com/hiboma/esdump/compare/v0.0.1...v0.0.2) - 2026-09-07

- feat: ログの出力間隔とタイムゾーンをオプションで指定できるようにする by @hiboma in https://github.com/hiboma/esdump/pull/4
- feat: parallel export with sliced scroll (--slices, --size) by @hiboma in https://github.com/hiboma/esdump/pull/9

## [v0.0.1](https://github.com/hiboma/esdump/commits/v0.0.1) - 2026-09-07

- add CLAUDE.md by @hiboma in https://github.com/hiboma/esdump/pull/1
- elasticdump compatible fields by @hiboma in https://github.com/hiboma/esdump/pull/2
- fix: treat io.EOF from ScrollService as normal completion by @hiboma in https://github.com/hiboma/esdump/pull/3
- Automate releases with tagpr and GoReleaser by @hiboma in https://github.com/hiboma/esdump/pull/5
