package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNetworkCreateBondRejectsRuntimeInterfaces(t *testing.T) {
	h := NewNetworkHandler(nil)
	req := httptest.NewRequest(http.MethodPost, "/api/network/bonds", strings.NewReader(`{
		"name":"bond0",
		"mode":"balance-alb",
		"members":["enp1s0","veth1234"]
	}`))
	w := httptest.NewRecorder()

	h.Route(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "cannot be used as a bond member") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestNetworkUpdateBondRejectsRuntimeInterfaces(t *testing.T) {
	h := NewNetworkHandler(nil)
	req := httptest.NewRequest(http.MethodPut, "/api/network/bonds/bond0", strings.NewReader(`{
		"mode":"balance-alb",
		"members":["gow0"]
	}`))
	w := httptest.NewRecorder()

	h.Route(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "cannot be used as a bond member") {
		t.Fatalf("body = %s", w.Body.String())
	}
}
