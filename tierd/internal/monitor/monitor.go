// Package monitor runs a background polling loop that checks storage health,
// records SMART history, evaluates alarm rules, and maintains a list of
// active alerts accessible via the API.
package monitor

import (
	"context"
	"log"
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

// ClearAlert removes an alert by ID.
func (m *Monitor) ClearAlert(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, a := range m.alerts {
		if a.ID == id {
			m.alerts = append(m.alerts[:i], m.alerts[i+1:]...)
			return
		}
	}
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
	m.checkSMART()
	m.cleanHistory()
	m.checkMdadm()
	m.checkZFS()
	m.cleanExpired()
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
					Device:    event.DevicePath,
					Timestamp: now,
				})
			}
		}

		// Check SMART health status.
		if !data.HealthPassed {
			m.addAlert(Alert{
				Source:    "smart",
				Severity:  "critical",
				Message:   "SMART health check FAILED",
				Device:    d.Path,
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
	for _, alert := range zfs.CheckARCAlerts(arcStats) {
		m.addAlert(Alert{
			Source:    "zfs",
			Severity:  alert.Severity,
			Message:   alert.Message,
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

// cleanExpired removes alerts older than 24 hours.
func (m *Monitor) cleanExpired() {
	m.mu.Lock()
	defer m.mu.Unlock()

	cutoff := time.Now().Add(-24 * time.Hour)
	var active []Alert
	for _, a := range m.alerts {
		if a.Timestamp.After(cutoff) {
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
