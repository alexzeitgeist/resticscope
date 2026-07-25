package model

import (
	"testing"
	"time"
)

func TestEvaluateStatus(t *testing.T) {
	now := time.Date(2026, 5, 23, 14, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }

	day := 24 * time.Hour
	params := StatusParams{
		ExpectedFrequency: day,
		StaleGrace:        12 * time.Hour,
		LockMaxAge:        30 * time.Minute,
	}

	tests := []struct {
		name  string
		state RepoState
		want  Status
	}{
		{
			name:  "fresh within frequency is green",
			state: RepoState{RefreshedAt: now, LastSnapshot: ago(23 * time.Hour)},
			want:  StatusGreen,
		},
		{
			name:  "within grace is amber",
			state: RepoState{RefreshedAt: now, LastSnapshot: ago(30 * time.Hour)},
			want:  StatusAmber,
		},
		{
			name:  "beyond grace is red",
			state: RepoState{RefreshedAt: now, LastSnapshot: ago(48 * time.Hour)},
			want:  StatusRed,
		},
		{
			name:  "exactly at frequency boundary is green",
			state: RepoState{RefreshedAt: now, LastSnapshot: ago(day)},
			want:  StatusGreen,
		},
		{
			name:  "exactly at grace boundary is amber",
			state: RepoState{RefreshedAt: now, LastSnapshot: ago(day + 12*time.Hour)},
			want:  StatusAmber,
		},
		{
			name:  "refresh error wins over fresh snapshot",
			state: RepoState{RefreshedAt: now, LastSnapshot: ago(time.Hour), LastError: "503 throttled"},
			want:  StatusError,
		},
		{
			name:  "never refreshed is grey",
			state: RepoState{},
			want:  StatusGrey,
		},
		{
			name:  "lock older than max age is red",
			state: RepoState{RefreshedAt: now, LastSnapshot: ago(time.Hour), LockedSince: new(ago(45 * time.Minute))},
			want:  StatusRed,
		},
		{
			name:  "young lock does not force red",
			state: RepoState{RefreshedAt: now, LastSnapshot: ago(time.Hour), LockedSince: new(ago(5 * time.Minute))},
			want:  StatusGreen,
		},
		{
			name:  "refreshed but no snapshots is red",
			state: RepoState{RefreshedAt: now},
			want:  StatusRed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EvaluateStatus(now, params, tt.state); got != tt.want {
				t.Errorf("EvaluateStatus = %q, want %q", got, tt.want)
			}
		})
	}
}
