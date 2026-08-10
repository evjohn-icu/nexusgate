package media

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRunWithFallbackReportsActualProducerAndOnlyStagingOutput(t *testing.T) {
	old := runFFmpeg
	defer func() { runFFmpeg = old }()
	var calls int
	runFFmpeg = func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("hardware unavailable")
		}
		if err := os.WriteFile(args[len(args)-1], []byte("software output"), 0o600); err != nil {
			t.Fatal(err)
		}
		return nil, nil
	}
	dir := t.TempDir()
	staged := filepath.Join(dir, ".derive-thumbnail.tmp.jpg")
	final := filepath.Join(dir, "thumbnail-cuda.jpg")
	plan := planFor("cuda", "", true, 1800)
	actual, err := atomicRunForTest(context.Background(), staged, plan)
	if err != nil {
		t.Fatal(err)
	}
	if actual.Mode != "software" {
		t.Fatalf("actual mode=%q, want software", actual.Mode)
	}
	if !UsableDerivedFile(staged) {
		t.Fatalf("staged output should exist before publish")
	}
	if _, err := os.Stat(final); !os.IsNotExist(err) {
		t.Fatalf("final output was written before publish: %v", err)
	}
}

func TestRunWithFallbackHardwareSuccessReportsHardware(t *testing.T) {
	old := runFFmpeg
	defer func() { runFFmpeg = old }()
	runFFmpeg = func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if err := os.WriteFile(args[len(args)-1], []byte("hardware output"), 0o600); err != nil {
			t.Fatal(err)
		}
		return nil, nil
	}
	actual, err := atomicRunForTest(context.Background(), filepath.Join(t.TempDir(), ".tmp.jpg"), planFor("cuda", "", true, 1800))
	if err != nil {
		t.Fatal(err)
	}
	if actual.Mode != "cuda" {
		t.Fatalf("actual mode=%q, want cuda", actual.Mode)
	}
}

func TestRunWithFallbackBothFailLeavesNothing(t *testing.T) {
	old := runFFmpeg
	defer func() { runFFmpeg = old }()
	runFFmpeg = func(context.Context, []string, ...string) ([]byte, error) { return nil, errors.New("failed") }
	staged := filepath.Join(t.TempDir(), ".tmp.jpg")
	_, err := atomicRunForTest(context.Background(), staged, planFor("cuda", "", true, 1800))
	if err == nil {
		t.Fatal("expected both attempts to fail")
	}
	if _, statErr := os.Lstat(staged); !os.IsNotExist(statErr) {
		t.Fatalf("staging file remains: %v", statErr)
	}
}

func TestPublishDerivedOutputAtomicity(t *testing.T) {
	dir := t.TempDir()
	staged := filepath.Join(dir, ".tmp.jpg")
	final := filepath.Join(dir, "nested", "thumbnail-software.jpg")
	if err := os.WriteFile(staged, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PublishDerivedOutput(staged, final); err != nil {
		t.Fatal(err)
	}
	if !UsableDerivedFile(final) {
		t.Fatal("published output is not usable")
	}
	if _, err := os.Lstat(staged); !os.IsNotExist(err) {
		t.Fatalf("staging remains: %v", err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PublishDerivedOutput(empty, filepath.Join(dir, "empty-final")); err == nil {
		t.Fatal("expected empty publish to fail")
	}
	if _, err := os.Lstat(empty); !os.IsNotExist(err) {
		t.Fatalf("empty staging remains: %v", err)
	}
	linkTarget := filepath.Join(dir, "outside")
	if err := os.WriteFile(linkTarget, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(dir, "linked.jpg")
	if err := os.Symlink(linkTarget, linked); err != nil {
		t.Fatal(err)
	}
	staged = filepath.Join(dir, ".linked.tmp.jpg")
	if err := os.WriteFile(staged, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PublishDerivedOutput(staged, linked); err == nil {
		t.Fatal("expected symlink destination to be rejected")
	}
	got, err := os.ReadFile(linkTarget)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old" {
		t.Fatalf("symlink target changed to %q", got)
	}
}

func atomicRunForTest(ctx context.Context, staged string, plan HardwarePlan) (HardwarePlan, error) {
	var actual HardwarePlan
	err := atomicFFmpegOutput(staged, func(out string) error {
		var err error
		actual, err = runWithFallback(ctx, "test", []string{"-i", "source", out}, plan, func() []string { return []string{"-i", "source", out} })
		return err
	})
	return actual, err
}
