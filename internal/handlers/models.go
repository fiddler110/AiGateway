package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/scottymacleod/aigateway/internal/server"
)

type modelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

// Models handles GET /v1/models — aggregates every configured upstream's
// declared model list into one OpenAI-shaped listing, de-duplicating by
// model id (first upstream to declare an id wins attribution).
func Models(srv *server.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := srv.CurrentState()
		if _, ok := st.Auth.Authenticate(r); !ok {
			server.WriteError(w, http.StatusUnauthorized, "invalid or missing credentials")
			return
		}
		seen := map[string]bool{}
		var data []modelEntry
		for name, up := range st.Cfg.Upstreams {
			for _, id := range up.Models {
				if seen[id] {
					continue
				}
				seen[id] = true
				data = append(data, modelEntry{ID: id, Object: "model", OwnedBy: name})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}
}
