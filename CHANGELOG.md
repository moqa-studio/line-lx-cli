# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.4.1] - 2026-11-23

### Changed

- `status` reads like the metro-lx map. Each agent is a terminus, each plugin a line, with the route number and the color the web app gives it, and its skills (●) and hooks (▲) are the stations on it, each shape on its part's pastel. Plugins come in the web app's order, then the ones the hub doesn't have, by name. Claude Code's `metro-lx` takes the interchange ring ◎ and comes last. "up to date" is green, "behind" shows both versions (`v1.0.0 → v1.1.0`), and "not installed", "not in the hub" and "other hub" are grey. Times read as in the web app ("2h ago", "yesterday", then the date). Colors are 24-bit when `COLORTERM` is `truecolor` or `24bit`, else the closest of xterm's 256. Without color, the shapes and words say the same, and the route shows as `(1)`. The output still fits in 80 columns.
- `status --json` gives each plugin its `route` (1 to 6) and `line`, from the hub. Both are `null` with a hub that doesn't give them.

## [0.4.0] - 2026-11-20

### Added

- `status` shows what's connected on this machine. For Claude Code, Codex and pi: whether the agent is connected to the hub, and each plugin it has from the hub, with its version, whether it's behind the hub's latest, when it was installed and last updated, its skills, and its hooks (the usage hook of Claude Code's `metro-lx`). For Claude Code, it also shows when it last fetched the marketplace. It reads Claude Code's own records, `installed_plugins.json` and `known_marketplaces.json`, and line-lx's stamps, then compares them with the hub's `GET /api/me/installed`. Without a sign-in, or without an answer from the hub, it shows what this machine has, and says why. It changes nothing. Names and versions are never cut, a bundle's 12-character version included; only descriptions are, by the columns they take, so a line with Chinese or emoji fits in 80 columns too. `--json` prints the same report in a stable format, documented in the README.

### Fixed

- Text from the hub or `claude` loses its terminal escape sequences whole (CSI, OSC and the others) before line-lx prints it. Before, only the escape character went, and the rest of a sequence, such as `[31m`, was printed.

## [0.3.0] - 2026-11-19

### Added

- `report-usage --agent claude-code --workspace <slug>` reports one use of the workspace's plugins to the hub: the plugin and the version Claude Code installed, the skill or MCP tool, the kind of use, who started it and whether it succeeded. The hub's Claude Code hook runs it while the workspace's agent reporting is on. It sends a name only when it belongs to a plugin Claude Code installed from the workspace's marketplace on the current hub, and a bare skill name only when no other skill or command Claude Code can find has it. It never sends commands, arguments, prompts, paths or session identifiers. It ends within 3 seconds, never retries, and always exits 0.
- `doctor` shows how many usage reports were dropped since when, until one reaches the hub again.

## [0.2.0] - 2026-11-19

### Changed

- line-lx signs in for the hub's API, resource `<hub>/api`, not for its MCP server, `<hub>/mcp`. `login`, `setup` and the token refresh ask for the API, and no longer read the hub's protected resource metadata. This needs a hub that serves the API resource. `auth-header` prints the new token.
- A sign-in from an earlier version is never sent to the hub: commands say to run `line-lx login`, `setup` signs in again, and `doctor` reports it.
- `login` revokes the refresh token of the sign-in it replaces, as `logout` does.
- Claude Code 2.1.319 or later is needed: an older one doesn't install a plugin published after setup by itself. `setup`, `sync` and `doctor` warn when `claude --version` is older, and say to update it.
- line-lx sends `User-Agent: line-lx/<version>` with every request to the hub, so the hub can tell line-lx's own reads of the marketplace from Claude Code's.

### Security

- Built with Go 1.27.4, which fixes GO-2026-6859 in net/http.

### Upgrading

- Run `line-lx login` once after upgrading.

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

[Unreleased]: https://github.com/moqa-studio/line-lx-cli/compare/v0.4.1...HEAD
[0.4.1]: https://github.com/moqa-studio/line-lx-cli/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/moqa-studio/line-lx-cli/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/moqa-studio/line-lx-cli/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/moqa-studio/line-lx-cli/compare/v0.1.2...v0.2.0
[0.1.2]: https://github.com/moqa-studio/line-lx-cli/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/moqa-studio/line-lx-cli/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/moqa-studio/line-lx-cli/releases/tag/v0.1.0
