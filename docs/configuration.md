# Configuration

resticscope reads `~/.config/resticscope/config.toml`, or the file given with
`--config PATH`. It lists which repositories exist and what you expect of them.

No passwords or access keys go in this file. They come from
`secrets_command` at runtime: see [secrets.md](secrets.md).

Unknown keys are rejected, so a typo fails at startup instead of being silently
ignored. Ready-made templates:
[`config.example.toml`](../config.example.toml) (short) and
[`config.explained.toml`](../config.explained.toml) (annotated).

## The smallest working config

One `[global]` block and one repository:

```toml
[global]
secrets_command = "pass show resticscope/credentials"

[repos.homeserver-system]
credential         = "hetzner-home"
endpoint           = "https://fsn1.your-objectstorage.com"
bucket             = "homeserver-backups"
expected_frequency = "24h"
```

The table name (`homeserver-system`) is how resticscope identifies the
repository everywhere: in the list, in the cache, and in the secrets JSON.

## Repositories

Every repository needs a location, given in **one of two ways**. Setting both
is an error.

### Object storage: `endpoint` + `bucket`

Convenient for Hetzner, MinIO, AWS S3, Wasabi, and other S3-compatible stores:

```toml
[repos.homeserver-system]
credential         = "hetzner-home"
endpoint           = "https://fsn1.your-objectstorage.com"
region             = "fsn1"
bucket             = "homeserver-backups"
path               = "restic"          # optional prefix inside the bucket
expected_frequency = "24h"
```

| Key | Required | Meaning |
| --- | --- | --- |
| `endpoint` | yes | Storage endpoint URL. Keep the scheme (`https://`). |
| `bucket` | yes | Bucket name. |
| `path` | no | Prefix inside the bucket. Omitted means the bucket root. |
| `region` | no | Exported to restic as `AWS_DEFAULT_REGION`. |
| `bucket_lookup` | no | `auto` (default), `dns`, or `path`. Passed as `-o s3.bucket-lookup`. |

The shorthand builds the repository string `s3:<endpoint>/<bucket>[/<path>]`.
Writing that as `url` yourself works too, but then `region` and `bucket_lookup`
have to move into `env` and `options`.

### Any other backend: `url`

`url` is the restic repository string, verbatim:

```toml
[repos.usb-archive]
url                = "/mnt/archive/restic-repo"     # local path; ~ is expanded
expected_frequency = "720h"

[repos.nas-sftp]
url                = "sftp:backup@nas:/srv/restic-repo"
options            = { "sftp.command" = "ssh backup@nas -i /home/me/.ssh/backup_ed25519 -s sftp" }
expected_frequency = "24h"

[repos.nas-offsite]
url                = "b2:nas-backups:repo"
credential         = "nas-b2"
expected_frequency = "168h"

[repos.gcs-photos]
url                = "gs:photo-backups:/"
credential         = "gcs-home"
env                = { GOOGLE_PROJECT_ID = "my-project-123" }
expected_frequency = "168h"
```

Accepted schemes: `local`, `sftp`, `rest`, `s3`, `swift`, `b2`, `azure`, `gs`,
`rclone`, or a bare absolute path. Anything else fails at startup. A repository
using `url` must not set any shorthand field (`endpoint`, `region`, `bucket`,
`path`, `bucket_lookup`).

### Keys every repository can use

| Key | Required | Meaning |
| --- | --- | --- |
| `expected_frequency` | yes | How often you expect a new snapshot, e.g. `"24h"`. Drives the status color. Can be inherited from a profile. |
| `credential` | no | Name of the backend login secret in the secrets JSON. |
| `labels` | no | Free-form tags used for filtering and grouping, e.g. `{ env = "home" }`. |
| `profile` | no | Name of a `[profiles.<name>]` table to inherit settings from. |
| `env` | no | Non-secret environment variables passed to restic, e.g. `AZURE_ACCOUNT_NAME`. |
| `options` | no | restic backend options, passed as `-o key=value`, e.g. `rest.connections`. |

`credential` is optional because not every backend needs one: a local disk needs
nothing, SFTP authenticates through your SSH config or agent, and rclone uses
its own config. Where a credential is needed, the name must match an entry under
`credentials` in the secrets JSON. One credential can serve several
repositories, and a repository's password is separate from it (see
[secrets.md](secrets.md)).

Put secrets in the credential, never in `env`. Names that resticscope manages
itself are rejected as `env` names: `PATH`, `HOME`, `RESTIC_REPOSITORY`,
`RESTIC_REPOSITORY_FILE`, `RESTIC_PASSWORD`, `RESTIC_PASSWORD_FILE`,
`RESTIC_PASSWORD_COMMAND`, `RESTIC_CACHE_DIR`, and anything starting with `LD_`
or `DYLD_`.

### Repository names

A name may contain letters, digits, `.`, `_`, and `-`, and must be unique. It
becomes a cache filename, so keep it stable if you want to keep a repository's
history. Quote the table header if the name contains a dot:

```toml
[repos."host.daily"]
url                = "/srv/restic-repo"
expected_frequency = "24h"
```

## Profiles: settings shared by several repositories

Put repeated settings in a `[profiles.<name>]` table and let repositories
inherit them:

```toml
[profiles.hetzner-nbg]
credential         = "hetzner-home"
endpoint           = "https://nbg1.your-objectstorage.com"
bucket_lookup      = "auto"
expected_frequency = "24h"
labels             = { entity = "private", hoster = "hetzner" }

[repos.thinkpad-x1]
profile = "hetzner-nbg"
bucket  = "backups"
path    = "laptop"

[repos.pve]
profile = "hetzner-nbg"
bucket  = "backups"
path    = "fileserver"
labels  = { location = "ch" }
```

Rules:

- A repository's own value replaces the profile's. It cannot clear one back to
  empty.
- `labels`, `env`, and `options` are merged key by key, and the repository wins
  on duplicates.
- Anything else is inherited as-is.
- A profile that sets S3 shorthand fields cannot be inherited by a repository
  that sets `url`: the merged repository would carry both forms, which is
  rejected. Give such a repository its own profile, or none.

Profiles are pure convenience. After loading, each repository behaves exactly as
if the inherited settings had been written into its own table.

## Labels, filtering, and grouping

Labels are your own key/value tags. `/` filters the list by repository name,
label value, or region, case-insensitively. `g` groups it by a label key, if
`[global].group_by` lists one:

```toml
[global]
group_by = ["env", "criticality"]

[repos.homeserver-system]
labels = { env = "home", criticality = "high" }
```

```text
resticscope · 5 repos · restic 0.18.1 · group: env                                        ? help

     Name                            Last  Snaps     Took  Labels
home (4)
▎ •  homeserver-system             3h ago    214   12m41s  high
  △  laptop-restic                 1d ago     96    4m12s  medium
  •  nas-offsite                   1d ago     42    1h36m  high
  …  usb-archive               never refreshed

vps (1)
  ×L vps-mail                      9h ago     61      51s  high


↑/↓ move • enter detail • / filter • o sort • g group • s shell • r refresh • q quit
```

`g` cycles through the configured keys in list order and then a flat list, so
`env` → `criticality` → flat → `env` → … . The list starts grouped by the first
key. Repositories missing the active key collect in a `(no <key>)` section at
the bottom.

With `group_by` empty or omitted, `g` reports `grouping not configured` and does
nothing. Keys must be non-empty, free of surrounding whitespace, and unique.

Sorting is independent of both: `o` cycles config order (the default), then most
urgent first, then name. Neither the sort order nor the grouping is saved.

## When is a repository late?

Status comes from the age of the newest snapshot, measured against that
repository's `expected_frequency` plus `[global].stale_grace`. With
`expected_frequency = "24h"` and the default `stale_grace = "12h"`:

| Glyph | Status | Condition |
| --- | --- | --- |
| `•` | green | last snapshot within 24h |
| `△` | amber | between 24h and 36h |
| `×` | red | beyond 36h, no snapshots at all, a lock older than `lock_max_age`, or the last refresh failed |
| `…` | grey | never refreshed successfully |

Two markers can follow the glyph: `L` for a locked repository, and `*` for
cached data older than `stale_after`.

Set `expected_frequency` per repository: a nightly backup uses `"24h"`, a
weekly one `"168h"`, a USB disk you plug in monthly `"720h"`.

## Every setting

Durations use Go syntax (`"30s"`, `"10m"`, `"24h"`) and must be positive. Sizes
accept `KiB`, `MiB`, `GiB`, or a plain byte count. Paths expand `~` to your home
directory.

### `[global]`

| Key | Default | Meaning |
| --- | --- | --- |
| `secrets_command` | *required* | Command printing the secrets JSON. See [secrets.md](secrets.md). |
| `parallelism` | `4` | How many restic commands may run at once during a refresh. Keep it modest on slow links. |
| `cache_dir` | `"~/.cache/resticscope"` | Holds the status cache, the log, restic's cache, and the browse index. No secrets. |
| `log_file` | `<cache_dir>/log.jsonl` | Operation log, one JSON object per line. Set only to move it elsewhere. |
| `refresh_on_open` | `true` | Show cached status at startup and refresh stale or unseen repositories in the background. |
| `stale_after` | `"10m"` | Cached status older than this counts as stale (marked `*`). |
| `stale_grace` | `"12h"` | Slack added to each repository's `expected_frequency` before it turns red. |
| `lock_max_age` | `"30m"` | A repository lock older than this turns the repository red. |
| `secrets_command_timeout` | `"30s"` | Timeout for `secrets_command`. Raise it if your secret store is slow to unlock. |
| `restic_command_timeout` | `"2m"` | Timeout for short restic calls. Browse, diff, and extract have their own. |
| `shell` | `$SHELL`, else `/bin/sh` | Shell used for `secrets_command` and for `s` / `resticscope exec`. |
| `shell_password_mode` | `"file"` | `file` writes a temporary `0600` `RESTIC_PASSWORD_FILE` and deletes it on exit; `env` sets `RESTIC_PASSWORD`. |
| `group_by` | `[]` | Label keys the list can group by, in cycle order. |

### `[browse]`

See [browsing.md](browsing.md).

| Key | Default | Meaning |
| --- | --- | --- |
| `index_timeout` | `"10m"` | Cap on the one-time file index of a snapshot. |
| `max_disk_bytes` | `"2GiB"` | Ceiling for the encrypted index across the whole session. `"0"` means unlimited. |

### `[diff]`

See [browsing.md](browsing.md).

| Key | Default | Meaning |
| --- | --- | --- |
| `timeout` | `"10m"` | Cap on one snapshot comparison. |

### `[extract]`

See [extract.md](extract.md).

| Key | Default | Meaning |
| --- | --- | --- |
| `target_root` | `"~/resticscope-extracts"` | Base directory for extracted files. Must resolve to an absolute path. |
| `extract_timeout` | `"30m"` | Cap on one extract. |
| `unsafe_symlinks` | `"keep"` | `keep`, `skip`, or `placeholder`. |
| `remember_target` | `true` | Reuse the target directory of the last extract, for this session only. |

### `[theme]`

See [themes.md](themes.md).

| Key | Default | Meaning |
| --- | --- | --- |
| `name` | `"gruvbox-dark"` | One of the built-in themes, or `terminal` to follow your terminal's colors. |
| `background` | `true` | Paint the terminal background with the theme while the TUI runs. |
| `[theme.colors]` | – | Overrides for the ten color roles. |

Every block except `[global]` is optional. Omit one and its defaults apply.
