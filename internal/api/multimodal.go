package api

import (
	"net/http"

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
