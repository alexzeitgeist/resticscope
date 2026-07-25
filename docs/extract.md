# Extracting files

Press `e` to copy something out of a backup: a file or directory in the browser,
a changed path in a diff, or a whole snapshot from the snapshot list. A review
screen shows the source and exactly where the output will land, and nothing
happens until you confirm.

| Key | On the extract screen |
| --- | --- |
| `enter` | run the extract |
| `t` | choose a different target directory |
| `p` | toggle "as root", to preserve the snapshot's owners and groups |
| `s` | open a shell in the extracted directory |
| `k` / `d` | keep or delete a leftover staging directory |
| `esc` `q` | back, or cancel an extract while it runs |

Extracting never touches the repository. It runs one
`restic --no-lock restore --overwrite never`, so no data is written and not even
a lock is taken. Output goes only into a directory you confirmed under
`target_root`.

What you get is what restic restored: original permissions (including suid, sgid,
and sticky bits), modification times, xattrs and ACLs, and, when run as root,
ownership. That is the same contract `tar`, `borg`, and `rsync` give. Device
nodes, FIFOs, and sockets are counted and reported rather than treated as errors,
so one unusual file never aborts a whole tree.

## Where the files land

Output mirrors the original path under a per-snapshot directory:

```
<target_root>/<repo>/<snapshot>/<original path>

  file /etc/hosts   ->  ~/resticscope-extracts/homeserver-system/d0e1f2a3/etc/hosts
  dir  /boot/grub   ->  ~/resticscope-extracts/homeserver-system/d0e1f2a3/boot/grub
  whole snapshot /  ->  ~/resticscope-extracts/homeserver-system/d0e1f2a3
```

Because the path itself distinguishes the output, `/boot/grub` and
`/usr/lib/grub` never collide. `<repo>` is your repository name, sanitized for
filesystem use, and `<snapshot>` is its short ID.

Several extracts from one snapshot share its directory and reuse parent
directories, but a merge only ever fills empty space. resticscope refuses an
extract whose exact target already exists, whether that is a file, a directory,
or a dangling symlink, and never overwrites or merges into existing output:

- `/etc/hosts` then `/etc/passwd`: both land under the same `etc/`.
- `/etc/hosts` then all of `/etc`: refused, the partial `etc` is in the way.
- `/etc` then `/etc/hosts`: refused, the file is already there.

To resolve a refusal, delete the existing output or press `t` and pick another
target directory.

### From a diff

Extracting inside a snapshot comparison restores both sides at once, each under
its own snapshot directory in one pair directory:

```
<target_root>/<repo>/diff-<first>-<second>/<snapshot>/<original path>
```

`<first>` and `<second>` are the two short IDs in the order the diff shows them,
so `x` renames the pair directory too. Only the changed paths below the directory
you selected are restored, and only those whose change type the `+ - M U T b`
filters leave visible. A side with nothing left to restore is skipped, and the
pair directory keeps both sides side by side for comparison.

## Preserving owners and groups

A normal extract runs as you, so restored files end up owned by you. To land the
snapshot's original user and group IDs, use the built-in privileged extract:
press `p` ("as root") on the review screen, then `enter`.

resticscope resolves your secrets in your own environment first, so your usual
secret store keeps working, and then re-runs only the restore step as root
through `sudo -n -- resticscope extract-helper`. The request and the resolved
credentials are passed on that helper's standard input, never through the command
line or the environment. The mirror directories stay owned by you, so later
ordinary extracts still merge into the same tree.

If sudo needs to authenticate, resticscope suspends the TUI and hands the real
terminal to an interactive `sudo -v`, so password, Duo, and smartcard prompts work
normally, then resumes. It never reads, stores, or forwards your sudo password.
Privileged extract is gated by your normal sudo policy; do not add a passwordless
sudoers rule for `extract-helper`.

On success the screen notes *extracted as root — snapshot file ownership
preserved*.

### Running the whole TUI as root instead

This also works, with one real drawback:

```sh
sudo -E resticscope --config ~/.config/resticscope/config.toml
# or preserve just HOME:
sudo --preserve-env=HOME resticscope --config ~/.config/resticscope/config.toml
```

User-scoped secret stores generally do not resolve as root even with `HOME`
preserved: `pass` and GPG need the per-user `gpg-agent` socket (via
`XDG_RUNTIME_DIR`) and `.gnupg` ownership, and desktop keyrings need your session
keyring. Plain-file and script `secrets_command` backends do work. Privileged
extract avoids the problem entirely, which is why it is the recommended path.

## Unsafe symlinks

A restored symlink whose target is absolute (`/etc/...`) or points outside the
extracted tree (`../../...`) no longer refers to backup data once it sits in a
scratch directory. It silently points at your **live** filesystem: reading
through it returns current data, and writing through it overwrites a real file.
Links that stay inside the extracted tree are always kept as they are.

```toml
[extract]
unsafe_symlinks = "keep"   # keep | skip | placeholder
```

| Value | Effect |
| --- | --- |
| `keep` | Default. Leave the link as restic restored it and show a count of how many. Matches restic and other restore tools; inspect the target before using it. |
| `skip` | Remove the unsafe link from the output. |
| `placeholder` | Replace it with an inert text file (mode `0600`) recording the target it pointed at. |

resticscope never follows an unsafe link, so live system data is never pulled into
an extract. Only the count of links kept, removed, or replaced is shown or
logged, never their names or targets.

## Timeouts and interrupted runs

```toml
[extract]
target_root     = "~/resticscope-extracts"
extract_timeout = "30m"
remember_target = true
```

During a run, files are written to a hidden staging directory next to the
snapshot directories, never inside the mirrored tree:

```
<target_root>/<repo>/.resticscope-staging-<snapshot>-<name>-<hash>/
```

`<hash>` is the first 16 hex characters of the SHA-256 of the source path, which
keeps the name unique per snapshot and source. On success the staging tree is
published into its final place: an atomic rename for a directory, a hard link
that will not replace anything for a single file.

Exceeding `extract_timeout` cancels the run and leaves the partial staging
directory in place. resticscope deletes nothing on its own; a cancelled or failed
extract asks whether to keep or delete the staging directory that run created.
A leftover staging directory therefore always means an interrupted extract.

`remember_target` keeps the target directory an extract actually ran with (picked
with `t`) as the default for the rest of the session. A selection you backed out
of does not count, nothing is written to disk, and the next start begins at
`target_root` again. Set it to `false` to always start at `target_root`.

## Platform support

Extracting requires Linux or macOS. The step that normalizes restored metadata,
classifies unsafe symlinks, and escapes single-file patterns is validated only
there, so on any other platform an extract is refused before restic is even
started and nothing is staged. Use `restic restore` from the repository shell
(`s`) instead.

## What is never written down

Neither source nor destination paths reach the operation log, the status cache,
or the stored repository state, and errors surfaced by the app are path-free. The
destination is shown on screen while the extract screen is open and cleared when
it closes. The `remember_target` memory lives in the process only.
