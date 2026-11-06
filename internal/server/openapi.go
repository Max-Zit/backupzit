package server

import (
	_ "embed"
	"encoding/json"
	"net/http"
)

// openAPISpec describes the REST API (OpenAPI 3.0). TestOpenAPIMatchesRoutes
// keeps it in step with the routes.
//
//go:embed openapi.json
var openAPISpec []byte

// handleOpenAPI serves the specification without a token: it holds nothing
// that the public documentation does not, and tools fetch it before they
// have one. The console's version is added as info.x-backupzit-version.
func (s *Server) handleOpenAPI(w http.ResponseWriter, _ *http.Request) {
	var doc map[string]any
	if err := json.Unmarshal(openAPISpec, &doc); err != nil {
		http.Error(w, "invalid specification", http.StatusInternalServerError)
		return
	}
	if info, ok := doc["info"].(map[string]any); ok {
		info["x-backupzit-version"] = s.Version
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	_ = json.NewEncoder(w).Encode(doc)
}
