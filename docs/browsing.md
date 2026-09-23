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
   showing 1–4 of 214

Selected · d0e1f2a3 · restic 0.18.1
  ID         d0e1f2a39f4c2d1e8b7a6053f1e2d3c4b5a69788cc17d2e34a5b6c7d8e9f0a1b
  User       root
  Backup     2026-05-23 11:00:00 → 2026-05-23 11:12:41 (12m41s)
  Churn      4.1 GiB packed · 1204 new · 863 changed · 2841097 files

↑/↓ move • enter browse • t mark • d diff • i info • e extract • g group • s shell • q back
```

The table is a window onto all 214 snapshots; `↑`/`↓` scroll it. The panel below
belongs to the selected snapshot, and it is the first thing to drop out as the
terminal gets shorter. `g` groups the table by host, tags, or paths, and `c`
collapses snapshots that share a tree.

`i` opens the whole record restic kept for the selected snapshot:

```text
info: homeserver-system · d0e1f2a3                                                        ? help

Identity
  ID                 d0e1f2a39f4c2d1e8b7a6053f1e2d3c4b5a69788cc17d2e34a5b6c7d8e9f0a1b
  Short ID           d0e1f2a3
  Parent             c7d8e9f09f4c2d1e8b7a6053f1e2d3c4b5a69788cc17d2e34a5b6c7d8e9f0a1b
  Tree               7ac1b2045d3e9a1c60b8742fe3d1a95c8b70642de1f3a08bc2d4e6f80a1b3c5d
  Program            restic 0.18.1

Source
  Hostname           homeserver
  Username           root
  UID                0
  GID                0
  Tags               system, daily
  Paths              /
  Excludes           /var/cache
                     /var/tmp
                     **/.cache
  showing lines 1–17 of 38

↑/↓ scroll • q back
```

Below the fold are the backup window and restic's churn counters: bytes and
blobs added, and how many files and directories were new, changed, or
unmodified. Sections restic did not record are left out rather than shown empty.

## The file browser

`b` (or `enter`) on a snapshot opens its file tree:

```text
browse: homeserver-system · d0e1f2a3                                                      ? help

Path       /
  6 entries
  Name ↓                                         Size  Modified          Perms             Owner
▎ ▸ boot/                                     323 MiB  2026-01-18 14:00  drwxr-xr-x          0:0
  ▸ etc/                                      234 KiB  2026-05-21 11:00  drwxr-xr-x          0:0
  ▸ home/                                      95 GiB  2026-04-16 02:00  drwxr-xr-x          0:0
  ▸ root/                                     8.9 MiB  2026-05-22 11:00  drwx------          0:0
  ▸ srv/                                      270 GiB  2026-04-03 14:00  drwxr-xr-x          0:0
  ▸ var/                                       23 GiB  2026-05-23 10:00  drwxr-xr-x          0:0

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
in:

```text
browse: homeserver-system · d0e1f2a3

Path       /
  4 matches
  Path                                           Size  Modified          Perms             Owner
▎   /etc/pam.d/passwd                            92 B  2025-11-19 06:44  -rw-r--r--          0:0
    /etc/passwd                               2.8 KiB  2026-04-02 08:19  -rw-r--r--          0:0
    /etc/passwd-                              2.7 KiB  2026-04-02 08:19  -rw-------          0:0
    /var/backups/passwd.bak                   2.8 KiB  2026-04-02 08:19  -rw-r--r--          0:0

/passwd▏  4 matches
↑/↓ move • enter open • esc cancel
```

Results are full paths, and matching is loose: your query's letters have to
appear in a name in order, so `cfg` finds `config`, and the closest matches come
first. `enter` opens the match, `esc` cancels, and `↑`/`↓` (or `ctrl+k`/`ctrl+j`)
step through matches. The footer counts every match and says how many it shows:
the list keeps the best 200.

### Versions of one file

`v` on a file lists every version of it in the repository, so you can find the
copy from before a change:

```text
versions: homeserver-system · d0e1f2a3                                                    ? help

Path       /etc/passwd
  2 versions across 51 snapshots · host: homeserver
  Modified                Size  Snaps  Latest                      Perms             Owner
▎ 2026-04-02 08:19     2.8 KiB      3  2026-05-23 11:00 d0e1f2a3   -rw-r--r--          0:0
  2026-01-27 09:41     2.7 KiB     48  2026-05-20 11:00 a1b2c3d4   -rw-r--r--          0:0

↑/↓ move • enter extract • a all hosts • q back
```

One row per distinct version — same size and modification time — with how many
snapshots hold it and the newest one that does. The search covers the host the
snapshot came from; `a` widens it to every host. `enter` or `e` extracts the
selected version.

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
a navigator you can walk like the file browser:

```text
diff: homeserver-system · c7d8e9f0 2026-05-22 11:00 → d0e1f2a3 2026-05-23 11:00           ? help

Path       /
  +2 -1 M9 U21 T1 · + in second snapshot · - in first snapshot · with metadata
  Change            Name
▎ U +1 -1 M2 U5 T1  ▸ etc/
  U M1 U4           ▸ home/
  U M1              ▸ root/
  U M2 U2           ▸ srv/
  U +1 M3 U5        ▸ var/

↑/↓ move • enter open • / search • i info • e extract • m metadata • +-MUTb filters • q back
```

A directory carries the totals for everything below it, so you can see where a
day's changes landed without opening anything. Files carry their own marker.
Markers describe the pair in the direction shown in the title:

| Marker | Meaning |
| --- | --- |
| `+` | only in the second snapshot |
| `-` | only in the first snapshot |
| `M` | contents modified |
| `U` | metadata only (owner, mode, timestamps); shown after `m` |
| `T` | type changed, e.g. file became a symlink |
| `?` | bitrot reported by restic |

Keys: `enter` or `→` opens a directory, `⌫` goes up, `/` searches the changed
paths, `i` shows what changed on the selected path, `x` swaps the comparison
direction, `m` includes metadata-only changes, `e` extracts changed paths, and
`+ - M U T b` toggle the corresponding change types. Every type starts visible,
so the first press hides one and the summary gains a `filter:` mask of the types
still shown; the totals keep counting everything, so what you hid stays visible.

restic leaves metadata-only changes out of a diff unless asked, so a file whose
owner or mode changed while its contents stayed the same does not appear at
first. `m` reruns the comparison with `restic diff --metadata` and the summary
reads `with metadata`; press it again to go back. The setting stays on for later
comparisons until you turn it off or quit.

- restic does not say which field changed, so a file rewritten with identical
  contents, which gets new timestamps and a new inode, shows the same `U` as one
  whose owner changed. `i` tells them apart.
- A symlink pointed somewhere else is a `U` too, so without `m` it does not
  appear at all.
- restic also marks directories above changed paths as `U`. The marker can mean
  that the directory's own metadata changed, its contents changed, or both, and
  `i` says which. The summary counts these directory markers, and each row
  shows its own `U` beside the totals for changes below it.

Comparing large trees can take minutes, so diff has its own timeout instead of
the shorter `restic_command_timeout` used for quick probes:

```toml
[diff]
timeout = "10m"
```

### What changed on a path

`i` reads the selected path's record from both snapshots and lays them side by
side, marking each field that differs with `≠`:

```text
info: homeserver-system · c7d8e9f0 2026-05-22 11:00 → d0e1f2a3 2026-05-23 11:00           ? help

Path       /etc/passwd
  U metadata only · differs in mtime, ctime, and inode

  Field        c7d8e9f0             d0e1f2a3
  Type         file                 file
  Size         2.8 KiB              2.8 KiB
  Mode         -rw-r--r--           -rw-r--r--
  Owner        root (0)             root (0)
  Group        root (0)             root (0)
≠ mtime        2026-04-02 09:14:07  2026-05-22 18:40:51
≠ ctime        2026-04-02 09:14:07  2026-05-22 18:40:51
≠ Inode        1311                 1874
  Links        1                    1
  Contents     identical

q back
```

Here the contents are identical but the file has a new inode and new times, so
something rewrote it unchanged, as `vipw` or a package script might. The first
line always restates restic's marker and, when both records exist, lists the
fields that differ. That works for every marker, not just `U`: an `M` file also
shows whether its owner or mode changed along with its contents, which restic's
marker hides; a `+` or `-` path shows its one record; and a directory says
whether its own metadata changed or only something below it.

A `ctime` that differs on its own is common: the kernel updates it whenever the
file's inode is touched, so a `chmod` or `chown` that sets the values the file
already had, as permission-fixing scripts do on every run, is enough to make the
file a `U`. The screen notes when that is the only change.

An `atime` row appears only for backups made with `restic backup --with-atime`;
otherwise restic stores a copy of the modification time. Extended attributes,
and the Windows attributes restic records, are listed by name and never by value;
when only a value changed, a note names the attribute.

Each `i` runs `restic --no-lock cat tree <snapshot>:<directory>` once for each
snapshot that has the path, both at once and under the diff timeout above, and
`esc` cancels them. The records stay in memory only while the screen is open.
