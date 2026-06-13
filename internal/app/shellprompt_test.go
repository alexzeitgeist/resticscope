package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"resticscope/internal/secrets"
)

func TestPromptTag(t *testing.T) {
	if got := promptTag("repo-a"); got != "(resticscope·repo-a)" {
		t.Errorf("promptTag(repo-a) = %q", got)
	}
	if got := promptTag(""); got != "(resticscope)" {
		t.Errorf("promptTag(\"\") = %q", got)
	}
}

func TestBashPromptRC(t *testing.T) {
	rc := bashPromptRC("(resticscope·repo-a)")

	// The user's own startup must run first — the rcfile replaces ~/.bashrc for
	// this launch, so skipping it would silently drop aliases/PATH/etc.
	if !strings.Contains(rc, `[ -f "$HOME/.bashrc" ] && . "$HOME/.bashrc"`) {
		t.Errorf("rcfile does not source the user's ~/.bashrc:\n%s", rc)
	}
	if !strings.Contains(rc, "(resticscope·repo-a)") {
		t.Errorf("rcfile missing the tag:\n%s", rc)
	}
	// The guard keeps a static PS1 from accumulating one tag per prompt.
	if !strings.Contains(rc, `"$_resticscope_ps1_tag"*) ;;`) {
		t.Errorf("rcfile missing the already-tagged guard:\n%s", rc)
	}
	// The hook must append to PROMPT_COMMAND (after framework hooks), never
	// replace it.
	if !strings.Contains(rc, "${PROMPT_COMMAND:+$PROMPT_COMMAND; }_resticscope_tag_prompt") {
		t.Errorf("rcfile does not append to PROMPT_COMMAND:\n%s", rc)
	}
	// SGR escapes must be wrapped in \[ \] so bash line editing measures the
	// prompt width correctly.
	if !strings.Contains(rc, `\[\e[1;33m\](resticscope·repo-a)\[\e[0m\] `) {
		t.Errorf("rcfile prefix not zero-width-wrapped:\n%s", rc)
	}
}

func TestZshPromptFiles(t *testing.T) {
	env := zshPromptZshenv(`"$HOME"`, "/tmp/zdot-x")
	if !strings.Contains(env, `[ -f "$HOME"/.zshenv ] && . "$HOME"/.zshenv`) {
		t.Errorf(".zshenv does not forward to the user's zshenv:\n%s", env)
	}
	// A user zshenv that reassigns ZDOTDIR (export ZDOTDIR=$HOME/.zsh) must not
	// skip the tag rc: the choice is remembered and ZDOTDIR re-pointed here.
	if !strings.Contains(env, `_resticscope_user_zdotdir="$ZDOTDIR"`) ||
		!strings.Contains(env, "ZDOTDIR='/tmp/zdot-x'") {
		t.Errorf(".zshenv does not capture a user ZDOTDIR reassignment:\n%s", env)
	}

	rc := zshPromptZshrc("(resticscope·repo-a)", `"$HOME"`, false)
	// Launched without a ZDOTDIR, the temp value must be unset again before the
	// user rc runs, so nothing the rc reads sees the throwaway directory.
	if !strings.Contains(rc, "unset ZDOTDIR") {
		t.Errorf(".zshrc does not unset the temp ZDOTDIR:\n%s", rc)
	}
	// A reassignment remembered by the .zshenv wins over the launch-time value.
	if !strings.Contains(rc, `ZDOTDIR="$_resticscope_user_zdotdir"`) {
		t.Errorf(".zshrc does not honor a user ZDOTDIR reassignment:\n%s", rc)
	}
	if !strings.Contains(rc, `[ -f "${ZDOTDIR:-$HOME}/.zshrc" ] && . "${ZDOTDIR:-$HOME}/.zshrc"`) {
		t.Errorf(".zshrc does not source the user's zshrc:\n%s", rc)
	}
	if !strings.Contains(rc, "%B%F{yellow}(resticscope·repo-a)%f%b ") {
		t.Errorf(".zshrc missing the zsh-styled tag:\n%s", rc)
	}
	// The tag is a precmd appended last, so it survives frameworks that rewrite
	// PROMPT each cycle; the guard prevents accumulation on static prompts.
	if !strings.Contains(rc, "precmd_functions+=(_resticscope_tag_prompt)") {
		t.Errorf(".zshrc missing the precmd hook:\n%s", rc)
	}
	if !strings.Contains(rc, `== "$_resticscope_prompt_tag"* ]] ||`) {
		t.Errorf(".zshrc missing the already-tagged guard:\n%s", rc)
	}

	// A pre-existing ZDOTDIR is restored verbatim and the rc sourced from there.
	rc = zshPromptZshrc("(resticscope·repo-a)", "'/custom/zdot'", true)
	if !strings.Contains(rc, "ZDOTDIR='/custom/zdot'") {
		t.Errorf(".zshrc does not restore the original ZDOTDIR:\n%s", rc)
	}
}

func TestFishPromptInit(t *testing.T) {
	init := fishPromptInit("(resticscope·repo-a)")
	// -C runs after config.fish, so copying fish_prompt wraps whatever the user
	// or a framework installed there.
	if !strings.Contains(init, "functions -c fish_prompt _resticscope_orig_prompt") {
		t.Errorf("init does not preserve the original prompt:\n%s", init)
	}
	if !strings.Contains(init, "'(resticscope·repo-a) '") {
		t.Errorf("init missing the quoted tag:\n%s", init)
	}
	// If the original prompt could not be copied, a minimal fallback must keep
	// the shell usable rather than showing the tag alone.
	if !strings.Contains(init, "prompt_pwd") {
		t.Errorf("init missing the fallback prompt:\n%s", init)
	}
	// _resticscope_orig_prompt must appear three times: the `functions -c` copy,
	// the `if functions -q` guard, and the then-body that actually invokes it.
	// A dropped then-body (only two occurrences) leaves the wrapper a no-op.
	if got := strings.Count(init, "_resticscope_orig_prompt"); got != 3 {
		t.Errorf("expected 3 _resticscope_orig_prompt references, got %d:\n%s", got, init)
	}
	// fish blocks are closed with `end`: the function plus its inner `if` need
	// exactly two. A malformed body (e.g. a dropped `end`) makes fish abort with
	// "Missing end to balance this function definition", so guard the balance.
	ends := 0
	for line := range strings.SplitSeq(init, "\n") {
		if strings.TrimSpace(line) == "end" {
			ends++
		}
	}
	if ends != 2 {
		t.Errorf("fish init is not block-balanced (want 2 `end`, got %d):\n%s", ends, init)
	}
}

func TestApplyPromptTagBash(t *testing.T) {
	sess := &ShellSession{Shell: "/usr/bin/bash", Cleanup: func() error { return nil }}
	if err := applyPromptTag(sess, "(resticscope·repo-a)"); err != nil {
		t.Fatalf("applyPromptTag: %v", err)
	}

	if len(sess.execArgv) != 4 || sess.execArgv[0] != "/usr/bin/bash" || sess.execArgv[1] != "--rcfile" || sess.execArgv[3] != "-i" {
		t.Fatalf("execArgv = %v, want [bash --rcfile <file> -i]", sess.execArgv)
	}
	rc := sess.execArgv[2]
	info, err := os.Stat(rc)
	if err != nil {
		t.Fatalf("stat rcfile: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("rcfile mode = %o, want 600", perm)
	}
	content, err := os.ReadFile(rc)
	if err != nil {
		t.Fatalf("read rcfile: %v", err)
	}
	if !strings.Contains(string(content), "(resticscope·repo-a)") {
		t.Errorf("rcfile does not carry the tag:\n%s", content)
	}
	if err := sess.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Stat(rc); !os.IsNotExist(err) {
		t.Errorf("rcfile still present after Cleanup: %v", err)
	}
}

func TestApplyPromptTagZsh(t *testing.T) {
	sess := &ShellSession{
		Shell:   "/usr/bin/zsh",
		Env:     []string{"PATH=/usr/bin", "ZDOTDIR=/custom/zdot"},
		Cleanup: func() error { return nil },
	}
	if err := applyPromptTag(sess, "(resticscope·repo-a)"); err != nil {
		t.Fatalf("applyPromptTag: %v", err)
	}

	if len(sess.execArgv) != 0 {
		t.Errorf("zsh needs no execArgv override, got %v", sess.execArgv)
	}
	// The plain env keeps the user's own ZDOTDIR untouched: `exec repo -- cmd`
	// reuses sess.Env for non-interactive commands, which must never see the
	// interactive prompt scaffolding.
	if v, _ := envLookup(sess.Env, "ZDOTDIR"); v != "/custom/zdot" {
		t.Errorf("sess.Env ZDOTDIR = %q, want the user's untouched value", v)
	}
	dir, ok := envLookup(sess.InteractiveEnv(), "ZDOTDIR")
	if !ok {
		t.Fatal("ZDOTDIR not set in the interactive env")
	}
	if dir == "/custom/zdot" {
		t.Fatal("interactive ZDOTDIR was not replaced with the throwaway directory")
	}
	if n := strings.Count(strings.Join(sess.InteractiveEnv(), "\n"), "ZDOTDIR="); n != 1 {
		t.Errorf("interactive ZDOTDIR appears %d times, want exactly 1", n)
	}
	rc, err := os.ReadFile(filepath.Join(dir, ".zshrc"))
	if err != nil {
		t.Fatalf("read generated .zshrc: %v", err)
	}
	// The original ZDOTDIR must be both restored and used as the rc source.
	if !strings.Contains(string(rc), "ZDOTDIR='/custom/zdot'") {
		t.Errorf(".zshrc does not restore the original ZDOTDIR:\n%s", rc)
	}
	if _, err := os.Stat(filepath.Join(dir, ".zshenv")); err != nil {
		t.Errorf("generated .zshenv missing: %v", err)
	}
	if err := sess.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("ZDOTDIR still present after Cleanup: %v", err)
	}
}

func TestApplyPromptTagFish(t *testing.T) {
	sess := &ShellSession{Shell: "/usr/bin/fish", Cleanup: func() error { return nil }}
	if err := applyPromptTag(sess, "(resticscope·repo-a)"); err != nil {
		t.Fatalf("applyPromptTag: %v", err)
	}

	if len(sess.execArgv) != 4 || sess.execArgv[1] != "-i" || sess.execArgv[2] != "-C" {
		t.Fatalf("execArgv = %v, want [fish -i -C <init>]", sess.execArgv)
	}
	if !strings.Contains(sess.execArgv[3], "(resticscope·repo-a)") {
		t.Errorf("fish init missing the tag: %q", sess.execArgv[3])
	}
}

func TestApplyPromptTagUnknownShellNoop(t *testing.T) {
	sess := &ShellSession{Shell: "/bin/dash", Env: []string{"PATH=/usr/bin"}, Cleanup: func() error { return nil }}
	if err := applyPromptTag(sess, "(resticscope)"); err != nil {
		t.Fatalf("applyPromptTag: %v", err)
	}
	if len(sess.execArgv) != 0 {
		t.Errorf("unknown shell must keep the default exec, got %v", sess.execArgv)
	}
	if _, ok := envLookup(sess.Env, "ZDOTDIR"); ok {
		t.Error("unknown shell must not touch the env")
	}
	if got := sess.InteractiveEnv(); len(got) != len(sess.Env) {
		t.Errorf("InteractiveEnv = %v, want the plain env unchanged", got)
	}
}

// The full session path: a bash repo shell launches via the generated rcfile,
// and one Cleanup removes both the password file and the rcfile.
func TestShellSessionBashPromptTag(t *testing.T) {
	a := shellApp("file", shellSecrets{mat: secrets.Material{ResticPassword: "pw"}})
	a.Cfg.Global.Shell = "/usr/bin/bash"

	sess, err := a.ShellSession("repo-a", nil)
	if err != nil {
		t.Fatalf("ShellSession: %v", err)
	}
	script := sess.InteractiveArgs()[2]
	if !strings.Contains(script, "--rcfile") {
		t.Errorf("interactive script does not use the rcfile: %q", script)
	}
	rc := sess.execArgv[2]
	content, err := os.ReadFile(rc)
	if err != nil {
		t.Fatalf("read rcfile: %v", err)
	}
	if !strings.Contains(string(content), "(resticscope·repo-a)") {
		t.Errorf("rcfile does not name the repo:\n%s", content)
	}
	pwFile, _ := envLookup(sess.Env, "RESTIC_PASSWORD_FILE")
	if err := sess.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	for _, gone := range []string{pwFile, rc} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s still present after Cleanup: %v", gone, err)
		}
	}
}

// The local "shell here" session is tagged with the bare app name — no repo is
// in scope, but the user must still see it is resticscope's shell.
func TestLocalShellSessionPromptTag(t *testing.T) {
	t.Setenv("SHELL", "/usr/bin/bash")
	a := localShellApp("")
	sess, err := a.LocalShellSession(t.TempDir())
	if err != nil {
		t.Fatalf("LocalShellSession: %v", err)
	}
	defer sess.Cleanup() //nolint:errcheck // test scaffolding only
	if len(sess.execArgv) != 4 {
		t.Fatalf("execArgv = %v, want the bash rcfile launch", sess.execArgv)
	}
	content, err := os.ReadFile(sess.execArgv[2])
	if err != nil {
		t.Fatalf("read rcfile: %v", err)
	}
	if !strings.Contains(string(content), "'\\[\\e[1;33m\\](resticscope)\\[\\e[0m\\] '") {
		t.Errorf("local shell tag should be the bare app name:\n%s", content)
	}
}

func TestInteractiveArgsExecArgvQuoted(t *testing.T) {
	s := &ShellSession{Shell: "/usr/bin/fish", execArgv: []string{"/usr/bin/fish", "-i", "-C", "echo it's tagged"}}
	args := s.InteractiveArgs()
	want := `exec '/usr/bin/fish' '-i' '-C' 'echo it'\''s tagged'`
	if args[2] != want {
		t.Errorf("script = %q, want %q", args[2], want)
	}
}

func TestApplyPromptTagVersionedShell(t *testing.T) {
	for _, shell := range []string{
		"/usr/bin/bash-5.2",
		"/opt/homebrew/bin/zsh-5.9",
		"/usr/local/bin/fish-3.6.1",
	} {
		sess := &ShellSession{Shell: shell, Cleanup: func() error { return nil }}
		if err := applyPromptTag(sess, "(resticscope·repo-a)"); err != nil {
			t.Fatalf("%s: applyPromptTag: %v", shell, err)
		}
		if len(sess.execArgv) == 0 && sess.promptZDotDir == "" {
			t.Fatalf("%s: neither execArgv nor ZDOTDIR set — versioned binary not matched", shell)
		}
		_ = sess.Cleanup()
	}
}
