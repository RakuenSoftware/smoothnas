package api

import (
	"encoding/json"
	"net/http"
)

// jsonError writes a JSON error response with the correct Content-Type header.
// It safely encodes the message, so newlines, quotes, and backslashes in
// error output from external commands (mdadm, lvm2, etc.) cannot break the JSON.
func jsonError(w http.ResponseWriter, message string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func serverError(w http.ResponseWriter, err error) {
	jsonError(w, "internal server error: "+err.Error(), http.StatusInternalServerError)
}
