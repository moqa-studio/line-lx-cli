# line-lx

The command-line client for [Metro-LX](https://metro-lx.moqa.studio/), the agent plugin registry for companies. It asks your company's Metro-LX hub which plugins you should have, fetches them and writes them for Codex and pi, and connects Claude Code to the hub, which then gets them by itself. It moves files; the hub makes every decision.

Releases are signed and versioned, for macOS and Linux, on amd64 and arm64.

## Install

With Homebrew:

```sh
brew install moqa-studio/tap/line-lx
line-lx setup
```

`line-lx setup` signs you in through your browser, then gives your organization's plugins to every agent it finds. You can run it again at any time. For a self-hosted hub, run `line-lx setup --hub https://hub.example.com`.

### Manual install

For IT teams that deploy with their own tooling. Each [release](https://github.com/moqa-studio/line-lx-cli/releases) has one archive per platform, `line-lx_<version>_<os>_<arch>.tar.gz`, where `<os>` is `darwin` or `linux` and `<arch>` is `amd64` or `arm64`.

```sh
version=0.1.0 os=darwin arch=arm64
gh release download "v$version" --repo moqa-studio/line-lx-cli \
  --pattern "line-lx_${version}_${os}_${arch}.tar.gz" --pattern 'checksums.txt*'
```

[Verify the download](#verify-a-release), then put the binary on the PATH:

```sh
tar -xzf "line-lx_${version}_${os}_${arch}.tar.gz" line-lx
sudo install -m 0755 line-lx /usr/local/bin/line-lx
```

The macOS binaries aren't notarized, so macOS blocks one that comes from an archive downloaded with a browser. Download the archive with `gh` or `curl`.

## Verify a release

Run these in the folder that holds the download. Check the signature of `checksums.txt` with [cosign](https://docs.sigstore.dev/cosign/system_config/installation/), then the archive against `checksums.txt`:

```sh
cosign verify-blob checksums.txt \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity "https://github.com/moqa-studio/line-lx-cli/.github/workflows/release.yml@refs/tags/v$version" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

sha256sum --ignore-missing -c checksums.txt    # on macOS: shasum -a 256 --ignore-missing -c checksums.txt
```

Check the archive's build provenance with the GitHub CLI:

```sh
gh attestation verify "line-lx_${version}_${os}_${arch}.tar.gz" --repo moqa-studio/line-lx-cli
```

Each archive also has an SBOM in the release, `<archive>.sbom.json`.

## Commands

| Command | Does |
|---|---|
| `line-lx setup [--hub URL]` | Signs in through the browser if needed, syncs, then says what each agent got and the next step. Never reads stdin. Exit codes as `sync`, except that no agent on the machine isn't a failure. |
| `line-lx login [--hub URL]` | Signs in through the browser. `--hub` is remembered for the next commands. |
| `line-lx logout` | Revokes the refresh token at the hub (best effort) and forgets this machine's sign-in. |
| `line-lx whoami` | Shows you, your workspace, your role and the hub. |
| `line-lx sync` | Writes the newest version of each plugin to Codex and pi, removes the ones gone from the hub, and reports each delivery to the hub. Connects Claude Code to the hub. Exit 1 if a target failed or no agent is found. |
| `line-lx list` | Shows the plugins installed here per agent, and whether they're up to date. |
| `line-lx status [--json]` | Shows, for each agent, whether it's connected to the hub, and each plugin it has from the hub: its version, whether it's behind, when it was last updated, its skills with their security review, and its hooks. Changes nothing. See [Status](#status). |
| `line-lx usage [--month] [--json]` | Shows your own runs of your workspace's plugins over the last 7 days (`--month`: 30), per plugin and per agent. Needs a sign-in. Changes nothing. See [Your usage](#your-usage). |
| `line-lx doctor` | Checks the hub, your sign-in and each agent, with the fix under each problem: Claude Code's connection and version, the plugins it lacks a day after they came out, and Codex and pi against the hub. Exit 1 when something is wrong. |
| `line-lx auth-header` | Prints your sign-in as one line of JSON, `{"Authorization":"Bearer <token>"}`. Only for Claude Code, which runs it to sign in to the hub. |
| `line-lx report-usage --agent claude-code --workspace <slug>` | Reports one use of your workspace's plugins to the hub. Only for the hub's Claude Code hook, which gives it the use on stdin. Always exits 0. See [Usage reports](#usage-reports). |
| `line-lx notice --agent claude-code --workspace <slug>` | Says in Claude Code, when a session starts, what's wrong with your plugins and how to fix it, and which were updated. Only for the hub's Claude Code hook. Prints one JSON object or nothing, and always exits 0. See [Session notices](#session-notices). |
| `line-lx --version` | Prints the version. |

Agents are used only when found on the machine: Claude Code (`~/.claude` exists, or `claude` is on the PATH), Codex (`~/.codex` exists) and pi (`~/.pi/agent` exists). line-lx only changes folders carrying its stamp, and only for the hub that wrote them.

Every command but `auth-header`, `report-usage`, `notice` and `completion` prints for people, with one look: the metro-lx mark, then rows of checks (✓ ok, ✗ failed, ! a warning, · nothing to check) with the fix under each problem, or cards. It fits in 80 columns with normal data: names and versions are never cut, so a long one widens its column; other text is cut with "…" or wrapped. It has color only on a terminal, when `NO_COLOR` isn't set and `TERM` isn't `dumb`: 24-bit when `COLORTERM` is `truecolor` or `24bit`, else the closest of xterm's 256 colors. Green is kept for what's connected, set up, up to date or ok, and red for what failed. Without color, the shapes and words say the same, and a plugin's route shows in brackets, `(1)`. `sync` shows a spinner on the agent it's working on only on a terminal with color.

## Status

`line-lx status` shows what's connected on this machine: a card for each agent found here, and in it, each plugin it has from the hub, with the badge of the plugin's line, its version, its state and its last update. Under each plugin, its skills (●), each with its security review, and its hooks (▲). Skills come in the hub's order, the web app's, for every agent, then the ones the hub doesn't list, by name.

```

  ▌▌▌ metro-lx  Acme                                          signed in as Ada
      hub  hub.example.com

  ━━━━━━━━━━────────────────  2 of 5 up to date · 2 behind · 1 not installed

  ╭─ Claude Code ─────────────────────────────── connected · fetched 2h ago ─╮
  │                                                                          │
  │  (1)  moqa-kit         v1.0.0 → v1.1.0   behind                  9h ago  │
  │        ●  review                         ✓ No findings                   │
  │        ●  website-creation               · No static findings            │
  │                                                                          │
  │  (4)  posthog-dev-kit  v1.3.0            up to date              8h ago  │
  │        ●  board-apple-ads                ! To review · 1 medium          │
  │        ●  capture                        ✓ No findings                   │
  │                                                                          │
  │   ◎   metro-lx         13cd9935af1d      up to date           yesterday  │
  │        ▲  SessionStart                   runs line-lx notice             │
  │        ▲  PostToolUse                    runs line-lx report-usage       │
  │                                                                          │
  │  Claude Code catches up with the hub by itself, in its next session.     │
  │                                                                          │
  ╰──────────────────────────────────────────────────────────────────────────╯

  ╭─ Codex ───────────────────────────────────────── set up · synced 2d ago ─╮
  │                                                                          │
  │  (1)  moqa-kit         —                 not installed                —  │
  │                                                                          │
  │  (4)  posthog-dev-kit  v1.2.0 → v1.3.0   behind                  2d ago  │
  │        ●  capture                        ✓ No findings                   │
  │                                                                          │
  │  Run `line-lx sync` to catch up with the hub.                            │
  │                                                                          │
  ╰──────────────────────────────────────────────────────────────────────────╯

  ○  pi  not found
```

- **This machine.** For Claude Code, line-lx reads how its settings connect it to the hub (as `doctor` does), then Claude Code's own records: `plugins/known_marketplaces.json` (when it last fetched the marketplace), `plugins/installed_plugins.json` (each plugin's version and scope, when it was installed and last updated) and each plugin's folder in `plugins/cache/<marketplace>/<plugin>/` (its skills, and the hooks in `hooks/hooks.json`). It reads plugin files only inside that cache. A marketplace declared outside the settings files line-lx reads (an MDM profile) counts when Claude Code's records have it from the hub's URL. For Codex and pi, line-lx reads its own stamps and the skill folders it wrote. A stamp has the time of the last write, not of the first.
- **The hub.** With your sign-in, line-lx asks the hub what it delivered to you (`GET /api/me/installed`): each plugin's line and route, its latest version, the version it delivered and when, each skill's security review, and when Claude Code last fetched the marketplace. It also asks who you are (`GET /api/me`), for the header. The hub's times are yours, on any of your machines. A plugin's last update shows this machine's time, else the hub's.
- **The line at the top** counts each plugin once per agent: up to date (━, green), behind, not installed and no longer in the hub. Another hub's plugins aren't counted.
- **Lines.** A plugin's badge has its route number on the color the web app gives its line: 1 engineering blue, 2 product purple, 3 marketing orange, 4 sales yellow, 5 support green, 6 everyone ink. `metro-lx`, Claude Code's bundle, isn't a team's line: it has the interchange ring ◎. A plugin with no line from the hub (another hub's, or one the hub no longer has) has no badge.
- **States.** `up to date` (green). `behind` (bold): the version here isn't the hub's latest, shown after the arrow. For `metro-lx`, whose versions are hashes, only the one here shows; the hub's is in `--json`. `not installed`: the hub has the plugin for that agent, this machine doesn't. `not in the hub`: it's here from this hub, which no longer has it. `other hub`: a stamp says it came from another hub, and line-lx leaves it alone. These three are grey.
- **Security reviews.** Each skill shows its review in the web app's words, as the hub gives them ("No findings", "No static findings", "Low notes", "To review", "Flagged"…), after a mark for its tone: ✓ ok, · neutral, ! needs attention, ✗ danger. When the review found something at medium or above, the worst severity and its count follow: `· 1 medium`. A review is never cut: one too long for the card goes on at its `·`, on the next line, under the label. A skill name too long for its column moves the reviews of every card to the right. A skill the hub has no review for shows none. A hook shows what it runs.
- **Without the hub.** Not signed in, a sign-in the hub refuses, a hub that doesn't answer, or one too old to have `GET /api/me/installed` (404): line-lx shows what this machine has, without badges, and says why.
- Descriptions and hook commands are in `--json`, not in the cards. It writes nothing, installs nothing and runs no agent. Like every command, it refreshes an expired sign-in. It never prints the token. It exits 0, unless the configured hub URL isn't valid.

### `status --json`

`line-lx status --json` prints the same report as one JSON object, for scripts and IT tooling. `schema` is 1: a change that can break a reader raises it, a new field doesn't. Times are ISO 8601, in UTC. A field line-lx can't tell is `null`, never missing, except the ones marked optional.

| Field | Is |
|---|---|
| `schema` | `1` |
| `hub` | The hub's URL |
| `workspace` | `{ "slug", "name" }` from the hub, or `null` without its answer |
| `hubStatus` | `ok`, `not-signed-in`, `sign-in-refused`, `unreachable`, `unsupported` (the hub has no `GET /api/me/installed`) or `error` |
| `hubMessage` | Optional: why, when `hubStatus` isn't `ok` |
| `agents[]` | One per agent line-lx knows, always in this order: `claude-code`, `codex`, `pi` |
| `agents[].target` | `claude-code`, `codex` or `pi` |
| `agents[].found` | Whether the agent is on this machine. When it isn't, `plugins` is empty. |
| `agents[].connected` | Claude Code: its settings or its records have the hub's marketplace. Codex and pi: line-lx wrote at least one plugin from this hub. |
| `agents[].location` | Optional. Claude Code: the settings file that connects it. Codex and pi: the skills folder. |
| `agents[].problem` | Optional: what's wrong with the connection |
| `agents[].marketplace` | Optional, Claude Code only: `{ "name", "fetched", "fetchedAt", "hubFetchedAt" }`. `fetched`: Claude Code has the marketplace in its records. `fetchedAt`: when it last fetched it, from its records. `hubFetchedAt`: when the hub last saw Claude Code fetch it, from any of your machines. |
| `agents[].plugins[]` | The hub's plugins in the hub's order, the web app's (so by route), then the ones the hub doesn't have (`not-in-hub`, `other-hub`) by name, then the bundle. Without the hub's answer, by name, then the bundle. |
| `.name` | The plugin's name |
| `.bundle` | `true` for Claude Code's `metro-lx`, which brings the workspace's other plugins |
| `.route` | From the hub: the route number of the plugin's line, 1 to 6, as the web app shows it. `null` for the bundle, a plugin from another hub or no longer in the hub, and with a hub that doesn't give lines. |
| `.line` | From the hub: the plugin's line, `engineering`, `product`, `marketing`, `sales`, `support` or `everyone`. `null` when `route` is. |
| `.state` | `up-to-date`, `behind`, `installed` (the hub's latest is unknown), `missing`, `not-in-hub` or `other-hub` |
| `.version` | The version installed here, `null` when missing |
| `.latestVersion` | The hub's latest version |
| `.behind` | `true` when `state` is `behind` |
| `.from` | Optional: the hub it came from, when it's `other-hub` |
| `.scope` | Optional, Claude Code only: `user`, `project` or `local`. With several, the `user` one. |
| `.installedAt`, `.updatedAt` | From this machine's records |
| `.deliveredVersion`, `.firstDeliveredAt`, `.lastDeliveredAt` | From the hub: what it delivered to you for that agent, and when |
| `.skills[]` | `{ "name", "description", "review" }`: the skills here, else the hub's |
| `.skills[].review` | From the hub: the security review of the skill in the version it delivered, `{ "status", "label", "tone", "counts" }`, or `null` without one (a hub that doesn't give reviews, or a skill it doesn't have). `status`: `pending`, `done` or `failed`. `label`: the web app's words, such as `No findings` or `To review`. `tone`: `ok`, `neutral`, `attention` or `danger`; a review with another tone is `null`. `counts`: `{ "critical", "high", "medium", "low" }`, the findings per severity. |
| `.hooks[]` | `{ "event", "command" }`: Claude Code's command hooks here, else the hub's |

## Your usage

`line-lx usage` shows your own runs of your workspace's plugins: the skills an agent reported it ran, as the web app's Usage page counts them. One bar per plugin, in its line's color and in the web app's order, then one per agent. `--month` shows the last 30 days instead of 7.

```

  ▌▌▌ metro-lx  Your runs · last 7 days                                53 runs

  (1)  moqa-kit          ████████████████████████████████████████   35

  (4)  posthog-dev-kit   ████████████████████▋                      18

  BY AGENT

       Claude Code       ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━   49
       Codex             ━━━                                         4

  Only your own runs. Admins see the workspace's totals in the web app.
  Run `line-lx usage --month` for the last 30 days.
```

- It reads `GET /api/usage/mine`, and counts the rows an agent reported (`source` `agent`) of a skill that ran (`kind` `activated`). Deliveries, fetches, MCP calls and failures aren't runs. The lines and the order come from `GET /api/me/installed`, and whether agent reporting is on from `GET /api/usage/settings`.
- Runs are only counted while an admin turns on agent reporting (see [Usage reports](#usage-reports)). With no runs, it says whether reporting is off.
- It needs a sign-in: without one, it says to run `line-lx login` and exits 1. It writes nothing.

`line-lx usage --json` prints the same as one JSON object. `schema` is 1, as for `status --json`.

| Field | Is |
|---|---|
| `schema` | `1` |
| `hub` | The hub's URL |
| `period` | `week` (7 days, today included) or `month` (30) |
| `since` | The first day counted, `YYYY-MM-DD`, from the hub |
| `agentReporting` | Whether the workspace's agent reporting is on, `null` when the hub didn't say |
| `runs` | Your runs in the period |
| `plugins[]` | `{ "name", "route", "line", "runs" }`, in the web app's order, then by name. `route` and `line` as in `status --json`, `null` for a plugin the hub gives no line. |
| `agents[]` | `{ "agent", "name", "runs" }`, most runs first. `agent` is the hub's id (`claude-code`, `codex`, `pi`…), `name` its name for people. |

## Claude Code

Claude Code gets the plugins from the hub's marketplace by itself. `line-lx setup` and `line-lx sync` connect it once, in its user settings (`$CLAUDE_CONFIG_DIR/settings.json`, else `~/.claude/settings.json`):

- the hub's marketplace, `metro-<workspace>`, fetched from `<hub>/api/connectors/claude-code/marketplace.json` with auto-update on. Its `headersHelper` is `<path to line-lx> auth-header`, so Claude Code sends your sign-in to the hub.
- each plugin of the marketplace, `<plugin>@metro-<workspace>`, enabled, so the first Claude Code session brings them all. A plugin you turned off stays off, and line-lx removes the ones gone from the hub.
- the plugin `metro-lx@metro-<workspace>`, enabled. It depends on every plugin of your workspace, so a plugin published later comes when Claude Code auto-updates. `line-lx sync` installs anything still missing at once.

New versions of the installed plugins come from Claude Code's auto-update: `line-lx sync` installs what's missing, it doesn't update.

Claude Code fetches the marketplace when it starts, and needs version 2.1.319 or later: an older one doesn't install a plugin published after setup by itself. `setup`, `sync` and `doctor` tell you when yours is older. The first time, the plugins come in a new Claude Code session: `claude plugin marketplace add` refuses a marketplace whose settings entry has a `headersHelper`, so line-lx can't add it to the running Claude Code. line-lx keeps the other settings, but rewrites `settings.json` formatted, with its keys sorted. It leaves Claude Code alone when it already has another marketplace under the hub's name.

`line-lx auth-header` answers only Claude Code, which names the marketplace or archive it asks headers for (`CLAUDE_CODE_MARKETPLACE_URL`, `CLAUDE_CODE_PLUGIN_ARCHIVE_URL`), and gives your sign-in only to your current hub: every URL named must be under the hub's URL. It refreshes the sign-in when needed, and stops after 9 seconds.

Admins can connect Claude Code on every machine with [managed settings](https://code.claude.com/docs/en/managed-settings) instead. Each person still needs line-lx on the PATH and signed in (`line-lx login`):

```json
{
  "extraKnownMarketplaces": {
    "metro-<slug>": {
      "source": { "source": "url", "url": "<hub>/api/connectors/claude-code/marketplace.json", "headersHelper": "/opt/homebrew/bin/line-lx auth-header" },
      "autoUpdate": true
    }
  },
  "enabledPlugins": { "metro-lx@metro-<slug>": true }
}
```

`<slug>` is your workspace's slug (`line-lx whoami`), and `<hub>` your hub's URL. Use the path where your tooling installs line-lx. Claude Code doesn't install the dependencies of `metro-lx` when it starts, so list each plugin in `enabledPlugins` too (`"<plugin>@metro-<slug>": true`), or have people run `line-lx sync`. `line-lx doctor` and `line-lx list` see a marketplace declared in Claude Code's managed settings files (`/Library/Application Support/ClaudeCode/` on macOS, `/etc/claude-code/` on Linux), not one from an MDM profile or server-managed settings.

### Session notices

The hub's `metro-lx` plugin carries a Claude Code hook that runs `line-lx notice --agent claude-code --workspace <slug>` when a session starts or resumes. When something is wrong, Claude Code shows you one line, with the fix. When all is well, it shows nothing.

| What's wrong | The fix it gives |
|---|---|
| Your line-lx sign-in is missing, expired, from an earlier line-lx, or refused by the hub. Claude Code then gets no updates. | `line-lx login` |
| Claude Code takes the workspace's plugins from another hub than the one line-lx is signed in to, or its connection to the hub is broken. | `line-lx doctor` |
| Your sign-in now acts in another workspace than the plugins Claude Code has. | `line-lx sync`, or switch back in the web app |
| Claude Code is older than 2.1.319, so new plugins don't arrive. | `claude update` |
| A plugin out for a day or more isn't installed. | `line-lx sync` |
| A plugin out for a day or more is behind. | Send a message, then `/reload-plugins` |

With several problems, it shows the first in this order and says how many more: `line-lx doctor` lists them all. A plugin you turned off in Claude Code isn't counted.

When a plugin was updated since your last session, it also shows which one and its version, once, and quotes the first line of its changelog as its publisher wrote it:

```text
Metro-LX · moqa-kit updated to v1.1.0 · its changelog: "Adds the website-creation skill."
```

- **What it reads.** Your sign-in, Claude Code's records (`plugins/known_marketplaces.json`, `plugins/installed_plugins.json`, each plugin's `.claude-plugin/plugin.json` in its cache) and your user settings, and `claude --version`, run again only when Claude Code's binary changes. From the hub, what `line-lx status` asks: `GET /api/me/installed`, with your sign-in. Nothing else is sent, and the hub records nothing of it.
- **It never refreshes your sign-in itself.** The hub replaces the refresh token as it answers a refresh, so a refresh stopped half-way would leave this computer with a revoked one, and Claude Code may stop a hook at any time. With a valid access token, it asks the hub at once. With an expired one, it starts `line-lx notice --refresh` as a process of its own, in its own session: it refreshes the sign-in as every command does, however long the hub takes, asks the hub, and records the answer for the next session. This session uses the last answer recorded (at most 3 days old), and still tells what it checks on this computer. When the hub refuses that refresh, the next session says your sign-in expired, until you sign in again.
- **What it remembers.** `notice.json` in line-lx's config folder, per hub and workspace (16 at most, the least recently used dropped first): each plugin's version it told (200 at most), and the hub's last answer (each plugin's latest version, when it came out, its changelog's first line). Per hub: the workspace your sign-in acts in, and whether the hub refused the last refresh. And Claude Code's version with its binary's path, size and time. No token. It writes it whole (a temporary file, then a rename) under `notice.lock`, so two sessions starting at once tell an update once. The first session for a workspace only remembers. An update is told once the hub's answer knows its version, with its changelog; a version older than the one told is remembered quietly.
- **Where it shows.** Claude Code shows the line to you, never to Claude: line-lx prints only `{"systemMessage": "…"}`, or nothing, and the hook passes nothing else on. Changelogs and names come from people in your workspace, so they never reach the model, and a changelog is always quoted as its publisher's. Text from the hub loses its escape sequences and control characters, and fits on one line: a changelog line is cut at 120 characters.
- **Never slower.** It answers within 1.5 seconds with what it knows by then. The hook passes on line-lx's output only when it is one line that starts with `{"systemMessage":` and ends with `}`, and always exits 0. So without line-lx, with a line-lx older than 0.4.3, or when it fails, the session shows nothing at all.
- **Known limit: sessions without a screen.** Claude Code doesn't tell a hook whether a person sees the session (`claude -p` and the Agent SDK run it too), so an update told in such a session counts as told.
- **Codex and pi** have no session hook here: `line-lx doctor` says what's behind. `line-lx sync` doesn't print changelogs: agents run it in their shell, and a changelog in its output would reach their model.
- **Windows.** line-lx is released for macOS and Linux. Claude Code runs hooks with Git Bash on Windows, or PowerShell without it, where the hook's shell form fails.

### Usage reports

When an admin turns on agent reporting for the workspace, the hub's `metro-lx` plugin also carries a reporting hook. It runs `line-lx report-usage --agent claude-code --workspace <slug>` in the background each time Claude runs a skill or an MCP tool of a plugin, and each time you type a skill. While reporting is off, the plugin has no reporting hook and nothing is reported.

A report says only which plugin and version, which skill or MCP tool, the kind of use (`activated`, `mcp_call` or `failed`), who started it (`person` or `agent`) and whether it succeeded:

```json
{"workspace":"<slug>","agent":"claude-code","kind":"activated","plugin":"<plugin>","version":"<version>","name":"<skill>","trigger":"agent","success":true}
```

- line-lx sends a name only when it belongs to a plugin Claude Code installed from the workspace's marketplace, `metro-<slug>`, and only when that marketplace comes from the current hub. It reads this from Claude Code's own records, `plugins/known_marketplaces.json` and `plugins/installed_plugins.json` in Claude Code's folder, and the plugin's folder in `plugins/cache/metro-<slug>/<plugin>/`. The version is the one in that folder's `.claude-plugin/plugin.json`, else the folder's name when it looks like a version (Claude Code names some folders with a hash instead). A skill of your own, a project's skill or a plugin from another marketplace is never sent.
- A skill named with its plugin, `<plugin>:<skill>`, always counts. Claude Code runs a plugin's skill under its bare name, `<skill>`, only when no other skill or command has that name, so line-lx sends a bare name only when the workspace's plugin is the one place that has it. It looks in the managed policy folder, Claude Code's folder (your skills, commands and skills-directory plugins), the `.claude` folder of the current folder, of the session's project and of every folder above them, every `.claude` folder inside the project, the folders in `permissions.additionalDirectories` of the settings files, and every installed plugin. The current folder is only read on this machine, never sent. When line-lx can't read one of these places, when the session runs at `/` or in your home folder, or when the project is too large to look through in time (over 20,000 folders, or over a second), it sends nothing for a bare name. Names with their plugin still count there.
- A folder added for one session with `/add-dir` or `--add-dir` isn't recorded anywhere, so a skill there with the same name can make line-lx count the workspace's skill by mistake; the name sent is still the workspace skill's own, which the hub already knows.
- Nothing is sent when line-lx can't tell which copy ran: the plugin is also installed from another marketplace, or Claude Code holds two versions of it.
- Commands, arguments, prompts, folders, paths and session identifiers are never sent. The hub takes who you are from your sign-in, and counts a use only under a version it delivered to you.
- The report goes to `<hub>/api/usage/reports` with your sign-in, once. line-lx ends within 3 seconds, reading its input included, never retries, prints nothing on stdout and always exits 0, so a report never slows Claude Code or fails a tool.
- A report the hub didn't take is dropped and counted. `line-lx doctor` shows how many since when, until a report reaches the hub again.

Two environment variables, for CI and managed machines:

- `LINE_LX_HUB`: the hub's URL, `https://host[/path]` (`http` only for localhost). Without it, line-lx uses the hub saved by `--hub`, else the Metro-LX cloud hub.
- `LINE_LX_TOKEN`: a token for the hub's API (resource `<hub>/api`) that every command uses instead of the keychain. It needs `LINE_LX_HUB` and goes only to that hub.

## Where it stores things

- **Sign-in**: tokens for the hub's API (resource `<hub>/api`), in the OS keychain, service `line-lx`, one entry per hub URL. On macOS, any process running as you can read this item. On Linux without a Secret Service on D-Bus (SSH boxes, containers), the sign-in goes to `<config dir>/line-lx/credentials/<hub id>.json`, mode 0600 in a 0700 folder.
- **Config**: `<config dir>/line-lx/`, with the current hub in `config.json`, the sync lock in `sync.lock`, the session notice's `notice.json`, `notice.lock` and `refresh.lock` (see [Session notices](#session-notices)), and, after a usage report was dropped, `usage-dropped.json`: a count and the time of the first drop, nothing else. The config dir is `~/Library/Application Support` on macOS and `~/.config` (or `$XDG_CONFIG_HOME`) on Linux.
- **Plugins**: Codex in `~/.codex/skills/<plugin>--<skill>/`, with records in `~/.codex/skills/.line-lx/`. pi in `~/.pi/agent/skills/<plugin>/<skill>/`. Claude Code keeps its own, installed from the hub's marketplace (see [Claude Code](#claude-code)).

## Build from source

Needs the Go version in `go.mod`.

```sh
go build -o line-lx .
```

See [CONTRIBUTING.md](CONTRIBUTING.md) to work on it, and [SECURITY.md](SECURITY.md) to report a vulnerability.

## License

[Apache-2.0](LICENSE)
