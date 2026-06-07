package resticx

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"resticscope/internal/model"
)

// testSnapID is a concrete 64-char lowercase-hex snapshot ID (16 hex chars × 4).
const testSnapID = "a1b2c3d4e5f67890a1b2c3d4e5f67890a1b2c3d4e5f67890a1b2c3d4e5f67890"

// extractTreeStreamFake feeds canned NDJSON to onStdout once, mirroring
// diffStreamFake: it records the argv/env, surfaces an onStdout (callback)
// error, and otherwise returns the canned run error so classify can be tested.
type extractTreeStreamFake struct {
	data    string
	stderr  []byte
	err     error
	block   bool
	gotArgs []string
	gotEnv  []string
}

func (f *extractTreeStreamFake) RunStream(ctx context.Context, env []string, password string, onStdout func(io.Reader) error, args ...string) ([]byte, error) {
	f.gotEnv = env
	f.gotArgs = args
	if f.block {
		<-ctx.Done()
		return f.stderr, ctx.Err()
	}
	cbErr := onStdout(strings.NewReader(f.data))
	if cbErr != nil {
		return f.stderr, cbErr
	}
	if ctx.Err() != nil {
		return f.stderr, ctx.Err()
	}
	return f.stderr, f.err
}

// --- argv tests (the safety-invariant core) ---------------------------------

func TestBuildExtractTreeArgs(t *testing.T) {
	tests := []struct {
		name string
		p    ExtractTreeParams
		want []string
	}{
		{
			name: "directory source",
			p:    ExtractTreeParams{SnapshotID: testSnapID, Source: "/etc/nginx", Target: "/abs/staging"},
			want: []string{"--no-lock", "restore", testSnapID + ":/etc/nginx", "--target", "/abs/staging", "--overwrite", "never", "--json"},
		},
		{
			name: "directory source dry-run appends --dry-run -vv",
			p:    ExtractTreeParams{SnapshotID: testSnapID, Source: "/etc/nginx", Target: "/abs/staging", DryRun: true},
			want: []string{"--no-lock", "restore", testSnapID + ":/etc/nginx", "--target", "/abs/staging", "--overwrite", "never", "--json", "--dry-run", "-vv"},
		},
		{
			name: "empty source is the whole snapshot (bare ID, no colon)",
			p:    ExtractTreeParams{SnapshotID: testSnapID, Source: "", Target: "/abs/staging"},
			want: []string{"--no-lock", "restore", testSnapID, "--target", "/abs/staging", "--overwrite", "never", "--json"},
		},
		{
			name: "root source is the whole snapshot (bare ID, no colon)",
			p:    ExtractTreeParams{SnapshotID: testSnapID, Source: "/", Target: "/abs/staging"},
			want: []string{"--no-lock", "restore", testSnapID, "--target", "/abs/staging", "--overwrite", "never", "--json"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildExtractTreeArgs(tt.p)
			if err != nil {
				t.Fatalf("buildExtractTreeArgs: %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("args =\n  %q\nwant\n  %q", got, tt.want)
			}
			// Invariants restated as standalone assertions so a future refactor
			// that still produces a "plausible" argv cannot drop one silently.
			assertNoForbiddenArgs(t, got)
			if !slices.Contains(got, "--no-lock") {
				t.Error("argv must always carry --no-lock")
			}
			if i := slices.Index(got, "--overwrite"); i < 0 || i+1 >= len(got) || got[i+1] != "never" {
				t.Error("argv must always carry --overwrite never")
			}
			if i := slices.Index(got, "--target"); i < 0 || i+1 >= len(got) || got[i+1] != tt.p.Target {
				t.Error("argv must carry --target <Target>")
			}
			if tt.p.DryRun {
				di := slices.Index(got, "--dry-run")
				vi := slices.Index(got, "-vv")
				if di < 0 || vi < 0 {
					t.Error("dry-run argv must carry both --dry-run and -vv")
				}
			}
		})
	}
}

func TestBuildExtractTreeArgsRejectsBadInput(t *testing.T) {
	tests := []struct {
		name    string
		p       ExtractTreeParams
		wantErr error
	}{
		{"parent-escape source", ExtractTreeParams{SnapshotID: testSnapID, Source: "../oops", Target: "/abs"}, ErrExtractInvalidSource},
		{"unrooted source", ExtractTreeParams{SnapshotID: testSnapID, Source: "etc/nginx", Target: "/abs"}, ErrExtractInvalidSource},
		{"uncleaned dotdot source", ExtractTreeParams{SnapshotID: testSnapID, Source: "/etc/../nginx", Target: "/abs"}, ErrExtractInvalidSource},
		{"NUL in source", ExtractTreeParams{SnapshotID: testSnapID, Source: "/etc/\x00ginx", Target: "/abs"}, ErrExtractInvalidSource},
		{"non-hex snapshot", ExtractTreeParams{SnapshotID: "not-hex!", Source: "/etc", Target: "/abs"}, ErrExtractInvalidSnapshotID},
		{"latest snapshot", ExtractTreeParams{SnapshotID: "latest", Source: "/etc", Target: "/abs"}, ErrExtractInvalidSnapshotID},
		// A short ID is a valid hex prefix but must be rejected: restic would
		// resolve it as an ambiguous prefix instead of the exact selected
		// snapshot (00-framework.md §5 — verified long ID verbatim only).
		{"short hex ID", ExtractTreeParams{SnapshotID: testSnapID[:8], Source: "/etc", Target: "/abs"}, ErrExtractInvalidSnapshotID},
		{"over-length hex ID", ExtractTreeParams{SnapshotID: testSnapID + "ab", Source: "/etc", Target: "/abs"}, ErrExtractInvalidSnapshotID},
		{"empty target", ExtractTreeParams{SnapshotID: testSnapID, Source: "/etc", Target: ""}, ErrExtractInvalidTarget},
		{"relative target", ExtractTreeParams{SnapshotID: testSnapID, Source: "/etc", Target: "relative/dir"}, ErrExtractInvalidTarget},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildExtractTreeArgs(tt.p)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got != nil {
				t.Errorf("argv must not be produced on rejection, got %q", got)
			}
		})
	}
}

// TestBuildExtractTreeArgsNoShellInterpolation pins that a previously-cleaned
// source carrying shell metacharacters survives as the exact literal byte
// sequence in the snap:source argv element — no expansion, quoting, escaping, or
// splitting on ';'/'$'. The args are an []string handed straight to exec, so no
// shell ever sees them; this test guards against a future refactor that might
// route through a shell or re-process the path.
func TestBuildExtractTreeArgsNoShellInterpolation(t *testing.T) {
	// Reference argv length for a plain (non-metachar) source — the metachar
	// cases must introduce no extra tokens.
	base, err := buildExtractTreeArgs(ExtractTreeParams{SnapshotID: testSnapID, Source: "/etc/plain", Target: "/abs"})
	if err != nil {
		t.Fatalf("baseline buildExtractTreeArgs: %v", err)
	}

	for _, src := range []string{"/etc/$(whoami)", "/etc/foo;rm -rf .;bar", "/etc/`id`", "/etc/a|b&c"} {
		t.Run(src, func(t *testing.T) {
			// Sanity: these are exactly what CleanBrowsePath leaves intact, so the
			// assert layer accepts them rather than the test pinning a fiction.
			if src != model.CleanBrowsePath(src) {
				t.Fatalf("test input %q is not already clean (CleanBrowsePath = %q)", src, model.CleanBrowsePath(src))
			}
			got, err := buildExtractTreeArgs(ExtractTreeParams{SnapshotID: testSnapID, Source: src, Target: "/abs"})
			if err != nil {
				t.Fatalf("buildExtractTreeArgs: %v", err)
			}
			wantElem := testSnapID + ":" + src
			if got[2] != wantElem {
				t.Errorf("snap:source element = %q, want exact literal %q", got[2], wantElem)
			}
			if len(got) != len(base) {
				t.Errorf("argv length = %d, want %d (metacharacters must add no tokens)", len(got), len(base))
			}
			assertNoForbiddenArgs(t, got)
		})
	}
}

// assertNoForbiddenArgs fails if the argv contains any flag/value the framework
// forbids for a read-only extract: a writing/deleting flag, a path filter, or
// the "latest" pseudo-reference. It also confirms the password is never sourced
// from a flag instead of fd 3.
func assertNoForbiddenArgs(t *testing.T, args []string) {
	t.Helper()
	for _, forbidden := range []string{"--path", "--delete", "latest", "--password-file", "--password-command"} {
		if slices.Contains(args, forbidden) {
			t.Errorf("argv must not contain %q: %q", forbidden, args)
		}
	}
}

// --- parser tests against captured fixtures ---------------------------------

func TestExtractTreeParsesProgressFixture(t *testing.T) {
	fs := &extractTreeStreamFake{data: string(readFixture(t, "restic-0.18-restore-progress.ndjson"))}
	c := &Client{Stream: fs}

	var events []ExtractTreeEvent
	err := c.ExtractTree(context.Background(), testTarget, Creds{ResticPassword: "pw"},
		ExtractTreeParams{SnapshotID: testSnapID, Source: "/etc/nginx", Target: "/abs/staging"},
		func(e ExtractTreeEvent) error { events = append(events, e); return nil })
	if err != nil {
		t.Fatalf("ExtractTree: %v", err)
	}

	var statuses, summaries int
	for _, e := range events {
		switch e.Kind {
		case ExtractTreeStatus:
			statuses++
			if e.TotalBytes <= 0 {
				t.Errorf("status event has non-positive TotalBytes: %+v", e)
			}
		case ExtractTreeSummary:
			summaries++
		}
	}
	if statuses < 1 {
		t.Errorf("want at least one status event, got %d", statuses)
	}
	if summaries != 1 {
		t.Fatalf("want exactly one summary event, got %d", summaries)
	}
	last := events[len(events)-1]
	if last.Kind != ExtractTreeSummary {
		t.Fatalf("stream must end with a summary, got kind %v", last.Kind)
	}
	// The captured run restored every file and byte: totals must be consistent.
	if last.FilesRestored != last.TotalFiles || last.BytesRestored != last.TotalBytes {
		t.Errorf("summary totals inconsistent: %+v", last)
	}
	if last.TotalFiles != 9 || last.TotalBytes != 268435571 {
		t.Errorf("summary = %+v, want 9 files / 268435571 bytes", last)
	}
}

func TestExtractTreeParsesDryRunFixture(t *testing.T) {
	fs := &extractTreeStreamFake{data: string(readFixture(t, "restic-0.18-restore-dryrun.ndjson"))}
	c := &Client{Stream: fs}

	var events []ExtractTreeEvent
	err := c.ExtractTree(context.Background(), testTarget, Creds{ResticPassword: "pw"},
		ExtractTreeParams{SnapshotID: testSnapID, Source: "/etc/nginx", Target: "/abs/staging", DryRun: true},
		func(e ExtractTreeEvent) error { events = append(events, e); return nil })
	if err != nil {
		t.Fatalf("ExtractTree: %v", err)
	}

	var verbose int
	var sawSized bool
	for _, e := range events {
		if e.Kind == ExtractTreeVerboseStatus {
			verbose++
			if e.Action == model.RestoreActionOther || e.Item == "" {
				t.Errorf("verbose_status missing action/item: %+v", e)
			}
			if e.Size > 0 {
				sawSized = true
			}
		}
	}
	if verbose < 1 {
		t.Errorf("want at least one verbose_status event, got %d", verbose)
	}
	if !sawSized {
		t.Error("want at least one verbose_status carrying a non-zero size")
	}
	if last := events[len(events)-1]; last.Kind != ExtractTreeSummary {
		t.Errorf("dry-run stream must end with a summary, got kind %v", last.Kind)
	}
	// The first verbose line is the big blob; pin its parsed shape.
	if first := events[0]; first.Kind != ExtractTreeVerboseStatus || first.Item != "/bigblob.bin" || first.Size != 268435456 {
		t.Errorf("first event = %+v, want verbose_status /bigblob.bin size 268435456", first)
	}
}

// TestRestoreActionOf pins every arm of the restic action vocabulary mapping.
// The dry-run fixture only carries "restored", so without this the metadata /
// skipped arms would be uncovered and a typo there would surface only as a wrong
// preview glyph two layers away.
func TestRestoreActionOf(t *testing.T) {
	cases := []struct {
		action string
		want   model.RestoreAction
	}{
		{"restored", model.RestoreActionRestored},
		{"updated metadata", model.RestoreActionMetadata},
		{"skipped", model.RestoreActionSkipped},
		{"deleted", model.RestoreActionOther},
		{"", model.RestoreActionOther},
	}
	for _, c := range cases {
		if got := restoreActionOf(c.action); got != c.want {
			t.Errorf("restoreActionOf(%q) = %v, want %v", c.action, got, c.want)
		}
	}
}

// TestExtractTreeUnknownMessageSkipped pins the forward-compat rule: an unknown
// message_type is silently skipped (not a parse error), while the known events
// around it still parse.
func TestExtractTreeUnknownMessageSkipped(t *testing.T) {
	fs := &extractTreeStreamFake{data: strings.Join([]string{
		`{"message_type":"status","percent_done":0.5,"total_files":2,"files_restored":1,"total_bytes":20,"bytes_restored":10}`,
		`{"message_type":"future_thing","whatever":true}`,
		`{"message_type":"summary","total_files":2,"files_restored":2,"total_bytes":20,"bytes_restored":20}`,
	}, "\n") + "\n"}
	c := &Client{Stream: fs}

	var kinds []ExtractTreeEventKind
	if err := c.ExtractTree(context.Background(), testTarget, Creds{ResticPassword: "pw"},
		ExtractTreeParams{SnapshotID: testSnapID, Source: "/etc", Target: "/abs"},
		func(e ExtractTreeEvent) error { kinds = append(kinds, e.Kind); return nil }); err != nil {
		t.Fatalf("ExtractTree: %v", err)
	}
	if want := []ExtractTreeEventKind{ExtractTreeStatus, ExtractTreeSummary}; !slices.Equal(kinds, want) {
		t.Errorf("kinds = %v, want %v (unknown message skipped)", kinds, want)
	}
}

func TestExtractTreeMalformedJSONIsParseError(t *testing.T) {
	fs := &extractTreeStreamFake{data: strings.Join([]string{
		`{"message_type":"status","percent_done":0.5,"total_bytes":20}`,
		`not json at all`,
	}, "\n") + "\n"}
	c := &Client{Stream: fs}
	err := c.ExtractTree(context.Background(), testTarget, Creds{ResticPassword: "pw"},
		ExtractTreeParams{SnapshotID: testSnapID, Source: "/etc", Target: "/abs"}, nil)
	var re *Error
	if !asResticError(err, &re) || re.Kind != KindParse {
		t.Fatalf("want KindParse, got %T %v", err, err)
	}
}

// --- exit-code classification -----------------------------------------------

func TestExtractTreeClassifiesExitCodes(t *testing.T) {
	tests := []struct {
		name   string
		exit   fakeExit
		stderr string
		want   ErrorKind
	}{
		{"repo not found", fakeExit(10), "repository does not exist", KindRepoNotFound},
		{"locked", fakeExit(11), string(readFixture(t, "restic-error-locked.stderr")), KindLocked},
		{"wrong password", fakeExit(12), string(readFixture(t, "restic-error-wrong-password.stderr")), KindWrongPassword},
		{"generic failure, no summary", fakeExit(1), "some other failure", KindUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := &extractTreeStreamFake{err: tt.exit, stderr: []byte(tt.stderr)}
			c := &Client{Stream: fs}
			err := c.ExtractTree(context.Background(), testTarget, Creds{ResticPassword: "pw"},
				ExtractTreeParams{SnapshotID: testSnapID, Source: "/etc", Target: "/abs"}, nil)
			var re *Error
			if !asResticError(err, &re) {
				t.Fatalf("expected *resticx.Error, got %T: %v", err, err)
			}
			if re.Kind != tt.want {
				t.Errorf("Kind = %v, want %v", re.Kind, tt.want)
			}
		})
	}
}

// TestExtractTreePartialFromSummaryThenExit1 is the real restic 0.18.1 partial
// signal: the final summary reaches stdout (RestoreTo completed, Finish ran),
// then restic exits 1 because some items failed (their per-item errors went to
// stderr). Exit 1 alone is generic; the seen summary disambiguates it.
func TestExtractTreePartialFromSummaryThenExit1(t *testing.T) {
	fs := &extractTreeStreamFake{
		data: `{"message_type":"summary","total_files":9,"files_restored":7,"total_bytes":100,"bytes_restored":80}` + "\n",
		err:  fakeExit(1),
	}
	c := &Client{Stream: fs}
	err := c.ExtractTree(context.Background(), testTarget, Creds{ResticPassword: "pw"},
		ExtractTreeParams{SnapshotID: testSnapID, Source: "/etc", Target: "/abs"}, nil)
	var re *Error
	if !asResticError(err, &re) || re.Kind != KindPartial {
		t.Fatalf("want KindPartial, got %T %v", err, err)
	}
}

// TestExtractTreeErrorEventMapsToPartial is the forward-compat path: if a restic
// version routes an "error" message to stdout, the parser surfaces it as an
// ExtractTreeError event and — even on a clean exit — the run is classified as a
// partial restore. (restic 0.18 sends these to stderr, so this is a contract for
// future versions, not a 0.18 capture.)
func TestExtractTreeErrorEventMapsToPartial(t *testing.T) {
	fs := &extractTreeStreamFake{data: strings.Join([]string{
		`{"message_type":"error","error":{"message":"could not restore"},"during":"restore","item":"/etc/x"}`,
		`{"message_type":"summary","total_files":2,"files_restored":1,"total_bytes":2,"bytes_restored":1}`,
	}, "\n") + "\n"}
	c := &Client{Stream: fs}

	var sawErr bool
	err := c.ExtractTree(context.Background(), testTarget, Creds{ResticPassword: "pw"},
		ExtractTreeParams{SnapshotID: testSnapID, Source: "/etc", Target: "/abs"},
		func(e ExtractTreeEvent) error {
			if e.Kind == ExtractTreeError {
				sawErr = true
				if e.ErrorMessage == "" || e.During != "restore" {
					t.Errorf("error event = %+v, want message + during=restore", e)
				}
			}
			return nil
		})
	if !sawErr {
		t.Error("expected an ExtractTreeError event to reach onEvent")
	}
	var re *Error
	if !asResticError(err, &re) || re.Kind != KindPartial {
		t.Fatalf("want KindPartial, got %T %v", err, err)
	}
}

// --- stderr sanitization (privacy) ------------------------------------------

// TestExtractTreeScrubsPathsKeepsSecretMask feeds stderr carrying a secret plus
// both the source and target paths. The returned error must show the secret's
// redaction mask (proving the redactor still ran) yet contain neither path.
func TestExtractTreeScrubsPathsKeepsSecretMask(t *testing.T) {
	const source = "/etc/secret-dir"
	const target = "/abs/staging/extract-7f3a"
	fs := &extractTreeStreamFake{
		err:    fakeExit(1),
		stderr: []byte("Fatal: AK-LEAK-123 failed restoring " + source + " into " + target + " boom"),
	}
	c := &Client{
		Stream: fs,
		Redact: func(s string) string { return strings.ReplaceAll(s, "AK-LEAK-123", "[REDACTED]") },
	}
	err := c.ExtractTree(context.Background(), testTarget, Creds{ResticPassword: "pw"},
		ExtractTreeParams{SnapshotID: testSnapID, Source: source, Target: target}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "[REDACTED]") {
		t.Errorf("expected secret mask in error, got %q", msg)
	}
	for _, leak := range []string{"AK-LEAK-123", source, target, "secret-dir", "extract-7f3a"} {
		if strings.Contains(msg, leak) {
			t.Errorf("error leaked %q: %q", leak, msg)
		}
	}
}

// TestExtractTreeDropsPathHeavyStderr asserts the residual-path guard: stderr
// that still carries an un-enumerated path after fragment scrubbing is dropped
// wholesale, while the *Error classification is preserved.
func TestExtractTreeDropsPathHeavyStderr(t *testing.T) {
	fs := &extractTreeStreamFake{
		err:    fakeExit(1),
		stderr: []byte("Fatal: error reading /var/lib/other/unrelated/file: permission denied"),
	}
	c := &Client{Stream: fs}
	err := c.ExtractTree(context.Background(), testTarget, Creds{ResticPassword: "pw"},
		ExtractTreeParams{SnapshotID: testSnapID, Source: "/etc", Target: "/abs/staging"}, nil)
	var re *Error
	if !asResticError(err, &re) {
		t.Fatalf("expected *resticx.Error, got %T: %v", err, err)
	}
	if re.Kind != KindUnknown {
		t.Errorf("Kind = %v, want KindUnknown (classification preserved)", re.Kind)
	}
	if re.Stderr != "" {
		t.Errorf("path-heavy stderr must be dropped, got %q", re.Stderr)
	}
	if strings.Contains(err.Error(), "/var/lib/other/unrelated/file") {
		t.Errorf("error leaked an un-enumerated path: %q", err.Error())
	}
}

// --- onEvent contract & process construction --------------------------------

func TestExtractTreeOnEventErrorAborts(t *testing.T) {
	fs := &extractTreeStreamFake{data: strings.Join([]string{
		`{"message_type":"status","percent_done":0.1,"total_bytes":20}`,
		`{"message_type":"status","percent_done":0.2,"total_bytes":20}`,
	}, "\n") + "\n"}
	c := &Client{Stream: fs}
	sentinel := errors.New("ui closed")
	err := c.ExtractTree(context.Background(), testTarget, Creds{ResticPassword: "pw"},
		ExtractTreeParams{SnapshotID: testSnapID, Source: "/etc", Target: "/abs"},
		func(ExtractTreeEvent) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel verbatim", err)
	}
}

func TestExtractTreeCanceledBeatsClassification(t *testing.T) {
	fs := &extractTreeStreamFake{block: true}
	c := &Client{Stream: fs}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := c.ExtractTree(ctx, testTarget, Creds{ResticPassword: "pw"},
		ExtractTreeParams{SnapshotID: testSnapID, Source: "/etc", Target: "/abs"}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestExtractTreePasswordOutOfBandAndBucketLookup(t *testing.T) {
	fs := &extractTreeStreamFake{data: ""}
	c := &Client{Stream: fs}
	tgt := testTarget
	tgt.BucketLookup = "dns"
	if err := c.ExtractTree(context.Background(), tgt,
		Creds{AccessKey: "AK", SecretKey: "SK", ResticPassword: "super-secret-pw"},
		ExtractTreeParams{SnapshotID: testSnapID, Source: "/etc", Target: "/abs"}, nil); err != nil {
		t.Fatalf("ExtractTree: %v", err)
	}
	env := strings.Join(fs.gotEnv, "\n")
	if strings.Contains(env, "super-secret-pw") {
		t.Error("password leaked into env")
	}
	if !strings.Contains(env, "RESTIC_PASSWORD_FILE=/dev/fd/3") {
		t.Error("env should reference the password file on fd 3")
	}
	// The password must never reach the argument list either.
	if strings.Contains(strings.Join(fs.gotArgs, " "), "super-secret-pw") {
		t.Error("password leaked into argv")
	}
	// The bucket-lookup option is prepended by ExtractTree (not buildExtractTreeArgs).
	if !slices.Equal(fs.gotArgs[:2], []string{"-o", "s3.bucket-lookup=dns"}) {
		t.Errorf("argv must prepend the bucket-lookup option, got %q", fs.gotArgs)
	}
	assertNoForbiddenArgs(t, fs.gotArgs)
}
