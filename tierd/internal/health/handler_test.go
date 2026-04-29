package health

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHandlerAggregatesWarnings(t *testing.T) {
	handler := NewHandler("test", time.Now(), func(context.Context) []Check {
		return []Check{{Name: "probe", Status: "warning", Message: "slow"}}
	})

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("ServeHTTP code = %d", w.Code)
	}
	var resp healthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "warning" {
		t.Fatalf("status = %q, want warning", resp.Status)
	}
	if len(resp.Checks) != 1 || resp.Checks[0].Name != "probe" {
		t.Fatalf("checks = %#v", resp.Checks)
	}
}

func TestAggregateStatusCriticalWins(t *testing.T) {
	got := aggregateStatus([]Check{
		{Name: "a", Status: "warning"},
		{Name: "b", Status: "critical"},
	})
	if got != "critical" {
		t.Fatalf("aggregateStatus = %q, want critical", got)
	}
}
