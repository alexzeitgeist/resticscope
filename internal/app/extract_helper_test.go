package app

// extract_helper_test.go covers the root-side helper runtime without root and
// without a real restic: opts.Euid is injected (the gate is tested, not the
// kernel), OwnerUID/GID stay -1 so every chown is disabled (the ownership
// hand-off itself is root-only behavior, covered by the manual test note in
// docs/extract-privileged.md), and fakeRestic materializes the staging tree
// the way the real restore would.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"resticscope/internal/resticx"
)

// helperPayloadFor builds a valid stdin payload for req against root.
func helperPayloadFor(t *testing.T, req ExtractRequest, root string) []byte {
	t.Helper()
	req.TargetRoot = root
	b, err := json.Marshal(helperPayload{
		Version:        helperPayloadVersion,
		Target:         resticx.Target{Name: "repo-a"},
		Creds:          resticx.Creds{Env: map[string]string{"AWS_ACCESS_KEY_ID": "AK", "AWS_SECRET_ACCESS_KEY": "SK"}, ResticPassword: "pw"},
		Request:        req,
		TimeoutSeconds: 120,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// openStdin models the production stdin: the payload followed by an OPEN pipe
// (the parent holds its end for the lifetime of the run — EOF means cancel, so
// a plain bytes.Reader would cancel the pipeline the moment it is decoded).
func openStdin(t *testing.T, payload []byte) io.Reader {
	t.Helper()
	pr, pw := io.Pipe()
	go func() { _, _ = pw.Write(payload) }()
	t.Cleanup(func() { _ = pw.Close() })
	return pr
}

// decodeHelperEvents splits the helper's NDJSON stdout into typed events.
func decodeHelperEvents(t *testing.T, out []byte) []helperEvent {
	t.Helper()
	var evs []helperEvent
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var ev helperEvent
		if err := dec.Decode(&ev); err != nil {
			if errors.Is(err, io.EOF) {
				return evs
			}
			t.Fatalf("decode helper event: %v", err)
		}
		evs = append(evs, ev)
	}
}

func helperOpts(drv extractTreeDriver) ExtractHelperOpts {
	return ExtractHelperOpts{Euid: 0, OwnerUID: -1, OwnerGID: -1, Clock: fixedClock{now}, Restic: drv}
}

func TestRunExtractHelperTreeHappyPath(t *testing.T) {
	root := t.TempDir()
	fake := fakeRestic{
		extractTreeEvents: []resticx.ExtractTreeEvent{
			{Kind: resticx.ExtractTreeStatus, BytesRestored: 10, TotalBytes: 20, FilesRestored: 1, TotalFiles: 2},
			{Kind: resticx.ExtractTreeSummary, FilesRestored: 2, BytesRestored: 20},
		},
		extractTreeSetup: dirStagingSetup("nginx", func(node string) error {
			return os.WriteFile(filepath.Join(node, "nginx.conf"), []byte("conf"), 0o644)
		}),
		extractCap: &extractCapture{},
	}

	var out bytes.Buffer
	err := RunExtractHelper(context.Background(), openStdin(t, helperPayloadFor(t, treeReq(), root)), &out, helperOpts(fake))
	if err != nil {
		t.Fatalf("RunExtractHelper: %v", err)
	}

	// No cache dir in the payload (and an unknown invoker): the restore must
	// fall back to --no-cache — a root restic may not touch a cache it cannot
	// hand back to the user.
	if !fake.extractCap.treeParams.NoCache {
		t.Error("helper restore params: NoCache = false, want true")
	}

	evs := decodeHelperEvents(t, out.Bytes())
	if len(evs) < 2 {
		t.Fatalf("got %d events, want progress + result", len(evs))
	}
	if evs[0].Kind != helperEventProgress || evs[0].Progress == nil || evs[0].Progress.BytesDone != 10 {
		t.Errorf("first event = %+v, want progress with BytesDone=10", evs[0])
	}
	last := evs[len(evs)-1]
	if last.Kind != helperEventResult || last.Result == nil {
		t.Fatalf("last event = %+v, want result", last)
	}
	wantFinal := filepath.Join(root, "repo-a", "abcd1234", "etc", "nginx")
	if last.Result.FinalPath != wantFinal || last.Result.FinalDir != wantFinal {
		t.Errorf("result final = %q / %q, want %q", last.Result.FinalPath, last.Result.FinalDir, wantFinal)
	}
	if last.Result.Files != 1 || last.Result.Dirs != 1 {
		t.Errorf("result counts = %d files / %d dirs, want 1 / 1 (normalizer split)", last.Result.Files, last.Result.Dirs)
	}
	if _, err := os.Stat(filepath.Join(wantFinal, "nginx.conf")); err != nil {
		t.Errorf("published file missing: %v", err)
	}
	// Staging container is removed after a clean publish.
	if _, err := os.Lstat(last.Result.StagingDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("staging dir still present after publish: %v", err)
	}
}

// cachePayloadFor is helperPayloadFor plus a cache root, for the shared-cache
// tests.
func cachePayloadFor(t *testing.T, req ExtractRequest, root, cacheRoot string) []byte {
	t.Helper()
	req.TargetRoot = root
	b, err := json.Marshal(helperPayload{
		Version:        helperPayloadVersion,
		Target:         resticx.Target{Name: "repo-a"},
		Creds:          resticx.Creds{Env: map[string]string{"AWS_ACCESS_KEY_ID": "AK", "AWS_SECRET_ACCESS_KEY": "SK"}, ResticPassword: "pw"},
		Request:        req,
		CacheDir:       cacheRoot,
		TimeoutSeconds: 120,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRunExtractHelperSharedCache(t *testing.T) {
	root, cacheRoot := t.TempDir(), t.TempDir()
	fake := fakeRestic{
		extractTreeEvents: []resticx.ExtractTreeEvent{
			{Kind: resticx.ExtractTreeSummary, FilesRestored: 1, BytesRestored: 4},
		},
		extractTreeSetup: dirStagingSetup("nginx", nil),
		extractCap:       &extractCapture{},
	}
	// A known invoker (ourselves — chown to one's own uid/gid needs no root)
	// plus a cache root in the payload enables the shared cache.
	opts := ExtractHelperOpts{Euid: 0, OwnerUID: os.Getuid(), OwnerGID: os.Getgid(), Clock: fixedClock{now}, Restic: fake}

	var out bytes.Buffer
	err := RunExtractHelper(context.Background(), openStdin(t, cachePayloadFor(t, treeReq(), root, cacheRoot)), &out, opts)
	if err != nil {
		t.Fatalf("RunExtractHelper: %v", err)
	}
	if fake.extractCap.treeParams.NoCache {
		t.Error("helper restore params: NoCache = true, want false (shared cache)")
	}
	// The per-repo cache dir chain is pre-created (user-owned in production).
	if info, err := os.Stat(resticx.RepoCacheDir(cacheRoot, "repo-a")); err != nil || !info.IsDir() {
		t.Errorf("per-repo cache dir not pre-created: %v", err)
	}
}

func TestRunExtractHelperUnknownInvokerForcesNoCache(t *testing.T) {
	root, cacheRoot := t.TempDir(), t.TempDir()
	fake := fakeRestic{
		extractTreeEvents: []resticx.ExtractTreeEvent{
			{Kind: resticx.ExtractTreeSummary, FilesRestored: 1, BytesRestored: 4},
		},
		extractTreeSetup: dirStagingSetup("nginx", nil),
		extractCap:       &extractCapture{},
	}
	// helperOpts leaves OwnerUID/GID at -1: with nobody to hand root-written
	// cache entries back to, the cache root in the payload must be ignored.
	var out bytes.Buffer
	err := RunExtractHelper(context.Background(), openStdin(t, cachePayloadFor(t, treeReq(), root, cacheRoot)), &out, helperOpts(fake))
	if err != nil {
		t.Fatalf("RunExtractHelper: %v", err)
	}
	if !fake.extractCap.treeParams.NoCache {
		t.Error("helper restore params: NoCache = false, want true (unknown invoker)")
	}
	if _, err := os.Stat(resticx.RepoCacheDir(cacheRoot, "repo-a")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("per-repo cache dir should not be created for an unknown invoker: %v", err)
	}
}

// TestRunExtractHelperCachePathNotADir covers the prepareHelperCache gate: a
// pre-existing file or symlink where the per-repo cache dir belongs must
// disable the shared cache (fall back to --no-cache), never be handed to a
// root-run restic or the recursive chown-back.
func TestRunExtractHelperCachePathNotADir(t *testing.T) {
	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, repoCache string)
	}{
		{"file", func(t *testing.T, repoCache string) {
			if err := os.WriteFile(repoCache, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink", func(t *testing.T, repoCache string) {
			if err := os.Symlink(t.TempDir(), repoCache); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, cacheRoot := t.TempDir(), t.TempDir()
			repoCache := resticx.RepoCacheDir(cacheRoot, "repo-a")
			if err := os.MkdirAll(filepath.Dir(repoCache), 0o700); err != nil {
				t.Fatal(err)
			}
			tc.plant(t, repoCache)

			fake := fakeRestic{
				extractTreeEvents: []resticx.ExtractTreeEvent{
					{Kind: resticx.ExtractTreeSummary, FilesRestored: 1, BytesRestored: 4},
				},
				extractTreeSetup: dirStagingSetup("nginx", nil),
				extractCap:       &extractCapture{},
			}
			opts := ExtractHelperOpts{Euid: 0, OwnerUID: os.Getuid(), OwnerGID: os.Getgid(), Clock: fixedClock{now}, Restic: fake}

			var out bytes.Buffer
			err := RunExtractHelper(context.Background(), openStdin(t, cachePayloadFor(t, treeReq(), root, cacheRoot)), &out, opts)
			if err != nil {
				t.Fatalf("RunExtractHelper: %v", err)
			}
			if !fake.extractCap.treeParams.NoCache {
				t.Errorf("helper restore params: NoCache = false, want true (%s at cache path)", tc.name)
			}
		})
	}
}

func TestRunExtractHelperRefusesNonRoot(t *testing.T) {
	opts := helperOpts(fakeRestic{})
	opts.Euid = 1000
	var out bytes.Buffer
	err := RunExtractHelper(context.Background(), bytes.NewReader(helperPayloadFor(t, treeReq(), t.TempDir())), &out, opts)
	if err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("err = %v, want a must-run-as-root refusal", err)
	}
	evs := decodeHelperEvents(t, out.Bytes())
	if len(evs) != 1 || evs[0].Kind != helperEventError || evs[0].Error.Code != helperCodeGeneric {
		t.Fatalf("events = %+v, want one generic error event", evs)
	}
}

func TestRunExtractHelperRefusesVersionMismatch(t *testing.T) {
	b, err := json.Marshal(helperPayload{Version: helperPayloadVersion + 1, Request: treeReq()})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	rerr := RunExtractHelper(context.Background(), bytes.NewReader(b), &out, helperOpts(fakeRestic{}))
	if rerr == nil || !strings.Contains(rerr.Error(), "version") {
		t.Fatalf("err = %v, want a payload-version refusal", rerr)
	}
}

func TestRunExtractHelperResticFailureKeepsStaging(t *testing.T) {
	root := t.TempDir()
	fake := fakeRestic{extractTreeErr: errors.New("restore: exit 1")}
	var out bytes.Buffer
	err := RunExtractHelper(context.Background(), openStdin(t, helperPayloadFor(t, treeReq(), root)), &out, helperOpts(fake))
	if err == nil {
		t.Fatal("RunExtractHelper: nil error for a failed restore")
	}
	evs := decodeHelperEvents(t, out.Bytes())
	last := evs[len(evs)-1]
	if last.Kind != helperEventError || last.Error == nil {
		t.Fatalf("last event = %+v, want error", last)
	}
	if last.Error.Code != helperCodeGeneric {
		t.Errorf("code = %q, want generic", last.Error.Code)
	}
	if !last.Error.StagingCreated || last.Error.StagingDir == "" {
		t.Fatalf("error event staging = %+v, want created+dir for keep-or-delete", last.Error)
	}
	if _, err := os.Lstat(last.Error.StagingDir); err != nil {
		t.Errorf("staging dir should be retained on failure: %v", err)
	}
}

func TestRunExtractHelperStdinEOFCancels(t *testing.T) {
	root := t.TempDir()
	fake := fakeRestic{extractBlock: true} // restore blocks until its ctx falls

	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write(helperPayloadFor(t, treeReq(), root))
		_ = pw.Close() // parent gone: EOF is the kill switch
	}()

	var out bytes.Buffer
	err := RunExtractHelper(context.Background(), pr, &out, helperOpts(fake))
	if err == nil {
		t.Fatal("RunExtractHelper: nil error after stdin EOF cancel")
	}
	evs := decodeHelperEvents(t, out.Bytes())
	last := evs[len(evs)-1]
	if last.Kind != helperEventError || last.Error.Code != helperCodeCanceled {
		t.Fatalf("last event = %+v, want canceled error", last)
	}
}

func TestRunExtractHelperOccupiedFinal(t *testing.T) {
	root := t.TempDir()
	final := filepath.Join(root, "repo-a", "abcd1234", "etc", "nginx")
	if err := os.MkdirAll(final, 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := RunExtractHelper(context.Background(), openStdin(t, helperPayloadFor(t, treeReq(), root)), &out, helperOpts(fakeRestic{}))
	if err == nil {
		t.Fatal("RunExtractHelper: nil error for an occupied final path")
	}
	evs := decodeHelperEvents(t, out.Bytes())
	if code := evs[len(evs)-1].Error.Code; code != helperCodeFinalExists {
		t.Fatalf("code = %q, want %q", code, helperCodeFinalExists)
	}
}

func TestMkdirAllOwnedDisabledOwner(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "a", "b", "c")
	if err := mkdirAllOwned(dir, -1, -1); err != nil {
		t.Fatalf("mkdirAllOwned: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Fatalf("dir not created: %v", err)
	}
	// Idempotent over existing chains.
	if err := mkdirAllOwned(dir, -1, -1); err != nil {
		t.Fatalf("mkdirAllOwned (existing): %v", err)
	}
}
