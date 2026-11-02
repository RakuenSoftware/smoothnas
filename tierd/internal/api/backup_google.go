package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/backup"
	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"golang.org/x/oauth2"
)

type googleSession struct {
	Status  string                  `json:"status"`
	Account *db.BackupGoogleAccount `json:"account,omitempty"`
	cancel  context.CancelFunc
	expires time.Time
}
type recoverySession struct {
	AccountID                  int64
	RemotePath, Password, Salt string
	Expires                    time.Time
}
type backupGoogleState struct {
	sync.Mutex
	sessions map[string]*googleSession
	recovery map[string]recoverySession
}

var googleOAuthEndpoint = oauth2.Endpoint{DeviceAuthURL: "https://oauth2.googleapis.com/device/code", TokenURL: "https://oauth2.googleapis.com/token", AuthStyle: oauth2.AuthStyleInParams}
var googleHTTPClient = &http.Client{Timeout: 30 * time.Second}

func randomSessionID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func (h *BackupHandler) googleRoute(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/backup/google/"), "/")
	switch {
	case path == "accounts":
		if r.Method != http.MethodGet {
			jsonMethodNotAllowed(w)
			return
		}
		accounts, err := h.store.ListBackupGoogleAccounts()
		if err != nil {
			serverError(w, err)
			return
		}
		json.NewEncoder(w).Encode(accounts)
	case strings.HasPrefix(path, "accounts/"):
		if r.Method != http.MethodDelete {
			jsonMethodNotAllowed(w)
			return
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(path, "accounts/"), 10, 64)
		if err != nil {
			jsonErrorCoded(w, "invalid account id", 400, "backup.invalid_account_id")
			return
		}
		configs, err := h.store.ListBackupConfigs()
		if err != nil {
			serverError(w, err)
			return
		}
		names := []string{}
		for _, c := range configs {
			if c.GoogleAccountID != nil && *c.GoogleAccountID == id {
				names = append(names, c.Name)
			}
		}
		if len(names) > 0 {
			jsonError(w, "Account is used by: "+strings.Join(names, ", "), 409)
			return
		}
		if err := h.store.DeleteBackupGoogleAccount(id); err != nil {
			jsonErrorCoded(w, "Account unavailable or still in use", 409, "backup.account_unavailable_or_still_in_use")
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "disconnected"})
	case path == "device/start":
		if r.Method != http.MethodPost {
			jsonMethodNotAllowed(w)
			return
		}
		h.startGoogleDevice(w, r)
	case strings.HasPrefix(path, "device/"):
		if r.Method != http.MethodGet && r.Method != http.MethodDelete {
			jsonMethodNotAllowed(w)
			return
		}
		id := strings.TrimPrefix(path, "device/")
		h.google.Lock()
		defer h.google.Unlock()
		s, ok := h.google.sessions[id]
		if !ok {
			jsonErrorCoded(w, "Sign-in session expired; connect again", 404, "backup.sign_in_session_expired_connect_again")
			return
		}
		if r.Method == http.MethodDelete {
			s.cancel()
			delete(h.google.sessions, id)
			json.NewEncoder(w).Encode(map[string]string{"status": "cancelled"})
			return
		}
		json.NewEncoder(w).Encode(s)
	case path == "recovery":
		if r.Method != http.MethodPost {
			jsonMethodNotAllowed(w)
			return
		}
		var req createBackupConfigRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			jsonInvalidRequestBody(w)
			return
		}
		req.RemotePath = strings.TrimSpace(req.RemotePath)
		if req.GoogleAccountID == nil || !validDrivePath(req.RemotePath) {
			jsonErrorCoded(w, "Google account and a relative Drive folder are required", 400, "backup.google_account_and_a_relative_drive_folder_are_required")
			return
		}
		a, err := h.store.GetBackupGoogleAccount(*req.GoogleAccountID)
		if err != nil {
			jsonErrorCoded(w, "Google account not found", 400, "backup.google_account_not_found")
			return
		}
		password, salt, err := backup.NewDriveKeys(r.Context())
		if err != nil {
			serverError(w, err)
			return
		}
		cfg := backup.Config{RemotePath: req.RemotePath, Drive: backup.DriveConfig{ClientID: a.ClientID, ClientSecret: a.ClientSecret, RefreshToken: a.RefreshToken, Encrypt: true, CryptPassword: password, CryptSalt: salt}}
		id := randomSessionID()
		h.google.Lock()
		for key, s := range h.google.recovery {
			if time.Now().After(s.Expires) {
				delete(h.google.recovery, key)
			}
		}
		if len(h.google.recovery) >= 64 {
			h.google.Unlock()
			jsonErrorCoded(w, "Too many recovery preparations; try again later", 429, "backup.too_many_recovery_preparations_try_again_later")
			return
		}
		h.google.recovery[id] = recoverySession{AccountID: a.ID, RemotePath: req.RemotePath, Password: password, Salt: salt, Expires: time.Now().Add(30 * time.Minute)}
		h.google.Unlock()
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(map[string]string{"session_id": id, "recovery_kit": backup.DriveRecoveryKit(cfg)})
	default:
		jsonNotFound(w)
	}
}
func (h *BackupHandler) startGoogleDevice(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Label        string `json:"label"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		jsonInvalidRequestBody(w)
		return
	}
	req.Label = strings.TrimSpace(req.Label)
	req.ClientID = strings.TrimSpace(req.ClientID)
	req.ClientSecret = strings.TrimSpace(req.ClientSecret)
	if req.Label == "" || strings.ContainsAny(req.ClientID+req.ClientSecret, "\r\n") || req.ClientID == "" || req.ClientSecret == "" {
		jsonErrorCoded(w, "Label, client ID and client secret are required", 400, "backup.label_client_id_and_client_secret_are_required")
		return
	}
	h.google.Lock()
	for id, s := range h.google.sessions {
		if time.Now().After(s.expires) {
			s.cancel()
			delete(h.google.sessions, id)
		}
	}
	full := len(h.google.sessions) >= 16
	h.google.Unlock()
	if full {
		jsonErrorCoded(w, "Too many sign-in sessions; cancel an existing attempt", 429, "backup.too_many_sign_in_sessions_cancel_an_existing_attempt")
		return
	}
	config := oauth2.Config{ClientID: req.ClientID, ClientSecret: req.ClientSecret, Scopes: []string{"openid", "email", "https://www.googleapis.com/auth/drive.file"}, Endpoint: googleOAuthEndpoint}
	ctx := context.WithValue(r.Context(), oauth2.HTTPClient, googleHTTPClient)
	auth, err := config.DeviceAuth(ctx)
	if err != nil {
		jsonErrorCoded(w, "Google could not start sign-in; check the OAuth client type and credentials", 502, "backup.google_could_not_start_sign_in_check_the_oauth_client_type_a")
		return
	}
	if auth.Expiry.IsZero() || auth.DeviceCode == "" || auth.UserCode == "" {
		jsonErrorCoded(w, "Invalid Google sign-in response", 502, "backup.invalid_google_sign_in_response")
		return
	}
	// Some Google responses use verification_url; oauth2 handles that alias.
	id := randomSessionID()
	pollCtx, cancel := context.WithDeadline(context.WithValue(context.Background(), oauth2.HTTPClient, googleHTTPClient), auth.Expiry)
	s := &googleSession{Status: "pending", cancel: cancel, expires: auth.Expiry.Add(10 * time.Minute)}
	h.google.Lock()
	h.google.sessions[id] = s
	h.google.Unlock()
	go func() {
		defer cancel()
		token, err := config.DeviceAccessToken(pollCtx, auth)
		h.google.Lock()
		defer h.google.Unlock()
		if h.google.sessions[id] != s || pollCtx.Err() == context.Canceled {
			return
		}
		if err != nil {
			s.Status = "failed"
			var retrieve *oauth2.RetrieveError
			if errors.As(err, &retrieve) {
				switch retrieve.ErrorCode {
				case "access_denied":
					s.Status = "denied"
				case "expired_token":
					s.Status = "expired"
				}
			}
			if errors.Is(err, context.DeadlineExceeded) {
				s.Status = "expired"
			}
			return
		}
		if token.RefreshToken == "" {
			s.Status = "failed"
			return
		}
		// Email is only a display label. It is never used for authorization.
		email := ""
		if raw, ok := token.Extra("id_token").(string); ok {
			parts := strings.Split(raw, ".")
			if len(parts) == 3 {
				if payload, e := base64.RawURLEncoding.DecodeString(parts[1]); e == nil {
					var claims struct {
						Email string `json:"email"`
					}
					if json.Unmarshal(payload, &claims) == nil {
						email = claims.Email
					}
				}
			}
		}
		account, err := h.store.CreateBackupGoogleAccount(db.BackupGoogleAccount{Label: req.Label, Email: email, ClientID: req.ClientID, ClientSecret: req.ClientSecret, RefreshToken: token.RefreshToken})
		if err != nil {
			s.Status = "failed"
			return
		}
		s.Status = "connected"
		s.Account = account
	}()
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{"session_id": id, "user_code": auth.UserCode, "verification_url": auth.VerificationURI, "expires_in": int(time.Until(auth.Expiry).Seconds())})
}
func validDrivePath(path string) bool {
	if path == "" || strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\r\n\x00\\:") {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "." || part == ".." || part == "" {
			return false
		}
	}
	return true
}
func (h *BackupHandler) driveFields(req createBackupConfigRequest, old *db.BackupConfig) (string, string, error) {
	if req.Method != "rclone" {
		if old != nil && old.Encrypt {
			return "", "", fmt.Errorf("encrypted Drive jobs cannot change method")
		}
		return "", "", nil
	}
	if req.GoogleAccountID == nil {
		return "", "", fmt.Errorf("Google account is required")
	}
	if _, err := h.store.GetBackupGoogleAccount(*req.GoogleAccountID); err != nil {
		return "", "", fmt.Errorf("Google account not found")
	}
	if old != nil && old.HasRun && req.Encrypt != old.Encrypt {
		return "", "", fmt.Errorf("encryption cannot change after the first run")
	}
	if !req.Encrypt {
		return "", "", nil
	}
	if old != nil && old.Encrypt {
		if old.GoogleAccountID == nil || *old.GoogleAccountID != *req.GoogleAccountID || old.RemotePath != req.RemotePath {
			return "", "", fmt.Errorf("encrypted jobs keep the account and folder recorded in their recovery kit; create a new job for another destination")
		}
		return old.CryptPassword, old.CryptSalt, nil
	}
	h.google.Lock()
	defer h.google.Unlock()
	s, ok := h.google.recovery[req.RecoverySession]
	if !ok || time.Now().After(s.Expires) || s.AccountID != *req.GoogleAccountID || s.RemotePath != req.RemotePath || !req.RecoveryConfirmed {
		return "", "", fmt.Errorf("download and acknowledge the recovery kit before saving")
	}
	return s.Password, s.Salt, nil
}

func (h *BackupHandler) consumeRecovery(id string) {
	h.google.Lock()
	defer h.google.Unlock()
	delete(h.google.recovery, id)
}

var backupValidationCodes = map[string]string{
	"schedule requires HH:MM and weekdays between 1 and 127": "backup.schedule_invalid",
	"Google Drive requires a Google account":                 "backup.google_account_required",
	"Drive folder must be a nonempty relative path":          "backup.drive_folder_invalid",
	"local_path must be absolute":                            "backup.local_path_absolute",
	"direction must be push or pull":                         "backup.direction_invalid",
	"Drive restores never delete local files":                "backup.drive_restore_no_delete",
	"invalid excludes":                                       "backup.excludes_invalid",
	"Google account and encryption require Google Drive":     "backup.drive_fields_invalid",
	"encrypted Drive jobs cannot change method":              "backup.encrypted_method_fixed",
	"Google account is required":                             "backup.google_account_required",
	"Google account not found":                               "backup.google_account_not_found",
	"encryption cannot change after the first run":           "backup.encryption_fixed",
	"encrypted jobs keep the account and folder recorded in their recovery kit; create a new job for another destination": "backup.encrypted_target_fixed",
	"download and acknowledge the recovery kit before saving":                                                             "backup.recovery_required",
}

func writeBackupValidationError(w http.ResponseWriter, err error) {
	if code, ok := backupValidationCodes[err.Error()]; ok {
		jsonErrorCoded(w, err.Error(), http.StatusBadRequest, code)
		return
	}
	jsonError(w, err.Error(), http.StatusBadRequest)
}
