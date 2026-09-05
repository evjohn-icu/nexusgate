package api

import (
	"errors"
	"net/http"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

// revokeWorker is the admin-only write that decommissions a Worker:
// POST /api/v1/hub/workers/{id}/revoke. It is the missing half of the
// revocation contract (WORKER-001): AuthenticateWorker and HeartbeatWorker
// already refuse status='revoked', but nothing could set that status before
// this endpoint existed. Revoking an unknown id answers 404 rather than the
// classifier's default 500.
func (s *Server) revokeWorker(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if err := s.service.RevokeWorker(r.Context(), r.PathValue("id")); err != nil {
		if errors.Is(err, domain.ErrWorkerNotFound) {
			writeAPIError(w, http.StatusNotFound, APIError{Code: "not_found", Message: "worker not found"})
			return
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": true})
}
