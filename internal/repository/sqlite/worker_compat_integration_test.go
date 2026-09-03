package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusslate/internal/domain"
	"github.com/evjohn-icu/nexusslate/internal/remote"
)

// TestLeaseNextWorkerDeriveGatesOnWorkerVersion pins the P1b lease gate:
// a worker whose binary predates the Hub's minimum compatible version must
// never receive a lease, while compatible and advisory (unknown) versions
// still lease the very same job. The sequence inside each case matters: an
// incompatible refusal must leave the job leasable for the next worker.
func TestLeaseNextWorkerDeriveGatesOnWorkerVersion(t *testing.T) {
	type step struct {
		version    string
		wantLeased bool
	}
	cases := []struct {
		name  string
		steps []step
	}{
		{"incompatible refused then compatible leased", []step{{"v0.24.0", false}, {"v0.26.0", true}}},
		{"minor one patch below refused", []step{{"v0.25.9", false}, {"v0.26.0", true}}},
		{"major mismatch refused", []step{{"v1.0.0", false}, {"v0.26.0", true}}},
		{"dev advisory still leased", []step{{"dev", true}}},
		{"empty version still leased", []step{{"", true}}},
		{"unparseable advisory still leased", []step{{"build-2026", true}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			repo, err := Open(filepath.Join(t.TempDir(), "worker-compat.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer repo.Close()
			if err := repo.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			now := formatTime(time.Now().UTC())
			if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('root-compat','/footage',?,?)`, now, now); err != nil {
				t.Fatal(err)
			}
			if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-compat','compat-fp',100,'discovered',?,?)`, now, now); err != nil {
				t.Fatal(err)
			}
			if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES('loc-compat','asset-compat','root-compat','clip.mp4','/footage/clip.mp4',1,1,1,?)`, now); err != nil {
				t.Fatal(err)
			}
			if err := repo.EnqueueJob(ctx, "asset-compat", domain.JobDerive, "derive-input", 90); err != nil {
				t.Fatal(err)
			}
			for i, step := range tc.steps {
				worker := remote.Worker{
					ID:      fmt.Sprintf("worker-%d", i),
					Version: step.version,
					Capabilities: remote.WorkerCapabilities{
						Proxy:        true,
						Thumbnail:    true,
						LibraryRoots: []string{"root-compat"},
					},
				}
				leased, err := repo.LeaseNextWorkerDerive(ctx, worker, time.Minute, domain.LeaseFilter{})
				if err != nil {
					t.Fatalf("version %q: lease err=%v", step.version, err)
				}
				if (leased != nil) != step.wantLeased {
					t.Fatalf("version %q: leased=%v want %v", step.version, leased != nil, step.wantLeased)
				}
			}
		})
	}
}

// TestLeaseNextWorkerDeriveGatesOnEnrolledVersion checks the same gate
// through the production path: the version the gate reads is the one
// persisted at enrollment (workers.version), returned by AuthenticateWorker
// and honored by the lease -- the Worker never supplies its own version on
// the lease request, so it cannot claim a newer binary than it shipped with.
func TestLeaseNextWorkerDeriveGatesOnEnrolledVersion(t *testing.T) {
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "worker-compat-enrolled.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now().UTC())
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('root-enrolled','/footage',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset-enrolled','enrolled-fp',100,'discovered',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES('loc-enrolled','asset-enrolled','root-enrolled','clip.mp4','/footage/clip.mp4',1,1,1,?)`, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnqueueJob(ctx, "asset-enrolled", domain.JobDerive, "derive-input", 90); err != nil {
		t.Fatal(err)
	}
	enroll := func(name, version string) remote.Worker {
		t.Helper()
		pairing, err := repo.CreateWorkerPairing(ctx, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		_, token, err := repo.EnrollWorker(ctx, pairing.Token, remote.WorkerRegistration{
			Name: name, Platform: "linux", Version: version,
			Capabilities: remote.WorkerCapabilities{Proxy: true, Thumbnail: true, LibraryRoots: []string{"root-enrolled"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		worker, err := repo.AuthenticateWorker(ctx, token)
		if err != nil {
			t.Fatal(err)
		}
		if worker.Version != version {
			t.Fatalf("AuthenticateWorker version=%q want %q", worker.Version, version)
		}
		return worker
	}

	old := enroll("old-worker", "v0.24.0")
	if leased, err := repo.LeaseNextWorkerDerive(ctx, old, time.Minute, domain.LeaseFilter{}); err != nil {
		t.Fatal(err)
	} else if leased != nil {
		t.Fatalf("enrolled incompatible worker leased a job: %+v", leased)
	}

	current := enroll("current-worker", "v0.26.0")
	leased, err := repo.LeaseNextWorkerDerive(ctx, current, time.Minute, domain.LeaseFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if leased == nil {
		t.Fatal("enrolled compatible worker did not lease the job the incompatible one was refused")
	}
}
