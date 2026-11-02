package db

import (
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestDriveMigrationPreservesExistingBackups(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "upgrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(store.db, "migrations", 27); err != nil {
		t.Fatal(err)
	}
	// Use the pre-upgrade shape, including IDs and secrets used by resumed runs.
	if _, err := store.db.Exec(`INSERT INTO backup_configs(id,name,target_type,host,share,smb_pass,ssh_pass,local_path,direction,method,parallelism,delete_mode) VALUES(42,'existing','nfs','nas','/share','smb-secret','ssh-secret','/data','push','rsync',8,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO backup_runs(id,config_id,status,summary) VALUES(77,42,'completed','original history')`); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	cfg, err := store.GetBackupConfig(42)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "existing" || cfg.SMBPass != "smb-secret" || cfg.SSHPass != "ssh-secret" || cfg.Parallelism != 8 || !cfg.DeleteMode || cfg.ScheduleEnabled || cfg.ScheduleDays != 127 {
		t.Fatalf("config changed: %+v", cfg)
	}
	run, err := store.GetBackupRun(77)
	if err != nil || run.ConfigID != 42 || run.Summary != "original history" {
		t.Fatalf("history changed: %+v %v", run, err)
	}
	if err := store.DeleteBackupConfig(42); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetBackupRun(77); err != ErrNotFound {
		t.Fatalf("cascade broken: %v", err)
	}
	a, err := store.CreateBackupGoogleAccount(BackupGoogleAccount{Label: "google", ClientID: "id", ClientSecret: "secret", RefreshToken: "refresh"})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err = store.CreateBackupConfig(BackupConfig{Name: "drive", TargetType: "gdrive", Method: "rclone", Direction: "push", GoogleAccountID: &a.ID, LocalPath: "/data", RemotePath: "backup", Encrypt: true, CryptPassword: "key", CryptSalt: "salt"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteBackupGoogleAccount(a.ID); err == nil {
		t.Fatal("foreign key allowed account removal")
	}
	if claimed, err := store.ClaimBackupSlot(cfg.ID, "", "2026-11-01T02:00:00Z"); err != nil || claimed {
		t.Fatal("disabled schedule claimed")
	}
	cfg.ScheduleEnabled = true
	cfg.ScheduleTime = "02:00"
	cfg.ScheduleDays = 127
	if _, err := store.UpdateBackupConfig(cfg.ID, *cfg); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		claimed, err := store.ClaimBackupSlot(cfg.ID, "", "2026-11-01T02:00:00Z")
		if err != nil || claimed != (i == 0) {
			t.Fatalf("claim %d=%v %v", i, claimed, err)
		}
	}
}
