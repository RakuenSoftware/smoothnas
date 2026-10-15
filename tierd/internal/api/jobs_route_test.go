package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sgauth "github.com/RakuenSoftware/smoothgui/auth"
)

func TestJobCollectionRoutesWithPersistedSession(t *testing.T) {
	store, router := setupTest(t)
	token, err := sgauth.NewSessionStore(store.DB(), time.Hour).CreateSession("jobs-test")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/jobs?tag=array-create", "/api/jobs/?tag=array-create"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.AddCookie(&http.Cookie{Name: "session", Value: token})
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			if w.Code != http.StatusOK || w.Body.String() != "[]\n" {
				t.Fatalf("collection: HTTP %d %s", w.Code, w.Body.String())
			}
		})
	}
}
