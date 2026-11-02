package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/backup"
	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"golang.org/x/oauth2"
)

func backupRequest(h *BackupHandler, method, path string, body any) *httptest.ResponseRecorder {
	data, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, bytes.NewReader(data))
	w := httptest.NewRecorder()
	h.Route(w, r)
	return w
}
func TestGoogleDeviceFlow(t *testing.T) {
	for _, scenario := range []string{"success", "access_denied", "expired_token", "slow_down", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			store := testBackupStore(t)
			h := NewBackupHandler(store)
			defer h.BeginShutdown()
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				r.ParseForm()
				if r.URL.Path == "/device" {
					if !strings.Contains(r.Form.Get("scope"), "drive.file") {
						t.Error("wrong scope")
					}
					fmt.Fprint(w, `{"device_code":"private-device","user_code":"ABC-123","verification_url":"https://www.google.com/device","expires_in":60,"interval":1}`)
					return
				}
				n := requests.Add(1)
				if r.Form.Get("client_secret") != "client-secret" || r.Form.Get("device_code") != "private-device" {
					t.Error("missing device credentials")
				}
				if scenario == "access_denied" || scenario == "expired_token" {
					w.WriteHeader(400)
					fmt.Fprintf(w, `{"error":%q}`, scenario)
					return
				}
				if n == 1 {
					w.WriteHeader(400)
					code := "authorization_pending"
					if scenario == "slow_down" {
						code = "slow_down"
					}
					fmt.Fprintf(w, `{"error":%q}`, code)
					return
				}
				fmt.Fprint(w, `{"access_token":"access-private","token_type":"Bearer","refresh_token":"refresh-private","expires_in":3600}`)
			}))
			defer server.Close()
			previous := googleOAuthEndpoint
			googleOAuthEndpoint = oauth2.Endpoint{DeviceAuthURL: server.URL + "/device", TokenURL: server.URL + "/token", AuthStyle: oauth2.AuthStyleInParams}
			defer func() { googleOAuthEndpoint = previous }()
			w := backupRequest(h, "POST", "/api/backup/google/device/start", map[string]string{"label": "Personal", "client_id": "client-id", "client_secret": "client-secret"})
			if w.Code != 200 {
				t.Fatalf("start %d: %s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "private-device") {
				t.Fatal("device code leaked")
			}
			var started struct {
				SessionID       string `json:"session_id"`
				VerificationURL string `json:"verification_url"`
			}
			json.Unmarshal(w.Body.Bytes(), &started)
			if started.VerificationURL != "https://www.google.com/device" {
				t.Fatal("Google verification_url alias missing")
			}
			if scenario == "cancel" {
				w = backupRequest(h, "DELETE", "/api/backup/google/device/"+started.SessionID, nil)
				if w.Code != 200 {
					t.Fatal(w.Body.String())
				}
				return
			}
			deadline := time.Now().Add(12 * time.Second)
			for time.Now().Before(deadline) {
				w = backupRequest(h, "GET", "/api/backup/google/device/"+started.SessionID, nil)
				var result struct {
					Status string `json:"status"`
				}
				json.Unmarshal(w.Body.Bytes(), &result)
				if result.Status != "pending" {
					want := "connected"
					if scenario == "access_denied" {
						want = "denied"
					}
					if scenario == "expired_token" {
						want = "expired"
					}
					if result.Status != want {
						t.Fatalf("status=%s want=%s", result.Status, want)
					}
					for _, secret := range []string{"refresh-private", "access-private", "client-secret"} {
						if strings.Contains(w.Body.String(), secret) {
							t.Fatal("secret in session response")
						}
					}
					if want == "connected" {
						a, err := store.GetBackupGoogleAccount(1)
						if err != nil || a.RefreshToken != "refresh-private" {
							t.Fatal("refresh token not stored")
						}
					}
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
			t.Fatal("device flow timed out")
		})
	}
}
func TestDriveConfigRecoveryAndAccountGuards(t *testing.T) {
	store := testBackupStore(t)
	h := NewBackupHandler(store)
	a, err := store.CreateBackupGoogleAccount(db.BackupGoogleAccount{Label: "Personal", ClientID: "client", ClientSecret: "secret", RefreshToken: "refresh"})
	if err != nil {
		t.Fatal(err)
	}
	req := createBackupConfigRequest{Name: "offsite", Method: "rclone", TargetType: "gdrive", GoogleAccountID: &a.ID, LocalPath: "/data", RemotePath: "smoothnas/storage", Direction: "push", Encrypt: true, ScheduleDays: 127}
	w := backupRequest(h, "POST", "/api/backup/configs", req)
	if w.Code != 400 {
		t.Fatal("encrypted job saved without kit")
	}
	// Keys have already been obscured by rclone in the preparation endpoint.
	h.google.recovery["kit"] = recoverySession{AccountID: a.ID, RemotePath: req.RemotePath, Password: "crypt-secret", Salt: "salt-secret", Expires: time.Now().Add(time.Minute)}
	req.RecoverySession = "kit"
	req.RecoveryConfirmed = true
	w = backupRequest(h, "POST", "/api/backup/configs", req)
	if w.Code != 201 {
		t.Fatalf("create %d: %s", w.Code, w.Body.String())
	}
	for _, secret := range []string{"crypt-secret", "salt-secret", "refresh", "client_secret"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("secret leaked: %s", w.Body.String())
		}
	}
	cfg, err := store.GetBackupConfig(1)
	if err != nil || cfg.CryptPassword != "crypt-secret" {
		t.Fatal("missing encryption keys")
	}
	w = backupRequest(h, "DELETE", fmt.Sprintf("/api/backup/google/accounts/%d", a.ID), nil)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "offsite") {
		t.Fatal("in-use account disconnected")
	}
	runID, err := store.CreateBackupRun(cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.CreateBackupRun(cfg.ID); err != db.ErrDuplicate {
		t.Fatal("overlapping run allowed")
	}
	store.CompleteBackupRun(runID, "done")
	req.Encrypt = false
	w = backupRequest(h, "PUT", "/api/backup/configs/1", req)
	if w.Code != 400 {
		t.Fatal("encryption changed after run")
	}
	req.Encrypt = true
	req.Name = "renamed"
	w = backupRequest(h, "PUT", "/api/backup/configs/1", req)
	if w.Code != 200 {
		t.Fatalf("edit failed: %s", w.Body.String())
	}
	cfg, err = store.GetBackupConfig(1)
	if err != nil || cfg.CryptPassword != "crypt-secret" {
		t.Fatal("keys not preserved on edit")
	}
	w = backupRequest(h, "GET", "/api/backup/configs", nil)
	if strings.Contains(w.Body.String(), "crypt-secret") {
		t.Fatal("list leaked keys")
	}
}
func TestSchedulerClaimsAndSkipsActiveRuns(t *testing.T) {
	store := testBackupStore(t)
	cfg := testBackupConfig(t, store)
	cfg.ScheduleEnabled = true
	cfg.ScheduleTime = "02:00"
	cfg.ScheduleDays = 127
	if _, err := store.UpdateBackupConfig(cfg.ID, *cfg); err != nil {
		t.Fatal(err)
	}
	id, err := store.CreateBackupRun(cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)
	h := NewBackupHandler(store)
	h.scheduleTick(now)
	got, err := store.GetBackupConfig(cfg.ID)
	if err != nil || got.LastScheduledAt != "2026-11-01T02:00:00Z" {
		t.Fatalf("not claimed: %+v %v", got, err)
	}
	runs, _ := store.ListBackupRunsByConfig(cfg.ID, false)
	if len(runs) != 1 {
		t.Fatal("active schedule stacked")
	}
	store.CompleteBackupRun(id, "done")
	orig := runBackupFunc
	defer func() { runBackupFunc = orig }()
	var calls atomic.Int32
	runBackupFunc = func(context.Context, backup.Config, func(string, int, int)) (string, error) {
		calls.Add(1)
		return "done", nil
	}
	h = NewBackupHandler(store)
	h.scheduleTick(now)
	h.Wait()
	if calls.Load() != 0 {
		t.Fatal("restart repeated claimed slot")
	}
	h.scheduleTick(now.AddDate(0, 0, 1))
	h.Wait()
	if calls.Load() != 1 {
		t.Fatal("next slot was not run")
	}
	h.BeginShutdown()
	h.scheduleTick(now.AddDate(0, 0, 2))
	if calls.Load() != 1 {
		t.Fatal("ran while draining")
	}
}
func TestDriveValidation(t *testing.T) {
	id := int64(1)
	valid := createBackupConfigRequest{Name: "drive", Method: "rclone", TargetType: "gdrive", GoogleAccountID: &id, RemotePath: "backup/storage", LocalPath: "/data", Direction: "push"}
	if err := validateBackupConfig(valid); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", "/", "../backup", "a/../b", "a\nb", "a:b"} {
		req := valid
		req.RemotePath = path
		if validateBackupConfig(req) == nil {
			t.Errorf("accepted path %q", path)
		}
	}
	req := valid
	req.Direction = "pull"
	req.DeleteMode = true
	if validateBackupConfig(req) == nil {
		t.Fatal("destructive restore allowed")
	}
	req = valid
	req.ScheduleEnabled = true
	req.ScheduleTime = "25:00"
	req.ScheduleDays = 127
	if validateBackupConfig(req) == nil {
		t.Fatal("invalid schedule")
	}
}
