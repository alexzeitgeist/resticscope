package model

import "time"

// Status is a repository health classification.
type Status string

// The Status values, each commented with the condition EvaluateStatus assigns it for.
const (
	StatusGreen Status = "green" // last snapshot within expected_frequency
	StatusAmber Status = "amber" // within expected_frequency + stale_grace
	StatusRed   Status = "red"   // beyond grace, or locked too long, or no snapshots
	StatusGrey  Status = "grey"  // never successfully refreshed
	StatusError Status = "error" // the last refresh returned an error
)

// StatusParams contains the thresholds used to classify repository health.
type StatusParams struct {
	ExpectedFrequency time.Duration // a snapshot is expected at least this often
	StaleGrace        time.Duration // additional slack before green/amber becomes red
	LockMaxAge        time.Duration // a lock older than this is treated as red
}

// EvaluateStatus classifies observed repository state. Precedence is refresh
// error, never refreshed, stale lock, no snapshots, then snapshot age against
// ExpectedFrequency and StaleGrace.
func EvaluateStatus(now time.Time, p StatusParams, s RepoState) Status {
	if s.LastError != "" {
		return StatusError
	}
	if !s.Refreshed() {
		return StatusGrey
	}
	if s.LockedSince != nil && p.LockMaxAge > 0 && now.Sub(*s.LockedSince) > p.LockMaxAge {
		return StatusRed
	}
	if s.LastSnapshot.IsZero() {
		return StatusRed
	}

	age := now.Sub(s.LastSnapshot)
	switch {
	case age <= p.ExpectedFrequency:
		return StatusGreen
	case age <= p.ExpectedFrequency+p.StaleGrace:
		return StatusAmber
	default:
		return StatusRed
	}
}
