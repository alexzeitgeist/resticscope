package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"resticscope/internal/model"
	"resticscope/internal/resticx"
)

// ShellSession is everything needed to launch an interactive shell scoped to one
// repository (and optionally a selected snapshot): the resolved shell, the child
// environment with RESTIC_*/backend-credential/RESTICSCOPE_* vars preloaded, a human banner to
// print before the prompt, and a Cleanup that removes any temporary password
// file. For bash/zsh/fish the session also tags the shell's prompt with a
// persistent "(resticscope·repo)" prefix (shellprompt.go), so the user keeps
// seeing whose shell they are in long after the banner scrolled away.
//
// The caller owns launching the process — the TUI via tea.ExecProcess, the
// `exec` subcommand via os/exec — and MUST call Cleanup once the child exits.
// Building a session has a side effect in the default "file" password mode: it
// writes the restic password to a 0600 temp file, which Cleanup deletes (plan
// §8, engineering rules Rule 9).
type ShellSession struct {
	Shell   string       // resolved interactive shell binary
	Env     []string     // child environment; restic password handled per mode
	Banner  string       // printed before the prompt by InteractiveArgs; empty => no banner
	Dir     string       // optional working directory for the launched shell; empty => inherit
	Cleanup func() error // removes the temp password file and prompt-tag scaffolding

	// execArgv overrides the default `Shell -i` exec when the prompt-tag setup
	// needs extra flags (bash --rcfile, fish -C); empty means the default.
	execArgv []string
	// promptZDotDir is the throwaway ZDOTDIR holding the zsh prompt-tag rc.
	// It reaches the child only via InteractiveEnv, so non-interactive uses of
	// Env (`exec repo -- cmd`) stay free of interactive-only scaffolding.
	promptZDotDir string
}

// ShellSession resolves a repo's credentials and assembles a shell session for
// it. snap is optional: when non-nil its ID is exported as
// RESTICSCOPE_SNAPSHOT_ID and referenced in the banner. The error path resolves
// secrets first, so a missing repo/credential or a secrets failure is reported
// without ever having created a password file.
func (a *App) ShellSession(repoName string, snap *model.Snapshot) (*ShellSession, error) {
	r, ok := a.repo(repoName)
	if !ok {
		return nil, unknownRepoError(repoName)
	}
	// credential is optional (local/sftp backends); when set it must resolve.
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

// ErrLocalShellInvalidDir is returned by LocalShellSession when the requested
// working directory is empty, not absolute, or not an existing directory. It is
// deliberately path-free: the wrapped reason names the failed check but never the
// dir value, so the caller (the extract success view) can surface it without
// echoing a destination path (framework §3 privacy contract, §16).
var ErrLocalShellInvalidDir = errors.New("local shell: invalid working directory")

// LocalShellSession returns a ShellSession that drops the user into their shell
// rooted at dir, with no repository contact whatsoever. Unlike the
// snapshot-scoped ShellSession it sets no RESTIC_*/backend env vars and passes no
// password file — it is the purely cosmetic "open a shell in the extracted
// directory" launch from the extract success view (framework §16). The
// inherited environment is filtered so no credential the parent process happens
// to carry leaks into the child. It prints no banner, but the prompt still gets
// the bare "(resticscope)" tag so the user can tell this apart from a plain
// terminal (Cleanup removes the tag scaffolding where one is written).
//
// When dir itself is not enterable by the user — a privileged extract can
// leave the target root-owned 0700 — the session starts in the nearest
// enterable ancestor instead, and the (normally empty) Banner says so. Probing
// up front matters: the alternative is the child shell's chdir failing AFTER
// tea.ExecProcess has already suspended the TUI, which renders as a screen
// flicker plus a cryptic "fork/exec: permission denied". After a privileged
// extract the fallback lands in the immediate parent, because the helper keeps
// every scaffolding dir owned by the invoking user. The banner names paths;
// that is fine — it prints only in the user's own terminal, never into errors
// or logs (the §3 privacy contract covers those).
//
// dir must be an absolute path to an existing directory; otherwise a path-free
// ErrLocalShellInvalidDir is returned and no session is built.
func (a *App) LocalShellSession(dir string) (*ShellSession, error) {
	switch {
	case dir == "":
		return nil, fmt.Errorf("%w: dir empty", ErrLocalShellInvalidDir)
	case !filepath.IsAbs(dir):
		return nil, fmt.Errorf("%w: dir not absolute", ErrLocalShellInvalidDir)
	}
	// Discard os.Stat's error: it embeds the path, which must never reach the
	// returned error. The static reasons below carry the failed check only.
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

// nearestEnterableDir walks from dir toward the filesystem root and returns
// the first directory the user can enter, or "" when even the root is closed.
// dir is absolute and exists (the caller validated it); in the common case it
// is returned unchanged.
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

// InteractiveArgs returns the argv that (optionally) prints the banner and then
// replaces itself with an interactive shell — execArgv when the prompt-tag
// setup installed one, plain `Shell -i` otherwise. The wrapper always runs
// under /bin/sh so it is POSIX regardless of the user's login shell; only the
// final, exec'd shell is the user's choice. The banner and every exec word are
// single-quoted so none can break out of the wrapper. An empty Banner (the
// local "shell here" session) emits only the exec wrapper, with no leading
// blank printf line.
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
	return []string{"/bin/sh", "-c", script}
}

// InteractiveEnv returns the child environment for the interactive launch: Env
// plus the prompt-tag ZDOTDIR override when zsh scaffolding was written. Env
// itself stays the plain credential environment on purpose, because `exec repo
// -- cmd` reuses it for arbitrary non-interactive commands that must not see
// interactive-only vars.
func (s *ShellSession) InteractiveEnv() []string {
	if s.promptZDotDir == "" {
		return s.Env
	}
	return append(envWithout(s.Env, "ZDOTDIR"), "ZDOTDIR="+s.promptZDotDir)
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

// repoShellKeptVars are credential-family names the repo shell deliberately
// keeps from the inherited environment even though stripCredEnv's families
// would catch them: explicit AWS env keys take precedence over the
// profile/file provider, so these cannot affect restic, and the user may want
// them for other tooling in the shell (the aws CLI inspecting the same
// bucket). The local "shell here" session still strips them — its contract is
// no credential context at all.
var repoShellKeptVars = map[string]bool{
	"AWS_PROFILE":     true,
	"AWS_CONFIG_FILE": true,
}

// shellEnvOpts carries the repo/snapshot inputs buildShellEnv injects on top of
// the inherited base environment.
type shellEnvOpts struct {
	target   resticx.Target
	cacheDir string
	creds    resticx.Creds
	snap     *model.Snapshot
	mode     string // "file" | "env"
	pwFile   string
}

// buildShellEnv assembles the child environment from a base (os.Environ() in
// production) plus the repo's restic/backend coordinates and resticscope
// context. It is pure and the security-critical seam, so it is exercised
// directly by tests: in file mode the password is delivered only by
// RESTIC_PASSWORD_FILE and never appears in the environment; env mode is the
// documented opt-in that exports RESTIC_PASSWORD instead (plan §8).
//
// The base keeps the user's PATH/HOME/TERM for a usable interactive shell, but
// it is first reduced to the same credential-free floor the local shell uses
// (stripCredEnv's families, minus repoShellKeptVars), and exactly this repo's
// vars are layered on top. Stripping whole families rather than only the keys
// this session sets matters two ways: an unrelated inherited credential
// (B2_ACCOUNT_KEY in an s3 repo's shell) must not ride along, and a stale
// same-family leftover the repo does NOT set (AWS_SESSION_TOKEN beside fresh
// static keys, AZURE_ACCOUNT_SAS beside a fresh account key) would otherwise
// be paired with the repo's credentials and break authentication. It also
// keeps a manual `restic` in the shell faithful to resticscope's own runner,
// which builds restic's environment from scratch. Backend vars this session
// sets that fall outside the known families are stripped from the base too, so
// the child sees exactly one value for every var we export.
func buildShellEnv(base []string, o shellEnvOpts) []string {
	// The repo's backend env, exactly as resticx exports it for its own restic
	// invocations: config's non-secret vars merged with the credential's secret
	// vars, sorted, reserved names dropped.
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
	// Point restic at the same per-repo cache the refresh runner warms, so a
	// manual `restic stats`/`ls`/`mount` in the shell reuses it instead of
	// cold-starting one under ~/.cache/restic that `cache prune` can't see.
	// When no cache_dir is configured the var is omitted, leaving restic's own
	// default rather than exporting an empty RESTIC_CACHE_DIR.
	if dir := resticx.RepoCacheDir(o.cacheDir, o.target.Name); dir != "" {
		env = append(env, "RESTIC_CACHE_DIR="+dir)
	}
	if o.snap != nil {
		env = append(env, "RESTICSCOPE_SNAPSHOT_ID="+o.snap.ID)
	}
	if o.mode == "env" {
		env = append(env, "RESTIC_PASSWORD="+o.creds.ResticPassword)
	} else {
		env = append(env, "RESTIC_PASSWORD_FILE="+o.pwFile)
	}
	return env
}

// credEnvPrefixes name the environment-variable prefixes that carry repository
// credentials or resticscope context. They are the credential-free floor both
// shell flavors share: the local "shell here" session strips them outright
// (and is done — it carries no credentials by contract), while the repo shell
// strips them minus repoShellKeptVars and then layers exactly its own repo's
// vars back on top. The list covers every restic backend's credential family —
// AWS_* (s3), B2_* (Backblaze), AZURE_*, GOOGLE_* (gs), OS_*/ST_* (swift), and
// RCLONE_* — not just the families a configured repo happens to use.
var credEnvPrefixes = []string{
	"RESTIC_", "RESTICSCOPE_",
	"AWS_", "B2_", "AZURE_", "GOOGLE_", "OS_", "ST_", "RCLONE_",
}

// credEnvExact are credential keys with no shared prefix to match on.
var credEnvExact = map[string]bool{"GOOGLE_APPLICATION_CREDENTIALS": true}

// stripCredEnv returns base with every credential / repo-context variable
// removed (see credEnvPrefixes / credEnvExact). It is pure and the
// security-critical seam of LocalShellSession, so it is exercised directly by
// tests. Malformed entries (no '=') are passed through untouched.
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

// shellBanner is the orientation text printed before the prompt (plan §8). It
// names the repo, echoes RESTIC_REPOSITORY (and the snapshot id when one was
// selected), and lists a few common restic commands.
func shellBanner(repoName, repoURL string, snap *model.Snapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "resticscope shell · %s\n", repoName)
	fmt.Fprintf(&b, "  RESTIC_REPOSITORY=%s\n", repoURL)
	if snap != nil {
		// Show the 8-char prefix the rest of the UI uses — the full 64-char id
		// would run to the terminal edge. The variable itself holds the full id.
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

// posixQuote wraps s in single quotes, escaping any embedded single quote, so it
// is safe as a single word inside a /bin/sh command string.
func posixQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
