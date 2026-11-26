package app

import (
	"context"
	"fmt"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/inferencecatalog"
	"github.com/evjohn-icu/nexusgate/internal/providers/modelinstall"
)

// modelProbeExecutor is the narrow runtime contract a real load-only model
// probe needs. The shared local inference executor implements it (the Python
// dispatcher's `--probe-model` path); test doubles that only implement
// Capabilities/Execute do not, and an explicit probe against them fails
// instead of silently passing on stale heartbeat data.
type modelProbeExecutor interface {
	// ProbeModel runs a locked, load-only runtime check for one exact model
	// identity. It must not upload source material or execute inference.
	ProbeModel(ctx context.Context, modelID, revision string) error
	// InvalidateCapabilities clears the runtime's cached availability so the
	// next inventory reflects what install/probe just proved.
	InvalidateCapabilities()
}

// localModelManager installs locked artifacts and probes the local runtime. It
// is the Hub-local implementation of ModelManager; a Worker has its own copy of
// the same package and the same lock file.
type localModelManager struct {
	lock      *modelinstall.Lock
	installer *modelinstall.Installer
	executor  InferenceExecutor
}

// NewLocalModelManager loads the lock file and builds an installer rooted at
// modelDir. A missing lock is an error: without it nothing is verifiable and
// the installer would have to trust a URL.
func NewLocalModelManager(lockPath, modelDir string, executor InferenceExecutor) (ModelManager, error) {
	lock, err := modelinstall.LoadLock(lockPath)
	if err != nil {
		return nil, err
	}
	return &localModelManager{
		lock:      lock,
		installer: &modelinstall.Installer{Lock: lock, ModelDir: modelDir},
		executor:  executor,
	}, nil
}

// resolveLockedEntry validates the task's identity against both copies of the
// lock: the installer's file and the Go catalog mirror. A task whose manifest
// digest no longer matches the current catalog entry is refused, so a lock
// that changed underneath the queue can never be installed or attested as the
// revision the operator saw when approving it.
func (m *localModelManager) resolveLockedEntry(task domain.ModelManagementTask) (modelinstall.Entry, error) {
	entry, ok := m.lock.Lookup(task.CatalogID)
	if !ok {
		return modelinstall.Entry{}, domain.Permanent(fmt.Errorf("catalog id %q is not locked", task.CatalogID))
	}
	model, ok := inferencecatalog.Lookup(task.CatalogID)
	if !ok || model.ManifestDigest() != task.ManifestDigest {
		return modelinstall.Entry{}, domain.Permanent(fmt.Errorf("task manifest digest does not match the locked catalog entry %q", task.CatalogID))
	}
	if model.ModelID != entry.ModelID || model.Revision != entry.Revision {
		return modelinstall.Entry{}, domain.Permanent(fmt.Errorf("model lock and catalog mirror disagree on %q", task.CatalogID))
	}
	return entry, nil
}

// Install downloads and verifies one catalog entry. Licence acceptance is
// required in the queued task; the installer itself never assumes permission.
func (m *localModelManager) Install(ctx context.Context, task domain.ModelManagementTask) error {
	if !task.LicenseAccepted {
		return domain.Permanent(fmt.Errorf("model %q requires explicit licence acceptance", task.CatalogID))
	}
	if _, err := m.resolveLockedEntry(task); err != nil {
		return err
	}
	if _, err := m.installer.Install(ctx, task.CatalogID); err != nil {
		return err
	}
	// The install changed what this runtime can load; drop any cached
	// inventory so the next capability report reflects it.
	if prober, ok := m.executor.(modelProbeExecutor); ok {
		prober.InvalidateCapabilities()
	}
	return nil
}

// Probe verifies the installed bytes and asks the runtime whether it can load
// the exact locked revision. "Installed but unloadable" is reported as an
// unavailable model, not as a successful install. An explicit probe must do a
// load-only runtime check; a runtime that cannot answer one must not be
// attested on the strength of an old capability report.
func (m *localModelManager) Probe(ctx context.Context, task domain.ModelManagementTask) error {
	entry, err := m.resolveLockedEntry(task)
	if err != nil {
		return err
	}
	if err := m.installer.VerifyInstalled(task.CatalogID); err != nil {
		return domain.Permanent(err)
	}
	if m.executor == nil {
		return domain.Permanent(fmt.Errorf("no local inference runtime is configured to probe %q", task.CatalogID))
	}
	prober, ok := m.executor.(modelProbeExecutor)
	if !ok {
		return domain.Permanent(fmt.Errorf("runtime cannot run the load-only probe for %q", task.CatalogID))
	}
	err = prober.ProbeModel(ctx, entry.ModelID, entry.Revision)
	// Invalidate after the attempt in both directions: success and failure
	// both change what the inventory should now report.
	prober.InvalidateCapabilities()
	return err
}

// CatalogModel returns the Go mirror entry for a catalog id, used by the admin
// catalog route and by routing decisions.
func CatalogModel(catalogID string) (inferencecatalog.Model, bool) {
	return inferencecatalog.Lookup(catalogID)
}
