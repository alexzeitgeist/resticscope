package main

import (
	"bytes"
	"slices"
	"testing"

	"resticscope/internal/app"
)

func TestParseExecArgs(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCfg  string
		wantRepo string
		wantCmd  []string
		wantOK   bool
	}{
		{
			name:     "repo only",
			args:     []string{"repo-a"},
			wantRepo: "repo-a",
			wantOK:   true,
		},
		{
			name:     "config flag then repo",
			args:     []string{"--config", "/etc/rs.toml", "repo-a"},
			wantCfg:  "/etc/rs.toml",
			wantRepo: "repo-a",
			wantOK:   true,
		},
		{
			name:     "repo with command",
			args:     []string{"repo-a", "--", "restic", "snapshots"},
			wantRepo: "repo-a",
			wantCmd:  []string{"restic", "snapshots"},
			wantOK:   true,
		},
		{
			name:     "config flag, repo, and command",
			args:     []string{"--config", "/etc/rs.toml", "repo-a", "--", "echo", "hi"},
			wantCfg:  "/etc/rs.toml",
			wantRepo: "repo-a",
			wantCmd:  []string{"echo", "hi"},
			wantOK:   true,
		},
		{
			name:     "flags after -- are command args, not parsed",
			args:     []string{"repo-a", "--", "ls", "--config", "x"},
			wantRepo: "repo-a",
			wantCmd:  []string{"ls", "--config", "x"},
			wantOK:   true,
		},
		{
			name:   "no repo",
			args:   []string{},
			wantOK: false,
		},
		{
			name:   "command but no repo",
			args:   []string{"--", "echo", "hi"},
			wantOK: false,
		},
		{
			name:   "extra bare args (forgotten --) are rejected",
			args:   []string{"repo-a", "restic", "snapshots"},
			wantOK: false,
		},
		{
			name:   "extra bare args before a real command are rejected",
			args:   []string{"repo-a", "typo", "--", "restic", "snapshots"},
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var errBuf bytes.Buffer
			cfg, repo, cmdArgs, ok := parseExecArgs(tt.args, &errBuf)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (stderr=%q)", ok, tt.wantOK, errBuf.String())
			}
			if !ok {
				return
			}
			if cfg != tt.wantCfg {
				t.Errorf("cfgPath = %q, want %q", cfg, tt.wantCfg)
			}
			if repo != tt.wantRepo {
				t.Errorf("repo = %q, want %q", repo, tt.wantRepo)
			}
			if !slices.Equal(cmdArgs, tt.wantCmd) {
				t.Errorf("cmdArgs = %v, want %v", cmdArgs, tt.wantCmd)
			}
		})
	}
}

// runExec in command mode must return the child's own exit code so it composes
// in scripts, and 1 (with a message) when the command cannot be launched at
// all. The session env is irrelevant to these process-level outcomes, so a bare
// session is enough — no secrets, no repo.
func TestRunExecExitCodes(t *testing.T) {
	tests := []struct {
		name     string
		cmdArgs  []string
		wantCode int
		wantErr  bool
	}{
		{name: "clean exit", cmdArgs: []string{"sh", "-c", "exit 0"}, wantCode: 0},
		{name: "nonzero exit propagates", cmdArgs: []string{"sh", "-c", "exit 7"}, wantCode: 7},
		{name: "missing binary is exit 1", cmdArgs: []string{"resticscope-no-such-binary-xyz"}, wantCode: 1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess := &app.ShellSession{}
			var out, errBuf bytes.Buffer
			code := runExec(t.Context(), sess, tt.cmdArgs, &out, &errBuf)
			if code != tt.wantCode {
				t.Errorf("exit = %d, want %d (stderr=%q)", code, tt.wantCode, errBuf.String())
			}
			if tt.wantErr && errBuf.Len() == 0 {
				t.Errorf("expected an error message on stderr, got none")
			}
		})
	}
}
