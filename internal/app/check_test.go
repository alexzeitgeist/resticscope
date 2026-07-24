package app

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/config"
	"github.com/alexzeitgeist/resticscope/internal/model"
	"github.com/alexzeitgeist/resticscope/internal/resticx"
)

type recordingRestic struct {
	mu      sync.Mutex
	targets map[string]resticx.Target
}

func (r *recordingRestic) Snapshots(ctx context.Context, t resticx.Target, c resticx.Creds) ([]model.Snapshot, error) {
	return nil, nil
}

func (r *recordingRestic) CatConfig(ctx context.Context, t resticx.Target, c resticx.Creds) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.targets == nil {
		r.targets = map[string]resticx.Target{}
	}
	r.targets[t.Name] = t
	return nil
}

func (r *recordingRestic) StreamSnapshotTree(ctx context.Context, t resticx.Target, c resticx.Creds, snapshotID string, timeout time.Duration, onNode func(model.BrowseNode) error) (model.BrowseScanSummary, error) {
	return model.BrowseScanSummary{}, nil
}

func (r *recordingRestic) FindMatches(ctx context.Context, t resticx.Target, c resticx.Creds, host, pattern string) ([]model.FindSnapshotResult, error) {
	return nil, nil
}

func (r *recordingRestic) StreamDiff(ctx context.Context, t resticx.Target, c resticx.Creds, olderID, newerID string, timeout time.Duration, onEntry func(model.DiffEntry) error, onProgress func(seen int)) (model.SnapshotDiff, error) {
	return model.SnapshotDiff{}, nil
}

func (r *recordingRestic) ExtractTree(ctx context.Context, t resticx.Target, c resticx.Creds, params resticx.ExtractTreeParams, onEvent func(resticx.ExtractTreeEvent) error) error {
	return nil
}

func (r *recordingRestic) target(name string) (resticx.Target, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.targets[name]
	return t, ok
}

func TestCheckAllReachable(t *testing.T) {
	a := &App{
		Cfg:     testConfig(),
		Cache:   newFakeCache(),
		Clock:   fixedClock{now},
		Secrets: fakeSecrets{},
		Restic:  fakeRestic{},
	}
	checks, err := a.Check(t.Context())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(checks) != 1 || !checks[0].OK() || checks[0].Name != "repo-a" {
		t.Fatalf("expected one OK repo-a, got %+v", checks)
	}
}

func TestCheckRecordsResticFailure(t *testing.T) {
	a := &App{
		Cfg:     testConfig(),
		Cache:   newFakeCache(),
		Clock:   fixedClock{now},
		Secrets: fakeSecrets{},
		Restic:  fakeRestic{catErr: errors.New("restic cat config: wrong password (exit 12)")},
	}
	checks, err := a.Check(t.Context())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if checks[0].OK() {
		t.Fatal("expected repo-a to be unreachable")
	}
	if checks[0].Err == nil {
		t.Error("expected the restic error to be recorded")
	}
}

func TestCheckRecordsSecretsFailure(t *testing.T) {
	a := &App{
		Cfg:     testConfig(),
		Cache:   newFakeCache(),
		Clock:   fixedClock{now},
		Secrets: fakeSecrets{err: errors.New("secrets: no repo \"repo-a\"")},
		Restic:  fakeRestic{},
	}
	checks, err := a.Check(t.Context())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if checks[0].OK() {
		t.Fatal("expected an unresolved secret to fail the repo check")
	}
}

func TestCheckPreservesOrder(t *testing.T) {
	cfg := testConfig()
	cfg.Repos = append(cfg.Repos,
		config.Repo{Name: "repo-b", Credential: "cred-a", Endpoint: "https://hel1.example.com", Bucket: "bucket-b", Path: "nested/repo", BucketLookup: "dns", ExpectedFrequency: config.Duration(24 * time.Hour)},
		config.Repo{Name: "repo-c", Credential: "cred-a", Endpoint: "https://nbg1.example.com", Region: "nbg1", Bucket: "bucket-c", BucketLookup: "path", ExpectedFrequency: config.Duration(24 * time.Hour)},
	)
	restic := &recordingRestic{}
	a := &App{
		Cfg:     cfg,
		Cache:   newFakeCache(),
		Clock:   fixedClock{now},
		Secrets: fakeSecrets{},
		Restic:  restic,
	}
	checks, err := a.Check(t.Context())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	for i, name := range []string{"repo-a", "repo-b", "repo-c"} {
		if checks[i].Name != name {
			t.Errorf("checks[%d].Name = %q, want %q (order not preserved)", i, checks[i].Name, name)
		}
	}

	wantB := resticx.Target{
		Name:    "repo-b",
		Repo:    "s3:https://hel1.example.com/bucket-b/nested/repo",
		Options: map[string]string{"s3.bucket-lookup": "dns"},
	}
	if got, ok := restic.target("repo-b"); !ok || !reflect.DeepEqual(got, wantB) {
		t.Errorf("repo-b target = %+v, %v; want %+v, true", got, ok, wantB)
	}
	wantC := resticx.Target{
		Name:    "repo-c",
		Repo:    "s3:https://nbg1.example.com/bucket-c",
		Options: map[string]string{"s3.bucket-lookup": "path"},
		Env:     map[string]string{"AWS_DEFAULT_REGION": "nbg1"},
	}
	if got, ok := restic.target("repo-c"); !ok || !reflect.DeepEqual(got, wantC) {
		t.Errorf("repo-c target = %+v, %v; want %+v, true", got, ok, wantC)
	}
}

func TestCheckCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	a := &App{
		Cfg:     testConfig(),
		Cache:   newFakeCache(),
		Clock:   fixedClock{now},
		Secrets: fakeSecrets{},
		Restic:  fakeRestic{},
	}
	if _, err := a.Check(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
