package model

import "time"

// Status is the health classification of a repository. It is rendered as a
// glyph in the TUI and as a word by `resticscope status`.
type Status string

const (
	StatusGreen Status = "green" // last snapshot within expected_frequency
	StatusAmber Status = "amber" // within expected_frequency + stale_grace
	StatusRed   Status = "red"   // beyond grace, or locked too long, or no snapshots
	StatusGrey  Status = "grey"  // never successfully refreshed
	StatusError Status = "error" // the last refresh returned an error
)

// StatusParams are the thresholds that turn observed state into a Status. They
// are derived from config by the caller; EvaluateStatus itself reads no config.
type StatusParams struct {
	ExpectedFrequency time.Duration // a snapshot is expected at least this often
	StaleGrace        time.Duration // additional slack before green/amber becomes red
	LockMaxAge        time.Duration // a lock older than this is treated as red
}

// EvaluateStatus is the core product behavior: a pure function from thresholds
// and observed state to a health classification. It performs no I/O, reads no
// environment, and does not log. Keep it that way (engineering rules, Rule 7).
//
// Precedence, highest first:
//  1. a recorded refresh error          -> error
//  2. never refreshed                   -> grey
//  3. a lock held longer than LockMaxAge -> red
//  4. refreshed but no snapshots exist  -> red
//  5. otherwise classify by snapshot age against ExpectedFrequency (+grace)
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
