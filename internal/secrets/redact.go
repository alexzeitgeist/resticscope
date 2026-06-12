package secrets

import "strings"

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
	var pairs []string
	for _, v := range values {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		pairs = append(pairs, v, redactionMask)
	}
	if len(pairs) == 0 {
		return &Redactor{}
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
