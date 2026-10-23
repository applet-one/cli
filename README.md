# Applet CLI

Create, deploy, and manage Cloudflare Worker apps with [Applet](https://applet.one).

`applet` is a single static binary for macOS, Linux and Windows (amd64 and arm64). It needs no Node.js or npm: Workers are bundled with esbuild's Go API, and `applet dev` runs your Worker locally on [workerd](https://github.com/cloudflare/workerd), the open-source Workers runtime.

## Install

Download the archive for your platform from the [latest release](https://github.com/applet-one/cli/releases/latest), extract it, and put `applet` on your `PATH`:

```sh
# macOS (Apple silicon) example
curl -fsSLO https://github.com/applet-one/cli/releases/latest/download/applet_<version>_darwin_arm64.tar.gz
tar -xzf applet_<version>_darwin_arm64.tar.gz applet
sudo mv applet /usr/local/bin/
```

Each release includes `checksums.txt` for verifying downloads. To build from source, use `go install github.com/applet-one/cli/cmd/applet@latest`.

## Quick start

```sh
applet init my-app
cd my-app
applet dev        # http://127.0.0.1:8787
applet login
applet deploy
```

The first `applet dev` downloads workerd into your user cache directory, verifying the release checksum pinned in the CLI. To use your own binary, set `APPLET_WORKERD=/path/to/workerd`.

## Commands

| Command | Purpose |
| --- | --- |
| `applet init <project-name\|.>` | Create a starter project; `.` scaffolds the current directory. |
| `applet migrate-config` | Split a legacy combined `applet.jsonc` into `wrangler.jsonc` and hosting settings. |
| `applet login` / `logout` | Sign in with browser device authorization / remove the saved login. |
| `applet status [--json]` | Show sign-in status and deployed apps. |
| `applet dev [--host HOST] [--port PORT]` | Run the Worker locally. Durable Object state persists under `.applet/dev`; `.dev.vars` supplies local secrets. |
| `applet build --dry-run` | Validate the project and summarize its artifact without uploading. |
| `applet deploy [--secrets-file PATH]` | Build and deploy, optionally publishing secrets with the code. |
| `applet access [private\|public --yes]` | Show or set public/private access in config and on the live app. |
| `applet url [check NAME \| set NAME [--yes]]` | Show or change the hosted app's public URL (Pro). |
| `applet logs [--limit N]` | Show the latest 1–100 log entries from the last 24 hours. |
| `applet secrets set\|list\|delete` | Manage write-only deployed app secrets. Values come from a hidden prompt or `--stdin`, never from arguments. |
| `applet backup` | Download the app's configured state backup. |
| `applet delete [app-name] [--yes] [--no-input]` | Permanently delete a hosted app. |
| `applet version` | Print the CLI version. |

Run `applet <command> --help` for details. Project configuration is described in the platform docs (`docs/applet-project-config-v1.md`).

### Environment

| Variable | Purpose |
| --- | --- |
| `APPLET_API_URL` | Use a non-default Applet API endpoint. |
| `APPLET_WORKERD` | Path to a workerd binary for `applet dev`. |
| `APPLET_DEVICE_NAME` | Device name shown when approving `applet login`. |
| `APPLET_NO_UPDATE_CHECK` | Disable the daily, best-effort check for a newer release. |
| `XDG_CONFIG_HOME` | Location of the saved login (`applet-one/config.json`, default `~/.config`). |

The saved login is shared with the earlier npm CLI (`@applet-one/cli`), so existing sign-ins keep working.

## Development

Requires Go (see `go.mod`), [Task](https://taskfile.dev) and [golangci-lint](https://golangci-lint.run) v2.

```sh
task build              # bin/applet for this platform
task run -- status      # run from source
task test               # unit tests
task test:integration   # downloads workerd and runs applet dev end to end
task check              # lint + race tests (what CI runs)
task release:snapshot   # build every release archive into dist/
```

Layout:

```text
cmd/applet          entry point
internal/cli        cobra commands
internal/api        Applet API client
internal/auth       saved login
internal/project    wrangler.jsonc / applet.jsonc loading, access edits, migration
internal/jsonc      comment-preserving JSONC parsing and editing
internal/build      validation, esbuild bundling, .applet.tgz artifacts
internal/dev        workerd download and local runtime
internal/secrets    secret validation, stdin/prompt input, secrets files
internal/dotenv     Node.js-compatible dotenv parsing (no interpolation)
internal/update     release update check
```

To update the local runtime, change `WorkerdVersion` and the per-platform SHA-256 digests in `internal/dev/workerd.go` (the digests are listed on each [workerd release](https://github.com/cloudflare/workerd/releases)).

## Releasing

CI (`.github/workflows/ci.yml`) lints, tests on Linux (amd64 and arm64), macOS and Windows, and cross-compiles every target. To publish a release, push a semver tag:

```sh
git tag v0.3.0
git push origin v0.3.0
```

`.github/workflows/release.yml` runs the tests and GoReleaser, which attaches `tar.gz` archives (`zip` on Windows) for linux, darwin and windows on amd64 and arm64, plus `checksums.txt`, to the GitHub release. The CLI's update notice points at the latest release of this repository.
