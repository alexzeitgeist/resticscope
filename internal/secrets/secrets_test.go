package secrets

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const validJSON = `{
  "credentials": {
    "cred-a": { "access_key": "AK-AAAA", "secret_key": "SK-BBBB" }
  },
  "repos": {
    "repo-a": { "restic_password": "hunter2-secret" }
  }
}`

func TestParseAndResolve(t *testing.T) {
	store, err := Parse([]byte(validJSON))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	m, err := store.Resolve("repo-a", "cred-a")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if m.AccessKey != "AK-AAAA" || m.SecretKey != "SK-BBBB" || m.ResticPassword != "hunter2-secret" {
		t.Errorf("unexpected material: %+v", m)
	}
}

func TestValidateMissingFields(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		wantSub string
	}{
		{
			name:    "missing restic_password",
			json:    `{"credentials":{"cred-a":{"access_key":"a","secret_key":"b"}},"repos":{"repo-a":{}}}`,
			wantSub: "missing restic_password",
		},
		{
			name:    "missing secret_key",
			json:    `{"credentials":{"cred-a":{"access_key":"a"}},"repos":{"repo-a":{"restic_password":"p"}}}`,
			wantSub: "missing access_key or secret_key",
		},
		{
			name:    "credential absent entirely",
			json:    `{"credentials":{},"repos":{"repo-a":{"restic_password":"p"}}}`,
			wantSub: `credential "cred-a" missing from credentials map`,
		},
		{
			name:    "repo absent entirely",
			json:    `{"credentials":{"cred-a":{"access_key":"a","secret_key":"b"}},"repos":{}}`,
			wantSub: `repo "repo-a" missing from repos map`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, err := Parse([]byte(tt.json))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			_, err = store.Validate([]string{"cred-a"}, []string{"repo-a"})
			if err == nil || !strings.Contains(err.Error(), tt.wantSub) {
				t.Fatalf("Validate error = %v, want substring %q", err, tt.wantSub)
			}
		})
	}
}

func TestValidateWarnsAndIgnoresExtras(t *testing.T) {
	json := `{
      "credentials": {"cred-a": {"access_key":"a","secret_key":"b"}, "cred-extra": {"access_key":"x","secret_key":"y"}},
      "repos": {"repo-a": {"restic_password":"p"}, "repo-extra": {"restic_password":"q"}}
    }`
	store, err := Parse([]byte(json))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	warnings, err := store.Validate([]string{"cred-a"}, []string{"repo-a"})
	if err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "cred-extra") || !strings.Contains(joined, "repo-extra") {
		t.Errorf("expected warnings for extras, got %q", joined)
	}
}

func TestErrorsNeverLeakSecretValues(t *testing.T) {
	// A blob whose only complete value is a password we must never see echoed.
	const password = "TОP-SECRET-PASSWORD"
	json := `{"credentials":{"cred-a":{"access_key":"a"}},"repos":{"repo-a":{"restic_password":"` + password + `"}}}`
	store, err := Parse([]byte(json))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	_, err = store.Validate([]string{"cred-a"}, []string{"repo-a"})
	if err == nil {
		t.Fatal("expected validation error")
	}
	if strings.Contains(err.Error(), password) {
		t.Errorf("validation error leaked the password: %q", err.Error())
	}
	if strings.Contains(err.Error(), "secret_key") && strings.Contains(err.Error(), password) {
		t.Error("error must not contain secret values")
	}
}

func TestParseErrorOmitsPayload(t *testing.T) {
	const secret = "leak-me-not"
	_, err := Parse([]byte(`{"credentials": not json ` + secret))
	if err == nil {
		t.Fatal("expected parse error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("parse error leaked payload: %q", err.Error())
	}
}

func TestLoadNonZeroExit(t *testing.T) {
	const (
		secretStdout = "should-never-appear"
		// A realistic failing provider can spill secret fragments to stderr.
		secretStderr = "gpg: decryption failed using key AK-LEAKED-123 (restic_password=hunter2)"
	)
	run := func(ctx context.Context, shell, command string) ([]byte, []byte, error) {
		return []byte(secretStdout), []byte(secretStderr), errors.New("exit status 2")
	}
	_, err := Load(context.Background(), run, "/bin/sh", "pass show x")
	if err == nil {
		t.Fatal("expected error from non-zero exit")
	}
	// Neither stdout (the secret payload) nor stderr (which can carry secret
	// fragments) may appear: no Redactor exists on this failure path.
	if strings.Contains(err.Error(), secretStdout) {
		t.Errorf("error leaked stdout (secret payload): %q", err.Error())
	}
	for _, leak := range []string{"AK-LEAKED-123", "hunter2", "gpg: decryption failed"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error leaked secrets_command stderr (%q): %q", leak, err.Error())
		}
	}
	// The payload-free exit error must still be reported so the user can debug.
	if !strings.Contains(err.Error(), "exit status 2") {
		t.Errorf("error should report the exit failure, got %q", err.Error())
	}
}

func TestLoadSuccess(t *testing.T) {
	run := func(ctx context.Context, shell, command string) ([]byte, []byte, error) {
		return []byte(validJSON), nil, nil
	}
	store, err := Load(context.Background(), run, "/bin/sh", "cat secrets.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := store.Resolve("repo-a", "cred-a"); err != nil {
		t.Errorf("Resolve after Load: %v", err)
	}
}

func TestTemplate(t *testing.T) {
	creds := []string{"hetzner-home", "hetzner-cold"}
	repos := []string{"homeserver-system", "laptop-photos"}

	data, err := Template(creds, repos)
	if err != nil {
		t.Fatalf("Template: %v", err)
	}

	// The skeleton must be structurally valid input to Parse (incomplete, but the
	// right shape) and round-trip to exactly the names it was given, all blank.
	store, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse(Template): %v", err)
	}
	for _, c := range creds {
		for _, r := range repos {
			m, err := store.Resolve(r, c)
			if err != nil {
				t.Fatalf("Resolve(%q, %q): %v", r, c, err)
			}
			if m.AccessKey != "" || m.SecretKey != "" || m.ResticPassword != "" {
				t.Errorf("template value not blank for %q/%q: %+v", r, c, m)
			}
		}
	}

	// Shape check: every name present, and empty fields rendered as "" (the
	// entry types carry no omitempty), so the user sees blanks to fill in.
	s := string(data)
	for _, name := range append(append([]string{}, creds...), repos...) {
		if !strings.Contains(s, name) {
			t.Errorf("template missing name %q:\n%s", name, s)
		}
	}
	for _, field := range []string{`"access_key": ""`, `"secret_key": ""`, `"restic_password": ""`} {
		if !strings.Contains(s, field) {
			t.Errorf("template missing blank field %q:\n%s", field, s)
		}
	}
}

func TestTemplateEmptyConfig(t *testing.T) {
	// No credentials or repos configured yet — the scaffold still emits a valid,
	// parseable document with empty maps.
	data, err := Template(nil, nil)
	if err != nil {
		t.Fatalf("Template: %v", err)
	}
	if _, err := Parse(data); err != nil {
		t.Fatalf("Parse(Template(nil,nil)): %v", err)
	}
	for _, key := range []string{`"credentials": {}`, `"repos": {}`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("expected empty map %q, got:\n%s", key, data)
		}
	}
}

func TestRedactor(t *testing.T) {
	store, err := Parse([]byte(validJSON))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	r := store.Redactor()
	text := "restic error: AK-AAAA used with hunter2-secret and SK-BBBB"
	got := r.Redact(text)
	for _, secret := range []string{"AK-AAAA", "SK-BBBB", "hunter2-secret"} {
		if strings.Contains(got, secret) {
			t.Errorf("redacted text still contains %q: %q", secret, got)
		}
	}
	if !strings.Contains(got, redactionMask) {
		t.Errorf("expected mask in output, got %q", got)
	}
}

func TestNilRedactorIsSafe(t *testing.T) {
	var r *Redactor
	if got := r.Redact("plain text"); got != "plain text" {
		t.Errorf("nil redactor changed text: %q", got)
	}
	empty := NewRedactor("", "")
	if got := empty.Redact("plain text"); got != "plain text" {
		t.Errorf("empty redactor changed text: %q", got)
	}
}
