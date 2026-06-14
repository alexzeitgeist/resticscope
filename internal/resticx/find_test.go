package resticx

import (
	"strings"
	"testing"
)

func TestResticFindLiteralPattern(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"a[1].txt", `a\[1\].txt`},
		{"star*.txt", `star\*.txt`},
		{"q?.log", `q\?.log`},
		{`path\with\backslash`, `path\\with\\backslash`},
		{"plain/path/file.txt", "plain/path/file.txt"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := resticFindLiteralPattern(tc.in); got != tc.want {
			t.Errorf("resticFindLiteralPattern(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFindMatchesParses017(t *testing.T) {
	fr := &fakeRunner{stdout: readFixture(t, "find-0.17.json")}
	c := &Client{Runner: fr}
	rs, err := c.FindMatches(t.Context(), testTarget, Creds{ResticPassword: "pw"}, "homeserver", "/etc/hostname")
	if err != nil {
		t.Fatalf("FindMatches: %v", err)
	}
	if len(rs) != 2 {
		t.Fatalf("got %d snapshot results, want 2", len(rs))
	}
	if rs[0].Hits != 1 || len(rs[0].Matches) != 1 {
		t.Errorf("first result: hits=%d matches=%d", rs[0].Hits, len(rs[0].Matches))
	}
	m := rs[0].Matches[0]
	if m.Path != "/etc/hostname" || m.Size != 12 || m.Permissions != "-rw-r--r--" {
		t.Errorf("unexpected match: %+v", m)
	}
	if m.ModTime.IsZero() {
		t.Errorf("mtime decoded as zero: %+v", m)
	}
}

func TestFindMatchesParses018WithUnknownFields(t *testing.T) {
	fr := &fakeRunner{stdout: readFixture(t, "find-0.18.json")}
	c := &Client{Runner: fr}
	rs, err := c.FindMatches(t.Context(), testTarget, Creds{ResticPassword: "pw"}, "", "/etc/hostname")
	if err != nil {
		t.Fatalf("FindMatches: %v", err)
	}
	if len(rs) != 3 {
		t.Fatalf("got %d snapshot results, want 3", len(rs))
	}
	if len(rs[2].Matches) != 0 || rs[2].Hits != 0 {
		t.Errorf("zero-match snapshot result not preserved: %+v", rs[2])
	}
	// Newer fixture's extra fields (inode, device_id, links, etc.) must be
	// ignored, not error.
	if rs[0].Matches[0].Size != 12 {
		t.Errorf("size mismatch on extended fixture: %+v", rs[0].Matches[0])
	}
}

func TestFindMatchesArgsWithHost(t *testing.T) {
	fr := &fakeRunner{stdout: []byte("[]")}
	c := &Client{Runner: fr}
	_, err := c.FindMatches(t.Context(), testTarget, Creds{ResticPassword: "pw"}, "homeserver", "/etc/hostname")
	if err != nil {
		t.Fatalf("FindMatches: %v", err)
	}
	got := strings.Join(fr.gotArgs, " ")
	want := "--no-lock find --json --long --host homeserver /etc/hostname"
	if got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

func TestFindMatchesArgsWithoutHost(t *testing.T) {
	fr := &fakeRunner{stdout: []byte("[]")}
	c := &Client{Runner: fr}
	_, err := c.FindMatches(t.Context(), testTarget, Creds{ResticPassword: "pw"}, "", "/etc/hostname")
	if err != nil {
		t.Fatalf("FindMatches: %v", err)
	}
	got := strings.Join(fr.gotArgs, " ")
	want := "--no-lock find --json --long /etc/hostname"
	if got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
	// to ensure no stray --host slipped in when host is empty.
	if strings.Contains(got, "--host") {
		t.Errorf("--host should be absent when host is empty, got %q", got)
	}
}

func TestFindMatchesEscapesGlobInArgs(t *testing.T) {
	fr := &fakeRunner{stdout: []byte("[]")}
	c := &Client{Runner: fr}
	_, err := c.FindMatches(t.Context(), testTarget, Creds{ResticPassword: "pw"}, "", "/data/a[1].txt")
	if err != nil {
		t.Fatalf("FindMatches: %v", err)
	}
	// to ensure restic receives the escaped pattern, not the raw literal that
	// would over-match `a1.txt` from a sibling.
	last := fr.gotArgs[len(fr.gotArgs)-1]
	want := `/data/a\[1\].txt`
	if last != want {
		t.Errorf("pattern reached restic as %q, want escaped %q", last, want)
	}
}
