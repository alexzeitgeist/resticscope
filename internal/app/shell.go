package app

import (
	"fmt"
	"os"
	"strings"

	"resticscope/internal/model"
	"resticscope/internal/resticx"
)

// ShellSession is everything needed to launch an interactive shell scoped to one
// repository (and optionally a selected snapshot): the resolved shell, the child
// environment with RESTIC_*/AWS_*/RESTICSCOPE_* preloaded, a human banner to
// print before the prompt, and a Cleanup that removes any temporary password
// file.
//
// The caller owns launching the process — the TUI via tea.ExecProcess, the
// `exec` subcommand via os/exec — and MUST call Cleanup once the child exits.
// Building a session has a side effect in the default "file" password mode: it
// writes the restic password to a 0600 temp file, which Cleanup deletes (plan
// §8, engineering rules Rule 9).
type ShellSession struct {
	Shell   string       // resolved interactive shell binary
	Env     []string     // child environment; restic password handled per mode
	Banner  string       // printed before the prompt by InteractiveArgs
	Cleanup func() error // removes the temp password file; no-op in env mode
}

// ShellSession resolves a repo's credentials and assembles a shell session for
// it. snap is optional: when non-nil its ID is exported as
// RESTICSCOPE_SNAPSHOT_ID and referenced in the banner. The error path resolves
// secrets first, so a missing repo/credential or a secrets failure is reported
// without ever having created a password file.
func (a *App) ShellSession(repoName string, snap *model.Snapshot) (*ShellSession, error) {
	r, ok := a.repo(repoName)
	if !ok {
		return nil, fmt.Errorf("no repo %q in config", repoName)
	}
	if _, ok := a.Cfg.Credential(r.Credential); !ok { // unreachable after config validation, but stay defensive
		return nil, fmt.Errorf("credential %q not found", r.Credential)
	}
	material, err := a.Secrets.Resolve(r.Name, r.Credential)
	if err != nil {
		return nil, err
	}

	target := targetOf(r)
	creds := resticCreds(material)
	mode := passwordMode(a.Cfg.Global.ShellPasswordMode)

	pwFile, cleanup, err := writePasswordFile(mode, material.ResticPassword)
	if err != nil {
		return nil, err
	}

	return &ShellSession{
		Shell:   resolveShell(a.Cfg.Global.Shell, os.Getenv("SHELL")),
		Env:     buildShellEnv(os.Environ(), target, a.Cfg.Global.CacheDir, creds, snap, mode, pwFile),
		Banner:  shellBanner(r.Name, resticx.RepoURL(target), snap),
		Cleanup: cleanup,
	}, nil
}

// InteractiveArgs returns the argv that prints the banner and then replaces
// itself with an interactive shell. The wrapper always runs under /bin/sh so its
// three lines are POSIX regardless of the user's login shell; only the final,
// exec'd shell is the user's choice. The banner and shell path are single-quoted
// so neither can break out of the wrapper.
func (s *ShellSession) InteractiveArgs() []string {
	script := "printf '%s\\n' " + posixQuote(s.Banner) + "; exec " + posixQuote(s.Shell) + " -i"
	return []string{"/bin/sh", "-c", script}
}

// passwordMode normalizes the configured shell_password_mode, defaulting to the
// safer "file" mode for any unset/unknown value (config validation already
// rejects unknown values, so this is just belt-and-suspenders).
func passwordMode(configured string) string {
	if configured == "env" {
		return "env"
	}
	return "file"
}

// writePasswordFile writes the restic password to a fresh 0600 temp file in file
// mode and returns its path plus a Cleanup that removes it. In env mode it
// creates no file and returns a no-op cleanup. os.CreateTemp creates the file
// 0600, so the password is never world- or group-readable.
func writePasswordFile(mode, password string) (string, func() error, error) {
	if mode == "env" {
		return "", func() error { return nil }, nil
	}
	f, err := os.CreateTemp("", "resticscope-pw-*")
	if err != nil {
		return "", nil, fmt.Errorf("create password file: %w", err)
	}
	name := f.Name()
	if _, err := f.WriteString(password); err != nil {
		f.Close()
		os.Remove(name)
		return "", nil, fmt.Errorf("write password file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", nil, fmt.Errorf("close password file: %w", err)
	}
	return name, func() error { return os.Remove(name) }, nil
}

// resolveShell picks the interactive shell: the configured value wins, then
// $SHELL, then /bin/sh as a last resort.
func resolveShell(configured, envShell string) string {
	switch {
	case configured != "":
		return configured
	case envShell != "":
		return envShell
	default:
		return "/bin/sh"
	}
}

// ownedShellVars are the environment keys the shell-out sets itself. Any
// inherited copy is stripped from the base so the child sees exactly one,
// unambiguous value for each — in particular so a stale RESTIC_PASSWORD in the
// parent can't override file mode's RESTIC_PASSWORD_FILE. AWS_SESSION_TOKEN is
// stripped even though we never set one: restic's S3 credential chain reads it
// from the environment alongside the access/secret keys, so a leftover token
// from some other AWS context would be paired with our fresh static keys and
// break authentication. (AWS_PROFILE/AWS_CONFIG_FILE are intentionally left
// alone: explicit env keys take precedence over the profile/file provider, so
// they don't affect restic, and the user may want them for other tooling.)
var ownedShellVars = map[string]bool{
	"RESTIC_REPOSITORY":       true,
	"RESTIC_PASSWORD":         true,
	"RESTIC_PASSWORD_FILE":    true,
	"RESTIC_PASSWORD_COMMAND": true,
	"RESTIC_CACHE_DIR":        true,
	"AWS_ACCESS_KEY_ID":       true,
	"AWS_SECRET_ACCESS_KEY":   true,
	"AWS_SESSION_TOKEN":       true,
	"AWS_DEFAULT_REGION":      true,
	"RESTICSCOPE_REPO":        true,
	"RESTICSCOPE_SNAPSHOT_ID": true,
}

// buildShellEnv assembles the child environment from a base (os.Environ() in
// production) plus the repo's restic/S3 coordinates and resticscope context. It
// is pure and the security-critical seam, so it is exercised directly by tests:
// in file mode the password is delivered only by RESTIC_PASSWORD_FILE and never
// appears in the environment; env mode is the documented opt-in that exports
// RESTIC_PASSWORD instead (plan §8). The base keeps the user's PATH/HOME/TERM
// for a usable interactive shell, but every var we own is stripped first.
func buildShellEnv(base []string, t resticx.Target, cacheDir string, creds resticx.Creds, snap *model.Snapshot, mode, pwFile string) []string {
	env := make([]string, 0, len(base)+9)
	for _, kv := range base {
		if k, _, ok := strings.Cut(kv, "="); ok && ownedShellVars[k] {
			continue
		}
		env = append(env, kv)
	}

	env = append(env,
		"RESTIC_REPOSITORY="+resticx.RepoURL(t),
		"AWS_ACCESS_KEY_ID="+creds.AccessKey,
		"AWS_SECRET_ACCESS_KEY="+creds.SecretKey,
		"RESTICSCOPE_REPO="+t.Name,
	)
	// Region is optional; export it only when set, mirroring resticx.buildEnv,
	// so the shell never sees an empty AWS_DEFAULT_REGION.
	if t.Region != "" {
		env = append(env, "AWS_DEFAULT_REGION="+t.Region)
	}
	// Point restic at the same per-repo cache the refresh runner warms, so a
	// manual `restic stats`/`ls`/`mount` in the shell reuses it instead of
	// cold-starting one under ~/.cache/restic that `cache prune` can't see.
	// When no cache_dir is configured the var is omitted, leaving restic's own
	// default rather than exporting an empty RESTIC_CACHE_DIR.
	if dir := resticx.RepoCacheDir(cacheDir, t.Name); dir != "" {
		env = append(env, "RESTIC_CACHE_DIR="+dir)
	}
	if snap != nil {
		env = append(env, "RESTICSCOPE_SNAPSHOT_ID="+snap.ID)
	}
	if mode == "env" {
		env = append(env, "RESTIC_PASSWORD="+creds.ResticPassword)
	} else {
		env = append(env, "RESTIC_PASSWORD_FILE="+pwFile)
	}
	return env
}

// shellBanner is the orientation text printed before the prompt (plan §8). It
// names the repo, echoes RESTIC_REPOSITORY (and the snapshot id when one was
// selected), and lists a few common restic commands.
func shellBanner(repoName, repoURL string, snap *model.Snapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "resticscope shell — %s\n", repoName)
	fmt.Fprintf(&b, "  RESTIC_REPOSITORY=%s\n", repoURL)
	if snap != nil {
		fmt.Fprintf(&b, "  RESTICSCOPE_SNAPSHOT_ID=%s\n", snap.ID)
	}
	b.WriteString("\n  Common commands:\n")
	b.WriteString("    restic snapshots\n")
	if snap != nil {
		b.WriteString("    restic ls $RESTICSCOPE_SNAPSHOT_ID\n")
		b.WriteString("    restic dump $RESTICSCOPE_SNAPSHOT_ID /path/to/file | less\n")
	}
	b.WriteString("    restic mount /tmp/restic-mount\n")
	b.WriteString("\n  Type 'exit' to return to resticscope.\n")
	return b.String()
}

// posixQuote wraps s in single quotes, escaping any embedded single quote, so it
// is safe as a single word inside a /bin/sh command string.
func posixQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
