package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/evjohn-icu/timingdex/internal/app"
)

// maxTestDriveBodyBytes bounds the POST body of /api/v1/test-drive. The
// payload is at most three asset IDs; a body of this size can only be noise.
const maxTestDriveBodyBytes = 16 << 10

// runTestDrive starts a Test Drive batch: enqueue up to three assets and run
// one pipeline pass so the operator has committed analysis to look at. It is
// administrative because it moves the queue and spends Provider calls.
func (s *Server) runTestDrive(w http.ResponseWriter, r *http.Request) {
	var input struct {
		AssetIDs []string `json:"asset_ids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTestDriveBodyBytes)).Decode(&input); err != nil {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "invalid test drive payload"})
		return
	}
	result, err := s.service.TestDrive(r.Context(), input.AssetIDs)
	if err != nil {
		if errors.Is(err, app.ErrTestDriveInvalid) {
			// The message names the rejected value — the batch size or the
			// missing asset — which is the only feedback the caller has.
			writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: clipText(err.Error(), 300)})
			return
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// testDriveSuggestions answers with the query strings distilled from the
// given assets' committed analysis. Read-only over canonical shot rows, so it
// sits behind the trusted-network read scoping like every other read route.
func (s *Server) testDriveSuggestions(w http.ResponseWriter, r *http.Request) {
	var ids []string
	for _, part := range strings.Split(r.URL.Query().Get("assets"), ",") {
		if part = strings.TrimSpace(part); part != "" {
			ids = append(ids, part)
		}
	}
	if len(ids) == 0 {
		writeAPIError(w, http.StatusBadRequest, APIError{Code: "invalid_request", Message: "assets query parameter is required"})
		return
	}
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil {
		limit = 0
	}
	suggestions, err := s.service.TestDriveSuggestions(r.Context(), ids, limit)
	if err != nil {
		writeError(w, err)
		return
	}
	if suggestions == nil {
		suggestions = []string{}
	}
	writeJSON(w, http.StatusOK, map[string][]string{"suggestions": suggestions})
}
