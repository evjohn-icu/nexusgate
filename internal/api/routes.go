package api

import (
	"net/http"
	"strings"
	"time"
)

// routeAuthClass is audit metadata for a route. It deliberately does not
// apply middleware: handlers are wrapped at construction time so the route
// inventory cannot silently change authentication or CSRF behavior.
type routeAuthClass string

const (
	routeAuthPublic          routeAuthClass = "public"
	routeAuthTrustedRead     routeAuthClass = "trusted-read"
	routeAuthHubAdmin        routeAuthClass = "hub-admin"
	routeAuthAgentOrAdmin    routeAuthClass = "agent-or-admin"
	routeAuthWorker          routeAuthClass = "worker"
	routeAuthWorkerBootstrap routeAuthClass = "worker-bootstrap"
	routeAuthWorkerEnroll    routeAuthClass = "worker-enroll"
	routeAuthBrowserSession  routeAuthClass = "browser-session"
	routeAuthBrowserPage     routeAuthClass = "browser-page"
	routeAuthWebDAVBasic     routeAuthClass = "webdav-basic"
	routeAuthCatchAll        routeAuthClass = "catch-all"
)

// routeSpec is the single auditable description of a registered ServeMux
// route. Pattern is the exact Go 1.22 method/path pattern, including method
// qualifiers and wildcards. Handler is already wrapped with the route's
// existing authentication and request-policy checks.
type routeSpec struct {
	Name    string
	Pattern string
	Auth    routeAuthClass
	Handler http.Handler
}

func newRouteSpec(name, pattern string, auth routeAuthClass, handler http.Handler) routeSpec {
	return routeSpec{Name: name, Pattern: pattern, Auth: auth, Handler: handler}
}

func (s *Server) routeSpecs() []routeSpec {
	specs := make([]routeSpec, 0, 109)
	specs = append(specs, s.coreRouteSpecs()...)
	if s.webdav != nil {
		specs = append(specs, newRouteSpec("webdav-space-delivery", "/spaces/", routeAuthWebDAVBasic, s.webdav.Handler()))
	}
	specs = append(specs, s.webDAVRouteSpecs()...)
	specs = append(specs, s.libraryRouteSpecs()...)
	specs = append(specs, s.collectionRouteSpecs()...)
	specs = append(specs, s.providerRouteSpecs()...)
	specs = append(specs, s.workerRouteSpecs()...)
	specs = append(specs, s.pipelineRouteSpecs()...)
	specs = append(specs, s.searchRouteSpecs()...)
	specs = append(specs, s.repurposeRouteSpecs()...)
	specs = append(specs, s.workerSetupRouteSpecs()...)
	specs = append(specs, s.pageRouteSpecs()...)
	specs = append(specs, s.catchAllRouteSpecs()...)
	return specs
}

// routeInventory is kept separate from Handler so tests and documentation can
// inspect the complete route table without starting a listener.
func (s *Server) routeInventory() []routeSpec {
	return s.routeSpecs()
}

func (s *Server) registerRoutes(mux *http.ServeMux) []routeSpec {
	specs := s.routeSpecs()
	for _, spec := range specs {
		mux.Handle(spec.Pattern, spec.Handler)
	}
	return specs
}

func (s *Server) coreRouteSpecs() []routeSpec {
	return []routeSpec{
		newRouteSpec("health", "GET /api/v1/health", routeAuthPublic, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		})),
		newRouteSpec("admin-session-create", "POST /api/v1/auth/admin/session", routeAuthBrowserSession, http.HandlerFunc(s.createAdminSession)),
		newRouteSpec("admin-session-current", "GET /api/v1/auth/admin/session", routeAuthBrowserSession, http.HandlerFunc(s.currentAdminSession)),
		newRouteSpec("admin-session-delete", "DELETE /api/v1/auth/admin/session", routeAuthBrowserSession, http.HandlerFunc(s.deleteAdminSession)),
		newRouteSpec("hardware", "GET /api/v1/hardware", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.hardwareReport))),
		newRouteSpec("setup-status", "GET /api/v1/setup/status", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.setupStatus))),
		newRouteSpec("agent-capabilities", "GET /api/v1/agent/capabilities", routeAuthPublic, http.HandlerFunc(s.agentCapabilities)),
		newRouteSpec("favicon", "GET /favicon.ico", routeAuthPublic, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})),
	}
}

func (s *Server) webDAVRouteSpecs() []routeSpec {
	return []routeSpec{
		newRouteSpec("webdav-accounts-list", "GET /api/v1/admin/webdav/accounts", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.listWebDAVAccounts))),
		newRouteSpec("webdav-account-create", "POST /api/v1/admin/webdav/accounts", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.createWebDAVAccount))),
		newRouteSpec("webdav-account-delete", "DELETE /api/v1/admin/webdav/accounts/{username}", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.deleteWebDAVAccount))),
		newRouteSpec("webdav-space-create", "POST /api/v1/admin/webdav/spaces", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.createWebDAVSpace))),
		newRouteSpec("webdav-spaces-list", "GET /api/v1/admin/webdav/spaces", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.listWebDAVSpaces))),
		newRouteSpec("webdav-space-delete", "DELETE /api/v1/admin/webdav/spaces/{id}", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.deleteWebDAVSpace))),
		newRouteSpec("webdav-space-link", "POST /api/v1/admin/webdav/spaces/{id}/links", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.linkWebDAVAsset))),
	}
}

func (s *Server) libraryRouteSpecs() []routeSpec {
	return []routeSpec{
		newRouteSpec("library-assets", "GET /api/v1/assets", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.listAssetCards))),
		newRouteSpec("library-processing-summary", "GET /api/v1/library/processing-summary", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.processingSummary))),
		newRouteSpec("shoot-sessions", "GET /api/v1/shoot-sessions", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.listShootSessions))),
		newRouteSpec("asset-detail", "GET /api/v1/assets/{id}", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.assetDetail))),
		newRouteSpec("asset-shots", "GET /api/v1/assets/{id}/shots", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.assetShots))),
		newRouteSpec("asset-transcript", "GET /api/v1/assets/{id}/transcript", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.assetTranscript))),
		newRouteSpec("asset-thumbnail", "GET /api/v1/assets/{id}/thumbnail", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.assetThumbnail))),
		newRouteSpec("asset-proxy", "GET /api/v1/assets/{id}/proxy", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.assetProxy))),
		newRouteSpec("jobs", "GET /api/v1/jobs", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.listJobs))),
		newRouteSpec("jobs-summary", "GET /api/v1/jobs/summary", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.jobSummary))),
		newRouteSpec("cost-summary", "GET /api/v1/cost/summary", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.costSummary))),
		newRouteSpec("issues", "GET /api/v1/issues", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.jobIssues))),
		newRouteSpec("storage-overview", "GET /api/v1/storage/overview", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.storageOverview))),
		newRouteSpec("pipeline-supervisor", "GET /api/v1/pipeline/supervisor", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.librarySupervisorStatus))),
		newRouteSpec("pipeline-throttle-get", "GET /api/v1/pipeline/throttle", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.getPipelineThrottle))),
	}
}

func (s *Server) collectionRouteSpecs() []routeSpec {
	return []routeSpec{
		newRouteSpec("collections-list", "GET /api/v1/collections", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.listCollections))),
		newRouteSpec("collection-get", "GET /api/v1/collections/{id}", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.getCollection))),
		newRouteSpec("collection-assets", "GET /api/v1/collections/{id}/assets", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.listCollectionAssets))),
		newRouteSpec("collection-create", "POST /api/v1/collections", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.saveCollection))),
		newRouteSpec("collection-delete", "DELETE /api/v1/collections/{id}", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.deleteCollection))),
		newRouteSpec("collection-shots-list", "GET /api/v1/collections/{id}/shots", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.listCollectionShots))),
		newRouteSpec("collection-shot-add", "POST /api/v1/collections/{id}/shots", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.addCollectionShot))),
		newRouteSpec("collection-shot-remove", "DELETE /api/v1/collections/{id}/shots/{shot_id}", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.removeCollectionShot))),
		newRouteSpec("collection-shot-reorder", "POST /api/v1/collections/{id}/shots/reorder", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.reorderCollectionShots))),
	}
}

func (s *Server) providerRouteSpecs() []routeSpec {
	return []routeSpec{
		newRouteSpec("provider-channels-list", "GET /api/v1/admin/provider-channels", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.listProviderChannels))),
		newRouteSpec("provider-channel-status", "GET /api/v1/admin/provider-channels/status", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.providerChannelRuntimeStatus))),
		newRouteSpec("provider-channel-create", "POST /api/v1/admin/provider-channels", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.saveProviderChannel))),
		newRouteSpec("provider-channel-update", "PATCH /api/v1/admin/provider-channels/{id}", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.updateProviderChannel))),
		newRouteSpec("provider-channel-enable", "POST /api/v1/admin/provider-channels/{id}/enable", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.enableProviderChannel))),
		newRouteSpec("provider-channel-disable", "POST /api/v1/admin/provider-channels/{id}/disable", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.disableProviderChannel))),
		newRouteSpec("provider-channel-delete", "DELETE /api/v1/admin/provider-channels/{id}", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.deleteProviderChannel))),
		newRouteSpec("provider-channel-test", "POST /api/v1/admin/provider-channels/{id}/test", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.testProviderChannel))),
		newRouteSpec("provider-channel-probe-models", "POST /api/v1/admin/provider-channels/probe-models", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.probeProviderModelList))),
		newRouteSpec("asset-capture-location", "GET /api/v1/admin/assets/{id}/capture-location", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.assetCaptureLocation))),
	}
}

func (s *Server) workerRouteSpecs() []routeSpec {
	return []routeSpec{
		newRouteSpec("worker-enroll", "POST /api/v1/worker/enroll", routeAuthWorkerEnroll, http.HandlerFunc(s.enrollWorker)),
		newRouteSpec("worker-heartbeat", "POST /api/v1/worker/heartbeat", routeAuthWorker, http.HandlerFunc(s.workerHeartbeat)),
		newRouteSpec("worker-lease", "POST /api/v1/worker/lease", routeAuthWorker, http.HandlerFunc(s.workerLease)),
		newRouteSpec("worker-job-complete", "POST /api/v1/worker/jobs/{id}/complete", routeAuthWorker, http.HandlerFunc(s.workerCompleteJob)),
		newRouteSpec("worker-job-progress", "POST /api/v1/worker/jobs/{id}/progress", routeAuthWorker, http.HandlerFunc(s.workerProgress)),
		newRouteSpec("worker-job-credentials", "POST /api/v1/worker/jobs/{id}/credentials/{operation}", routeAuthWorker, http.HandlerFunc(s.workerCredential)),
		newRouteSpec("worker-job-provider", "POST /api/v1/worker/jobs/{id}/provider/{operation}", routeAuthWorker, http.HandlerFunc(s.workerProviderProxy)),
		newRouteSpec("worker-job-artifact-multipart", "POST /api/v1/worker/jobs/{id}/artifacts", routeAuthWorker, http.HandlerFunc(s.workerUploadArtifactMultipart)),
		newRouteSpec("worker-job-artifact-raw", "PUT /api/v1/worker/jobs/{id}/artifacts/{type}", routeAuthWorker, http.HandlerFunc(s.workerUploadArtifactRaw)),
	}
}

func (s *Server) pipelineRouteSpecs() []routeSpec {
	return []routeSpec{
		newRouteSpec("root-list", "GET /api/v1/roots", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.listRoots))),
		newRouteSpec("root-create", "POST /api/v1/roots", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.createRoot))),
		newRouteSpec("root-scan", "POST /api/v1/roots/{id}/scan", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.scanRoot))),
		newRouteSpec("root-discover", "POST /api/v1/roots/discover", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.discoverRoots))),
		newRouteSpec("root-inspect", "POST /api/v1/roots/inspect", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.inspectRoot))),
		newRouteSpec("root-health", "GET /api/v1/roots/health", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.rootsHealth))),
		newRouteSpec("worker-pairing-create", "POST /api/v1/hub/worker-pairings", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.createWorkerPairing))),
		newRouteSpec("worker-list", "GET /api/v1/hub/workers", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.listWorkers))),
		newRouteSpec("worker-revoke", "POST /api/v1/hub/workers/{id}/revoke", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.revokeWorker))),
		newRouteSpec("worker-job-status", "GET /api/v1/admin/worker-jobs/{id}", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.workerJobStatus))),
		newRouteSpec("worker-job-assignment", "POST /api/v1/admin/worker-jobs/{id}/assignment", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.setWorkerJobAssignment))),
		newRouteSpec("pipeline-run", "POST /api/v1/pipeline/run", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.runPipeline))),
		newRouteSpec("pipeline-retry-failed", "POST /api/v1/pipeline/retry-failed", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.retryFailedJobs))),
		newRouteSpec("pipeline-resume-deferred", "POST /api/v1/pipeline/resume-deferred", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.resumeDeferredJobs))),
		newRouteSpec("test-drive-run", "POST /api/v1/test-drive", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.runTestDrive))),
		newRouteSpec("test-drive-suggestions", "GET /api/v1/test-drive/suggestions", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.testDriveSuggestions))),
		newRouteSpec("pipeline-throttle-save", "PUT /api/v1/pipeline/throttle", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.savePipelineThrottle))),
	}
}

func (s *Server) searchRouteSpecs() []routeSpec {
	return []routeSpec{
		newRouteSpec("search-legacy", "GET /api/v1/search", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.search))),
		newRouteSpec("search-shots-legacy", "GET /api/v1/search/shots", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.searchShots))),
		newRouteSpec("search-shots-hybrid", "GET /api/v1/search/shots/hybrid", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.hybridSearchShots))),
		newRouteSpec("search-shots-v2", "POST /api/v1/search/shots", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.searchShotsV2))),
		newRouteSpec("shot-detail", "GET /api/v1/shots/{id}", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.shotDetail))),
		newRouteSpec("similar-shots", "GET /api/v1/shots/{id}/similar", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.similarShots))),
		newRouteSpec("rare-shots", "GET /api/v1/discover/rare-shots", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.rareShots))),
		newRouteSpec("tags-list", "GET /api/v1/tags", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.listTags))),
		newRouteSpec("tags-unresolved", "GET /api/v1/tags/unresolved", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.listUnresolvedTags))),
		newRouteSpec("tags-curate", "POST /api/v1/tags/curate", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.runTagCurator))),
		newRouteSpec("tags-clusters", "POST /api/v1/tags/clusters", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.runTagEmbeddingClusters))),
		newRouteSpec("tag-proposals", "GET /api/v1/tags/proposals", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.listTagProposals))),
		newRouteSpec("tag-proposal-review", "POST /api/v1/tags/proposals/{id}/review", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.reviewTagProposal))),
	}
}

func (s *Server) repurposeRouteSpecs() []routeSpec {
	return []routeSpec{
		newRouteSpec("library-summary", "GET /api/v1/library/summary", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.latestLibrarySummary))),
		newRouteSpec("library-summary-generate", "POST /api/v1/library/summary/generate", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.generateLibrarySummary))),
		newRouteSpec("repurpose-plan-list", "GET /api/v1/repurpose/plans", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.listRepurposePlans))),
		newRouteSpec("repurpose-plan-create", "POST /api/v1/repurpose/plans", routeAuthAgentOrAdmin, http.HandlerFunc(s.requireAgentOrAdmin(s.createRepurposePlan))),
		newRouteSpec("repurpose-plan-get", "GET /api/v1/repurpose/plans/{id}", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.getRepurposePlan))),
		newRouteSpec("repurpose-plan-revisions", "GET /api/v1/repurpose/plans/{id}/revisions", routeAuthTrustedRead, http.HandlerFunc(s.requireTrustedRead(s.listRepurposePlanRevisions))),
		newRouteSpec("repurpose-plan-revise", "POST /api/v1/repurpose/plans/{id}/revisions", routeAuthAgentOrAdmin, http.HandlerFunc(s.requireAgentOrAdmin(s.reviseRepurposePlan))),
		newRouteSpec("repurpose-plan-approve", "POST /api/v1/repurpose/plans/{id}/revisions/{revision}/approve", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.approveRepurposePlanRevision))),
		newRouteSpec("repurpose-plan-export-edl", "GET /api/v1/repurpose/plans/{id}/export.edl", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.exportRepurposePlanEDL))),
		newRouteSpec("repurpose-plan-export-fcpxml", "GET /api/v1/repurpose/plans/{id}/export.fcpxml", routeAuthHubAdmin, http.HandlerFunc(s.requireHubAdmin(s.exportRepurposePlanFCPXML))),
	}
}

func (s *Server) pageRouteSpecs() []routeSpec {
	return []routeSpec{
		newRouteSpec("page-library", "GET /{$}", routeAuthBrowserPage, http.HandlerFunc(s.index)),
		newRouteSpec("page-setup", "GET /setup", routeAuthBrowserPage, http.HandlerFunc(s.setupPage)),
		newRouteSpec("page-progress", "GET /progress", routeAuthBrowserPage, http.HandlerFunc(s.progressPage)),
		newRouteSpec("page-workers", "GET /workers", routeAuthBrowserPage, http.HandlerFunc(s.workersPage)),
		newRouteSpec("page-library-roots", "GET /library-roots", routeAuthBrowserPage, http.HandlerFunc(s.libraryRootsPage)),
		newRouteSpec("page-repurpose", "GET /repurpose", routeAuthBrowserPage, http.HandlerFunc(s.repurposePage)),
		newRouteSpec("page-tags", "GET /tags", routeAuthBrowserPage, http.HandlerFunc(s.tagsPage)),
		newRouteSpec("page-providers", "GET /providers", routeAuthBrowserPage, http.HandlerFunc(s.providersPage)),
		newRouteSpec("page-collections", "GET /collections", routeAuthBrowserPage, http.HandlerFunc(s.collectionsPage)),
		newRouteSpec("page-settings", "GET /settings", routeAuthBrowserPage, http.HandlerFunc(s.settingsPage)),
	}
}

func (s *Server) catchAllRouteSpecs() []routeSpec {
	return []routeSpec{
		newRouteSpec("api-catch-all", "/api/v1/", routeAuthCatchAll, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeAPIError(w, http.StatusNotFound, APIError{Code: "not_found", Message: "not found"})
		})),
	}
}

// routeAuthWorkerBootstrap guards the Worker binary download. It admits the
// existing trusted-read path (a trusted source address, or a Hub admin/agent
// token) for requests that carry no pairing credential, OR a valid, unredeemed
// X-Timingdex-Pairing-Token header, so an operator can bootstrap a Worker from
// a remote network before enrollment without making the route public. A token
// that IS presented is validated strictly — a redeemed or expired token gets a
// 403 even from a trusted network, never a silent trusted-read fallback, so a
// spent credential cannot be replayed after enrollment. The pairing token never
// travels in the URL and is never consumed here: only the worker enroll
// endpoint redeems it, so the same token can authorize the download and then
// enroll the Worker once.
func (s *Server) routeAuthWorkerBootstrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		raw := strings.TrimSpace(r.Header.Get("X-Timingdex-Pairing-Token"))
		if raw == "" {
			if s.fromTrustedNetwork(r) || s.isHubAdmin(r) || s.isHubAgent(r) {
				next(w, r)
				return
			}
			writeAPIError(w, http.StatusForbidden, APIError{Code: "trusted_read_denied", Message: "Worker binaries are restricted to trusted networks, a Hub token, or a valid pairing token", Action: "present_hub_token_or_pairing_token"})
			return
		}
		valid, err := s.service.WorkerPairingValid(r.Context(), raw, time.Now())
		if err != nil || !valid {
			writeAPIError(w, http.StatusForbidden, APIError{Code: "pairing_token_invalid", Message: "pairing token is invalid or expired", Action: "generate_a_new_pairing_token"})
			return
		}
		next(w, r)
	}
}
