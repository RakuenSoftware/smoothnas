package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestJSONError_NoCode(t *testing.T) {
	rr := httptest.NewRecorder()
	jsonError(rr, "boom", http.StatusBadRequest)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400 got %d", rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("content-type: want application/json got %q", got)
	}
	var body map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["error"] != "boom" {
		t.Errorf("error: want %q got %q", "boom", body["error"])
	}
	if _, ok := body["code"]; ok {
		t.Errorf("plain jsonError should not emit a code field, got %v", body)
	}
}

func TestJSONErrorCoded_RoundTrip(t *testing.T) {
	rr := httptest.NewRecorder()
	jsonErrorCoded(rr, "invalid language tag", http.StatusBadRequest, "language.invalid_tag")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400 got %d", rr.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["error"] != "invalid language tag" {
		t.Errorf("error: want %q got %q", "invalid language tag", body["error"])
	}
	if body["code"] != "language.invalid_tag" {
		t.Errorf("code: want %q got %q", "language.invalid_tag", body["code"])
	}
}
