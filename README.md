# resticscope

A terminal dashboard for the [restic](https://restic.net/) repositories you
already back up to. It answers three questions quickly: **is every repository
current**, **what is inside a snapshot**, and **how do I get a file back**.

resticscope does not create backups. Your cron jobs, systemd timers, or NAS
tasks keep doing that. resticscope watches the result and gets data out again.

```text
resticscope · 5 repos · restic 0.18.1                                                     ? help

     Name                            Last  Snaps     Took  Labels
▎ •  homeserver-system             3h ago    214   12m41s  high · home
  △  laptop-restic                 1d ago     96    4m12s  medium · home
  •  nas-offsite                   1d ago     42    1h36m  high · home
  ×L vps-mail                      9h ago     61      51s  high · vps
  …  usb-archive               never refreshed

↑/↓ move • enter detail • / filter • o sort • g group • s shell • r refresh • q quit
```

## What it does

- **Overview:** one line per repository with status, age of the last snapshot,
  snapshot count, how long the last backup took, and your own labels. Filter,
  sort, and group the list.
- **Snapshot detail:** size, newly added data, host, tags, paths, and the
  backup window for every snapshot in a repository.
- **File browser:** walk a snapshot's tree with sizes, timestamps, permissions,
  and owners. Directory sizes are recursive.
- **Search:** find a filename anywhere in a snapshot, or track one file across
  every snapshot that contains it.
- **Diff:** compare two snapshots and step through the changed paths.
- **Extract:** restore a file, a directory, or a whole snapshot to a scratch
  directory with its original metadata. Optionally as root, so the snapshot's
  own owners and groups land on disk.
- **Repo shell:** drop into a shell with `RESTIC_*` and backend credentials
  already set, for anything the TUI does not cover.
- **Any restic backend:** S3-compatible, B2, Azure, GCS, Swift, SFTP, REST,
  rclone, or a local path.

## Read-only, and no stored secrets

- Every restic call resticscope makes against a repository is a read
  (`snapshots`, `cat config`, `ls`, `find`, `diff`, `restore`), each with
  `--no-lock`. It never runs `backup`, `forget`, `prune`, or `unlock`, and never
  takes a repository lock. The repository shell is the one place you can run
  anything you like.
- The config file holds no passwords. Backend credentials and repository
  passwords come from a command you choose (`pass`, `gpg`, `age`, SOPS,
  1Password, a file) and stay in memory for the session.
- Filenames from a snapshot are written only to an encrypted index whose key
  never leaves memory, and it is deleted when the app exits.

## Requirements

- [restic](https://restic.net/) 0.17.0 or newer on your `PATH`.
- Linux or macOS. Extracting files is refused on other platforms.
- Go 1.26.8 or newer, to build from source.

## Install

**Prebuilt binary.** Every release ships Linux and macOS archives for amd64 and
arm64, a `checksums.txt`, and signed build provenance. With the
[GitHub CLI](https://cli.github.com/):

```sh
tag=v0.1.0
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
asset="resticscope_${tag#v}_${os}_${arch}.tar.gz"

gh release download "$tag" --repo alexzeitgeist/resticscope \
  --pattern "$asset" --pattern checksums.txt

grep "$asset" checksums.txt | shasum -a 256 -c -   # GNU: sha256sum -c -
gh attestation verify "$asset" --repo alexzeitgeist/resticscope

tar -xzf "$asset" resticscope
install -m 0755 resticscope ~/.local/bin/
```

`gh attestation verify` checks GitHub's signed provenance: that this exact
archive was built by this repository's release workflow, from the tagged commit.

**From source.**

```sh
go install github.com/alexzeitgeist/resticscope/cmd/resticscope@latest
```

## Set up

**1. Write a config.** Create `~/.config/resticscope/config.toml`:

```toml
[global]
secrets_command = "pass show resticscope/credentials"

[repos.homeserver-system]
credential         = "hetzner-home"
endpoint           = "https://fsn1.your-objectstorage.com"
bucket             = "homeserver-backups"
expected_frequency = "24h"
```

Add one `[repos.<name>]` table per repository. Copyable templates:
[`config.example.toml`](config.example.toml) (short) and
[`config.explained.toml`](config.explained.toml) (annotated). Every setting is
listed in [docs/configuration.md](docs/configuration.md).

**2. Store your secrets.** `resticscope secrets template` prints the JSON
skeleton for your config. Fill in the blanks and put the result wherever
`secrets_command` reads it from. See [docs/secrets.md](docs/secrets.md).

**3. Verify.** `resticscope check` validates the config, the secrets, your
restic version, and whether each repository is reachable.

**4. Run.** `resticscope`

## Keys

`?` lists the keys for whatever screen you are on, plus the ones that work
everywhere:

```text
help: keybindings                                                                         ? help

Global
  R                          refresh all repos
  ?                          toggle this help
  ctrl+c                     quit

List
  ↑/k ↓/j                    move repo cursor
  pgup/ctrl+b pgdown/ctrl+f  page up/down
  enter                      open repo detail
  /                          filter by name/label
  o                          cycle sort order
  g                          cycle group key
  s                          shell with repo env
  r                          refresh this repo
  q                          quit
  showing lines 1–15 of 82

↑/↓ scroll • q back
```

The list is per screen, so it never offers a key that would do nothing. Across
all of them, these are the ones worth learning first:

| Key | Action |
| --- | --- |
| `↑` `↓` or `k` `j` | move |
| `enter` | open: repository, then snapshot, then directory |
| `esc` `q` | back |
| `/` | filter the repo list, or search inside a snapshot |
| `b` | browse a snapshot's files |
| `v` | show the versions of the selected file |
| `t` then `d` | mark one snapshot, then diff it against another |
| `e` | extract the selected file or directory |
| `s` | shell scoped to the repository |
| `r` `R` | refresh this repository / all of them |
| `o` `g` | cycle sort order / grouping |
| `i` | snapshot info |
| `?` | full keybinding list |

## Commands

```
resticscope [tui]                       launch the interactive TUI (default)
resticscope status [--refresh]          one line per repo, from cache
resticscope check                       validate config, secrets, restic, reachability
resticscope exec <repo>                 shell scoped to <repo>
resticscope exec <repo> -- <cmd>        run <cmd> in that environment
resticscope cache prune [--all]         drop restic caches for repos no longer configured
resticscope secrets template            print a blank secrets JSON skeleton
resticscope version                     print resticscope and restic versions
```

Every command that reads the config accepts `--config PATH` (default
`~/.config/resticscope/config.toml`). Details and exit codes:
[docs/cli.md](docs/cli.md).

## Documentation

| Guide | Contents |
| --- | --- |
| [Configuration](docs/configuration.md) | repositories, profiles, labels, and every setting |
| [Secrets](docs/secrets.md) | `secrets_command`, the JSON document, secret stores |
| [Command line](docs/cli.md) | subcommands, output, exit codes |
| [Browsing and diffing](docs/browsing.md) | file browser, search, snapshot diff |
| [Extracting files](docs/extract.md) | restoring files, output layout, extract as root |
| [Themes](docs/themes.md) | built-in themes and color overrides |

## License

resticscope is available under [GPL-3.0](LICENSE). Binary distributions must
also include the notices in
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
