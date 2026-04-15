package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/tiering"
	mdadmadapter "github.com/JBailes/SmoothNAS/tierd/internal/tiering/mdadm"
	"github.com/JBailes/SmoothNAS/tierd/internal/tiering/meta"
)

// metaAdapterStub embeds stubAdapter to satisfy TieringAdapter and adds
// the two optional interfaces the meta-store endpoints type-assert on:
// metaStatsAdapter and fileListAdapter.
type metaAdapterStub struct {
	stubAdapter
	stats map[string][]meta.ShardStats

	gotNS     string
	gotPrefix string
	gotLimit  int
	files     []mdadmadapter.FileEntry
	filesErr  error
}

func (m *metaAdapterStub) MetaStats() map[string][]meta.ShardStats { return m.stats }

func (m *metaAdapterStub) ListNamespaceFiles(nsID, prefix string, limit int) ([]mdadmadapter.FileEntry, error) {
	m.gotNS = nsID
	m.gotPrefix = prefix
	m.gotLimit = limit
	return m.files, m.filesErr
}

func TestMetaStatsEndpoint(t *testing.T) {
	h := newTieringTestHandler(t)
	stub := &metaAdapterStub{
		stubAdapter: stubAdapter{kind: "mdadm"},
		stats: map[string][]meta.ShardStats{
			"media": {
				{
					Index:            0,
					QueueDepth:       5,
					QueueCapacity:    4096,
					BatchesCommitted: 100,
					RecordsWritten:   25000,
				},
				{
					Index:            1,
					QueueDepth:       0,
					QueueCapacity:    4096,
					BatchesCommitted: 98,
					RecordsWritten:   24800,
				},
			},
		},
	}
	if err := h.RegisterAdapter(stub); err != nil {
		t.Fatalf("register adapter: %v", err)
	}

	rec := doRequest(t, h, http.MethodGet, "/api/tiering/meta/stats", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200; body=%s", rec.Code, rec.Body)
	}

	var out map[string][]meta.ShardStats
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	shards, ok := out["media"]
	if !ok {
		t.Fatalf("missing pool 'media' in response")
	}
	if len(shards) != 2 {
		t.Fatalf("shards = %d, want 2", len(shards))
	}
	if shards[0].RecordsWritten != 25000 {
		t.Fatalf("shard 0 RecordsWritten = %d, want 25000", shards[0].RecordsWritten)
	}
}

func TestMetaStatsEndpointMethodNotAllowed(t *testing.T) {
	h := newTieringTestHandler(t)
	rec := doRequest(t, h, http.MethodPost, "/api/tiering/meta/stats", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status %d, want 405", rec.Code)
	}
}

func TestMetaStatsEndpointNoAdapters(t *testing.T) {
	h := newTieringTestHandler(t)
	rec := doRequest(t, h, http.MethodGet, "/api/tiering/meta/stats", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200; body=%s", rec.Code, rec.Body)
	}
	var out map[string][]meta.ShardStats
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("no adapters should yield empty map, got %d pools", len(out))
	}
}

func TestListNamespaceFilesEndpoint(t *testing.T) {
	h := newTieringTestHandler(t)
	stub := &metaAdapterStub{
		stubAdapter: stubAdapter{kind: "mdadm"},
		files: []mdadmadapter.FileEntry{
			{Path: "a.txt", Size: 100, Inode: 1, TierRank: 1, PinState: "none"},
			{Path: "sub/b.txt", Size: 200, Inode: 2, TierRank: 1, PinState: "pinned-hot"},
		},
	}
	if err := h.RegisterAdapter(stub); err != nil {
		t.Fatalf("register: %v", err)
	}

	rec := doRequest(t, h, http.MethodGet, "/api/tiering/namespaces/ns-123/files?prefix=sub&limit=42", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200; body=%s", rec.Code, rec.Body)
	}

	// Query params reached the adapter.
	if stub.gotNS != "ns-123" {
		t.Errorf("adapter saw nsID %q, want ns-123", stub.gotNS)
	}
	if stub.gotPrefix != "sub" {
		t.Errorf("adapter saw prefix %q, want 'sub'", stub.gotPrefix)
	}
	if stub.gotLimit != 42 {
		t.Errorf("adapter saw limit %d, want 42", stub.gotLimit)
	}

	var got []mdadmadapter.FileEntry
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if got[1].PinState != "pinned-hot" {
		t.Errorf("entry 1 pin_state = %q, want pinned-hot", got[1].PinState)
	}
}

func TestListNamespaceFilesEndpointDefaults(t *testing.T) {
	h := newTieringTestHandler(t)
	stub := &metaAdapterStub{
		stubAdapter: stubAdapter{kind: "mdadm"},
		files:       []mdadmadapter.FileEntry{},
	}
	if err := h.RegisterAdapter(stub); err != nil {
		t.Fatalf("register: %v", err)
	}

	rec := doRequest(t, h, http.MethodGet, "/api/tiering/namespaces/ns/files", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	// No ?limit → handler defaults to 500.
	if stub.gotLimit != 500 {
		t.Errorf("default limit = %d, want 500", stub.gotLimit)
	}

	// Empty files → must be JSON [] not null.
	body := rec.Body.String()
	if body == "null\n" || body == "null" {
		t.Errorf("body = %q, want empty array", body)
	}
}

// Ensure the type assertions we rely on compile.
var (
	_ metaStatsAdapter        = (*metaAdapterStub)(nil)
	_ fileListAdapter         = (*metaAdapterStub)(nil)
	_ tiering.TieringAdapter  = (*metaAdapterStub)(nil)
)
