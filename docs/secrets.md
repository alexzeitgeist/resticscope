# Secrets

resticscope stores no credentials. When it needs them, it runs the command in
`[global].secrets_command` and reads one JSON document from its standard output.
That document holds your backend logins and your repository passwords.

This is the same idea as restic's own `RESTIC_PASSWORD_COMMAND` or git's
`credential.helper`: point it at whatever already guards your secrets.

```toml
[global]
secrets_command = "pass show resticscope/credentials"
```

## The JSON document

Two objects. `credentials` holds backend logins, one per `credential` name your
repositories reference. `repos` holds one restic password per configured
repository.

```json
{
  "credentials": {
    "hetzner-home": { "access_key": "HZ-...", "secret_key": "HZ-..." },
    "nas-b2":       { "env": { "B2_ACCOUNT_ID": "...", "B2_ACCOUNT_KEY": "..." } },
    "rest-server":  { "env": { "RESTIC_REST_USERNAME": "...", "RESTIC_REST_PASSWORD": "..." } }
  },
  "repos": {
    "homeserver-system": { "restic_password": "..." },
    "nas-offsite":       { "restic_password": "..." }
  }
}
```

- A credential is either the S3 shorthand (`access_key` and `secret_key`) or a
  generic `env` map holding whatever variables the backend reads. Not both in
  one entry. The shorthand is equivalent to an `env` map with
  `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`.
- Every repository needs its own `restic_password`, even when its backend needs
  no credential.
- The reserved names listed in [configuration.md](configuration.md#keys-every-repository-can-use)
  are rejected here too.
- Entries that match nothing in your config are ignored, and noted in the
  operation log, so one shared document can serve several machines.

Not sure of the variable names for your backend? See
[restic's environment variables](https://restic.readthedocs.io/en/stable/040_backup.html#environment-variables).

## Where to keep it

| Secret store | `secrets_command` |
| --- | --- |
| pass | `pass show resticscope/credentials` |
| gpg | `gpg -d ~/.config/resticscope/secrets.json.gpg` |
| age | `age -d -i ~/.age/key.txt ~/.config/resticscope/secrets.json.age` |
| SOPS | `sops -d ~/.config/resticscope/secrets.json` |
| 1Password | `op read 'op://Private/resticscope/credentials'` |
| plain file | `cat ~/.config/resticscope/secrets.json` |

A plain file works and takes the same code path as everything else, so nothing
has to be migrated later. Keep it mode `0600`.

The command runs through your shell, so pipes and flags are fine. If the stored
value is wrapped in other text (a `pass` entry with extra lines, for example),
pipe it through `jq` or `sed` so that only the JSON reaches standard output.

## Generating the skeleton

`resticscope secrets template` prints the document your config needs, with every
value blank:

```
$ resticscope secrets template
{
  "credentials": {
    "hetzner-home": {
      "access_key": "",
      "secret_key": ""
    },
    "nas-b2": {
      "env": {}
    }
  },
  "repos": {
    "homeserver-system": {
      "restic_password": ""
    },
    "laptop-photos": {
      "restic_password": ""
    }
  }
}
```

Credentials used only by S3-shorthand repositories get `access_key` and
`secret_key`; any other credential gets an empty `env` map to fill in.

The command reads no secrets and makes no network or restic calls, so it works
before any secret exists. The JSON goes to standard output and a one-line hint
to standard error.

A typical first-time flow:

```sh
umask 077
tmp=$(mktemp "${TMPDIR:-/tmp}/resticscope-secrets.XXXXXX")
resticscope secrets template > "$tmp"
$EDITOR "$tmp"                                    # fill in the blanks
pass insert -m resticscope/credentials < "$tmp"   # or your store of choice
rm -f "$tmp"
resticscope check
```

## When it runs, and what happens to the values

Commands that talk to your repositories run `secrets_command` once, at startup:
the TUI, `check`, `status --refresh`, and `exec`. Cache-only commands never run
it: plain `status`, `secrets template`, `cache prune`, and `version`.

The TUI resolves everything before the interface appears, so a refresh you
trigger later with `r` or `R`, a shell you open with `s`, and an extract all
reuse what is already in memory. Editing your secret store therefore has no
effect until you restart resticscope.

Resolved secrets live in memory for the session only. They are never written to
the status cache, the operation log, or error messages. A missing or incomplete
entry is reported by name, never by value.

`[global].secrets_command_timeout` (default `30s`) bounds the command. Raise it
if your store is slow to unlock.

## Trust

`secrets_command` runs with full shell access. Anyone who can edit your config
can already run anything you can, so this grants no new privilege, but do not
point it at a command from a config you did not write.

With `pass` or `gpg`, `gpg-agent` handles the unlock prompt. resticscope runs
the command before starting the TUI, so any passphrase prompt appears at your
normal terminal.
