package api

import (
	"testing"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
)

// applyLiveProgress must only touch rows whose SQL status is "running"
// — terminal rows have real values in the progress columns (or were
// intentionally wiped by CompleteBackupRun / FailBackupRun) and must
// not be overwritten by a stale in-memory entry.
func TestApplyLiveProgressOnlyAffectsRunningRows(t *testing.T) {
	h := NewBackupHandler(nil)
	h.setLiveProgress(1, "copying", 5, 10)
	h.setLiveProgress(2, "copying", 7, 14)

	runs := []db.BackupRun{
		{ID: 1, Status: "running"},
		{ID: 2, Status: "completed", Progress: "original", ProgressPct: 100, FilesDone: 99, FilesTotal: 100},
	}
	h.applyLiveProgress(runs)

	if runs[0].Progress != "copying" || runs[0].FilesDone != 5 || runs[0].FilesTotal != 10 || runs[0].ProgressPct != 50 {
		t.Fatalf("running row not overlaid: %+v", runs[0])
	}
	// Row 2 is completed — must not be modified by the overlay.
	if runs[1].Progress != "original" || runs[1].ProgressPct != 100 || runs[1].FilesDone != 99 {
		t.Fatalf("completed row was overwritten: %+v", runs[1])
	}
}

func TestApplyLiveProgressSkipsRowsWithoutEntry(t *testing.T) {
	h := NewBackupHandler(nil)
	// No setLiveProgress for ID 42.
	runs := []db.BackupRun{{ID: 42, Status: "running", Progress: "stale-from-sql", ProgressPct: 77}}
	h.applyLiveProgress(runs)
	if runs[0].Progress != "stale-from-sql" || runs[0].ProgressPct != 77 {
		t.Fatalf("missing entry should leave row untouched: %+v", runs[0])
	}
}

func TestApplyLiveProgressPercentIndeterminate(t *testing.T) {
	h := NewBackupHandler(nil)
	// total = -1 means rsync hasn't reported a count yet; pct must be
	// -1 (indeterminate), matching the old SQL write path.
	h.setLiveProgress(1, "rsync: 8.00 MB/s", -1, -1)
	runs := []db.BackupRun{{ID: 1, Status: "running"}}
	h.applyLiveProgress(runs)
	if runs[0].ProgressPct != -1 {
		t.Fatalf("indeterminate progress should set pct=-1, got %d", runs[0].ProgressPct)
	}
	if runs[0].Progress != "rsync: 8.00 MB/s" {
		t.Fatalf("progress message not applied: %+v", runs[0])
	}
}

func TestApplyLiveProgressPercentClampsTo100(t *testing.T) {
	h := NewBackupHandler(nil)
	// done > total can happen if rsync revises its total mid-run.
	// Match the old UpdateBackupRunProgress clamp.
	h.setLiveProgress(1, "copying", 200, 100)
	runs := []db.BackupRun{{ID: 1, Status: "running"}}
	h.applyLiveProgress(runs)
	if runs[0].ProgressPct != 100 {
		t.Fatalf("pct should clamp to 100, got %d", runs[0].ProgressPct)
	}
}

func TestClearLiveProgressRemovesOverlay(t *testing.T) {
	h := NewBackupHandler(nil)
	h.setLiveProgress(1, "copying", 5, 10)
	h.clearLiveProgress(1)
	runs := []db.BackupRun{{ID: 1, Status: "running", Progress: "sql-value", ProgressPct: 0}}
	h.applyLiveProgress(runs)
	if runs[0].Progress != "sql-value" {
		t.Fatalf("cleared entry should not overlay: %+v", runs[0])
	}
}

func TestApplyLiveProgressOneSingleRow(t *testing.T) {
	h := NewBackupHandler(nil)
	h.setLiveProgress(7, "single", 3, 6)
	run := &db.BackupRun{ID: 7, Status: "running"}
	h.applyLiveProgressOne(run)
	if run.Progress != "single" || run.FilesDone != 3 || run.FilesTotal != 6 || run.ProgressPct != 50 {
		t.Fatalf("single-row overlay wrong: %+v", run)
	}
}

func TestApplyLiveProgressOneSkipsNonRunning(t *testing.T) {
	h := NewBackupHandler(nil)
	h.setLiveProgress(8, "stale", 1, 2)
	run := &db.BackupRun{ID: 8, Status: "failed", Progress: "original", Error: "boom"}
	h.applyLiveProgressOne(run)
	if run.Progress != "original" {
		t.Fatalf("non-running single row was overwritten: %+v", run)
	}
}
