package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alexzeitgeist/resticscope/internal/model"
	"github.com/alexzeitgeist/resticscope/internal/resticx"
)

// ShellSession is everything needed to launch an interactive shell scoped to one
// repository (and optionally a selected snapshot): the resolved shell, the child
// environment with RESTIC_*/backend-credential/RESTICSCOPE_* vars preloaded, a human banner to
// print before the prompt, and a Cleanup that removes any temporary password
// file. For bash/zsh/fish the session also tags the shell's prompt with a
// persistent "(resticscope·repo)" prefix (shellprompt.go), so the user keeps
// seeing whose shell they are in long after the banner scrolled away.
//
// The caller owns launching the process - the TUI via tea.ExecProcess, the
// `exec` subcommand via os/exec - and MUST call Cleanup once the child exits.
// Building a session has a side effect in the default "file" password mode: it
// writes the restic password to a 0600 temp file, which Cleanup deletes.
type ShellSession struct {
	Shell   string       // resolved interactive shell binary
	Env     []string     // child environment; restic password handled per mode
	Banner  string       // printed before the prompt by InteractiveArgs; empty => no banner
	Dir     string       // optional working directory for the launched shell; empty => inherit
	Cleanup func() error // removes the temp password file and prompt-tag scaffolding

	// execArgv overrides the default interactive shell arguments for prompt setup.
	execArgv []string
	// promptZDotDir reaches only interactive zsh launches, not command execution.
	promptZDotDir string
}

// ShellSession resolves repository credentials and creates a scoped session.
// A non-nil snap exports its ID. Secret resolution precedes password-file creation.
func (a *App) ShellSession(repoName string, snap *model.Snapshot) (*ShellSession, error) {
	r, ok := a.repo(repoName)
	if !ok {
		return nil, unknownRepoError(repoName)
	}
	// Credential is optional for local and SFTP backends; when set, it must resolve.
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

	sess := &ShellSession{
		Shell: resolveShell(a.Cfg.Global.Shell, os.Getenv("SHELL")),
		Env: buildShellEnv(os.Environ(), shellEnvOpts{
			target:   target,
			cacheDir: a.Cfg.Global.CacheDir,
			creds:    creds,
			snap:     snap,
			mode:     mode,
			pwFile:   pwFile,
		}),
		Banner:  shellBanner(r.Name, target.Repo, snap),
		Cleanup: cleanup,
	}
	if err := applyPromptTag(sess, promptTag(r.Name)); err != nil {
		a.logger().Warn("shell prompt tag skipped", "err", err)
	}
	return sess, nil
}

// ErrLocalShellInvalidDir identifies an invalid or inaccessible working directory
// without including its path.
var ErrLocalShellInvalidDir = errors.New("local shell: invalid working directory")

// LocalShellSession creates a shell without repository setup, removing recognized
// restic/backend credential and resticscope variables from its environment. It
// starts at an absolute, existing directory or the nearest enterable ancestor,
// describing a fallback only in the terminal banner. Invalid input returns
// path-free ErrLocalShellInvalidDir without creating a session.
func (a *App) LocalShellSession(dir string) (*ShellSession, error) {
	switch {
	case dir == "":
		return nil, fmt.Errorf("%w: dir empty", ErrLocalShellInvalidDir)
	case !filepath.IsAbs(dir):
		return nil, fmt.Errorf("%w: dir not absolute", ErrLocalShellInvalidDir)
	}
	// Replace os.Stat's path-bearing error with a fixed reason.
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: dir not accessible", ErrLocalShellInvalidDir)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: dir not a directory", ErrLocalShellInvalidDir)
	}
	shellDir := nearestEnterableDir(dir)
	if shellDir == "" { // not even / is enterable; nowhere sane to start
		return nil, fmt.Errorf("%w: dir not enterable", ErrLocalShellInvalidDir)
	}
	banner := ""
	if shellDir != dir {
		banner = fmt.Sprintf("%s is enterable only by its owner — starting in %s instead (use sudo to enter it)", dir, shellDir)
	}
	sess := &ShellSession{
		Shell:   resolveShell(a.Cfg.Global.Shell, os.Getenv("SHELL")),
		Env:     stripCredEnv(os.Environ()),
		Banner:  banner,
		Dir:     shellDir,
		Cleanup: func() error { return nil },
	}
	if err := applyPromptTag(sess, promptTag("")); err != nil {
		a.logger().Warn("shell prompt tag skipped", "err", err)
	}
	return sess, nil
}

// nearestEnterableDir returns the first enterable ancestor of validated dir, or
// empty when even the root is closed.
func nearestEnterableDir(dir string) string {
	for cur := dir; ; cur = filepath.Dir(cur) {
		if canEnterDir(cur) {
			return cur
		}
		if cur == filepath.Dir(cur) {
			return ""
		}
	}
}

// InteractiveArgs returns a POSIX wrapper that optionally prints Banner and
// replaces itself with the prompt-configured or default interactive shell. Every
// dynamic word is quoted; an empty banner emits only the exec wrapper.
func (s *ShellSession) InteractiveArgs() []string {
	argv := s.execArgv
	if len(argv) == 0 {
		argv = []string{s.Shell, "-i"}
	}
	words := make([]string, len(argv))
	for i, w := range argv {
		words[i] = posixQuote(w)
	}
	script := "exec " + strings.Join(words, " ")
	if s.Banner != "" {
		script = "printf '%s\\n' " + posixQuote(s.Banner) + "; " + script
	}
	return []string{posixShell, "-c", script}
}

// InteractiveEnv adds temporary zsh prompt configuration to Env without exposing
// it to non-interactive commands.
func (s *ShellSession) InteractiveEnv() []string {
	if s.promptZDotDir == "" {
		return s.Env
	}
	return append(envWithout(s.Env, "ZDOTDIR"), "ZDOTDIR="+s.promptZDotDir)
}

// Password modes select a private file by default or explicit environment export.
const (
	passwordModeFile = "file"
	passwordModeEnv  = "env"
)

// passwordMode defaults unset or unknown values to private-file delivery.
func passwordMode(configured string) string {
	if configured == passwordModeEnv {
		return passwordModeEnv
	}
	return passwordModeFile
}

// writePasswordFile returns a private temporary password file and its cleanup.
// Environment mode creates no file and returns a no-op cleanup.
func writePasswordFile(mode, password string) (string, func() error, error) {
	if mode == passwordModeEnv {
		return "", func() error { return nil }, nil
	}
	f, err := os.CreateTemp("", "resticscope-pw-*")
	if err != nil {
		return "", nil, fmt.Errorf("create password file: %w", err)
	}
	name := f.Name()
	if _, err := f.WriteString(password); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", nil, fmt.Errorf("write password file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", nil, fmt.Errorf("close password file: %w", err)
	}
	return name, func() error { return os.Remove(name) }, nil
}

// posixShell runs launch wrappers and is the final interactive fallback.
const posixShell = "/bin/sh"

// resolveShell prefers configuration, then $SHELL, then posixShell.
func resolveShell(configured, envShell string) string {
	switch {
	case configured != "":
		return configured
	case envShell != "":
		return envShell
	default:
		return posixShell
	}
}

// repoShellKeptVars preserves AWS profile/config selectors for other shell tools.
// Supplied AWS keys take precedence over AWS_PROFILE, but a profile can supply
// restic's S3 credentials when a repository has no explicit keys. Local shells
// remove both selectors with the recognized AWS credential family.
var repoShellKeptVars = map[string]bool{
	"AWS_PROFILE":     true,
	"AWS_CONFIG_FILE": true,
}

// shellEnvOpts contains repository context layered onto inherited environment.
type shellEnvOpts struct {
	target   resticx.Target
	cacheDir string
	creds    resticx.Creds
	snap     *model.Snapshot
	mode     string // "file" | "env"
	pwFile   string
}

// buildShellEnv strips inherited credential families before adding exactly one
// repository context. File mode exports only RESTIC_PASSWORD_FILE; environment
// mode explicitly exports RESTIC_PASSWORD. Removing unrelated and stale family
// values prevents credential leakage and mixed authentication, while ordinary
// user environment remains.
func buildShellEnv(base []string, o shellEnvOpts) []string {
	// Match resticx's sorted, reserved-name-filtered backend environment.
	backend := resticx.BackendEnviron(o.target, o.creds)

	owned := make(map[string]bool, len(backend))
	for _, kv := range backend {
		if k, _, ok := strings.Cut(kv, "="); ok {
			owned[k] = true
		}
	}

	env := make([]string, 0, len(base)+len(backend)+5)
	for _, kv := range base {
		if k, _, ok := strings.Cut(kv, "="); ok {
			if owned[k] || (isCredEnvKey(k) && !repoShellKeptVars[k]) {
				continue
			}
		}
		env = append(env, kv)
	}

	env = append(env,
		"RESTIC_REPOSITORY="+o.target.Repo,
		"RESTICSCOPE_REPO="+o.target.Name,
	)
	env = append(env, backend...)
	// Reuse resticscope's repository cache; omit the variable to preserve restic's
	// default when cache storage is unset.
	if dir := resticx.RepoCacheDir(o.cacheDir, o.target.Name); dir != "" {
		env = append(env, "RESTIC_CACHE_DIR="+dir)
	}
	if o.snap != nil {
		env = append(env, "RESTICSCOPE_SNAPSHOT_ID="+o.snap.ID)
	}
	if o.mode == passwordModeEnv {
		env = append(env, "RESTIC_PASSWORD="+o.creds.ResticPassword)
	} else {
		env = append(env, "RESTIC_PASSWORD_FILE="+o.pwFile)
	}
	return env
}

// credEnvPrefixes lists the recognized restic, resticscope, and backend
// credential families removed from inherited shell environments.
var credEnvPrefixes = []string{
	"RESTIC_", "RESTICSCOPE_",
	"AWS_", "B2_", "AZURE_", "GOOGLE_", "OS_", "ST_", "RCLONE_",
}

// credEnvExact are credential keys with no shared prefix to match on.
var credEnvExact = map[string]bool{"GOOGLE_APPLICATION_CREDENTIALS": true}

// stripCredEnv removes recognized restic/backend credential and repository
// context variables while passing malformed entries through unchanged.
func stripCredEnv(base []string) []string {
	out := make([]string, 0, len(base))
	for _, kv := range base {
		k, _, ok := strings.Cut(kv, "=")
		if ok && isCredEnvKey(k) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func isCredEnvKey(k string) bool {
	if credEnvExact[k] {
		return true
	}
	for _, p := range credEnvPrefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// shellBanner describes the repository and optional snapshot before the prompt.
func shellBanner(repoName, repoURL string, snap *model.Snapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "resticscope shell · %s\n", repoName)
	fmt.Fprintf(&b, "  RESTIC_REPOSITORY=%s\n", repoURL)
	if snap != nil {
		// Display the short ID while exporting the full value.
		id, note := snap.ID, ""
		if len(id) > 8 {
			id, note = id[:8]+"…", " (full id exported)"
		}
		fmt.Fprintf(&b, "  RESTICSCOPE_SNAPSHOT_ID=%s%s\n", id, note)
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

// posixQuote safely encodes one word in a POSIX shell command string.
func posixQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
