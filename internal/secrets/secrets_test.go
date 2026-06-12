package secrets

import (
	"context"
	"errors"
	"sort"
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
	if m.Env["AWS_ACCESS_KEY_ID"] != "AK-AAAA" || m.Env["AWS_SECRET_ACCESS_KEY"] != "SK-BBBB" || m.ResticPassword != "hunter2-secret" {
		t.Errorf("unexpected material: %+v", m)
	}
}

// TestParseAndResolveEnvCredential covers the generic credential shape: env
// vars reach Material verbatim, so any restic backend's secrets can ride.
func TestParseAndResolveEnvCredential(t *testing.T) {
	json := `{
	  "credentials": {
	    "b2-home": { "env": { "B2_ACCOUNT_ID": "id-123", "B2_ACCOUNT_KEY": "key-456" } }
	  },
	  "repos": {
	    "repo-a": { "restic_password": "hunter2-secret" }
	  }
	}`
	store, err := Parse([]byte(json))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := store.Validate([]string{"b2-home"}, []string{"repo-a"}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	m, err := store.Resolve("repo-a", "b2-home")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if m.Env["B2_ACCOUNT_ID"] != "id-123" || m.Env["B2_ACCOUNT_KEY"] != "key-456" || m.ResticPassword != "hunter2-secret" {
		t.Errorf("unexpected material: %+v", m)
	}
}

// TestRestBackendEnvAllowed pins the reserved-name boundary: only the env vars
// resticscope itself owns (RESTIC_REPOSITORY, the RESTIC_PASSWORD* family,
// RESTIC_CACHE_DIR, ...) are reserved — NOT the whole RESTIC_ prefix. The rest
// backend's documented credential shape is RESTIC_REST_USERNAME /
// RESTIC_REST_PASSWORD, and it must validate and resolve.
func TestRestBackendEnvAllowed(t *testing.T) {
	json := `{
	  "credentials": {
	    "rest-server": { "env": { "RESTIC_REST_USERNAME": "u", "RESTIC_REST_PASSWORD": "p" } }
	  },
	  "repos": {
	    "repo-a": { "restic_password": "pw" }
	  }
	}`
	store, err := Parse([]byte(json))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := store.Validate([]string{"rest-server"}, []string{"repo-a"}); err != nil {
		t.Fatalf("RESTIC_REST_* must be allowed as credential env, got %v", err)
	}
	m, err := store.Resolve("repo-a", "rest-server")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if m.Env["RESTIC_REST_USERNAME"] != "u" || m.Env["RESTIC_REST_PASSWORD"] != "p" {
		t.Errorf("unexpected material: %+v", m)
	}
}

// TestResolveWithoutCredential covers credential-less repos (local/sftp
// backends): the material is the password alone.
func TestResolveWithoutCredential(t *testing.T) {
	store, err := Parse([]byte(validJSON))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	m, err := store.Resolve("repo-a", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(m.Env) != 0 || m.ResticPassword != "hunter2-secret" {
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
		{
			name:    "credential with neither shape",
			json:    `{"credentials":{"cred-a":{}},"repos":{"repo-a":{"restic_password":"p"}}}`,
			wantSub: "provides no secrets",
		},
		{
			name:    "credential mixing shapes",
			json:    `{"credentials":{"cred-a":{"access_key":"a","secret_key":"b","env":{"AWS_ACCESS_KEY_ID":"x"}}},"repos":{"repo-a":{"restic_password":"p"}}}`,
			wantSub: "mixes access_key/secret_key with env",
		},
		{
			name:    "env var with empty value",
			json:    `{"credentials":{"cred-a":{"env":{"B2_ACCOUNT_KEY":""}}},"repos":{"repo-a":{"restic_password":"p"}}}`,
			wantSub: `env "B2_ACCOUNT_KEY" must not be empty`,
		},
		{
			name:    "invalid env var name",
			json:    `{"credentials":{"cred-a":{"env":{"BAD-NAME":"v"}}},"repos":{"repo-a":{"restic_password":"p"}}}`,
			wantSub: "not a valid environment variable name",
		},
		{
			name:    "reserved env var name",
			json:    `{"credentials":{"cred-a":{"env":{"RESTIC_PASSWORD":"v"}}},"repos":{"repo-a":{"restic_password":"p"}}}`,
			wantSub: "reserved by resticscope",
		},
		{
			name:    "linker injection env var name",
			json:    `{"credentials":{"cred-a":{"env":{"LD_PRELOAD":"v"}}},"repos":{"repo-a":{"restic_password":"p"}}}`,
			wantSub: "reserved by resticscope",
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

// TestValidateWarningsAreSorted guards against the map-iteration order leaking
// into the (logged) warnings. With several extra entries the unsorted order is
// random per run, so a sorted result is the only deterministic contract.
func TestValidateWarningsAreSorted(t *testing.T) {
	json := `{
	  "credentials": {
	    "cred-a": {"access_key":"a","secret_key":"b"},
	    "cred-z": {"access_key":"x","secret_key":"y"},
	    "cred-m": {"access_key":"x","secret_key":"y"}
	  },
	  "repos": {
	    "repo-a": {"restic_password":"p"},
	    "repo-z": {"restic_password":"q"},
	    "repo-m": {"restic_password":"q"}
	  }
	}`
	store, err := Parse([]byte(json))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	warnings, err := store.Validate([]string{"cred-a"}, []string{"repo-a"})
	if err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if len(warnings) != 4 {
		t.Fatalf("expected 4 warnings (two extra creds, two extra repos), got %d: %v", len(warnings), warnings)
	}
	if !sort.StringsAreSorted(warnings) {
		t.Errorf("warnings are not sorted: %v", warnings)
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
	secretCommand := `printf '{"repos":{"repo-a":{"restic_password":"hunter2"}}}'`
	_, err := Load(context.Background(), run, "/bin/sh", secretCommand)
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
	if strings.Contains(err.Error(), secretCommand) || strings.Contains(err.Error(), "restic_password") {
		t.Errorf("error leaked the configured command, which may contain secrets: %q", err.Error())
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
	creds := []TemplateCred{{Name: "hetzner-home", S3: true}, {Name: "nas-b2", S3: false}}
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
			m, err := store.Resolve(r, c.Name)
			if err != nil {
				t.Fatalf("Resolve(%q, %q): %v", r, c.Name, err)
			}
			if len(m.Env) != 0 || m.ResticPassword != "" {
				t.Errorf("template value not blank for %q/%q: %+v", r, c.Name, m)
			}
		}
	}

	// Shape check: every name present, blank shorthand fields rendered as ""
	// for the s3 credential, and an empty env map for the generic one, so the
	// user sees exactly the blanks their backends need filled in.
	s := string(data)
	for _, name := range append([]string{"hetzner-home", "nas-b2"}, repos...) {
		if !strings.Contains(s, name) {
			t.Errorf("template missing name %q:\n%s", name, s)
		}
	}
	for _, field := range []string{`"access_key": ""`, `"secret_key": ""`, `"env": {}`, `"restic_password": ""`} {
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
	json := `{
	  "credentials": {
	    "cred-a": { "access_key": "AK-AAAA", "secret_key": "SK-BBBB" },
	    "cred-b": { "env": { "B2_ACCOUNT_KEY": "b2-key-secret" } }
	  },
	  "repos": {
	    "repo-a": { "restic_password": "hunter2-secret" }
	  }
	}`
	store, err := Parse([]byte(json))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	r := store.Redactor()
	text := "restic error: AK-AAAA used with hunter2-secret, SK-BBBB and b2-key-secret"
	got := r.Redact(text)
	for _, secret := range []string{"AK-AAAA", "SK-BBBB", "hunter2-secret", "b2-key-secret"} {
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
