// Package integration runs end-to-end tests against a real tierd instance.
// These tests require root (PAM authentication + system user management).
package integration_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	sgauth "github.com/RakuenSoftware/smoothgui/auth"

	"github.com/JBailes/SmoothNAS/tierd/internal/api"
	"github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/smart"
)

func skipIfNotRoot(t *testing.T) {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("skipping: requires root for PAM and system user management")
	}
}

type testEnv struct {
	store   *db.Store
	handler http.Handler
	history *smart.HistoryStore
	alarms  *smart.AlarmStore
}

func setupEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	store, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	history, err := smart.NewHistoryStore(store.DB())
	if err != nil {
		t.Fatalf("history store: %v", err)
	}
	alarms, err := smart.NewAlarmStore(store.DB())
	if err != nil {
		t.Fatalf("alarm store: %v", err)
	}

	handler := api.NewRouterFull(store, "test", time.Now(), history, alarms, nil)
	return &testEnv{store: store, handler: handler, history: history, alarms: alarms}
}

func createTestUser(t *testing.T, username, password string) {
	t.Helper()
	users := sgauth.NewUserManager("tierd")
	users.EnsureGroup()
	if err := users.Create(username, password); err != nil {
		t.Fatalf("create test user: %v", err)
	}
	t.Cleanup(func() {
		exec.Command("userdel", "--remove", username).Run()
	})
}

func doRequest(t *testing.T, env *testEnv, method, path string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var reqBody *bytes.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reqBody = bytes.NewReader(data)
	} else {
		reqBody = bytes.NewReader(nil)
	}

	req := httptest.NewRequest(method, path, reqBody)
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	env.handler.ServeHTTP(w, req)
	return w
}

func login(t *testing.T, env *testEnv, username, password string) *http.Cookie {
	t.Helper()
	w := doRequest(t, env, "POST", "/api/auth/login",
		map[string]string{"username": username, "password": password}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("login failed: %d %s", w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == "session" {
			return c
		}
	}
	t.Fatal("no session cookie")
	return nil
}

// --- Integration tests ---

func TestFullAuthFlow(t *testing.T) {
	skipIfNotRoot(t)
	env := setupEnv(t)
	createTestUser(t, "integ-auth", "testpass12345")

	// Login.
	cookie := login(t, env, "integ-auth", "testpass12345")

	// Access protected endpoint.
	w := doRequest(t, env, "GET", "/api/users", nil, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("list users: %d %s", w.Code, w.Body.String())
	}

	// Change password.
	w = doRequest(t, env, "PUT", "/api/auth/password", map[string]string{
		"current_password": "testpass12345",
		"new_password":     "newpass12345",
	}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("change password: %d %s", w.Code, w.Body.String())
	}

	// Login with new password.
	cookie = login(t, env, "integ-auth", "newpass12345")

	// Logout.
	w = doRequest(t, env, "POST", "/api/auth/logout", nil, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("logout: %d %s", w.Code, w.Body.String())
	}

	// Should be rejected after logout.
	w = doRequest(t, env, "GET", "/api/users", nil, cookie)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 after logout, got %d", w.Code)
	}
}

func TestUserCRUDFlow(t *testing.T) {
	skipIfNotRoot(t)
	env := setupEnv(t)
	createTestUser(t, "integ-admin", "adminpass12345")

	cookie := login(t, env, "integ-admin", "adminpass12345")

	// Create a new user.
	w := doRequest(t, env, "POST", "/api/users", map[string]string{
		"username": "integ-newuser",
		"password": "newuserpass12345",
	}, cookie)
	if w.Code != http.StatusCreated {
		t.Fatalf("create user: %d %s", w.Code, w.Body.String())
	}
	t.Cleanup(func() {
		exec.Command("userdel", "--remove", "integ-newuser").Run()
	})

	// The new user should be able to login.
	newCookie := login(t, env, "integ-newuser", "newuserpass12345")
	_ = newCookie

	// List users should include both.
	w = doRequest(t, env, "GET", "/api/users", nil, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("list users: %d %s", w.Code, w.Body.String())
	}

	var users []map[string]any
	json.Unmarshal(w.Body.Bytes(), &users)
	found := 0
	for _, u := range users {
		name, _ := u["username"].(string)
		if name == "integ-admin" || name == "integ-newuser" {
			found++
		}
	}
	if found < 2 {
		t.Fatalf("expected both users, found %d", found)
	}

	// Delete the new user.
	w = doRequest(t, env, "DELETE", "/api/users/integ-newuser", nil, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("delete user: %d %s", w.Code, w.Body.String())
	}

	// Deleted user should not be able to login (session invalidated).
	w = doRequest(t, env, "GET", "/api/users", nil, newCookie)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for deleted user, got %d", w.Code)
	}
}

func TestHealthEndpointNoAuth(t *testing.T) {
	env := setupEnv(t)

	// Health should be accessible without authentication.
	w := doRequest(t, env, "GET", "/api/health", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("health: %d %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", resp["status"])
	}
	if resp["version"] != "test" {
		t.Fatalf("expected version test, got %v", resp["version"])
	}
}

func TestProtectedEndpointsRequireAuth(t *testing.T) {
	env := setupEnv(t)

	endpoints := []struct {
		method string
		path   string
	}{
		{"GET", "/api/users"},
		{"GET", "/api/disks"},
		{"GET", "/api/arrays"},
		{"GET", "/api/pools"},
		{"GET", "/api/datasets"},
		{"GET", "/api/zvols"},
		{"GET", "/api/snapshots"},
		{"GET", "/api/protocols"},
		{"GET", "/api/smb/shares"},
		{"GET", "/api/nfs/exports"},
		{"GET", "/api/iscsi/targets"},
		{"GET", "/api/network/interfaces"},
		{"GET", "/api/smart/alarms"},
	}

	for _, ep := range endpoints {
		w := doRequest(t, env, ep.method, ep.path, nil, nil)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: expected 401, got %d", ep.method, ep.path, w.Code)
		}
	}
}

func TestSMARTAlarmFlow(t *testing.T) {
	skipIfNotRoot(t)
	env := setupEnv(t)
	createTestUser(t, "integ-smart", "smartpass12345")
	cookie := login(t, env, "integ-smart", "smartpass12345")

	// List default alarm rules.
	w := doRequest(t, env, "GET", "/api/smart/alarms", nil, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("list alarms: %d %s", w.Code, w.Body.String())
	}

	var rules []map[string]any
	json.Unmarshal(w.Body.Bytes(), &rules)
	if len(rules) != 8 {
		t.Fatalf("expected 8 default alarm rules, got %d", len(rules))
	}

	// Create a custom alarm rule.
	w = doRequest(t, env, "POST", "/api/smart/alarms", map[string]any{
		"attribute_id":   199,
		"attribute_name": "UDMA_CRC_Error_Count",
		"warning_above":  100,
		"critical_above": 500,
	}, cookie)
	if w.Code != http.StatusCreated {
		t.Fatalf("create alarm: %d %s", w.Code, w.Body.String())
	}

	// Should now have 9 rules.
	w = doRequest(t, env, "GET", "/api/smart/alarms", nil, cookie)
	json.Unmarshal(w.Body.Bytes(), &rules)
	if len(rules) != 9 {
		t.Fatalf("expected 9 rules, got %d", len(rules))
	}

	// Get the ID of the custom rule (last one).
	customID := rules[len(rules)-1]["id"].(float64)

	// Delete it.
	w = doRequest(t, env, "DELETE", "/api/smart/alarms/"+formatFloat(customID), nil, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("delete alarm: %d %s", w.Code, w.Body.String())
	}

	// Back to 8.
	w = doRequest(t, env, "GET", "/api/smart/alarms", nil, cookie)
	json.Unmarshal(w.Body.Bytes(), &rules)
	if len(rules) != 8 {
		t.Fatalf("expected 8 rules after delete, got %d", len(rules))
	}
}

func TestRateLimiting(t *testing.T) {
	skipIfNotRoot(t)
	env := setupEnv(t)
	createTestUser(t, "integ-rate", "ratepass12345")

	// Make 5 failed login attempts.
	for i := 0; i < 5; i++ {
		doRequest(t, env, "POST", "/api/auth/login",
			map[string]string{"username": "integ-rate", "password": "wrong"}, nil)
	}

	// 6th attempt should be rate limited, even with correct password.
	w := doRequest(t, env, "POST", "/api/auth/login",
		map[string]string{"username": "integ-rate", "password": "ratepass12345"}, nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", w.Code)
	}
}

func TestDiskEndpointAuthenticated(t *testing.T) {
	skipIfNotRoot(t)
	env := setupEnv(t)
	createTestUser(t, "integ-disk", "diskpass12345")
	cookie := login(t, env, "integ-disk", "diskpass12345")

	// List disks (should succeed, returns whatever lsblk finds).
	w := doRequest(t, env, "GET", "/api/disks", nil, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("list disks: %d %s", w.Code, w.Body.String())
	}

	var disks []map[string]any
	json.Unmarshal(w.Body.Bytes(), &disks)
	// Should be a valid JSON array (may be empty in test env).
	if disks == nil {
		t.Fatal("expected non-nil disk array")
	}
}

func TestAlarmHistoryEndpoint(t *testing.T) {
	skipIfNotRoot(t)
	env := setupEnv(t)
	createTestUser(t, "integ-hist", "histpass12345")
	cookie := login(t, env, "integ-hist", "histpass12345")

	// Alarm history should return empty array initially.
	w := doRequest(t, env, "GET", "/api/smart/alarms/history", nil, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("alarm history: %d %s", w.Code, w.Body.String())
	}

	var events []map[string]any
	json.Unmarshal(w.Body.Bytes(), &events)
	if len(events) != 0 {
		t.Fatalf("expected 0 events initially, got %d", len(events))
	}
}

func TestSessionSlidingWindow(t *testing.T) {
	skipIfNotRoot(t)
	env := setupEnv(t)
	createTestUser(t, "integ-slide", "slidepass12345")

	cookie := login(t, env, "integ-slide", "slidepass12345")

	// Access an endpoint (should extend session).
	w := doRequest(t, env, "GET", "/api/disks", nil, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("first access: %d", w.Code)
	}

	// Access again (session should still be valid).
	w = doRequest(t, env, "GET", "/api/disks", nil, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("second access: %d", w.Code)
	}
}

func TestPreventSelfDeletion(t *testing.T) {
	skipIfNotRoot(t)
	env := setupEnv(t)
	createTestUser(t, "integ-self", "selfpass12345")

	cookie := login(t, env, "integ-self", "selfpass12345")

	// Try to delete yourself.
	w := doRequest(t, env, "DELETE", "/api/users/integ-self", nil, cookie)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for self-deletion, got %d %s", w.Code, w.Body.String())
	}
}

// --- helpers ---

func formatFloat(f float64) string {
	n := int(f)
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}
