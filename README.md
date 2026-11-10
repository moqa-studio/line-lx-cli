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
| `line-lx doctor` | Checks the hub, your sign-in and each agent. Exit 1 when something is wrong. |
| `line-lx auth-header` | Prints your sign-in as one line of JSON, `{"Authorization":"Bearer <token>"}`. Only for Claude Code, which runs it to sign in to the hub. |
| `line-lx --version` | Prints the version. |

Agents are used only when found on the machine: Claude Code (`~/.claude` exists, or `claude` is on the PATH), Codex (`~/.codex` exists) and pi (`~/.pi/agent` exists). line-lx only changes folders carrying its stamp, and only for the hub that wrote them.

## Claude Code

Claude Code gets the plugins from the hub's marketplace by itself. `line-lx setup` and `line-lx sync` connect it once, in its user settings (`$CLAUDE_CONFIG_DIR/settings.json`, else `~/.claude/settings.json`):

- the hub's marketplace, `metro-<workspace>`, fetched from `<hub>/api/connectors/claude-code/marketplace.json` with auto-update on. Its `headersHelper` is `<path to line-lx> auth-header`, so Claude Code sends your sign-in to the hub.
- the plugin `metro-lx@metro-<workspace>`, enabled. It depends on every plugin of your workspace, so Claude Code installs the new ones, updates them and removes the ones gone from the hub.

Claude Code fetches the marketplace when it starts, and needs version 2.1.238 or later. The first time, the plugins come in a new Claude Code session: `claude plugin marketplace add` refuses a marketplace whose settings entry has a `headersHelper`, so line-lx can't add it to the running Claude Code. line-lx keeps the other settings, but rewrites `settings.json` formatted, with its keys sorted. It leaves Claude Code alone when it already has another marketplace under the hub's name.

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

`<slug>` is your workspace's slug (`line-lx whoami`), and `<hub>` your hub's URL. Use the path where your tooling installs line-lx. `line-lx doctor` and `line-lx list` see a marketplace declared in Claude Code's managed settings files (`/Library/Application Support/ClaudeCode/` on macOS, `/etc/claude-code/` on Linux), not one from an MDM profile or server-managed settings.

Two environment variables, for CI and managed machines:

- `LINE_LX_HUB`: the hub's URL, `https://host[/path]` (`http` only for localhost). Without it, line-lx uses the hub saved by `--hub`, else the Metro-LX cloud hub.
- `LINE_LX_TOKEN`: a token every command uses instead of the keychain. It needs `LINE_LX_HUB` and goes only to that hub.

## Where it stores things

- **Sign-in**: the OS keychain, service `line-lx`, one entry per hub URL. On macOS, any process running as you can read this item. On Linux without a Secret Service on D-Bus (SSH boxes, containers), the sign-in goes to `<config dir>/line-lx/credentials/<hub id>.json`, mode 0600 in a 0700 folder.
- **Config**: `<config dir>/line-lx/`, with the current hub in `config.json` and the sync lock in `sync.lock`. The config dir is `~/Library/Application Support` on macOS and `~/.config` (or `$XDG_CONFIG_HOME`) on Linux.
- **Plugins**: Codex in `~/.codex/skills/<plugin>--<skill>/`, with records in `~/.codex/skills/.line-lx/`. pi in `~/.pi/agent/skills/<plugin>/<skill>/`. Claude Code keeps its own, installed from the hub's marketplace (see [Claude Code](#claude-code)).

## Build from source

Needs the Go version in `go.mod`.

```sh
go build -o line-lx .
```

See [CONTRIBUTING.md](CONTRIBUTING.md) to work on it, and [SECURITY.md](SECURITY.md) to report a vulnerability.

## License

[Apache-2.0](LICENSE)
