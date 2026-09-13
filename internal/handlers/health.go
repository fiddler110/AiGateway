package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/scottymacleod/aigateway/internal/server"
)

// Health handles GET /health: an unauthenticated liveness probe for
// orchestrators and load balancers. It returns only {"status":"ok"} so it
// doesn't disclose upstream names or circuit state (P0.14).
func Health(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

// HealthDetail handles GET /health/detail: per-upstream circuit state,
// behind the same auth as /v1. P1.6's /metrics will supersede it.
func HealthDetail(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := srv.CurrentState()
		if _, ok := st.Auth.Authenticate(r); !ok {
			server.WriteError(w, http.StatusUnauthorized, "invalid or missing credentials")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":          "ok",
			"upstream_health": st.UpstreamMgr.Status(),
		})
	}
}
