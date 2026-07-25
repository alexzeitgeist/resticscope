# Browsing and diffing snapshots

Press `enter` on a repository to see its snapshots:

```text
detail: homeserver-system · • on schedule                                                 ? help

  Backend    s3
  Repository s3:https://fsn1.your-objectstorage.com/homeserver-backups
  Snapshots  214
  Hosts      homeserver
  Program    restic 0.18.1
  Tags       system, daily
  Last       2026-05-23 11:00 (3h ago)

Snapshots
   ID        Time              Hostname                   Size      Added  Tags
▎  d0e1f2a3  2026-05-23 11:00  homeserver              414 GiB   +4.6 GiB  system,daily
   c7d8e9f0  2026-05-22 11:00  homeserver              413 GiB   +4.6 GiB  system,daily
   b4c5d6e7  2026-05-21 11:00  homeserver              412 GiB   +4.6 GiB  system,daily
   a1b2c3d4  2026-05-20 11:00  homeserver              411 GiB   +4.6 GiB  system,daily

↑/↓ move • enter browse • t mark • d diff • i info • e extract • g group • s shell • q back
```

`i` shows everything restic recorded for the selected snapshot. `g` groups the
table by host, tags, or paths, and `c` collapses snapshots that share a tree. On
a taller terminal, a panel below the table adds the selected snapshot's full ID,
backup window, and what that run added.

## The file browser

`b` (or `enter`) on a snapshot opens its file tree:

```text
browse: homeserver-system · d0e1f2a3                                                      ? help

Path       /
  6 entries
  Name ↓                                         Size  Modified          Perms             Owner
▎ ▸ boot/                                     486 MiB  2026-01-18 14:00  drwxr-xr-x          0:0
  ▸ etc/                                       24 MiB  2026-05-21 11:00  drwxr-xr-x          0:0
  ▸ home/                                      96 GiB  2026-04-16 02:00  drwxr-xr-x          0:0
  ▸ root/                                     9.0 MiB  2026-05-22 11:00  drwx------          0:0
  ▸ srv/                                      288 GiB  2026-04-03 14:00  drwxr-xr-x          0:0
  ▸ var/                                       31 GiB  2026-05-23 10:00  drwxr-xr-x          0:0

↑/↓ move • enter open • ⌫ parent • / search • v versions • e extract • o sort • s shell • q back
```

Directory sizes are recursive. `o` cycles the sort order. The Owner, Perms, and
Modified columns drop out as the terminal gets narrower; Name and Size always
stay. The browser is read-only. To extract something, press `e`
([extract.md](extract.md)).

### Why the first open takes a moment

Opening a repository is the expensive part of any restic call, and on object
storage it costs seconds. Returning more data once it is open is cheap. So the
browser opens a snapshot once and takes everything: the first time you browse a
snapshot, a single
`restic --no-lock ls --json --recursive <snapshot> /` streams its entire file
list into a local index.

Every move after that is a query against that index, so navigation is instant
and complete. Nothing is truncated, there is no "load more", and going back to a
snapshot you already indexed costs nothing.

### Limits

A snapshot can hold millions of files, so the index is bounded:

```toml
[browse]
index_timeout  = "10m"    # cap on indexing one snapshot
max_disk_bytes = "2GiB"   # ceiling for the whole session; "0" = unlimited
```

`max_disk_bytes` covers every snapshot you index during one run, not each one
separately. If either limit is hit, the snapshot is not marked as indexed and
nothing partial is shown: the browse is rolled back and reported, and you can
retry or drop into the repository shell with `s`. A listing you see is always a
complete one.

### Searching

`/` searches filenames anywhere in the snapshot, not just the directory you are
in. Matching is loose: your query's letters have to appear in a name in order, so
`cfg` finds `config`, and the closest matches come first. `enter` opens the
match, `esc` cancels, and `↑`/`↓` (or `ctrl+k`/`ctrl+j`) step through matches.
The footer counts every match and says how many it shows: the list keeps the
best 200.

`v` on a file opens the versions view: one row per distinct version of that path
(same size and modification time), with the snapshots holding it, so you can
find the copy from before a change. It searches the host the snapshot came from;
`a` widens that to every host. `enter` or `e` extracts the selected version.

### What is stored on disk

The file list is written to a temporary SQLite database under `cache_dir`, and
that database is encrypted with a random 32-byte key that exists only in memory.
The key is never derived from your repository password and never written to disk,
the cache, or the log. The database is created on your first browse and deleted
when the app exits cleanly. A file left behind by a crash cannot be read, because
the key died with the process; a later start deletes it, once it is old enough
and provably not in use by another running resticscope.

Leaving the browser keeps the database for the rest of the session, so returning
to an indexed snapshot is instant.

The status cache and the operation log record that a snapshot exists and how big
it is, never which files it contains. The one exception is transient: if restic
itself fails and prints a path on its standard error, that message appears in the
status line so you can diagnose it. It is never written to disk.

## Comparing two snapshots

In the snapshot list, mark one snapshot with `t` and press `d` on another one, or
mark both and press `d`. A third mark replaces the oldest. resticscope starts by
comparing them oldest to newest, running
`restic --no-lock diff --json <older> <newer>`, and streams the changed paths into
a navigator you can walk like the file browser.

Markers describe the pair in the direction shown in the title:

| Marker | Meaning |
| --- | --- |
| `+` | only in the second snapshot |
| `-` | only in the first snapshot |
| `M` | contents modified |
| `U` | metadata only (owner, mode, timestamps) |
| `T` | type changed, e.g. file became a symlink |
| `?` | bitrot reported by restic |

Keys: `enter` or `→` opens a directory, `⌫` goes up, `/` searches the changed
paths, `x` swaps the comparison direction, `e` extracts changed paths, and
`+ - M U T b` toggle the corresponding change types.

Comparing large trees can take minutes, so diff has its own timeout instead of
the shorter `restic_command_timeout` used for quick probes:

```toml
[diff]
timeout = "10m"
```
