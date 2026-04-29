package api

import (
	"encoding/json"
	"net/http"

	sgauth "github.com/RakuenSoftware/smoothgui/auth"
	"github.com/JBailes/SmoothNAS/tierd/internal/db"
)

// UserPrefsHandler serves authenticated GET / PUT /api/users/me/language.
// Cross-browser language persistence: the LanguagePicker writes the
// user's choice here so a different browser logging in as the same
// user sees the same language.
type UserPrefsHandler struct {
	store *db.Store
}

func NewUserPrefsHandler(store *db.Store) *UserPrefsHandler {
	return &UserPrefsHandler{store: store}
}

func (h *UserPrefsHandler) Route(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/users/me/language" {
		jsonNotFound(w)
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.getLanguage(w, r)
	case http.MethodPut:
		h.setLanguage(w, r)
	default:
		jsonMethodNotAllowed(w)
	}
}

func (h *UserPrefsHandler) getLanguage(w http.ResponseWriter, r *http.Request) {
	username := sgauth.GetUsername(r)
	if username == "" {
		jsonAuthRequired(w)
		return
	}
	lang, err := h.store.GetUserLanguage(username)
	if err != nil {
		jsonError(w, "load language: "+err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"language": lang})
}

func (h *UserPrefsHandler) setLanguage(w http.ResponseWriter, r *http.Request) {
	username := sgauth.GetUsername(r)
	if username == "" {
		jsonAuthRequired(w)
		return
	}
	var req struct {
		Language string `json:"language"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonInvalidRequestBody(w)
		return
	}
	// Validate. Same whitelist as /api/locale: BCP-47-style short
	// tag, lowercase + optional region. Empty string clears the
	// preference (sets back to "no preference recorded").
	if req.Language != "" && !validLocaleTag(req.Language) {
		jsonErrorCoded(w, "invalid language tag", http.StatusBadRequest, "language.invalid_tag")
		return
	}
	if err := h.store.SetUserLanguage(username, req.Language); err != nil {
		jsonError(w, "save language: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write([]byte(`{"status":"ok"}`))
}
