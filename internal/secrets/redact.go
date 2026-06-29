package secrets

import (
	"slices"
	"strings"
)

// redactionMask replaces any secret value found in scrubbed text.
const redactionMask = "[REDACTED]"

// Redactor replaces known secret values with a mask. Build it from a Store
// (Store.Redactor) or from explicit values (NewRedactor), then run any text
// that might contain a secret through Redact before logging or returning it.
type Redactor struct {
	replacer *strings.Replacer
}

// NewRedactor builds a Redactor for the given values. Empty and duplicate
// values are ignored.
func NewRedactor(values ...string) *Redactor {
	seen := make(map[string]bool, len(values))
	var uniq []string
	for _, v := range values {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		uniq = append(uniq, v)
	}
	if len(uniq) == 0 {
		return &Redactor{}
	}
	// strings.Replacer prefers the first matching old value at a position, so a
	// shorter secret must not shadow a longer secret with the same prefix.
	slices.SortFunc(uniq, func(a, b string) int {
		return len(b) - len(a)
	})
	pairs := make([]string, 0, len(uniq)*2)
	for _, v := range uniq {
		pairs = append(pairs, v, redactionMask)
	}
	return &Redactor{replacer: strings.NewReplacer(pairs...)}
}

// Redact returns text with every known secret value replaced by the mask. A nil
// or empty Redactor returns text unchanged.
func (r *Redactor) Redact(text string) string {
	if r == nil || r.replacer == nil {
		return text
	}
	return r.replacer.Replace(text)
}

// Redactor returns a Redactor covering every secret value in the store.
func (s *Store) Redactor() *Redactor {
	var values []string
	for _, c := range s.credentials {
		values = append(values, c.AccessKey, c.SecretKey)
		for _, v := range c.Env {
			values = append(values, v)
		}
	}
	for _, r := range s.repos {
		values = append(values, r.ResticPassword)
	}
	return NewRedactor(values...)
}
