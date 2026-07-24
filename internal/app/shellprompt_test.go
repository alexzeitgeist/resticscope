package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexzeitgeist/resticscope/internal/secrets"
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

	// The replacement rcfile must preserve the user's startup first.
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
	// Append after framework hooks without replacing PROMPT_COMMAND.
	if !strings.Contains(rc, "${PROMPT_COMMAND:+$PROMPT_COMMAND; }_resticscope_tag_prompt") {
		t.Errorf("rcfile does not append to PROMPT_COMMAND:\n%s", rc)
	}
	// Mark SGR escapes zero-width for line editing.
	if !strings.Contains(rc, `\[\e[1;33m\](resticscope·repo-a)\[\e[0m\] `) {
		t.Errorf("rcfile prefix not zero-width-wrapped:\n%s", rc)
	}
}

func TestZshPromptFiles(t *testing.T) {
	env := zshPromptZshenv(`"$HOME"`, "/tmp/zdot-x")
	if !strings.Contains(env, `[ -f "$HOME"/.zshenv ] && . "$HOME"/.zshenv`) {
		t.Errorf(".zshenv does not forward to the user's zshenv:\n%s", env)
	}
	// Preserve user ZDOTDIR reassignment while still reaching the tag rc.
	if !strings.Contains(env, `_resticscope_user_zdotdir="$ZDOTDIR"`) ||
		!strings.Contains(env, "ZDOTDIR='/tmp/zdot-x'") {
		t.Errorf(".zshenv does not capture a user ZDOTDIR reassignment:\n%s", env)
	}

	rc := zshPromptZshrc("(resticscope·repo-a)", `"$HOME"`, false)
	// Hide temporary ZDOTDIR from a user rc launched without one.
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
	// Append after framework rewrites and guard against accumulation.
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
	// Copy the prompt installed by config.fish before wrapping it.
	if !strings.Contains(init, "functions -c fish_prompt _resticscope_orig_prompt") {
		t.Errorf("init does not preserve the original prompt:\n%s", init)
	}
	if !strings.Contains(init, "'(resticscope·repo-a) '") {
		t.Errorf("init missing the quoted tag:\n%s", init)
	}
	// Keep a usable fallback when no original prompt exists.
	if !strings.Contains(init, "prompt_pwd") {
		t.Errorf("init missing the fallback prompt:\n%s", init)
	}
	// Require copy, guard, and invocation references so the wrapper is not a no-op.
	if got := strings.Count(init, "_resticscope_orig_prompt"); got != 3 {
		t.Errorf("expected 3 _resticscope_orig_prompt references, got %d:\n%s", got, init)
	}
	// The function and nested if each require a balanced end.
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
	// Non-interactive Env must retain user ZDOTDIR without prompt scaffolding.
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

// Full-session cleanup removes both password and prompt files.
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

// A local shell uses the bare application tag because no repository is in scope.
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
