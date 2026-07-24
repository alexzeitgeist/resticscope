package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/alexzeitgeist/resticscope/internal/app"
)

func TestApplyShellExit(t *testing.T) {
	tests := []struct {
		name   string
		msg    shellExitedMsg
		want   string
		substr string
	}{
		{
			name: "clean exit leaves no notice",
			msg:  shellExitedMsg{},
			want: "",
		},
		{
			name:   "shell error only",
			msg:    shellExitedMsg{err: errors.New("exit status 3")},
			want:   "shell:",
			substr: "exit status 3",
		},
		{
			name:   "cleanup error only",
			msg:    shellExitedMsg{cleanupErr: errors.New("remove /tmp/resticscope-pw-x: permission denied")},
			want:   "shell cleanup:",
			substr: "permission denied",
		},
		{
			name:   "cleanup error wins over shell error",
			msg:    shellExitedMsg{err: errors.New("exit status 3"), cleanupErr: errors.New("remove failed")},
			want:   "shell cleanup:",
			substr: "remove failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Model{}.applyShellExit(tt.msg).statusMsg
			if tt.want == "" {
				if got != "" {
					t.Fatalf("statusMsg = %q, want empty", got)
				}
				return
			}
			if !strings.HasPrefix(got, tt.want) {
				t.Fatalf("statusMsg = %q, want prefix %q", got, tt.want)
			}
			if tt.substr != "" && !strings.Contains(got, tt.substr) {
				t.Errorf("statusMsg = %q, want it to contain %q", got, tt.substr)
			}
		})
	}
}

func TestShellExitCallbackReportsCleanupError(t *testing.T) {
	cleanupErr := errors.New("remove failed")
	cleaned := false
	sess := &app.ShellSession{Cleanup: func() error { cleaned = true; return cleanupErr }}

	msg, ok := shellExitCallback(sess)(nil).(shellExitedMsg)
	if !ok {
		t.Fatalf("callback produced %T, want shellExitedMsg", msg)
	}
	if !cleaned {
		t.Error("Cleanup was not invoked")
	}
	if !errors.Is(msg.cleanupErr, cleanupErr) {
		t.Errorf("cleanupErr = %v, want %v", msg.cleanupErr, cleanupErr)
	}
}

func TestShellExitCallbackForwardsShellError(t *testing.T) {
	shellErr := errors.New("exit status 1")
	sess := &app.ShellSession{Cleanup: func() error { return nil }}

	msg, ok := shellExitCallback(sess)(shellErr).(shellExitedMsg)
	if !ok {
		t.Fatalf("callback produced %T, want shellExitedMsg", msg)
	}
	if msg.cleanupErr != nil {
		t.Errorf("cleanupErr = %v, want nil on successful cleanup", msg.cleanupErr)
	}
	if !errors.Is(msg.err, shellErr) {
		t.Errorf("err = %v, want %v", msg.err, shellErr)
	}
}
