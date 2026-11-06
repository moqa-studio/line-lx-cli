# line-lx

The command-line client for [Metro-LX](https://metro-lx.moqa.studio/), the agent plugin registry for companies. It asks your company's Metro-LX hub which plugins you should have, fetches them and writes them for each agent on your machine: Claude Code, Codex and pi. It moves files; the hub makes every decision.

Releases are signed and versioned, for macOS and Linux, on amd64 and arm64.

## Install

With Homebrew:

```sh
brew install moqa-studio/tap/line-lx
line-lx setup
```

`line-lx setup` signs you in through your browser, then writes your organization's plugins to every agent it finds. You can run it again at any time. For a self-hosted hub, run `line-lx setup --hub https://hub.example.com`.

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
| `line-lx sync` | Writes the newest version of each plugin to every agent found here, removes the ones gone from the hub, and reports each delivery to the hub. Exit 1 if a target failed or no agent is found. |
| `line-lx list` | Shows the plugins installed here per agent, and whether they're up to date. |
| `line-lx doctor` | Checks the hub, your sign-in and each agent. Exit 1 when something is wrong. |
| `line-lx --version` | Prints the version. |

Agents are used only when found on the machine: Claude Code (`claude` on the PATH), Codex (`~/.codex` exists) and pi (`~/.pi/agent` exists). line-lx only changes folders carrying its stamp, and only for the hub that wrote them.

Two environment variables, for CI and managed machines:

- `LINE_LX_HUB`: the hub's URL, `https://host[/path]` (`http` only for localhost). Without it, line-lx uses the hub saved by `--hub`, else the Metro-LX cloud hub.
- `LINE_LX_TOKEN`: a token every command uses instead of the keychain. It needs `LINE_LX_HUB` and goes only to that hub.

## Where it stores things

- **Sign-in**: the OS keychain, service `line-lx`, one entry per hub URL. On macOS, any process running as you can read this item. On Linux without a Secret Service on D-Bus (SSH boxes, containers), the sign-in goes to `<config dir>/line-lx/credentials/<hub id>.json`, mode 0600 in a 0700 folder.
- **Config**: `<config dir>/line-lx/`, with the current hub in `config.json`, the sync lock in `sync.lock`, and the Claude Code marketplace in `claude-code/`. The config dir is `~/Library/Application Support` on macOS and `~/.config` (or `$XDG_CONFIG_HOME`) on Linux.
- **Plugins**: Codex in `~/.codex/skills/<plugin>--<skill>/`, with records in `~/.codex/skills/.line-lx/`. pi in `~/.pi/agent/skills/<plugin>/<skill>/`. Claude Code through `claude plugin`, from a local marketplace that the hub names `metro-<workspace>`.

## Build from source

Needs the Go version in `go.mod`.

```sh
go build -o line-lx .
```

See [CONTRIBUTING.md](CONTRIBUTING.md) to work on it, and [SECURITY.md](SECURITY.md) to report a vulnerability.

## License

[Apache-2.0](LICENSE)
