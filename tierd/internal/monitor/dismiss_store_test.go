package monitor

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	tierddb "github.com/JBailes/SmoothNAS/tierd/internal/db"
	_ "github.com/mattn/go-sqlite3"
)

func TestDismissalSurvivesRestart(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "test.db")+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := tierddb.MigrateDB(db); err != nil {
		t.Fatalf("migrate db: %v", err)
	}

	scope := [][2]string{{"smart", "/dev/sdg"}}
	failed := Alert{Source: "smart", Severity: "critical", Message: "SMART health check FAILED", Device: "/dev/sdg", Timestamp: time.Now()}

	first := New(nil, nil)
	if err := first.SetDismissalStore(db); err != nil {
		t.Fatalf("set dismissal store: %v", err)
	}
	runCycle(first, scope, failed)
	first.ClearAlert(first.GetAlerts()[0].ID)

	restarted := New(nil, nil)
	if err := restarted.SetDismissalStore(db); err != nil {
		t.Fatalf("set dismissal store: %v", err)
	}
	runCycle(restarted, scope, failed)
	if restarted.AlertCount() != 0 {
		t.Fatalf("dismissal lost across restart: %d alerts", restarted.AlertCount())
	}

	// Condition clears: the stored dismissal is forgotten.
	runCycle(restarted, scope)
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM alert_dismissals").Scan(&n); err != nil {
		t.Fatalf("count dismissals: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected cleared dismissal to be deleted, %d rows remain", n)
	}
}
