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
	const secretStdout = "should-never-appear"
	run := func(ctx context.Context, shell, command string) ([]byte, []byte, error) {
		return []byte(secretStdout), []byte("gpg: decryption failed"), errors.New("exit status 2")
	}
	_, err := Load(context.Background(), run, "/bin/sh", "pass show x")
	if err == nil {
		t.Fatal("expected error from non-zero exit")
	}
	if strings.Contains(err.Error(), secretStdout) {
		t.Errorf("error leaked stdout (secret payload): %q", err.Error())
	}
	if !strings.Contains(err.Error(), "gpg: decryption failed") {
		t.Errorf("error should surface stderr, got %q", err.Error())
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
