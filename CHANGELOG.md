# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- Claude Code 2.1.319 or later is needed: an older one doesn't install a plugin published after setup by itself. `setup`, `sync` and `doctor` warn when `claude --version` is older, and say to update it.
- line-lx sends `User-Agent: line-lx/<version>` with every request to the hub, so the hub can tell line-lx's own reads of the marketplace from Claude Code's.

## [0.1.2] - 2026-11-10

### Added

- `auth-header` prints your sign-in as an HTTP header, `{"Authorization":"Bearer <token>"}`: Claude Code runs it to sign in to the hub. It answers only when Claude Code names the marketplace or archive it asks for, and only when that is under the current hub's URL.

### Changed

- Claude Code gets the plugins from the hub's marketplace by itself. `setup` and `sync` connect it once, in its user settings: the hub's marketplace, signed in with `line-lx auth-header` and auto-updated, each of its plugins, enabled, so the first Claude Code session brings them all, and the hub's `metro-lx` plugin, which depends on all the others, so a plugin published later comes when Claude Code auto-updates. `sync` installs anything still missing at once; a plugin it can't install comes at Claude Code's next start. New versions come from Claude Code's auto-update. This needs Claude Code 2.1.238 or later, and a hub that serves `metro-lx`. The first time, the plugins come in a new Claude Code session, because `claude plugin marketplace add` refuses a marketplace whose settings entry has a `headersHelper`.
- `sync` moves Claude Code off the local marketplace that earlier versions wrote, then deletes its folder, `<config dir>/line-lx/claude-code/`. The plugins come back when Claude Code starts again.
- `doctor` checks Claude Code's connection to the hub, in its settings or its managed settings files, the `line-lx` it runs, and its version. `list` shows what Claude Code installed from the hub.
- Claude Code is found by its folder, `~/.claude` or `$CLAUDE_CONFIG_DIR`, as well as by `claude` on the PATH.

### Removed

- line-lx no longer downloads plugins for Claude Code, installs, updates or uninstalls them one by one, or reports their deliveries to the hub.
- The special case for plugins Claude Code also gets from claude.ai (`<plugin>@synced`): Claude Code loads one copy by itself.

## [0.1.1] - 2026-11-09

### Fixed

- `login` and `setup` read the hub's protected resource metadata at its RFC 9728 address, `/.well-known/oauth-protected-resource/mcp`. They failed with a 404 against a real hub.

## [0.1.0] - 2026-11-09

### Added

- `line-lx` commands: `setup`, `login`, `logout`, `whoami`, `sync`, `list` and `doctor`.
- Delivery of a Metro-LX hub's plugins to Claude Code, Codex and pi.
- Signed releases for macOS and Linux, on amd64 and arm64, with SBOMs and build provenance.

[Unreleased]: https://github.com/moqa-studio/line-lx-cli/compare/v0.1.2...HEAD
[0.1.2]: https://github.com/moqa-studio/line-lx-cli/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/moqa-studio/line-lx-cli/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/moqa-studio/line-lx-cli/releases/tag/v0.1.0
