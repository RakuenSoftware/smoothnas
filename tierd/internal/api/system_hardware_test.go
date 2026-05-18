package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSystemHardwareGPUsIsArray(t *testing.T) {
	h := NewSystemHandler(nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/system/hardware", nil)
	rr := httptest.NewRecorder()

	h.getHardware(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(resp["gpus"]) == "null" {
		t.Fatalf("gpus must be an array, got body=%s", rr.Body.String())
	}
	var gpus []GPUInfo
	if err := json.Unmarshal(resp["gpus"], &gpus); err != nil {
		t.Fatalf("gpus is not an array: %v body=%s", err, rr.Body.String())
	}
}
