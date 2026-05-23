package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/backup"
	"github.com/JBailes/SmoothNAS/tierd/internal/db"
)

func testBackupStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "tierd.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	return store
}

func testBackupConfig(t *testing.T, store *db.Store) *db.BackupConfig {
	t.Helper()
	cfg, err := store.CreateBackupConfig(db.BackupConfig{
		Name:        "media",
		TargetType:  "nfs",
		Host:        "backup-host",
		Share:       "/exports/media",
		LocalPath:   "/mnt/media/storage/backup",
		Direction:   "pull",
		Method:      "rsync",
		Parallelism: 1,
		UseSSH:      true,
	})
	if err != nil {
		t.Fatalf("create backup config: %v", err)
	}
	return cfg
}

func TestResumeActiveRunsRestartsRunningRows(t *testing.T) {
	store := testBackupStore(t)
	cfg := testBackupConfig(t, store)
	runID, err := store.CreateBackupRun(cfg.ID)
	if err != nil {
		t.Fatalf("create backup run: %v", err)
	}

	started := make(chan backup.Config, 1)
	release := make(chan struct{})
	origRun := runBackupFunc
	runBackupFunc = func(_ context.Context, cfg backup.Config, progress func(string, int, int)) (string, error) {
		progress("resumed", -1, -1)
		started <- cfg
		<-release
		return "resume complete", nil
	}
	t.Cleanup(func() { runBackupFunc = origRun })

	h := NewBackupHandler(store)
	h.ResumeActiveRuns()

	select {
	case got := <-started:
		if got.Host != "backup-host" || got.LocalPath != "/mnt/media/storage/backup" {
			t.Fatalf("resumed config mismatch: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("running backup row was not resumed")
	}
	if n := h.ActiveRunCount(); n != 1 {
		t.Fatalf("active run count = %d, want 1", n)
	}

	close(release)
	waitForBackupDrain(t, h)

	run, err := store.GetBackupRun(runID)
	if err != nil {
		t.Fatalf("get backup run: %v", err)
	}
	if run.Status != "completed" || run.Summary != "resume complete" {
		t.Fatalf("run after resume = %+v, want completed resume", run)
	}
}

func TestBeginShutdownRejectsNewBackupRuns(t *testing.T) {
	store := testBackupStore(t)
	cfg := testBackupConfig(t, store)

	called := false
	origRun := runBackupFunc
	runBackupFunc = func(context.Context, backup.Config, func(string, int, int)) (string, error) {
		called = true
		return "unexpected", nil
	}
	t.Cleanup(func() { runBackupFunc = origRun })

	h := NewBackupHandler(store)
	h.BeginShutdown()

	req := httptest.NewRequest(http.MethodPost, "/api/backup/configs/1/run", nil)
	rec := httptest.NewRecorder()
	h.runBackup(rec, req, cfg.ID)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if called {
		t.Fatal("backup runner was called while handler was draining")
	}
	runs, err := store.ListBackupRunsByConfig(cfg.ID, false)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("draining request created runs: %+v", runs)
	}
}

func waitForBackupDrain(t *testing.T, h *BackupHandler) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		h.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("backup handler did not drain")
	}
}
