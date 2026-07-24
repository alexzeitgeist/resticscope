package model

// Backend environment variables may use any valid name except those reserved
// here, keeping validation consistent across config, secrets, and resticx.

import "strings"

// reservedBackendEnvNames prevents user values from overriding resticscope's.
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

// reservedBackendEnvPrefixes prevents dynamic-linker injection when privileged
// extraction runs restic with the payload's backend environment.
var reservedBackendEnvPrefixes = []string{"LD_", "DYLD_"}

// ReservedBackendEnvName reports whether name is owned by resticscope or
// otherwise blocked from user-supplied backend environments.
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

// ValidBackendEnvName reports whether name matches [A-Za-z_][A-Za-z0-9_]*.
// It does not check whether name is reserved.
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
