package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

// healRepo implements Repository by embedding it as a nil field (the package
// pattern for fakes: unimplemented methods panic if called) and records the
// HealStaleRunningJobs call so the wrapper's contract — call it exactly, pass
// the caller's time, surface its error — is asserted against the fake rather
// than against SQLite.
type healRepo struct {
	Repository
	released int
	terminal int
	healErr  error
	called   bool
	gotNow   time.Time
}

func (r *healRepo) HealStaleRunningJobs(_ context.Context, now time.Time) (int, int, error) {
	r.called = true
	r.gotNow = now
	return r.released, r.terminal, r.healErr
}

func TestHealOnStartupCallsRepositorySweep(t *testing.T) {
	repo := &healRepo{released: 2, terminal: 1}
	service := &Service{repo: repo}
	if err := service.HealOnStartup(context.Background()); err != nil {
		t.Fatalf("HealOnStartup error: %v", err)
	}
	if !repo.called {
		t.Fatal("HealStaleRunningJobs was not called")
	}
	if repo.gotNow.IsZero() {
		t.Error("HealStaleRunningJobs was called without a timestamp")
	}
}

func TestHealOnStartupPropagatesSweepError(t *testing.T) {
	repo := &healRepo{healErr: errors.New("sweep failed")}
	service := &Service{repo: repo}
	if err := service.HealOnStartup(context.Background()); err == nil {
		t.Fatal("expected the repository's error to surface, got nil")
	}
}
