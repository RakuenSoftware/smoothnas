package api_test

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
)

// These tests require root (for PAM authentication and system user management).
// Skip if not running as root.
func skipIfNotRoot(t *testing.T) {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("skipping: requires root for PAM and system user management")
	}
}

func setupTest(t *testing.T) (*db.Store, http.Handler) {
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

	router := api.NewRouter(store, "test", time.Now())
	return store, router
}

// createTestUser creates a system user for testing and returns a cleanup function.
func createTestUser(t *testing.T, username, password string) {
	t.Helper()

	// Ensure tierd group exists.
	users := sgauth.NewUserManager("tierd")
	users.EnsureGroup()

	// Create user.
	if err := users.Create(username, password); err != nil {
		t.Fatalf("create test user: %v", err)
	}

	t.Cleanup(func() {
		exec.Command("userdel", "--remove", username).Run()
	})
}

func loginAs(t *testing.T, router http.Handler, username, password string) *http.Cookie {
	t.Helper()

	body, _ := json.Marshal(map[string]string{
		"username": username,
		"password": password,
	})
	req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(body))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("login failed: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	for _, c := range w.Result().Cookies() {
		if c.Name == "session" {
			return c
		}
	}
	t.Fatal("no session cookie returned")
	return nil
}

func TestHealthEndpoint(t *testing.T) {
	_, router := setupTest(t)

	req := httptest.NewRequest("GET", "/api/health", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "ok" {
		t.Fatalf("expected status ok, got %s", resp["status"])
	}
}

func TestLoginLogout(t *testing.T) {
	skipIfNotRoot(t)
	_, router := setupTest(t)
	createTestUser(t, "tierd-test-login", "testpass123")

	cookie := loginAs(t, router, "tierd-test-login", "testpass123")

	// Logout.
	req := httptest.NewRequest("POST", "/api/auth/logout", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("logout: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Session should be invalid after logout.
	req = httptest.NewRequest("GET", "/api/users", nil)
	req.AddCookie(cookie)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 after logout, got %d", w.Code)
	}
}

func TestLoginBadCredentials(t *testing.T) {
	skipIfNotRoot(t)
	_, router := setupTest(t)
	createTestUser(t, "tierd-test-bad", "testpass123")

	body, _ := json.Marshal(map[string]string{
		"username": "tierd-test-bad",
		"password": "wrongpassword",
	})
	req := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(body))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUnauthenticatedAccessDenied(t *testing.T) {
	_, router := setupTest(t)

	req := httptest.NewRequest("GET", "/api/users", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUserCRUD(t *testing.T) {
	skipIfNotRoot(t)
	_, router := setupTest(t)
	createTestUser(t, "tierd-test-admin", "adminpass123")

	cookie := loginAs(t, router, "tierd-test-admin", "adminpass123")

	// Create user via API.
	createBody, _ := json.Marshal(map[string]string{
		"username": "tierd-test-new",
		"password": "newuserpass123",
	})
	req := httptest.NewRequest("POST", "/api/users", bytes.NewReader(createBody))
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("create user: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// Clean up the created user.
	t.Cleanup(func() {
		exec.Command("userdel", "--remove", "tierd-test-new").Run()
	})

	// List users.
	req = httptest.NewRequest("GET", "/api/users", nil)
	req.AddCookie(cookie)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("list users: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var users []map[string]any
	json.Unmarshal(w.Body.Bytes(), &users)

	// Should have at least the two test users.
	found := 0
	for _, u := range users {
		name, _ := u["username"].(string)
		if name == "tierd-test-admin" || name == "tierd-test-new" {
			found++
		}
	}
	if found < 2 {
		t.Fatalf("expected at least 2 tierd users, found %d in %d total", found, len(users))
	}

	// Delete user.
	req = httptest.NewRequest("DELETE", "/api/users/tierd-test-new", nil)
	req.AddCookie(cookie)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("delete user: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify deleted.
	if sgauth.UserExists("tierd-test-new") {
		t.Fatal("user should have been deleted")
	}
}

func TestChangePassword(t *testing.T) {
	skipIfNotRoot(t)
	_, router := setupTest(t)
	createTestUser(t, "tierd-test-pw", "oldpass12345")

	cookie := loginAs(t, router, "tierd-test-pw", "oldpass12345")

	// Change password.
	pwBody, _ := json.Marshal(map[string]string{
		"current_password": "oldpass12345",
		"new_password":     "newpass12345",
	})
	req := httptest.NewRequest("PUT", "/api/auth/password", bytes.NewReader(pwBody))
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("change password: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Login with new password.
	loginBody, _ := json.Marshal(map[string]string{
		"username": "tierd-test-pw",
		"password": "newpass12345",
	})
	req = httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(loginBody))
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("login with new password: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}
