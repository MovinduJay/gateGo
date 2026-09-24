package gateway

import (
	"crypto/subtle"
	"net/http"
)

func (g *Gateway) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	provided := []byte(r.Header.Get("X-Admin-Key"))
	expected := []byte(g.adminKey)
	if len(expected) > 0 && len(provided) == len(expected) && subtle.ConstantTimeCompare(provided, expected) == 1 {
		return true
	}
	g.respondJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
	return false
}
