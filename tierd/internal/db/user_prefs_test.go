package db

import (
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestUserPrefs_GetMissingReturnsEmpty(t *testing.T) {
	store := openTestStore(t)
	got, err := store.GetUserLanguage("alice")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != "" {
		t.Errorf("want empty got %q", got)
	}
}

func TestUserPrefs_SetThenGet(t *testing.T) {
	store := openTestStore(t)
	if err := store.SetUserLanguage("alice", "nl"); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := store.GetUserLanguage("alice")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != "nl" {
		t.Errorf("want nl got %q", got)
	}
}

func TestUserPrefs_Upsert(t *testing.T) {
	store := openTestStore(t)
	if err := store.SetUserLanguage("alice", "nl"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetUserLanguage("alice", "en"); err != nil {
		t.Fatal(err)
	}
	got, _ := store.GetUserLanguage("alice")
	if got != "en" {
		t.Errorf("want en got %q", got)
	}
}

func TestUserPrefs_Independent(t *testing.T) {
	store := openTestStore(t)
	if err := store.SetUserLanguage("alice", "nl"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetUserLanguage("bob", "en"); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.GetUserLanguage("alice"); got != "nl" {
		t.Errorf("alice: want nl got %q", got)
	}
	if got, _ := store.GetUserLanguage("bob"); got != "en" {
		t.Errorf("bob: want en got %q", got)
	}
}

func TestUserPrefs_ClearByEmpty(t *testing.T) {
	store := openTestStore(t)
	if err := store.SetUserLanguage("alice", "nl"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetUserLanguage("alice", ""); err != nil {
		t.Fatal(err)
	}
	got, _ := store.GetUserLanguage("alice")
	if got != "" {
		t.Errorf("want empty after clear got %q", got)
	}
}

func TestUserPrefs_EmptyUsernameRejected(t *testing.T) {
	store := openTestStore(t)
	if err := store.SetUserLanguage("", "nl"); err == nil {
		t.Error("expected error for empty username")
	}
	if got, err := store.GetUserLanguage(""); err != nil || got != "" {
		t.Errorf("get empty: want (\"\", nil) got (%q, %v)", got, err)
	}
}
