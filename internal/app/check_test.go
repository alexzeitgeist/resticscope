package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"resticscope/internal/config"
)

func TestCheckAllReachable(t *testing.T) {
	a := &App{
		Cfg:     testConfig(),
		Cache:   newFakeCache(),
		Clock:   fixedClock{now},
		Secrets: fakeSecrets{},
		Restic:  fakeRestic{}, // CatConfig returns nil
	}
	checks, err := a.Check(context.Background())
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
	checks, err := a.Check(context.Background())
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
	checks, err := a.Check(context.Background())
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
		config.Repo{Name: "repo-b", Credential: "cred-a", Bucket: "bucket-b", ExpectedFrequency: config.Duration(24 * time.Hour)},
		config.Repo{Name: "repo-c", Credential: "cred-a", Bucket: "bucket-c", ExpectedFrequency: config.Duration(24 * time.Hour)},
	)
	a := &App{
		Cfg:     cfg,
		Cache:   newFakeCache(),
		Clock:   fixedClock{now},
		Secrets: fakeSecrets{},
		Restic:  fakeRestic{},
	}
	checks, err := a.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	for i, name := range []string{"repo-a", "repo-b", "repo-c"} {
		if checks[i].Name != name {
			t.Errorf("checks[%d].Name = %q, want %q (order not preserved)", i, checks[i].Name, name)
		}
	}
}

func TestCheckCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
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
