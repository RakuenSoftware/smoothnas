package api

import (
	"encoding/json"
	"net/http"
)

// jsonError writes a JSON error response with the correct Content-Type header.
// It safely encodes the message, so newlines, quotes, and backslashes in
// error output from external commands (mdadm, lvm2, etc.) cannot break the JSON.
func jsonError(w http.ResponseWriter, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// jsonErrorCoded is the same as jsonError but additionally emits a stable
// `code` field that frontends can use to look up a localised message
// (via SmoothGUI's extractError translator). The message remains the
// English fallback for callers that haven't been updated to translate.
//
// Codes are dotted lowercase identifiers, e.g. "auth.invalid_credentials"
// or "language.invalid_tag". Group them by surface so they remain stable
// when error text gets reworded.
func jsonErrorCoded(w http.ResponseWriter, message string, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message, "code": code})
}

func serverError(w http.ResponseWriter, err error) {
	jsonError(w, "internal server error: "+err.Error(), http.StatusInternalServerError)
}

// jsonMethodNotAllowed writes the canonical 405 response. Use it
// for the method-guard at the top of HTTP handlers that only
// accept a specific verb. Frontends translate the stable code
// `request.method_not_allowed` via SmoothGUI's useExtractError.
func jsonMethodNotAllowed(w http.ResponseWriter) {
	jsonErrorCoded(w, "method not allowed", http.StatusMethodNotAllowed, "request.method_not_allowed")
}

// jsonInvalidRequestBody writes the canonical 400 response for a
// JSON-decode failure. Frontends translate the stable code
// `request.invalid_body`.
func jsonInvalidRequestBody(w http.ResponseWriter) {
	jsonErrorCoded(w, "invalid request body", http.StatusBadRequest, "request.invalid_body")
}

// jsonNotFound writes the canonical 404 response for the
// fallthrough default in route dispatchers — i.e. the request URL
// did not match any handler. Resource-specific 404s (pool not
// found, namespace not found, etc.) keep their surface-specific
// codes; this helper covers only the generic route miss.
func jsonNotFound(w http.ResponseWriter) {
	jsonErrorCoded(w, "not found", http.StatusNotFound, "request.not_found")
}

// jsonAuthRequired writes the canonical 401 response for
// authenticated handlers that find no session in the context.
// Mirrors the smoothgui auth middleware's `auth.required` code so
// the UI surfaces the same message regardless of which layer
// rejected the request.
func jsonAuthRequired(w http.ResponseWriter) {
	jsonErrorCoded(w, "authentication required", http.StatusUnauthorized, "auth.required")
}
