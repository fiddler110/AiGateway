package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/scottymacleod/aigateway/internal/server"
)

// Health handles GET /health — unauthenticated by design, matching the
// reference gateway's contract (health checks from orchestrators/load
// balancers must not require a credential).
func Health(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := srv.CurrentState()
		names := make([]string, 0, len(st.Cfg.Upstreams))
		for name := range st.Cfg.Upstreams {
			names = append(names, name)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":          "ok",
			"upstreams":       names,
			"upstream_health": st.UpstreamMgr.Status(),
		})
	}
}
