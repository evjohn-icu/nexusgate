package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/app"
	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/inferencecatalog"
	"github.com/evjohn-icu/nexusgate/internal/media"
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
		newRouteSpec("inference-catalog", "GET /api/v1/admin/inference/catalog", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.inferenceCatalog))),
		newRouteSpec("inference-model-install", "POST /api/v1/admin/inference/models/{catalog_id}/install", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.installInferenceModel))),
		newRouteSpec("inference-model-probe", "POST /api/v1/admin/inference/models/{catalog_id}/probe", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.probeInferenceModel))),
		newRouteSpec("inference-operation-status", "GET /api/v1/admin/inference/operations/{id}", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.inferenceOperationStatus))),
		newRouteSpec("search-reference-create", "POST /api/v1/admin/search-references", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.createSearchReference))),
		newRouteSpec("search-reference-get", "GET /api/v1/admin/search-references/{id}", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.getSearchReference))),
		newRouteSpec("search-reference-delete", "DELETE /api/v1/admin/search-references/{id}", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.deleteSearchReference))),
		newRouteSpec("worker-job-input", "GET /api/v1/worker/jobs/{id}/input", routeAuthWorker, http.HandlerFunc(s.workerInferenceInput)),
		newRouteSpec("asset-timeline", "GET /api/v1/assets/{id}/timeline", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.assetTimeline))),
		newRouteSpec("asset-waveform", "GET /api/v1/assets/{id}/waveform", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.assetWaveform))),
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

// assetTimeline renders every track with one time base. Intervals keep their
// stored bounds; the viewport filters, it never clips, so a client seeking to
// an event's start always reaches the real start.
func (s *Server) assetTimeline(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	startMS, endMS, ok := parseViewport(w, r)
	if !ok {
		return
	}
	limit := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > 1000 {
			writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "limit must be between 1 and 1000"})
			return
		}
		limit = parsed
	}
	timeline, truncated, err := s.service.AssetTimeline(r.Context(), assetID, startMS, endMS, limit)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeAPIError(w, http.StatusNotFound, APIError{Code: "not_found", Message: "asset not found"})
			return
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"timeline":               timeline,
		"truncated":              truncated,
		"processing_unavailable": len(timeline.Coverage) == 0,
	})
}

// parseViewport reads start_ms/end_ms, rejecting a reversed or non-numeric
// window instead of silently serving a different one.
func parseViewport(w http.ResponseWriter, r *http.Request) (int64, int64, bool) {
	parse := func(name string) (int64, bool) {
		raw := strings.TrimSpace(r.URL.Query().Get(name))
		if raw == "" {
			return 0, true
		}
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 0 {
			writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: name + " must be a non-negative integer"})
			return 0, false
		}
		return value, true
	}
	startMS, ok := parse("start_ms")
	if !ok {
		return 0, 0, false
	}
	endMS, ok := parse("end_ms")
	if !ok {
		return 0, 0, false
	}
	if endMS != 0 && endMS <= startMS {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "end_ms must be greater than start_ms"})
		return 0, 0, false
	}
	return startMS, endMS, true
}

// speakerRouteSpecs is the biometric surface. Reads are admin-only too: the
// existence of an opted-in profile is itself information about the recording,
// and the plan requires every speaker route to require the Hub administrator.
func (s *Server) speakerRouteSpecs() []routeSpec {
	return []routeSpec{
		newRouteSpec("speaker-consent-get", "GET /api/v1/admin/speaker-consent", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.getSpeakerConsent))),
		newRouteSpec("speaker-consent-put", "PUT /api/v1/admin/speaker-consent", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.putSpeakerConsent))),
		newRouteSpec("speaker-profiles-list", "GET /api/v1/admin/speaker-profiles", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.listSpeakerProfiles))),
		newRouteSpec("speaker-profile-create", "POST /api/v1/admin/speaker-profiles", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.createSpeakerProfile))),
		newRouteSpec("speaker-profile-delete", "DELETE /api/v1/admin/speaker-profiles/{id}", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.deleteSpeakerProfile))),
		newRouteSpec("speaker-profile-matches", "POST /api/v1/admin/speaker-profiles/{id}/matches", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.speakerMatches))),
		newRouteSpec("speaker-association-patch", "PATCH /api/v1/admin/speaker-associations/{id}", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.patchSpeakerAssociation))),
		newRouteSpec("speaker-cluster-split", "POST /api/v1/admin/speaker-clusters/{id}/split", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.splitSpeakerCluster))),
	}
}

func (s *Server) getSpeakerConsent(w http.ResponseWriter, r *http.Request) {
	consent, err := s.service.GetSpeakerConsent(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, consent)
}

func (s *Server) putSpeakerConsent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeStrictJSON(w, r, &body, 4<<10) {
		return
	}
	consent, err := s.service.SetSpeakerConsent(r.Context(), body.Enabled)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, consent)
}

func (s *Server) listSpeakerProfiles(w http.ResponseWriter, r *http.Request) {
	profiles, err := s.service.ListSpeakerProfiles(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"profiles": profiles})
}

func (s *Server) createSpeakerProfile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AssetID                 string `json:"asset_id"`
		DiarizationGenerationID string `json:"diarization_generation_id"`
		LocalSpeakerID          string `json:"local_speaker_id"`
	}
	if !decodeStrictJSON(w, r, &body, 8<<10) {
		return
	}
	profile, err := s.service.CreateSpeakerProfile(r.Context(), body.AssetID, body.DiarizationGenerationID, body.LocalSpeakerID)
	if err != nil {
		if errors.Is(err, app.ErrSpeakerConsentRequired) {
			writeAPIError(w, http.StatusForbidden, APIError{Code: "consent_required", Message: "speaker consent is disabled"})
			return
		}
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 300)})
		return
	}
	writeJSON(w, http.StatusCreated, profile)
}

func (s *Server) deleteSpeakerProfile(w http.ResponseWriter, r *http.Request) {
	if err := s.service.DeleteSpeakerProfile(r.Context(), r.PathValue("id")); err != nil {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 300)})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) speakerMatches(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Limit int `json:"limit"`
	}
	if r.ContentLength > 0 && !decodeStrictJSON(w, r, &body, 4<<10) {
		return
	}
	matches, err := s.service.SearchSpeakerMatches(r.Context(), r.PathValue("id"), body.Limit)
	if err != nil {
		if errors.Is(err, app.ErrSpeakerConsentRequired) {
			writeAPIError(w, http.StatusForbidden, APIError{Code: "consent_required", Message: "speaker consent is disabled"})
			return
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"associations": matches})
}

func (s *Server) patchSpeakerAssociation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		State string `json:"state"`
	}
	if !decodeStrictJSON(w, r, &body, 4<<10) {
		return
	}
	if err := s.service.SetSpeakerAssociationState(r.Context(), r.PathValue("id"), body.State); err != nil {
		if errors.Is(err, app.ErrSpeakerConsentRequired) {
			writeAPIError(w, http.StatusForbidden, APIError{Code: "consent_required", Message: "speaker consent is disabled"})
			return
		}
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 300)})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) splitSpeakerCluster(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProfileIDs []string `json:"profile_ids"`
	}
	if !decodeStrictJSON(w, r, &body, 8<<10) {
		return
	}
	if err := s.service.SplitSpeakerCluster(r.Context(), r.PathValue("id"), body.ProfileIDs); err != nil {
		if errors.Is(err, app.ErrSpeakerConsentRequired) {
			writeAPIError(w, http.StatusForbidden, APIError{Code: "consent_required", Message: "speaker consent is disabled"})
			return
		}
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 300)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "split"})
}

// assetWaveform serves a viewport of the stored waveform pyramid. The response
// is capped at 4096 buckets so a zoomed-out request cannot stream a megabyte of
// JSON per repaint; the client asks for a coarser level instead.
func (s *Server) assetWaveform(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	startMS, endMS, ok := parseViewport(w, r)
	if !ok {
		return
	}
	level := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("level")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 || parsed > 24 {
			writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "level must be between 0 and 24"})
			return
		}
		level = parsed
	}
	artifact, err := s.service.GetArtifactForAsset(r.Context(), assetID, "waveform")
	if err != nil || artifact == nil {
		writeAPIError(w, http.StatusNotFound, APIError{Code: "waveform_unavailable", Message: "no waveform has been generated for this asset"})
		return
	}
	manifestRaw, err := os.ReadFile(artifact.LocalPath)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, APIError{Code: "waveform_unavailable", Message: "waveform manifest is unreadable"})
		return
	}
	var manifest media.WaveformManifest
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		writeAPIError(w, http.StatusInternalServerError, APIError{Code: "waveform_corrupt", Message: "waveform manifest is corrupt"})
		return
	}
	payload, err := os.ReadFile(strings.TrimSuffix(artifact.LocalPath, ".json") + ".bin")
	if err != nil {
		writeAPIError(w, http.StatusNotFound, APIError{Code: "waveform_unavailable", Message: "waveform payload is unreadable"})
		return
	}
	if endMS == 0 {
		endMS = manifest.DurationMS
	}
	buckets, err := media.ReadWaveformBuckets(manifest, payload, level, startMS, endMS, 4096)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 200)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"level":       level,
		"bucket_ms":   int64(manifest.BucketMS) << level,
		"duration_ms": manifest.DurationMS,
		"buckets":     buckets,
	})
}

func (s *Server) inferenceCatalog(w http.ResponseWriter, r *http.Request) {
	entries := inferencecatalog.All()
	models := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		models = append(models, map[string]any{
			"catalog_id":      entry.CatalogID,
			"model_id":        entry.ModelID,
			"revision":        entry.Revision,
			"operations":      entry.Operations,
			"dimension":       entry.Dimension,
			"native_video":    entry.NativeVideo,
			"matryoshka":      entry.Matryoshka,
			"manifest_digest": app.ModelManifestDigest(entry),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"schema_version": inferencecatalog.LockSchemaVersion,
		"models":         models,
		"local_runtime":  map[string]any{"available": s.service.InferenceExecutorStatus()},
	})
}

func (s *Server) installInferenceModel(w http.ResponseWriter, r *http.Request) {
	s.queueModelManagement(w, r, "install")
}

func (s *Server) probeInferenceModel(w http.ResponseWriter, r *http.Request) {
	s.queueModelManagement(w, r, "probe")
}

func (s *Server) queueModelManagement(w http.ResponseWriter, r *http.Request, action string) {
	var body struct {
		WorkerID        string `json:"worker_id"`
		ManifestDigest  string `json:"manifest_digest"`
		LicenseAccepted bool   `json:"license_accepted"`
	}
	if !decodeStrictJSON(w, r, &body, 8<<10) {
		return
	}
	jobID, err := s.service.QueueModelManagement(r.Context(), action, r.PathValue("catalog_id"), body.WorkerID, body.ManifestDigest, body.LicenseAccepted)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 300)})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "pending", "job_id": jobID})
}

func (s *Server) inferenceOperationStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.service.ModelManagementStatus(r.Context(), r.PathValue("id"))
	if err != nil {
		writeAPIError(w, http.StatusNotFound, APIError{Code: "not_found", Message: "operation not found"})
		return
	}
	writeJSON(w, http.StatusOK, status)
}
