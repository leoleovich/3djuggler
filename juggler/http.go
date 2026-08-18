package juggler

import (
	"net/http"
)

// SetHeaders sets the CORS headers used by every juggler endpoint, so the
// intern page can query the daemon directly from a browser. Callers set their
// own Content-Type: not every endpoint returns JSON.
func SetHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Allow-Methods", "GET")
}

// SetJSONHeaders sets the CORS headers plus a JSON content type.
func SetJSONHeaders(w http.ResponseWriter) {
	SetHeaders(w)
	w.Header().Set("Content-Type", "application/json")
}
