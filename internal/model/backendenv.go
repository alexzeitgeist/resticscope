package model

// backendenv.go is the contract for user-supplied backend environment
// variable names — the secret env maps a credential provides (internal/secrets)
// and the non-secret per-repo env from config (internal/config). resticscope is
// storage-agnostic: it does not know which env vars each restic backend reads
// (AWS_*, AZURE_*, B2_*, GOOGLE_*, OS_*, ...), so any syntactically valid name
// is allowed except the reserved set below. The policy lives in model — the
// shared leaf — so config and secrets validate against exactly the rules
// resticx enforces when it assembles the restic process environment.

import "strings"

// reservedBackendEnvNames are the env vars resticscope itself owns when it
// runs restic. A user-supplied var must not collide: the repository, password
// delivery, and cache location are resticscope's contract with restic, and
// PATH/HOME select the binary and its configuration.
var reservedBackendEnvNames = map[string]bool{
	"PATH":                    true,
	"HOME":                    true,
	"RESTIC_REPOSITORY":       true,
	"RESTIC_REPOSITORY_FILE":  true,
	"RESTIC_PASSWORD":         true,
	"RESTIC_PASSWORD_FILE":    true,
	"RESTIC_PASSWORD_COMMAND": true,
	"RESTIC_CACHE_DIR":        true,
}

// reservedBackendEnvPrefixes block dynamic-linker injection. The privileged
// extract helper runs restic as root with the payload's backend env, so
// LD_PRELOAD & co. would otherwise turn a sudoers rule that only allows the
// helper into arbitrary root code execution.
var reservedBackendEnvPrefixes = []string{"LD_", "DYLD_"}

// ReservedBackendEnvName reports whether name is owned by resticscope (or
// blocked outright) and therefore must not be user-supplied. resticx skips
// reserved names at env-assembly time even though config and secrets already
// reject them, because the assembly also runs in the privileged extract helper
// on a payload that crossed a process boundary.
func ReservedBackendEnvName(name string) bool {
	if reservedBackendEnvNames[name] {
		return true
	}
	for _, p := range reservedBackendEnvPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// ValidBackendEnvName reports whether name is syntactically usable as an env
// var: a POSIX-style identifier ([A-Za-z_][A-Za-z0-9_]*). Reservation is a
// separate question (ReservedBackendEnvName), so validators can report "not a
// valid name" and "reserved" distinctly.
func ValidBackendEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
