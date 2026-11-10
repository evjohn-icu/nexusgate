package api

import (
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/app"
	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/inferencecatalog"
)

// multimodalRouteSpecs is the admin surface for the inference boundary: what
// is switched on, which locked profile each operation uses, and the two repair
// actions an operator needs (rebuild one thing, cancel one thing).
//
// Every route here is Hub-admin, reads included. The settings are not secret,
// but they decide which models run on the operator's hardware and which
// biometric work is allowed, so an unauthenticated read would still leak a
// deployment's capabilities.
func (s *Server) multimodalRouteSpecs() []routeSpec {
	return []routeSpec{
		newRouteSpec("multimodal-settings-get", "GET /api/v1/admin/multimodal/settings", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.getMultimodalSettings))),
		newRouteSpec("multimodal-settings-put", "PUT /api/v1/admin/multimodal/settings", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.saveMultimodalSettings))),
		newRouteSpec("modality-rebuild", "POST /api/v1/admin/assets/{id}/modalities/{operation}/rebuild", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.rebuildAssetModality))),
		newRouteSpec("inference-job-cancel", "POST /api/v1/admin/inference/jobs/{id}/cancel", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.cancelInferenceJob))),
		newRouteSpec("search-reference-create", "POST /api/v1/admin/search-references", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.createSearchReference))),
		newRouteSpec("search-reference-get", "GET /api/v1/admin/search-references/{id}", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.getSearchReference))),
		newRouteSpec("search-reference-delete", "DELETE /api/v1/admin/search-references/{id}", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.deleteSearchReference))),
		newRouteSpec("worker-job-input", "GET /api/v1/worker/jobs/{id}/input", routeAuthWorker, http.HandlerFunc(s.workerInferenceInput)),
	}
}

func (s *Server) getMultimodalSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := s.service.GetMultimodalSettings(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"settings": settings,
		"catalog":  inferenceCatalogView(),
		"local_runtime": map[string]any{
			"available": s.service.InferenceExecutorStatus(),
		},
	})
}

func (s *Server) saveMultimodalSettings(w http.ResponseWriter, r *http.Request) {
	var settings domain.MultimodalSettings
	if !decodeStrictJSON(w, r, &settings, 32<<10) {
		return
	}
	saved, err := s.service.SaveMultimodalSettings(r.Context(), settings)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 300)})
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) rebuildAssetModality(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	operation := domain.InferenceOperation(r.PathValue("operation"))
	if assetID == "" {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "asset id is required"})
		return
	}
	jobID, err := s.service.RebuildAssetModality(r.Context(), assetID, operation)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 300)})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "pending", "job_id": jobID})
}

func (s *Server) cancelInferenceJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")
	if jobID == "" {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "job id is required"})
		return
	}
	if err := s.service.CancelInferenceTask(r.Context(), jobID); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "cancel_requested"})
}

// inferenceCatalogView is the UI-facing projection of the lock file: identity,
// dimension and licence only. It never includes hashes or download hosts.
func inferenceCatalogView() []map[string]any {
	out := make([]map[string]any, 0, 8)
	for _, model := range inferencecatalog.All() {
		out = append(out, map[string]any{
			"catalog_id":   model.CatalogID,
			"model_id":     model.ModelID,
			"dimension":    model.Dimension,
			"operations":   model.Operations,
			"native_video": model.NativeVideo,
		})
	}
	return out
}

func (s *Server) createSearchReference(w http.ResponseWriter, r *http.Request) {
	// 8 MiB of image plus multipart framing; the service enforces the pixel
	// bound after decoding, which is the limit a small file can still violate.
	body := http.MaxBytesReader(w, r.Body, 9<<20)
	ref, err := s.service.CreateSearchReference(r.Context(), body)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 300)})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         ref.ID,
		"media_type": ref.MediaType,
		"width":      ref.Width,
		"height":     ref.Height,
		"expires_at": ref.ExpiresAt,
	})
}

func (s *Server) getSearchReference(w http.ResponseWriter, r *http.Request) {
	ref, _, err := s.service.GetSearchReference(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, domain.ErrSearchReferenceExpired) {
			writeAPIError(w, http.StatusGone, APIError{Code: "reference_expired", Message: "query image expired"})
			return
		}
		writeAPIError(w, http.StatusNotFound, APIError{Code: "not_found", Message: "query image not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         ref.ID,
		"media_type": ref.MediaType,
		"width":      ref.Width,
		"height":     ref.Height,
		"expires_at": ref.ExpiresAt,
	})
}

func (s *Server) deleteSearchReference(w http.ResponseWriter, r *http.Request) {
	if err := s.service.DeleteSearchReference(r.Context(), r.PathValue("id")); err != nil {
		if errors.Is(err, app.ErrSearchReferenceNotFound) {
			writeAPIError(w, http.StatusNotFound, APIError{Code: "not_found", Message: "query image not found"})
			return
		}
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// workerInferenceInput streams the exact query-image bytes bound to the lease.
// It is authenticated as a Worker and checks lease ownership before touching
// the file.
func (s *Server) workerInferenceInput(w http.ResponseWriter, r *http.Request) {
	worker, ok := s.authenticatedWorker(w, r)
	if !ok {
		return
	}
	descriptor, path, err := s.service.WorkerInferenceInput(r.Context(), worker.ID, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, domain.ErrJobLeaseLost) {
			writeAPIError(w, http.StatusConflict, APIError{Code: "lease_lost", Message: "inference lease is no longer held by this worker"})
			return
		}
		writeAPIError(w, http.StatusNotFound, APIError{Code: "not_found", Message: "no input is bound to this job"})
		return
	}
	file, err := os.Open(path)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, APIError{Code: "not_found", Message: "input bytes are unavailable"})
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", descriptor.MediaType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Digest", descriptor.Digest)
	http.ServeContent(w, r, "", time.Time{}, file)
}
