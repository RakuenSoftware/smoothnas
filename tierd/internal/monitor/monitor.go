// Package monitor runs a background polling loop that checks storage health,
// records SMART history, evaluates alarm rules, and maintains a list of
// active alerts accessible via the API.
package monitor

import (
	"context"
	"database/sql"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/disk"
	"github.com/JBailes/SmoothNAS/tierd/internal/mdadm"
	"github.com/JBailes/SmoothNAS/tierd/internal/smart"
	"github.com/JBailes/SmoothNAS/tierd/internal/zfs"
)

// Alert represents an active system alert.
type Alert struct {
	ID        string    `json:"id"`
	Source    string    `json:"source"`     // "smart", "mdadm", "zfs", "disk", "system"
	Severity  string    `json:"severity"`   // "warning", "critical"
	Message   string    `json:"message"`
	Device    string    `json:"device"`
	Timestamp time.Time `json:"timestamp"`
}

// Monitor runs the background health polling loop.
type Monitor struct {
	mu                 sync.RWMutex
	alerts             []Alert
	history            *smart.HistoryStore
	alarms             *smart.AlarmStore
	pollInterval       time.Duration
	historyInterval    time.Duration
	historyRetention   time.Duration
	lastHistory        time.Time
	lastCleanup        time.Time
	stopCh             chan struct{}
	stopOnce           sync.Once
	alertCounter       int
	arraySizes         map[string]uint64
	onArraySizeChanged func(string)

	// dismissed holds alerts the user dismissed, keyed by alertKey. A
	// dismissed alert is not raised again while its condition persists;
	// the dismissal is dropped once a check sees the condition gone, so a
	// later recurrence alerts again. Persisted in dismissDB when set.
	dismissed map[string]bool
	dismissDB *sql.DB

	// Per-poll bookkeeping: alert keys raised this cycle, and the
	// source+device scopes that were checked successfully this cycle.
	seen     map[string]bool
	checked  map[string]bool
	lastSeen map[string]bool // seen from the last completed cycle

	// DNS check state: lookups failed in a row, and when the last check ran.
	dnsFailures  int
	lastDNSCheck time.Time

	// context lifecycle (created on Start, cancelled on Stop)
	ctx    context.Context
	cancel context.CancelFunc
}

// New creates a Monitor. Call Start() to begin polling.
func New(history *smart.HistoryStore, alarms *smart.AlarmStore) *Monitor {
	return &Monitor{
		history:          history,
		alarms:           alarms,
		pollInterval:     30 * time.Second,
		historyInterval:  1 * time.Hour,
		historyRetention: 90 * 24 * time.Hour, // 90 days
		stopCh:           make(chan struct{}),
		arraySizes:       make(map[string]uint64),
		dismissed:        make(map[string]bool),
	}
}

// NewWithIntervals creates a Monitor with custom intervals (for testing).
func NewWithIntervals(history *smart.HistoryStore, alarms *smart.AlarmStore, poll, historyInt time.Duration) *Monitor {
	return &Monitor{
		history:         history,
		alarms:          alarms,
		pollInterval:    poll,
		historyInterval: historyInt,
		stopCh:          make(chan struct{}),
		arraySizes:      make(map[string]uint64),
		dismissed:       make(map[string]bool),
	}
}

// SetArraySizeChangedCallback registers a function to call when an mdadm
// array's reported size increases. The callback receives the array device path
// and is invoked in its own goroutine so the polling loop is not blocked.
func (m *Monitor) SetArraySizeChangedCallback(fn func(string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onArraySizeChanged = fn
}

// Start begins the polling loop in a goroutine.
func (m *Monitor) Start() {
	m.ctx, m.cancel = context.WithCancel(context.Background())
	go m.run()
}

// Stop signals the polling loop and all heat engine goroutines to exit.
// Safe to call multiple times.
func (m *Monitor) Stop() {
	m.stopOnce.Do(func() {
		if m.cancel != nil {
			m.cancel()
		}
		close(m.stopCh)
	})
}

// GetAlerts returns a copy of the current active alerts.
func (m *Monitor) GetAlerts() []Alert {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]Alert, len(m.alerts))
	copy(result, m.alerts)
	return result
}

// AlertCount returns the number of active alerts.
func (m *Monitor) AlertCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.alerts)
}

// AddAlert adds an external alert (e.g. from the GUI or API).
func (m *Monitor) AddAlert(alert Alert) {
	m.addAlert(alert)
}

// ClearAlert dismisses an alert by ID. The alert stays dismissed while its
// condition persists, across restarts when a dismissal store is set.
func (m *Monitor) ClearAlert(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, a := range m.alerts {
		if a.ID == id {
			m.alerts = append(m.alerts[:i], m.alerts[i+1:]...)
			m.dismissed[alertKey(a.Source, a.Device, a.Message)] = true
			m.persistDismissal(a, true)
			return
		}
	}
}

// SetDismissalStore loads and persists dismissals in db (table
// alert_dismissals). Call before Start.
func (m *Monitor) SetDismissalStore(db *sql.DB) error {
	rows, err := db.Query("SELECT source, device, message FROM alert_dismissals")
	if err != nil {
		return err
	}
	defer rows.Close()
	m.mu.Lock()
	defer m.mu.Unlock()
	for rows.Next() {
		var source, device, message string
		if err := rows.Scan(&source, &device, &message); err != nil {
			return err
		}
		m.dismissed[alertKey(source, device, message)] = true
	}
	m.dismissDB = db
	return rows.Err()
}

// persistDismissal records or forgets a dismissal. Caller holds m.mu.
func (m *Monitor) persistDismissal(a Alert, dismissed bool) {
	if m.dismissDB == nil {
		return
	}
	var err error
	if dismissed {
		_, err = m.dismissDB.Exec(
			"INSERT OR REPLACE INTO alert_dismissals (source, device, message, dismissed_at) VALUES (?, ?, ?, ?)",
			a.Source, a.Device, a.Message, time.Now().UTC().Format(time.RFC3339))
	} else {
		_, err = m.dismissDB.Exec(
			"DELETE FROM alert_dismissals WHERE source = ? AND device = ? AND message = ?",
			a.Source, a.Device, a.Message)
	}
	if err != nil {
		log.Printf("monitor: persist alert dismissal: %v", err)
	}
}

// driveID names a drive by model and serial number. /dev/sdX names are
// reassigned at boot, so alerts and their dismissals keyed by them moved
// between drives across reboots.
func driveID(data *smart.Data, path string) string {
	if data.SerialNumber == "" {
		return path
	}
	return strings.TrimSpace(data.ModelName + " " + data.SerialNumber)
}

func alertKey(source, device, message string) string {
	return source + "\x00" + device + "\x00" + message
}

func scopeKey(source, device string) string {
	return source + "\x00" + device
}

// run is the main polling loop.
func (m *Monitor) run() {
	// Run an initial check immediately.
	m.poll()

	ticker := time.NewTicker(m.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.poll()
		}
	}
}

// poll runs one cycle of all health checks.
func (m *Monitor) poll() {
	m.beginCycle()
	m.checkSMART()
	m.cleanHistory()
	m.checkMdadm()
	m.checkZFS()
	m.checkDNS()
	m.endCycle()
	m.cleanExpired()
}

// beginCycle resets the per-poll bookkeeping.
func (m *Monitor) beginCycle() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seen = make(map[string]bool)
	m.checked = make(map[string]bool)
}

// markChecked records that source's check of device ran successfully this
// cycle, so conditions it did not raise can be treated as cleared.
func (m *Monitor) markChecked(source, device string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.checked != nil {
		m.checked[scopeKey(source, device)] = true
	}
}

// endCycle resolves alerts whose condition a successful check no longer
// reports, and forgets their dismissals so a recurrence alerts again.
func (m *Monitor) endCycle() {
	m.mu.Lock()
	defer m.mu.Unlock()
	var active []Alert
	for _, a := range m.alerts {
		if m.checked[scopeKey(a.Source, a.Device)] && !m.seen[alertKey(a.Source, a.Device, a.Message)] {
			continue
		}
		active = append(active, a)
	}
	m.alerts = active
	for key := range m.dismissed {
		parts := strings.SplitN(key, "\x00", 3)
		if len(parts) != 3 {
			continue
		}
		if m.checked[scopeKey(parts[0], parts[1])] && !m.seen[key] {
			delete(m.dismissed, key)
			m.persistDismissal(Alert{Source: parts[0], Device: parts[1], Message: parts[2]}, false)
		}
	}
	m.lastSeen = m.seen
	m.seen = nil
	m.checked = nil
}

// checkSMART reads SMART data for all disks, evaluates alarms, and optionally records history.
func (m *Monitor) checkSMART() {
	disks, err := disk.List()
	if err != nil {
		return
	}

	now := time.Now()
	recordHistory := now.Sub(m.lastHistory) >= m.historyInterval

	for _, d := range disks {
		if d.Assignment == "os" {
			continue // Skip OS disks for SMART monitoring.
		}

		data, err := smart.ReadData(d.Path)
		if err != nil {
			continue
		}
		drive := driveID(data, d.Path)
		m.markChecked("smart", drive)

		// Record history hourly.
		if recordHistory && m.history != nil {
			if err := m.history.RecordSnapshot(data); err != nil {
				log.Printf("monitor: record SMART history for %s: %v", d.Path, err)
			}
		}

		// Evaluate alarms.
		if m.alarms != nil {
			events, err := m.alarms.Evaluate(data)
			if err != nil {
				log.Printf("monitor: evaluate alarms for %s: %v", d.Path, err)
				continue
			}

			for _, event := range events {
				// Persist the event.
				m.alarms.RecordEvent(event)

				// Add to active alerts (if not already present for this device+attribute).
				m.addAlert(Alert{
					Source:    "smart",
					Severity:  string(event.Severity),
					Message:   event.AttributeName + " = " + formatInt64(event.Value),
					Device:    drive,
					Timestamp: now,
				})
			}
		}

		// Check SMART health status. A device that reports no health result
		// at all (a card reader, a virtual device, no SMART support) has not
		// failed; it has nothing to report.
		if data.HealthKnown && !data.HealthPassed {
			m.addAlert(Alert{
				Source:    "smart",
				Severity:  "critical",
				Message:   "SMART health check FAILED",
				Device:    drive,
				Timestamp: now,
			})
		}
	}

	if recordHistory {
		m.lastHistory = now
	}
}

// checkMdadm checks mdadm array health and raises alerts for degraded or failed
// arrays. It also detects size increases and fires the array-size-changed
// callback so that the tier manager can trigger auto-expansion.
func (m *Monitor) checkMdadm() {
	arrays, err := mdadm.List()
	if err != nil {
		log.Printf("monitor: list mdadm arrays: %v", err)
		return
	}

	now := time.Now()
	for _, a := range arrays {
		m.markChecked("mdadm", a.Path)
		switch a.State {
		case "degraded":
			m.addAlert(Alert{
				Source:    "mdadm",
				Severity:  "warning",
				Message:   "Array " + a.Name + " is degraded (" + formatInt(a.ActiveDisks) + "/" + formatInt(a.TotalDisks) + " disks active)",
				Device:    a.Path,
				Timestamp: now,
			})
		case "inactive", "failed":
			m.addAlert(Alert{
				Source:    "mdadm",
				Severity:  "critical",
				Message:   "Array " + a.Name + " is " + a.State,
				Device:    a.Path,
				Timestamp: now,
			})
		}

		// Size-change detection for auto-expansion (proposal 14).
		if a.Size > 0 {
			m.mu.Lock()
			prev, known := m.arraySizes[a.Path]
			m.arraySizes[a.Path] = a.Size
			cb := m.onArraySizeChanged
			m.mu.Unlock()

			if known && a.Size > prev && cb != nil {
				go cb(a.Path)
			}
		}
	}
}

// checkZFS checks ZFS pool health and ARC statistics, emitting alerts for
// degraded pools, resilvering, checksum errors, L2ARC ineffectiveness, and
// ARC memory pressure.
func (m *Monitor) checkZFS() {
	now := time.Now()

	for _, alert := range zfs.CheckPoolAlerts() {
		m.addAlert(Alert{
			Source:    "zfs",
			Severity:  alert.Severity,
			Message:   alert.Message,
			Device:    alert.PoolName,
			Timestamp: now,
		})
	}

	arcStats, err := zfs.ReadARCStats()
	if err != nil {
		log.Printf("monitor: read ZFS ARC stats: %v", err)
		return
	}
	m.markChecked("zfs", "")
	for _, alert := range zfs.CheckARCAlerts(arcStats) {
		m.addAlert(Alert{
			Source:    "zfs",
			Severity:  alert.Severity,
			Message:   alert.Message,
			Timestamp: now,
		})
	}
}

// dnsCheckHost is resolved to tell whether name resolution works: it is the
// update archive, so a failure also means updates cannot install.
const dnsCheckHost = "updates.rakuensoftware.com"

// dnsCheckInterval spaces the DNS checks; the poll itself runs every 30 s.
const dnsCheckInterval = 5 * time.Minute

// lookupHost is a var so tests can fake resolution.
var lookupHost = func(ctx context.Context, host string) ([]string, error) {
	return net.DefaultResolver.LookupHost(ctx, host)
}

// dnsAlertMessage is the alert raised while names cannot be resolved.
const dnsAlertMessage = "DNS is not working: the NAS cannot resolve " + dnsCheckHost +
	". Updates, plugins and the speed test fail until a DNS server is set on the Network page."

// checkDNS raises an alert when name resolution fails twice in a row (one
// failure can be a blip) and clears it once a lookup succeeds. A NAS with a
// static address and no DNS server otherwise fails every update silently.
func (m *Monitor) checkDNS() {
	now := time.Now()
	m.mu.Lock()
	due := now.Sub(m.lastDNSCheck) >= dnsCheckInterval
	m.mu.Unlock()
	if !due {
		// Not checked this cycle: keep the alert's state as it is.
		m.mu.Lock()
		if m.lastSeen[alertKey("network", "", dnsAlertMessage)] && m.seen != nil {
			m.seen[alertKey("network", "", dnsAlertMessage)] = true
		}
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_, err := lookupHost(ctx, dnsCheckHost)
	cancel()

	m.mu.Lock()
	m.lastDNSCheck = now
	if err == nil {
		m.dnsFailures = 0
	} else {
		m.dnsFailures++
	}
	failing := m.dnsFailures >= 2
	m.mu.Unlock()

	m.markChecked("network", "")
	if failing {
		m.addAlert(Alert{
			Source:    "network",
			Severity:  "critical",
			Message:   dnsAlertMessage,
			Timestamp: now,
		})
	}
}

// cleanHistory removes SMART history entries older than the retention period.
// Runs once per day to avoid unnecessary database churn.
func (m *Monitor) cleanHistory() {
	if m.history == nil {
		return
	}

	now := time.Now()
	if now.Sub(m.lastCleanup) < 24*time.Hour {
		return
	}

	if err := m.history.CleanOlderThan(m.historyRetention); err != nil {
		log.Printf("monitor: clean SMART history: %v", err)
	}

	// Also clean expired sessions and login attempts from the store
	// (these are handled by the db package, not the monitor, but we
	// piggyback on this daily cleanup).

	m.lastCleanup = now
}

// addAlert adds an alert if one doesn't already exist for the same device+source+message.
func (m *Monitor) addAlert(alert Alert) {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := alertKey(alert.Source, alert.Device, alert.Message)
	if m.seen != nil {
		m.seen[key] = true
	}
	if m.dismissed[key] {
		return
	}

	// Deduplicate: don't add if same device+source+message already active.
	for _, a := range m.alerts {
		if a.Device == alert.Device && a.Source == alert.Source && a.Message == alert.Message {
			return
		}
	}

	m.alertCounter++
	alert.ID = formatInt(m.alertCounter)
	m.alerts = append(m.alerts, alert)
}

// cleanExpired removes alerts older than 24 hours, except those raised by a
// check that ran successfully this cycle: those stay until resolved or
// dismissed instead of disappearing and reappearing as new alerts.
func (m *Monitor) cleanExpired() {
	m.mu.Lock()
	defer m.mu.Unlock()

	cutoff := time.Now().Add(-24 * time.Hour)
	var active []Alert
	for _, a := range m.alerts {
		if a.Timestamp.After(cutoff) || m.lastSeen[alertKey(a.Source, a.Device, a.Message)] {
			active = append(active, a)
		}
	}
	m.alerts = active
}

func formatInt64(n int64) string {
	if n == 0 {
		return "0"
	}
	s := ""
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	if neg {
		s = "-" + s
	}
	return s
}

func formatInt(n int) string {
	return formatInt64(int64(n))
}
