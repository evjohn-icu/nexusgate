package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusslate/internal/remote"
)

func openWorkerOfflineRepo(t *testing.T, name string) *Repository {
	t.Helper()
	repo, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return repo
}

func insertWorkerRow(t *testing.T, repo *Repository, id, status, lastSeenRFC string) {
	t.Helper()
	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(context.Background(),
		`INSERT INTO workers(id,name,platform,version,token_hash,status,capabilities_json,last_seen_at,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		id, "worker-"+id, "linux", "v0.21.0", "hash-"+id, status, "{}", lastSeenRFC, now); err != nil {
		t.Fatal(err)
	}
}

func TestListWorkersDerivesOfflineFromStaleHeartbeat(t *testing.T) {
	repo := openWorkerOfflineRepo(t, "workers-offline.db")
	ctx := context.Background()

	now := formatTime(time.Now().UTC())
	stale := formatTime(time.Now().UTC().Add(-workerOfflineAfter - 30*time.Second))

	insertWorkerRow(t, repo, "fresh", "online", now)
	insertWorkerRow(t, repo, "stale", "online", stale)
	insertWorkerRow(t, repo, "revoked", "revoked", stale)

	workers, err := repo.ListWorkers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]remote.WorkerStatus{}
	for _, w := range workers {
		byID[w.ID] = w.Status
	}

	if got := byID["fresh"]; got != remote.WorkerOnline {
		t.Fatalf("fresh worker status = %q, want online", got)
	}
	if got := byID["stale"]; got != remote.WorkerOffline {
		t.Fatalf("stale worker status = %q, want offline (derived from last_seen_at)", got)
	}
	if got := byID["revoked"]; got != remote.WorkerRevoked {
		t.Fatalf("revoked worker status = %q, want revoked (not overwritten)", got)
	}
}

func TestListWorkersOfflineAtExactThreshold(t *testing.T) {
	repo := openWorkerOfflineRepo(t, "workers-threshold.db")
	ctx := context.Background()

	justNow := formatTime(time.Now().UTC())
	exactlyStale := formatTime(time.Now().UTC().Add(-workerOfflineAfter))

	insertWorkerRow(t, repo, "fresh", "online", justNow)
	insertWorkerRow(t, repo, "edge", "online", exactlyStale)

	workers, err := repo.ListWorkers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]remote.WorkerStatus{}
	for _, w := range workers {
		byID[w.ID] = w.Status
	}
	if got := byID["fresh"]; got != remote.WorkerOnline {
		t.Fatalf("fresh worker = %q, want online", got)
	}
	if got := byID["edge"]; got != remote.WorkerOffline {
		t.Fatalf("edge worker (>= threshold) = %q, want offline", got)
	}
}
