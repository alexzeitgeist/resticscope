package tui

// Every terminal screen printed in README.md and docs/*.md is rendered here from
// one shared fixture and compared against the markdown byte for byte, so a
// documented screen cannot drift away from what the app actually prints.
//
// All screens come from the same example repositories at the same width, which
// is what lets the guides read as one continuous session.

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/app"
	"github.com/alexzeitgeist/resticscope/internal/browsedb"
	"github.com/alexzeitgeist/resticscope/internal/config"
	"github.com/alexzeitgeist/resticscope/internal/model"

	tea "charm.land/bubbletea/v2"
)

// docWidth is the column count every documented screen is captured at.
const docWidth = 96

const gib = 1 << 30

// docNow is the wall clock the guides are written against: the newest snapshot
// of homeserver-system is three hours old.
var docNow = time.Date(2026, 5, 23, 14, 0, 0, 0, time.UTC)

// docShortIDs pins the short IDs the guides name. Older snapshots get
// synthesized IDs; they exist to make the counts real but never reach a screen.
var docShortIDs = []string{"d0e1f2a3", "c7d8e9f0", "b4c5d6e7", "a1b2c3d4"}

// A restic snapshot ID is 64 hex characters, of which the short ID is the first
// eight. These tails pad the example IDs to the length the app validates.
const (
	docIDTail   = "9f4c2d1e8b7a6053f1e2d3c4b5a69788cc17d2e34a5b6c7d8e9f0a1b"
	docTreeTail = "5d3e9a1c60b8742fe3d1a95c8b70642de1f3a08bc2d4e6f80a1b3c5d"
)

// docSnapshotID expands the i-th example snapshot's short ID to a full one.
func docSnapshotID(i int) string { return docShortID(i) + docIDTail }

func docShortID(i int) string {
	if i < len(docShortIDs) {
		return docShortIDs[i]
	}
	return fmt.Sprintf("%08x", 0x5e6f7a80+i*0x1131)
}

// docSeries describes one repository's snapshot history.
type docSeries struct {
	count  int
	newest time.Time
	host   string
	tags   []string
	size   int64         // bytes processed by the newest snapshot
	took   time.Duration // backup window of the newest snapshot
}

// docSnapshots builds a daily snapshot history, newest first. Only the newest
// few ever reach a screen; the rest exist so the counts the guides quote are
// the real length of the list being windowed.
func docSnapshots(s docSeries) []model.Snapshot {
	snaps := make([]model.Snapshot, 0, s.count)
	for i := range s.count {
		start := s.newest.AddDate(0, 0, -i)
		snap := model.Snapshot{
			ID:             docSnapshotID(i),
			ShortID:        docShortID(i),
			Time:           start,
			Hostname:       s.host,
			Username:       "root",
			UID:            uint32p(0),
			GID:            uint32p(0),
			Tags:           s.tags,
			Paths:          []string{"/"},
			Excludes:       []string{"/var/cache", "/var/tmp", "**/.cache"},
			Tree:           fmt.Sprintf("%08x", 0x7ac1b204+i*0x2f11) + docTreeTail,
			ProgramVersion: "restic 0.18.1",
			Summary: &model.SnapshotSummary{
				TotalBytesProcessed: s.size - int64(i)*gib,
				DataAdded:           int64p(4939212390),
				DataAddedPacked:     int64p(4402341478),
				BackupStart:         start,
				BackupEnd:           start.Add(s.took),
				FilesNew:            uint64p(1204),
				FilesChanged:        uint64p(863),
				FilesUnmodified:     uint64p(2839030),
				TotalFilesProcessed: uint64p(2841097),
				DirsNew:             uint64p(38),
				DirsChanged:         uint64p(291),
				DirsUnmodified:      uint64p(184622),
				DataBlobs:           int64p(21486),
				TreeBlobs:           int64p(329),
			},
		}
		if i+1 < s.count {
			snap.Parent = docSnapshotID(i + 1)
		}
		snaps = append(snaps, snap)
	}
	return snaps
}

// docApp builds the five example repositories the guides use throughout.
func docApp(t *testing.T) *app.App {
	t.Helper()

	locked := docNow.Add(-30 * time.Hour)
	cfg := &config.Config{
		Global: config.Global{
			Parallelism: 4,
			Shell:       "/bin/sh",
			GroupBy:     []string{"env", "criticality"},
		},
		Repos: []config.Repo{
			{
				Name: "homeserver-system", Credential: "hetzner-home",
				Endpoint: "https://fsn1.your-objectstorage.com", Bucket: "homeserver-backups",
				ExpectedFrequency: config.Duration(24 * time.Hour),
				Labels:            map[string]string{"criticality": "high", "env": "home"},
			},
			{
				Name: "laptop-restic", Credential: "hetzner-home",
				Endpoint: "https://fsn1.your-objectstorage.com", Bucket: "laptop-backups",
				ExpectedFrequency: config.Duration(12 * time.Hour),
				Labels:            map[string]string{"criticality": "medium", "env": "home"},
			},
			{
				Name: "nas-offsite", Credential: "hetzner-home",
				Endpoint: "https://fsn1.your-objectstorage.com", Bucket: "nas-offsite",
				ExpectedFrequency: config.Duration(48 * time.Hour),
				Labels:            map[string]string{"criticality": "high", "env": "home"},
			},
			{
				Name: "vps-mail", Credential: "hetzner-vps",
				Endpoint: "https://nbg1.your-objectstorage.com", Bucket: "vps-mail",
				ExpectedFrequency: config.Duration(6 * time.Hour),
				Labels:            map[string]string{"criticality": "high", "env": "vps"},
			},
			{
				Name: "usb-archive", URL: "/mnt/usb/restic-archive",
				ExpectedFrequency: config.Duration(720 * time.Hour),
				Labels:            map[string]string{"criticality": "low", "env": "home"},
			},
		},
	}
	cfg.Global.StaleAfter = config.Duration(10 * time.Minute)
	cfg.Global.StaleGrace = config.Duration(12 * time.Hour)
	cfg.Global.LockMaxAge = config.Duration(2 * time.Hour)
	cfg.Global.ShellPasswordMode = "env"

	series := map[string]docSeries{
		"homeserver-system": {214, docNow.Add(-3 * time.Hour), "homeserver", []string{"system", "daily"}, 414 * gib, 12*time.Minute + 41*time.Second},
		"laptop-restic":     {96, docNow.Add(-24 * time.Hour), "laptop", []string{"home"}, 212 * gib, 4*time.Minute + 12*time.Second},
		"nas-offsite":       {42, docNow.Add(-25 * time.Hour), "nas", []string{"offsite"}, 7100 * gib, 96 * time.Minute},
		"vps-mail":          {61, docNow.Add(-9 * time.Hour), "vps", []string{"mail"}, 18 * gib, 51 * time.Second},
	}
	states := make(map[string]model.RepoState, len(series))
	for name, s := range series {
		states[name] = model.RepoState{
			Name: name, RefreshedAt: docNow,
			LastSnapshot: s.newest, SnapshotCount: s.count,
			Hosts: []string{s.host}, Tags: s.tags,
			Snapshots: docSnapshots(s),
		}
	}
	mail := states["vps-mail"]
	mail.LockedSince = &locked
	states["vps-mail"] = mail

	return &app.App{
		Cfg:     cfg,
		Cache:   stubCache{states: states},
		Clock:   fixedClock{docNow},
		Secrets: stubSecrets{},
		Restic:  stubRestic{},
	}
}

// docTree is the example snapshot's file tree, in the order `restic ls`
// emits it: every directory before its children. Directory sizes are never
// written here — the index folds them from the files below, exactly as it does
// for a real snapshot.
var docTree = []docNode{
	{"/boot", "drwxr-xr-x", 0, 0, 0, "2026-01-18 14:00"},
	{"/boot/System.map-6.8.0-45-generic", "-rw-------", 9_331_200, 0, 0, "2026-01-18 14:00"},
	{"/boot/config-6.8.0-45-generic", "-rw-r--r--", 293_888, 0, 0, "2026-01-18 14:00"},
	{"/boot/initrd.img-6.8.0-45-generic", "-rw-r--r--", 148_897_792, 0, 0, "2026-01-18 14:02"},
	{"/boot/vmlinuz-6.8.0-45-generic", "-rw-r--r--", 14_680_064, 0, 0, "2026-01-18 14:00"},
	{"/boot/initrd.img-6.8.0-44-generic", "-rw-r--r--", 148_635_648, 0, 0, "2025-12-04 09:31"},
	{"/boot/vmlinuz-6.8.0-44-generic", "-rw-r--r--", 14_667_776, 0, 0, "2025-12-04 09:31"},
	{"/boot/grub", "drwxr-xr-x", 0, 0, 0, "2026-01-18 14:03"},
	{"/boot/grub/grub.cfg", "-r--r--r--", 12_288, 0, 0, "2026-01-18 14:03"},
	{"/boot/grub/grubenv", "-rw-r--r--", 1_024, 0, 0, "2026-05-23 10:47"},
	{"/boot/grub/fonts", "drwxr-xr-x", 0, 0, 0, "2025-08-11 07:12"},
	{"/boot/grub/fonts/unicode.pf2", "-rw-r--r--", 2_509_824, 0, 0, "2025-08-11 07:12"},

	{"/etc", "drwxr-xr-x", 0, 0, 0, "2026-05-21 11:00"},
	{"/etc/fstab", "-rw-r--r--", 1_306, 0, 0, "2025-03-02 21:14"},
	{"/etc/group", "-rw-r--r--", 1_144, 0, 0, "2026-04-02 08:19"},
	{"/etc/hostname", "-rw-r--r--", 11, 0, 0, "2025-03-02 21:09"},
	{"/etc/hosts", "-rw-r--r--", 221, 0, 0, "2026-05-21 11:00"},
	{"/etc/passwd", "-rw-r--r--", 2_847, 0, 0, "2026-04-02 08:19"},
	{"/etc/passwd-", "-rw-------", 2_781, 0, 0, "2026-04-02 08:19"},
	{"/etc/shadow", "-rw-r-----", 1_612, 0, 42, "2026-04-02 08:19"},
	{"/etc/nginx", "drwxr-xr-x", 0, 0, 0, "2026-02-27 19:05"},
	{"/etc/nginx/nginx.conf", "-rw-r--r--", 1_538, 0, 0, "2026-02-27 19:05"},
	{"/etc/nginx/sites-enabled", "drwxr-xr-x", 0, 0, 0, "2026-02-27 19:05"},
	{"/etc/nginx/sites-enabled/default", "-rw-r--r--", 2_412, 0, 0, "2026-02-27 19:05"},
	{"/etc/pam.d", "drwxr-xr-x", 0, 0, 0, "2025-11-19 06:44"},
	{"/etc/pam.d/passwd", "-rw-r--r--", 92, 0, 0, "2025-11-19 06:44"},
	{"/etc/pam.d/sshd", "-rw-r--r--", 2_133, 0, 0, "2025-11-19 06:44"},
	{"/etc/ssh", "drwxr-xr-x", 0, 0, 0, "2026-01-09 12:38"},
	{"/etc/ssh/ssh_host_ed25519_key", "-rw-------", 411, 0, 0, "2025-03-02 21:11"},
	{"/etc/ssh/sshd_config", "-rw-r--r--", 3_298, 0, 0, "2026-01-09 12:38"},
	{"/etc/ssl", "drwxr-xr-x", 0, 0, 0, "2026-03-14 05:22"},
	{"/etc/ssl/certs", "drwxr-xr-x", 0, 0, 0, "2026-03-14 05:22"},
	{"/etc/ssl/certs/ca-certificates.crt", "-rw-r--r--", 219_136, 0, 0, "2026-03-14 05:22"},
	{"/etc/systemd", "drwxr-xr-x", 0, 0, 0, "2025-09-30 17:51"},
	{"/etc/systemd/system", "drwxr-xr-x", 0, 0, 0, "2025-09-30 17:51"},
	{"/etc/systemd/system/restic-backup.service", "-rw-r--r--", 412, 0, 0, "2025-09-30 17:51"},
	{"/etc/systemd/system/restic-backup.timer", "-rw-r--r--", 198, 0, 0, "2025-09-30 17:51"},

	{"/home", "drwxr-xr-x", 0, 0, 0, "2026-04-16 02:00"},
	{"/home/alex", "drwxr-x---", 0, 1000, 1000, "2026-04-16 02:00"},
	{"/home/alex/.ssh", "drwx------", 0, 1000, 1000, "2025-07-21 20:03"},
	{"/home/alex/.ssh/authorized_keys", "-rw-------", 1_186, 1000, 1000, "2025-07-21 20:03"},
	{"/home/alex/.ssh/config", "-rw-------", 842, 1000, 1000, "2026-01-27 11:45"},
	{"/home/alex/Photos", "drwxr-xr-x", 0, 1000, 1000, "2026-04-16 02:00"},
	{"/home/alex/Photos/library.db", "-rw-r--r--", 2_147_483_648, 1000, 1000, "2026-04-16 02:00"},
	{"/home/alex/Photos/2025", "drwxr-xr-x", 0, 1000, 1000, "2026-01-02 23:14"},
	{"/home/alex/Photos/2025/originals.tar", "-rw-r--r--", 31_138_512_896, 1000, 1000, "2026-01-02 23:14"},
	{"/home/alex/vm", "drwxr-xr-x", 0, 1000, 1000, "2026-03-28 18:40"},
	{"/home/alex/vm/debian-13.qcow2", "-rw-------", 68_719_476_736, 1000, 1000, "2026-03-28 18:40"},

	{"/root", "drwx------", 0, 0, 0, "2026-05-22 11:00"},
	{"/root/.bashrc", "-rw-r--r--", 3_526, 0, 0, "2025-03-02 21:09"},
	{"/root/.ssh", "drwx------", 0, 0, 0, "2025-03-02 21:12"},
	{"/root/.ssh/authorized_keys", "-rw-------", 1_186, 0, 0, "2025-03-02 21:12"},
	{"/root/backup-report.log", "-rw-r--r--", 9_338_880, 0, 0, "2026-05-22 11:00"},

	{"/srv", "drwxr-xr-x", 0, 0, 0, "2026-04-03 14:00"},
	{"/srv/media", "drwxr-xr-x", 0, 1000, 1000, "2026-04-03 14:00"},
	{"/srv/media/movies", "drwxr-xr-x", 0, 1000, 1000, "2026-04-03 14:00"},
	{"/srv/media/movies/archive-2024.mkv", "-rw-r--r--", 96_636_764_160, 1000, 1000, "2025-02-11 22:07"},
	{"/srv/media/movies/archive-2025.mkv", "-rw-r--r--", 118_111_600_640, 1000, 1000, "2026-04-03 14:00"},
	{"/srv/nextcloud", "drwxr-xr-x", 0, 33, 33, "2026-05-20 03:15"},
	{"/srv/nextcloud/data", "drwxr-x---", 0, 33, 33, "2026-05-20 03:15"},
	{"/srv/nextcloud/data/nextcloud.db", "-rw-r-----", 4_294_967_296, 33, 33, "2026-05-20 03:15"},
	{"/srv/nextcloud/data/files.tar", "-rw-r-----", 70_866_960_384, 33, 33, "2026-05-20 03:15"},

	{"/var", "drwxr-xr-x", 0, 0, 0, "2026-05-23 10:00"},
	{"/var/backups", "drwxr-xr-x", 0, 0, 0, "2026-05-23 06:25"},
	{"/var/backups/group.bak", "-rw-r--r--", 1_144, 0, 0, "2026-04-02 08:19"},
	{"/var/backups/passwd.bak", "-rw-r--r--", 2_847, 0, 0, "2026-04-02 08:19"},
	{"/var/lib", "drwxr-xr-x", 0, 0, 0, "2026-05-23 10:00"},
	{"/var/lib/docker", "drwx--x---", 0, 0, 0, "2026-05-23 10:00"},
	{"/var/lib/docker/overlay2.tar", "-rw-------", 24_696_061_952, 0, 0, "2026-05-23 10:00"},
	{"/var/log", "drwxr-xr-x", 0, 0, 4, "2026-05-23 09:17"},
	{"/var/log/journal", "drwxr-sr-x", 0, 0, 4, "2026-05-23 09:17"},
	{"/var/log/journal/system.journal", "-rw-r-----", 8_388_608, 0, 4, "2026-05-23 09:17"},
	{"/var/log/syslog", "-rw-r-----", 39_845_888, 0, 4, "2026-05-23 09:17"},
}

// docNode is one entry of docTree. A mode starting with d makes it a directory.
type docNode struct {
	path     string
	mode     string
	size     int64
	uid, gid uint32
	mtime    string
}

func docNodes(t *testing.T) []model.BrowseNode {
	t.Helper()
	nodes := make([]model.BrowseNode, 0, len(docTree))
	for _, f := range docTree {
		mt, err := time.ParseInLocation("2006-01-02 15:04", f.mtime, time.UTC)
		if err != nil {
			t.Fatalf("docTree %s: %v", f.path, err)
		}
		isDir := strings.HasPrefix(f.mode, "d")
		typ := model.NodeTypeFile
		if isDir {
			typ = model.NodeTypeDir
		}
		nodes = append(nodes, model.BrowseNode{
			Path: f.path, Name: path.Base(f.path), Type: typ, IsDir: isDir,
			Size: f.size, ModTime: mt, Permissions: f.mode,
			UID: f.uid, GID: f.gid, OwnerKnown: true,
		})
	}
	return nodes
}

// docBrowseStore adapts browsedb to app.BrowseStore. The guides' browse screens
// go through the real index, so the directory sizes and search ranking they
// show are the ones the app computes, not numbers chosen by hand.
type docBrowseStore struct{ db *browsedb.DB }

func (s *docBrowseStore) IsIndexed(ctx context.Context, repo, snap string) (bool, error) {
	return s.db.IsIndexed(ctx, repo, snap)
}

func (s *docBrowseStore) BeginIndex(ctx context.Context, repo, snap string) (app.IndexWriter, error) {
	tx, err := s.db.BeginIndex(ctx, repo, snap)
	if err != nil {
		return nil, err
	}
	return tx, nil
}

func (s *docBrowseStore) ListDir(ctx context.Context, repo, snap, dir string) ([]model.BrowseEntry, error) {
	return s.db.ListDir(ctx, repo, snap, dir)
}

func (s *docBrowseStore) SubtreeCounts(ctx context.Context, repo, snap, dir string) (int, int, bool, error) {
	return s.db.SubtreeCounts(ctx, repo, snap, dir)
}

func (s *docBrowseStore) Search(ctx context.Context, repo, snap, query string, limit int) (model.BrowseSearchResult, error) {
	return s.db.Search(ctx, repo, snap, query, limit)
}

func (s *docBrowseStore) Close() error { return s.db.Close() }

// docPasswdVersions is what `restic find /etc/passwd` returns for the example
// repository: the file was last edited on 2 April, so every snapshot since then
// holds one version and the snapshots before it hold the previous one.
func docPasswdVersions() []model.FindSnapshotResult {
	match := func(size int64, mtime string) model.FindMatch {
		mt, _ := time.ParseInLocation("2006-01-02 15:04", mtime, time.UTC)
		return model.FindMatch{
			Path: "/etc/passwd", Name: "passwd", Type: model.NodeTypeFile,
			Size: size, ModTime: mt, Permissions: "-rw-r--r--",
			UID: uint32p(0), GID: uint32p(0),
		}
	}

	var out []model.FindSnapshotResult
	for i := range 51 {
		m := match(2_847, "2026-04-02 08:19")
		if i >= 3 {
			m = match(2_781, "2026-01-27 09:41")
		}
		out = append(out, model.FindSnapshotResult{SnapshotID: docSnapshotID(i), Hits: 1, Matches: []model.FindMatch{m}})
	}
	return out
}

// docDiffNDJSON is what `restic diff --json` reports between the 22 May and
// 23 May snapshots. Keeping it on the wire format means the guides' markers come
// from the same parser that reads restic.
const docDiffNDJSON = `
{"message_type":"change","path":"/etc/nginx/nginx.conf","modifier":"M"}
{"message_type":"change","path":"/etc/nginx/sites-enabled/old-blog","modifier":"-"}
{"message_type":"change","path":"/etc/nginx/sites-enabled/wiki","modifier":"+"}
{"message_type":"change","path":"/etc/passwd","modifier":"U"}
{"message_type":"change","path":"/etc/resolv.conf","modifier":"T"}
{"message_type":"change","path":"/etc/ssl/certs/ca-certificates.crt","modifier":"M"}
{"message_type":"change","path":"/home/alex/.ssh/config","modifier":"U"}
{"message_type":"change","path":"/home/alex/vm/debian-13.qcow2","modifier":"M"}
{"message_type":"change","path":"/root/backup-report.log","modifier":"M"}
{"message_type":"change","path":"/srv/nextcloud/data/files.tar","modifier":"M"}
{"message_type":"change","path":"/srv/nextcloud/data/nextcloud.db","modifier":"M"}
{"message_type":"change","path":"/var/backups/passwd.bak","modifier":"+"}
{"message_type":"change","path":"/var/lib/docker/overlay2.tar","modifier":"M"}
{"message_type":"change","path":"/var/log/journal/system.journal","modifier":"M"}
{"message_type":"change","path":"/var/log/syslog","modifier":"M"}
`

func docDiffEntries(t *testing.T) []model.DiffEntry {
	t.Helper()
	d, err := model.ParseDiffNDJSON([]byte(docDiffNDJSON))
	if err != nil {
		t.Fatalf("parse doc diff: %v", err)
	}
	if d.ParseErrors > 0 {
		t.Fatalf("doc diff has %d malformed lines", d.ParseErrors)
	}
	return d.Entries
}

// docDiffApp marks the two newest snapshots and compares them.
func docDiffApp(t *testing.T) *app.App {
	t.Helper()
	a := docApp(t)
	a.Cfg.Diff = config.Diff{Timeout: config.Duration(10 * time.Minute)}
	a.Restic = stubRestic{diffEntries: docDiffEntries(t)}
	return a
}

// docOpenDiff marks the newest two snapshots and opens their comparison.
func docOpenDiff(t *testing.T, m Model) Model {
	t.Helper()
	m = update(t, m, press("enter"))
	m = update(t, m, press("t"))
	m = update(t, m, press("j"))
	m = update(t, m, press("t"))
	next, cmd := m.Update(press("d"))
	return drivePastDiff(t, next.(Model), cmd)
}

// docExtractRoot is the target_root the guides use. It is never written to: the
// review screen only plans paths, and the one screen that shows a finished
// extract is driven by a stub in place of restic.
const docExtractRoot = "/home/alex/resticscope-extracts"

// docBrowseApp adds the example tree to the fixture, reachable with `b`.
func docBrowseApp(t *testing.T) *app.App {
	t.Helper()
	a := docApp(t)
	a.Cfg.Browse = config.Browse{IndexTimeout: config.Duration(10 * time.Minute)}
	a.Cfg.Extract = config.Extract{
		TargetRoot:     docExtractRoot,
		ExtractTimeout: config.Duration(30 * time.Minute),
		RememberTarget: true,
		UnsafeSymlinks: "keep",
	}
	a.Restic = stubRestic{browseNodes: docNodes(t), findResults: docPasswdVersions()}

	dir := t.TempDir()
	a.Browse = app.NewBrowseSession(func(ctx context.Context) (app.BrowseStore, error) {
		db, err := browsedb.OpenContext(ctx, filepath.Join(dir, "db.sqlite"), make([]byte, 32), 0)
		if err != nil {
			return nil, err
		}
		return &docBrowseStore{db: db}, nil
	})
	t.Cleanup(func() { _ = a.Browse.Close() })
	return a
}

// docModel builds the root model over the documented fixture at the given
// height. Grouping starts flat so the list screen matches the guides.
func docModel(t *testing.T, a *app.App, height int) Model {
	t.Helper()
	rows, err := a.Statuses(t.Context())
	if err != nil {
		t.Fatalf("Statuses: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	m := newModel(ctx, cancel, a, rows, "0.18.1")
	m.groupIndex = 0
	return update(t, m, tea.WindowSizeMsg{Width: docWidth, Height: height})
}

// capture renders the model and reduces it to what a guide shows: no styling, no
// trailing blanks, and the padding that pins the footer to the bottom of a real
// terminal collapsed to the single blank line the app always leaves there.
func capture(m Model) string {
	lines := strings.Split(stripANSI(m.View().Content), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	// Collapse the blank run directly above the footer, which is the last
	// footerRows lines and may be a prompt plus its key bar.
	if foot := m.footerRows(); len(lines) > foot {
		body, footer := lines[:len(lines)-foot], lines[len(lines)-foot:]
		for len(body) > 1 && body[len(body)-1] == "" && body[len(body)-2] == "" {
			body = body[:len(body)-1]
		}
		trimmed := make([]string, 0, len(body)+len(footer))
		trimmed = append(trimmed, body...)
		trimmed = append(trimmed, footer...)
		lines = trimmed
	}
	return strings.Join(lines, "\n") + "\n"
}

// docScreen names one captured screen and the guide that must contain it. The
// height is how much of the screen the guide shows; the width is always docWidth.
type docScreen struct {
	name   string
	doc    string
	height int
	build  func(t *testing.T, h int) Model
}

// docScreens lists every screen in the guides, in the order a reader meets it.
func docScreens() []docScreen {
	return []docScreen{
		{"repository list", "README.md", 11, func(t *testing.T, h int) Model {
			t.Helper()
			return docModel(t, docApp(t), h)
		}},
		{"help overlay", "README.md", 20, func(t *testing.T, h int) Model {
			t.Helper()
			return update(t, docModel(t, docApp(t), h), press("?"))
		}},
		{"grouped repository list", "docs/configuration.md", 16, func(t *testing.T, h int) Model {
			t.Helper()
			return update(t, docModel(t, docApp(t), h), press("g"))
		}},
		{"snapshot list", "docs/browsing.md", 25, func(t *testing.T, h int) Model {
			t.Helper()
			return update(t, docModel(t, docApp(t), h), press("enter"))
		}},
		{"snapshot info", "docs/browsing.md", 22, func(t *testing.T, h int) Model {
			t.Helper()
			m := update(t, docModel(t, docApp(t), h), press("enter"))
			return update(t, m, press("i"))
		}},
		{"file browser", "docs/browsing.md", 16, func(t *testing.T, h int) Model {
			t.Helper()
			return openBrowse(t, docModel(t, docBrowseApp(t), h))
		}},
		{"filename search", "docs/browsing.md", 14, func(t *testing.T, h int) Model {
			t.Helper()
			m := openBrowse(t, docModel(t, docBrowseApp(t), h))
			return typeSearch(t, openSearch(t, m), "passwd")
		}},
		{"versions", "docs/browsing.md", 12, func(t *testing.T, h int) Model {
			t.Helper()
			m := docOpenEtc(t, docModel(t, docBrowseApp(t), h))
			m = docSelectBrowseRow(t, m, "passwd")
			next, cmd := m.Update(press("v"))
			return drivePastFind(t, next.(Model), cmd)
		}},
		{"snapshot diff", "docs/browsing.md", 14, func(t *testing.T, h int) Model {
			t.Helper()
			return docOpenDiff(t, docModel(t, docDiffApp(t), h))
		}},
		{"extract review", "docs/extract.md", 14, func(t *testing.T, h int) Model {
			t.Helper()
			m := docOpenEtc(t, docModel(t, docBrowseApp(t), h))
			return docOpenExtract(t, m, "nginx")
		}},
		{"extract done", "docs/extract.md", 14, docExtractDone},
	}
}

// docExtractDone runs the as-root extract of /etc/nginx to completion. Restic is
// swapped for a stub at the last moment, so the finished screen comes out of the
// real state machine without anything being written to disk.
func docExtractDone(t *testing.T, h int) Model {
	t.Helper()
	m := docOpenEtc(t, docModel(t, docBrowseApp(t), h))
	m = docOpenExtract(t, m, "nginx")
	m = update(t, m, press("p"))

	drv := &fakeExtractDriver{}
	drv.push(extractResp{result: app.ExtractResult{
		Files: 2, Dirs: 2, Bytes: 3950, Elapsed: 1200 * time.Millisecond,
		FinalDir:  m.extract.final,
		FinalPath: m.extract.final,
	}})
	m.extract.drv = drv

	// enter on an as-root review probes sudo first and runs second, so feed both
	// results back rather than assuming a single round trip.
	next, cmd := m.Update(press("enter"))
	m = next.(Model)
	for range 4 {
		if cmd == nil || m.extract.state == extractStateSuccess {
			break
		}
		var follow tea.Cmd
		for _, msg := range runBatchLeaves(t, cmd) {
			var c tea.Cmd
			if m, c = docUpdate(m, msg); c != nil {
				follow = c
			}
		}
		cmd = follow
	}
	if m.extract.state != extractStateSuccess {
		t.Fatalf("extract state = %v, want success", m.extract.state)
	}
	return m
}

// docOpenExtract selects an entry in the current listing and opens its extract
// review, including the indexed subtree counts.
func docOpenExtract(t *testing.T, m Model, name string) Model {
	t.Helper()
	m = docSelectBrowseRow(t, m, name)
	next, cmd := m.Update(press("e"))
	m = next.(Model)
	if m.view != extractView {
		t.Fatalf("e should open the extract review, view = %d, notice = %q", m.view, m.browseNotice)
	}
	for _, leaf := range leafCmds(t, cmd) {
		if msg, ok := leaf().(extractCountsMsg); ok {
			m = update(t, m, msg)
		}
	}
	return m
}

// docUpdate delivers one message and keeps the follow-up command.
func docUpdate(m Model, msg tea.Msg) (Model, tea.Cmd) {
	next, cmd := m.Update(msg)
	return next.(Model), cmd
}

// docOpenEtc opens the browser and steps into /etc, where several guides
// continue.
func docOpenEtc(t *testing.T, m Model) Model {
	t.Helper()
	m = docSelectBrowseRow(t, openBrowse(t, m), "etc")
	return pressBrowse(t, m, "enter")
}

// docSelectBrowseRow puts the browse cursor on a named entry.
func docSelectBrowseRow(t *testing.T, m Model, name string) Model {
	t.Helper()
	for i, r := range m.browseRows {
		if r.Name == name {
			m.browseCursor = i
			return m
		}
	}
	t.Fatalf("no %q in the current listing", name)
	return m
}

func TestDocScreensMatchMarkdown(t *testing.T) {
	root := repoRoot(t)
	for _, sc := range docScreens() {
		t.Run(sc.name, func(t *testing.T) {
			got := capture(sc.build(t, sc.height))
			doc := readDoc(t, root, sc.doc)
			if !strings.Contains(doc, "```text\n"+got+"```") {
				t.Errorf("%s does not contain the %s screen as rendered.\n"+
					"Paste this block into %s:\n\n```text\n%s```\n\nClosest block in the file:\n\n%s",
					sc.doc, sc.name, sc.doc, got, closestBlock(doc, got))
			}
		})
	}
}

// docFiles are every page a screen may appear on.
var docFiles = []string{
	"README.md",
	"docs/browsing.md",
	"docs/cli.md",
	"docs/configuration.md",
	"docs/extract.md",
	"docs/secrets.md",
	"docs/themes.md",
}

// TestNoUnverifiedScreensInDocs is the other half of the guarantee: not just
// that every screen the harness renders is in the guides, but that every screen
// in the guides was rendered. A block written by hand, or left behind after the
// app changed, fails here.
func TestNoUnverifiedScreensInDocs(t *testing.T) {
	root := repoRoot(t)

	rendered := make(map[string]bool)
	for _, sc := range docScreens() {
		rendered[capture(sc.build(t, sc.height))] = true
	}

	for _, name := range docFiles {
		for _, block := range textBlocks(readDoc(t, root, name)) {
			if !isTUIScreen(block) || rendered[block] {
				continue
			}
			t.Errorf("%s contains a terminal screen that no test renders:\n\n%s\n"+
				"Add it to docScreens, or delete it: a screen nothing renders cannot be kept honest.",
				name, block)
		}
	}
}

// isTUIScreen recognizes a block by the app's own chrome: the persistent help
// chip, or the footer key bar the frame pins to the last row.
func isTUIScreen(block string) bool {
	lines := strings.Split(strings.TrimRight(block, "\n"), "\n")
	if len(lines) == 0 {
		return false
	}
	if strings.HasSuffix(lines[0], "? help") {
		return true
	}
	return strings.Contains(lines[len(lines)-1], " • ")
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	return root
}

func readDoc(t *testing.T, root, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// closestBlock returns the fenced text block sharing the most leading lines with
// want, so a drift failure shows the block that needs updating.
func closestBlock(doc, want string) string {
	wantLines := strings.Split(want, "\n")
	best, bestScore := "", -1
	for _, b := range textBlocks(doc) {
		score := 0
		got := strings.Split(b, "\n")
		for i := range min(len(got), len(wantLines)) {
			if got[i] != wantLines[i] {
				break
			}
			score++
		}
		if score > bestScore {
			best, bestScore = b, score
		}
	}
	if bestScore <= 0 {
		return "(no similar block)"
	}
	return best
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
