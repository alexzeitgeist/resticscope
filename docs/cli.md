# Command line

The TUI is the default. The subcommands exist for setup, scripting, and
monitoring.

```
resticscope [tui]                       launch the interactive TUI (default)
resticscope status [--refresh]          one line per repo, from cache
resticscope check                       validate config, secrets, restic, reachability
resticscope exec <repo>                 shell scoped to <repo>
resticscope exec <repo> -- <cmd>        run <cmd> in that environment
resticscope cache prune [--all]         drop restic caches for repos no longer configured
resticscope secrets template            print a blank secrets JSON skeleton
resticscope version                     print resticscope and restic versions
resticscope help                        this list
```

Every command that reads the config accepts `--config PATH`, defaulting to
`~/.config/resticscope/config.toml`.

## `status`

Prints one line per repository from the local cache. No secrets are loaded and
no backend is contacted, so it is fast enough for a shell prompt or a monitoring
script.

```text
$ resticscope status
homeserver-system  green  3h ago  214 snaps  12m41s
laptop-restic      amber  1d ago  96 snaps   4m12s
nas-offsite        green  1d ago  42 snaps   1h36m  (stale cache)
vps-mail           red    9h ago  61 snaps   51s
usb-archive        grey   never refreshed
```

Columns: name, status, age of the newest snapshot, snapshot count, how long the
last backup took. `(stale cache)` means the cached data is older than
`stale_after`. A repository that has never been refreshed, or whose last refresh
failed, replaces those columns with the reason.

`--refresh` contacts every repository first, which loads secrets and takes as
long as your slowest backend.

| Exit code | Meaning |
| --- | --- |
| `0` | every repository green |
| `1` | at least one amber |
| `2` | at least one red, error, or grey, or the command itself failed |

## `check`

Validates the whole chain without starting the TUI. Run it after any config or
secret change.

```text
$ resticscope check
config   ok      5 repos, 2 credentials
secrets  ok      all credentials and repos resolved
restic   ok      0.18.1
repositories
  homeserver-system  ok
  laptop-restic      ok
  nas-offsite        ok
  vps-mail           ok
  usb-archive        FAILED: restic cat: repository does not exist (exit 10)

check failed: 1 of 5 repositories unreachable
```

It runs four stages in order and stops at the first failure that would make the
rest meaningless:

1. **config**: parse and validate `config.toml`.
2. **secrets**: run `secrets_command` and confirm every credential and every
   repository password resolves.
3. **restic**: confirm the `restic` on your `PATH` is at least 0.17.0, the
   version that introduced the stable exit codes and JSON output resticscope
   relies on. An unsupported or unreadable version stops the run rather than
   producing results that cannot be trusted.
4. **repositories**: reach each repository with `restic cat config` and report
   failures as diagnostics (does not exist, locked, wrong password) instead of
   raw stderr.

| Exit code | Meaning |
| --- | --- |
| `0` | everything passed |
| `1` | the check ran and found problems: restic too old or unparseable, or a repository unreachable |
| `2` | the check could not complete: bad config, secrets unavailable, no usable restic, or interrupted |

`check` reads no cache and changes nothing, so it is safe to run at any time.

## `exec`

Opens a shell with the repository's environment already set, which is useful for
anything the TUI does not cover (`restic dump`, `restic mount`, `restic forget`).
Pressing `s` in the TUI opens the same shell, and adds
`RESTICSCOPE_SNAPSHOT_ID` when you open it from a snapshot.

```sh
resticscope exec homeserver-system                    # interactive shell
resticscope exec homeserver-system -- restic snapshots --json
```

Preloaded for the session: `RESTIC_REPOSITORY`, the repository password (as
`RESTIC_PASSWORD_FILE` by default, or `RESTIC_PASSWORD` with
`shell_password_mode = "env"`), `RESTIC_CACHE_DIR`, `RESTICSCOPE_REPO`, and the
backend credential variables. A temporary password file is mode `0600` and is
deleted when the session ends.

Everything after `--` is executed directly, without shell interpretation. Leave
it out for an interactive shell. An interactive session prints the repository
and a few example commands, and in bash, zsh, and fish it also tags the prompt
with `(resticscope·<repo>)`, so you can still tell whose shell you are in once
the banner has scrolled away. Your own dotfiles are read first and never
modified.

| Exit code | Meaning |
| --- | --- |
| the command's own | a command run after `--` finished |
| `1` | the command could not be launched |
| `2` | setup failed, e.g. unknown repository or unavailable secrets |

## `cache prune`

restic keeps a local cache per repository under `<cache_dir>/restic-cache`.
Renaming or removing a repository in your config leaves its cache behind. This
command clears those orphans:

```sh
resticscope cache prune             # remove orphaned caches
resticscope cache prune --dry-run   # show what would go, delete nothing
resticscope cache prune --all       # remove every repository's cache
```

Every cache is listed with its size and what happens to it:

```text
$ resticscope cache prune --dry-run
restic cache: /home/alex/.cache/resticscope/restic-cache

  homeserver-system  412 MiB  keep
  laptop-restic      96 MiB   keep
  nas-offsite        1.2 GiB  keep
  old-laptop         88 MiB   would prune (orphan)
  vps-mail-old       37 MiB   would prune (orphan)

would free 125 MiB across 2 of 5 caches
```

Without `--dry-run` the same run reports `pruned (orphan)` and `freed`. `--all`
also drops caches still in use; restic rebuilds them on the next run, at the cost
of some backend traffic. The command loads no secrets and calls neither restic
nor the network.

## `secrets template`

Prints the secrets JSON skeleton your config needs, with blank values. See
[secrets.md](secrets.md#generating-the-skeleton).

## `version`

Prints the resticscope version and the version of the restic on your `PATH`, or
`restic not found on PATH`.
