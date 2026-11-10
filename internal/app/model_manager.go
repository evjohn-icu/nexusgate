package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/inferencecatalog"
	"github.com/evjohn-icu/nexusgate/internal/providers/modelinstall"
	"github.com/evjohn-icu/nexusgate/internal/remote"
)

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

// Install downloads and verifies one catalog entry. Licence acceptance is
// required in the queued task; the installer itself never assumes permission.
func (m *localModelManager) Install(ctx context.Context, task domain.ModelManagementTask) error {
	if !task.LicenseAccepted {
		return domain.Permanent(fmt.Errorf("model %q requires explicit licence acceptance", task.CatalogID))
	}
	if _, ok := m.lock.Lookup(task.CatalogID); !ok {
		return domain.Permanent(fmt.Errorf("catalog id %q is not locked", task.CatalogID))
	}
	if _, err := m.installer.Install(ctx, task.CatalogID); err != nil {
		return err
	}
	return nil
}

// Probe verifies the installed bytes and asks the runtime whether it can load
// the model. "Installed but unloadable" is reported as an unavailable model,
// not as a successful install.
func (m *localModelManager) Probe(ctx context.Context, task domain.ModelManagementTask) error {
	if err := m.installer.VerifyInstalled(task.CatalogID); err != nil {
		return domain.Permanent(err)
	}
	if m.executor == nil {
		return domain.Permanent(fmt.Errorf("no local inference runtime is configured to probe %q", task.CatalogID))
	}
	entry, _ := m.lock.Lookup(task.CatalogID)
	caps, err := m.executor.Capabilities(ctx)
	if err != nil {
		return err
	}
	for _, capability := range caps {
		if capability.Available && strings.EqualFold(capability.ModelID, entry.ModelID) {
			return nil
		}
	}
	return domain.Permanent(&remote.InferenceFailure{
		Code:    remote.FailureModelUnavailable,
		Message: fmt.Sprintf("runtime cannot load %s at revision %s", entry.ModelID, entry.Revision),
	})
}

// CatalogModel returns the Go mirror entry for a catalog id, used by the admin
// catalog route and by routing decisions.
func CatalogModel(catalogID string) (inferencecatalog.Model, bool) {
	return inferencecatalog.Lookup(catalogID)
}
