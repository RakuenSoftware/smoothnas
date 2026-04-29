package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocaleHandler_FileMissing(t *testing.T) {
	h := &LocaleHandler{configPath: filepath.Join(t.TempDir(), "missing")}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/locale", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: want 200 got %d", rr.Code)
	}
	if got, want := strings.TrimSpace(rr.Body.String()), `{"language":""}`; got != want {
		t.Errorf("body: want %q got %q", want, got)
	}
}

func TestLocaleHandler_FilePresent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "locale")
	if err := os.WriteFile(path, []byte("nl\n"), 0644); err != nil {
		t.Fatal(err)
	}
	h := &LocaleHandler{configPath: path}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/locale", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: want 200 got %d", rr.Code)
	}
	if got, want := strings.TrimSpace(rr.Body.String()), `{"language":"nl"}`; got != want {
		t.Errorf("body: want %q got %q", want, got)
	}
}

func TestLocaleHandler_FileGarbage(t *testing.T) {
	// A corrupt or attacker-controlled file shouldn't be able to
	// inject JSON into the response. validLocaleTag rejects
	// anything that isn't a clean BCP-47-style short tag.
	for _, in := range []string{
		`","language2":"`,
		`<script>alert(1)</script>`,
		`xx-yy-zz`,
		`123`,
		``,
		` `,
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, "locale")
		if err := os.WriteFile(path, []byte(in), 0644); err != nil {
			t.Fatal(err)
		}
		h := &LocaleHandler{configPath: path}
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/locale", nil)
		h.ServeHTTP(rr, req)
		if got, want := strings.TrimSpace(rr.Body.String()), `{"language":""}`; got != want {
			t.Errorf("input %q: want %q got %q", in, want, got)
		}
	}
}

func TestLocaleHandler_AcceptedTags(t *testing.T) {
	for _, tag := range []string{"en", "nl", "en-US", "fr-FR"} {
		dir := t.TempDir()
		path := filepath.Join(dir, "locale")
		if err := os.WriteFile(path, []byte(tag+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
		h := &LocaleHandler{configPath: path}
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/locale", nil)
		h.ServeHTTP(rr, req)
		want := `{"language":"` + tag + `"}`
		if got := strings.TrimSpace(rr.Body.String()); got != want {
			t.Errorf("tag %q: want %q got %q", tag, want, got)
		}
	}
}

func TestLocaleHandler_MethodNotAllowed(t *testing.T) {
	h := NewLocaleHandler()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/locale", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status: want 405 got %d", rr.Code)
	}
}
