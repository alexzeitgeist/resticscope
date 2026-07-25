package main

// The command-line transcripts printed in the guides are produced here by the
// real formatters and compared against the markdown, for the same reason the TUI
// screens are: a documented screen should not be able to drift.
//
// The example repositories are the ones the TUI guides use, so the command-line
// pages read as the same session.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/app"
	"github.com/alexzeitgeist/resticscope/internal/config"
	"github.com/alexzeitgeist/resticscope/internal/model"
	"github.com/alexzeitgeist/resticscope/internal/resticx"
)

var docNow = time.Date(2026, 5, 23, 14, 0, 0, 0, time.UTC)

// docCommands renders every transcript the guides show, keyed by the prompt line
// it appears under.
func docCommands(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"$ resticscope status\n":                docStatusOutput(),
		"$ resticscope check\n":                 docCheckOutput(t),
		"$ resticscope cache prune --dry-run\n": docCachePruneOutput(t),
	}
}

// TestDocCommandOutputMatchesMarkdown checks the guides show what the commands
// print.
func TestDocCommandOutputMatchesMarkdown(t *testing.T) {
	doc := readDoc(t, "docs/cli.md")
	for prompt, body := range docCommands(t) {
		if !strings.Contains(doc, "```text\n"+prompt+body+"```") {
			t.Errorf("docs/cli.md does not contain this output as rendered.\n"+
				"Paste this block into docs/cli.md:\n\n```text\n%s%s```\n", prompt, body)
		}
	}
}

// TestNoUnverifiedCommandOutputInDocs is the other half of the guarantee: every
// transcript in the guides must be one this file produced. A block written by
// hand, or left behind after the output changed, fails here.
func TestNoUnverifiedCommandOutputInDocs(t *testing.T) {
	rendered := make(map[string]bool)
	for prompt, body := range docCommands(t) {
		rendered[prompt+body] = true
	}

	for _, name := range []string{"README.md", "docs/cli.md", "docs/configuration.md", "docs/secrets.md"} {
		for _, block := range textBlocks(readDoc(t, name)) {
			if !strings.HasPrefix(block, "$ ") || rendered[block] {
				continue
			}
			t.Errorf("%s contains command output that no test renders:\n\n%s\n"+
				"Add it to docCommands, or delete it: output nothing renders cannot be kept honest.", name, block)
		}
	}
}

// docStatusOutput is what the cache holds for the example repositories: one
// healthy, one overdue, one green but read from a cache older than stale_after,
// one red behind a stale lock, and the external drive that has never been seen.
func docStatusOutput() string {
	row := func(name string, st model.Status, age time.Duration, count int, took time.Duration, stale bool) app.RepoStatus {
		last := docNow.Add(-age)
		return app.RepoStatus{
			Name: name, Status: st, Stale: stale,
			State: model.RepoState{
				Name: name, RefreshedAt: docNow, LastSnapshot: last, SnapshotCount: count,
				Snapshots: []model.Snapshot{{
					Time:    last,
					Summary: &model.SnapshotSummary{BackupStart: last, BackupEnd: last.Add(took)},
				}},
			},
		}
	}
	rows := []app.RepoStatus{
		row("homeserver-system", model.StatusGreen, 3*time.Hour, 214, 12*time.Minute+41*time.Second, false),
		row("laptop-restic", model.StatusAmber, 24*time.Hour, 96, 4*time.Minute+12*time.Second, false),
		row("nas-offsite", model.StatusGreen, 25*time.Hour, 42, 96*time.Minute, true),
		row("vps-mail", model.StatusRed, 9*time.Hour, 61, 51*time.Second, false),
		{Name: "usb-archive", Status: model.StatusGrey},
	}

	var buf bytes.Buffer
	formatStatusTable(&buf, rows, docNow)
	return buf.String()
}

// docCheckOutput runs the staged report with the version and repository probes
// stubbed. The external drive is not attached, which is also why it has never
// been refreshed in the other guides.
func docCheckOutput(t *testing.T) string {
	t.Helper()
	var out, errBuf bytes.Buffer
	checkLine(&out, "config", "ok", "5 repos, 2 credentials")
	checkLine(&out, "secrets", "ok", "all credentials and repos resolved")

	code := checkRestic(t.Context(), &out, &errBuf,
		func(context.Context) (string, error) { return "0.18.1", nil },
		func(context.Context) ([]app.RepoCheck, error) {
			return []app.RepoCheck{
				{Name: "homeserver-system"},
				{Name: "laptop-restic"},
				{Name: "nas-offsite"},
				{Name: "vps-mail"},
				{Name: "usb-archive", Err: &resticx.Error{Op: "cat", Kind: resticx.KindRepoNotFound, Code: 10}},
			}, nil
		})
	if code != 1 {
		t.Fatalf("check exit code = %d, want 1", code)
	}
	// A terminal interleaves both streams; the summary is the stderr line.
	return out.String() + errBuf.String()
}

// docCacheRoot is the location the guides show. The scan runs in a temporary
// directory; only this label replaces it, so the example path does not depend on
// where the test happened to run.
const docCacheRoot = "/home/alex/.cache/resticscope/restic-cache"

// docCaches is the restic cache directory the guide describes: three caches for
// configured repositories and two left behind by repositories that were renamed
// or removed.
var docCaches = []struct {
	name string
	mib  int64
}{
	{"homeserver-system", 412},
	{"laptop-restic", 96},
	{"nas-offsite", 1204},
	{"old-laptop", 88},
	{"vps-mail-old", 37},
}

// docCachePruneOutput scans a scratch cache tree so the sizes, the orphan
// classification, and the ordering are the ones the command computes.
func docCachePruneOutput(t *testing.T) string {
	t.Helper()
	cacheDir := filepath.Join(t.TempDir(), "resticscope")
	for _, c := range docCaches {
		dir := filepath.Join(cacheDir, "restic-cache", c.name, "data")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		// Sparse: the scan sums reported sizes, so this costs no disk.
		f, err := os.Create(filepath.Join(dir, "packs"))
		if err != nil {
			t.Fatalf("create cache file: %v", err)
		}
		if err := f.Truncate(c.mib << 20); err != nil {
			t.Fatalf("size cache file: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close cache file: %v", err)
		}
	}

	a := &app.App{Cfg: &config.Config{
		Global: config.Global{CacheDir: cacheDir},
		Repos: []config.Repo{
			{Name: "homeserver-system"},
			{Name: "laptop-restic"},
			{Name: "nas-offsite"},
			{Name: "vps-mail"},
			{Name: "usb-archive"},
		},
	}}

	res, err := a.PruneCache(t.Context(), false, true)
	if err != nil {
		t.Fatalf("PruneCache: %v", err)
	}
	res.Root = docCacheRoot

	var buf bytes.Buffer
	formatPruneResult(&buf, res, true)
	return buf.String()
}

// textBlocks returns the bodies of every ```text fence in doc.
func textBlocks(doc string) []string {
	var out []string
	rest := doc
	for {
		i := strings.Index(rest, "```text\n")
		if i < 0 {
			return out
		}
		rest = rest[i+len("```text\n"):]
		j := strings.Index(rest, "```")
		if j < 0 {
			return out
		}
		out = append(out, rest[:j])
		rest = rest[j+3:]
	}
}

func readDoc(t *testing.T, name string) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}
