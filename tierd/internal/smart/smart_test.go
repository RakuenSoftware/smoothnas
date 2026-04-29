package smart_test

import (
	"path/filepath"
	"testing"
	"time"

	tierddb "github.com/JBailes/SmoothNAS/tierd/internal/db"
	"github.com/JBailes/SmoothNAS/tierd/internal/smart"

	"database/sql"
	_ "github.com/mattn/go-sqlite3"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := tierddb.MigrateDB(db); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	return db
}

func TestHistoryRecordAndQuery(t *testing.T) {
	db := openTestDB(t)
	store, err := smart.NewHistoryStore(db)
	if err != nil {
		t.Fatalf("new history store: %v", err)
	}

	data := &smart.Data{
		DevicePath:  "/dev/sda",
		Temperature: 42,
		Attributes: []smart.Attribute{
			{ID: 5, Name: "Reallocated_Sector_Ct", Current: 100, RawValue: 0},
			{ID: 9, Name: "Power_On_Hours", Current: 90, RawValue: 12345},
		},
	}

	if err := store.RecordSnapshot(data); err != nil {
		t.Fatalf("record snapshot: %v", err)
	}

	// Query all attributes for the device.
	entries, err := store.Query("/dev/sda", nil, nil, nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	// Should have 2 attributes + 1 temperature synthetic = 3.
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	// Query filtered by attribute ID.
	attrID := 5
	entries, err = store.Query("/dev/sda", &attrID, nil, nil)
	if err != nil {
		t.Fatalf("query filtered: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry for attr 5, got %d", len(entries))
	}
	if entries[0].RawValue != 0 {
		t.Fatalf("expected raw_value 0, got %d", entries[0].RawValue)
	}

	// Query different device should return empty.
	entries, err = store.Query("/dev/sdb", nil, nil, nil)
	if err != nil {
		t.Fatalf("query other device: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries for sdb, got %d", len(entries))
	}
}

func TestHistoryCleanup(t *testing.T) {
	db := openTestDB(t)
	store, err := smart.NewHistoryStore(db)
	if err != nil {
		t.Fatalf("new history store: %v", err)
	}

	data := &smart.Data{
		DevicePath: "/dev/sda",
		Attributes: []smart.Attribute{
			{ID: 5, Name: "Reallocated_Sector_Ct", Current: 100, RawValue: 0},
		},
	}

	if err := store.RecordSnapshot(data); err != nil {
		t.Fatalf("record: %v", err)
	}

	// Backdate the entries so they are old enough to clean.
	oldTime := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339)
	db.Exec("UPDATE smart_history SET timestamp = ?", oldTime)

	// Clean entries older than 24 hours.
	if err := store.CleanOlderThan(24 * time.Hour); err != nil {
		t.Fatalf("clean: %v", err)
	}

	entries, _ := store.Query("/dev/sda", nil, nil, nil)
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries after cleanup, got %d", len(entries))
	}
}

func TestAlarmDefaults(t *testing.T) {
	db := openTestDB(t)
	store, err := smart.NewAlarmStore(db)
	if err != nil {
		t.Fatalf("new alarm store: %v", err)
	}

	rules, err := store.ListRules()
	if err != nil {
		t.Fatalf("list rules: %v", err)
	}

	// Should have 8 default rules.
	if len(rules) != 8 {
		t.Fatalf("expected 8 default rules, got %d", len(rules))
	}

	// Check that reallocated sector rule exists.
	found := false
	for _, r := range rules {
		if r.AttributeID == 5 {
			found = true
			if r.WarningAbove == nil || *r.WarningAbove != 0 {
				t.Fatal("reallocated sector warning_above should be 0")
			}
			if r.CriticalAbove == nil || *r.CriticalAbove != 50 {
				t.Fatal("reallocated sector critical_above should be 50")
			}
		}
	}
	if !found {
		t.Fatal("reallocated sector rule not found in defaults")
	}
}

func TestAlarmEvaluateWarning(t *testing.T) {
	db := openTestDB(t)
	store, err := smart.NewAlarmStore(db)
	if err != nil {
		t.Fatalf("new alarm store: %v", err)
	}

	// Disk with 3 reallocated sectors should trigger a warning (> 0, < 50).
	data := &smart.Data{
		DevicePath: "/dev/sda",
		Attributes: []smart.Attribute{
			{ID: 5, Name: "Reallocated_Sector_Ct", Current: 97, RawValue: 3},
		},
	}

	events, err := store.Evaluate(data)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Severity != smart.SeverityWarning {
		t.Fatalf("expected warning, got %s", events[0].Severity)
	}
	if events[0].Value != 3 {
		t.Fatalf("expected value 3, got %d", events[0].Value)
	}
}

func TestAlarmEvaluateCritical(t *testing.T) {
	db := openTestDB(t)
	store, err := smart.NewAlarmStore(db)
	if err != nil {
		t.Fatalf("new alarm store: %v", err)
	}

	// Disk with 100 reallocated sectors should trigger critical (> 50).
	data := &smart.Data{
		DevicePath: "/dev/sda",
		Attributes: []smart.Attribute{
			{ID: 5, Name: "Reallocated_Sector_Ct", Current: 50, RawValue: 100},
		},
	}

	events, err := store.Evaluate(data)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Severity != smart.SeverityCritical {
		t.Fatalf("expected critical, got %s", events[0].Severity)
	}
}

func TestAlarmEvaluateNoTrigger(t *testing.T) {
	db := openTestDB(t)
	store, err := smart.NewAlarmStore(db)
	if err != nil {
		t.Fatalf("new alarm store: %v", err)
	}

	// Healthy disk: 0 reallocated sectors, normal temp.
	data := &smart.Data{
		DevicePath: "/dev/sda",
		Attributes: []smart.Attribute{
			{ID: 5, Name: "Reallocated_Sector_Ct", Current: 100, RawValue: 0},
			{ID: 194, Name: "Temperature_Celsius", Current: 35, RawValue: 35},
			{ID: 9, Name: "Power_On_Hours", Current: 90, RawValue: 5000},
		},
	}

	events, err := store.Evaluate(data)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	if len(events) != 0 {
		t.Fatalf("expected 0 events for healthy disk, got %d", len(events))
	}
}

func TestAlarmEvaluateBelowThreshold(t *testing.T) {
	db := openTestDB(t)
	store, err := smart.NewAlarmStore(db)
	if err != nil {
		t.Fatalf("new alarm store: %v", err)
	}

	// SSD with wear leveling at 3% (below critical 5%).
	data := &smart.Data{
		DevicePath: "/dev/sda",
		Attributes: []smart.Attribute{
			{ID: 177, Name: "Wear_Leveling_Count", Current: 3, RawValue: 3},
		},
	}

	events, err := store.Evaluate(data)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Severity != smart.SeverityCritical {
		t.Fatalf("expected critical, got %s", events[0].Severity)
	}
}

func TestAlarmEvaluatePackedTemperature(t *testing.T) {
	db := openTestDB(t)
	store, err := smart.NewAlarmStore(db)
	if err != nil {
		t.Fatalf("new alarm store: %v", err)
	}

	// Simulate a disk whose Temperature_Celsius raw value has been correctly
	// masked from the packed smartctl value 0x3F00000025 (270583595045) down
	// to 0x25 (37). This must NOT trigger any alarm.
	data := &smart.Data{
		DevicePath: "/dev/sda",
		Attributes: []smart.Attribute{
			{ID: 194, Name: "Temperature_Celsius", Current: 37, RawValue: 37},
		},
	}

	events, err := store.Evaluate(data)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events for 37°C disk, got %d (severity %s, value %d)",
			len(events), events[0].Severity, events[0].Value)
	}
}

func TestAlarmCRUD(t *testing.T) {
	db := openTestDB(t)
	store, err := smart.NewAlarmStore(db)
	if err != nil {
		t.Fatalf("new alarm store: %v", err)
	}

	// Create a custom rule.
	warnAbove := int64(100)
	critAbove := int64(500)
	id, err := store.CreateRule(smart.AlarmRule{
		AttributeID:   199,
		AttributeName: "UDMA_CRC_Error_Count",
		WarningAbove:  &warnAbove,
		CriticalAbove: &critAbove,
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}

	// Should now have 9 rules (8 defaults + 1 custom).
	rules, _ := store.ListRules()
	if len(rules) != 9 {
		t.Fatalf("expected 9 rules, got %d", len(rules))
	}

	// Update the rule.
	newWarn := int64(50)
	if err := store.UpdateRule(id, smart.AlarmRule{
		AttributeID:   199,
		AttributeName: "UDMA_CRC_Error_Count",
		WarningAbove:  &newWarn,
		CriticalAbove: &critAbove,
	}); err != nil {
		t.Fatalf("update rule: %v", err)
	}

	// Delete the rule.
	if err := store.DeleteRule(id); err != nil {
		t.Fatalf("delete rule: %v", err)
	}

	rules, _ = store.ListRules()
	if len(rules) != 8 {
		t.Fatalf("expected 8 rules after delete, got %d", len(rules))
	}
}

func TestAlarmEventPersistence(t *testing.T) {
	db := openTestDB(t)
	store, err := smart.NewAlarmStore(db)
	if err != nil {
		t.Fatalf("new alarm store: %v", err)
	}

	// Record an event.
	event := smart.AlarmEvent{
		RuleID:        1,
		DevicePath:    "/dev/sda",
		AttributeID:   5,
		AttributeName: "Reallocated_Sector_Ct",
		Severity:      smart.SeverityWarning,
		Value:         3,
		Threshold:     0,
		Timestamp:     time.Now().UTC().Format(time.RFC3339),
	}

	if err := store.RecordEvent(event); err != nil {
		t.Fatalf("record event: %v", err)
	}

	// List events.
	events, err := store.ListEvents(nil, nil, 100)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].DevicePath != "/dev/sda" {
		t.Fatalf("expected /dev/sda, got %s", events[0].DevicePath)
	}

	// Filter by device.
	device := "/dev/sdb"
	events, _ = store.ListEvents(&device, nil, 100)
	if len(events) != 0 {
		t.Fatalf("expected 0 events for sdb, got %d", len(events))
	}
}
