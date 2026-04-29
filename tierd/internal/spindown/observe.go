package spindown

import (
	"sync"
	"time"
)

const maxRecentEvents = 20

type WakeEvent struct {
	At        string `json:"at"`
	FromState string `json:"from_state"`
	ToState   string `json:"to_state"`
	Reason    string `json:"reason"`
}

type PowerSummary struct {
	ObservedSince          string      `json:"observed_since,omitempty"`
	LastObservedAt         string      `json:"last_observed_at,omitempty"`
	LastWakeAt             string      `json:"last_wake_at,omitempty"`
	LastWakeReason         string      `json:"last_wake_reason,omitempty"`
	TimeInStandbyPct       float64     `json:"time_in_standby_pct"`
	ObservedStandbySeconds int64       `json:"observed_standby_seconds"`
	RecentWakeEvents       []WakeEvent `json:"recent_wake_events"`
}

type observedDisk struct {
	since           time.Time
	lastAt          time.Time
	lastState       string
	standbyDuration time.Duration
	lastWakeAt      time.Time
	lastWakeReason  string
	events          []WakeEvent
}

type PowerObserver struct {
	mu    sync.Mutex
	disks map[string]*observedDisk
	now   func() time.Time
}

func NewPowerObserver() *PowerObserver {
	return &PowerObserver{
		disks: make(map[string]*observedDisk),
		now:   time.Now,
	}
}

func (o *PowerObserver) Observe(device, state, reason string) PowerSummary {
	if o == nil {
		return PowerSummary{}
	}
	now := o.now()
	o.mu.Lock()
	defer o.mu.Unlock()
	d := o.disks[device]
	if d == nil {
		d = &observedDisk{since: now, lastAt: now, lastState: state}
		o.disks[device] = d
		return d.summary(now)
	}
	if standbyLike(d.lastState) && now.After(d.lastAt) {
		d.standbyDuration += now.Sub(d.lastAt)
	}
	if standbyLike(d.lastState) && !standbyLike(state) && state != "" && state != "unknown" {
		if reason == "" {
			reason = "state transition observed"
		}
		d.lastWakeAt = now
		d.lastWakeReason = reason
		d.events = append(d.events, WakeEvent{
			At:        now.UTC().Format(time.RFC3339),
			FromState: d.lastState,
			ToState:   state,
			Reason:    reason,
		})
		if len(d.events) > maxRecentEvents {
			d.events = d.events[len(d.events)-maxRecentEvents:]
		}
	}
	if state != "" {
		d.lastState = state
	}
	d.lastAt = now
	return d.summary(now)
}

func (o *PowerObserver) RecordEvent(device, fromState, toState, reason string) PowerSummary {
	if o == nil {
		return PowerSummary{}
	}
	now := o.now()
	o.mu.Lock()
	defer o.mu.Unlock()
	d := o.disks[device]
	if d == nil {
		d = &observedDisk{since: now, lastAt: now, lastState: toState}
		o.disks[device] = d
	}
	if standbyLike(d.lastState) && now.After(d.lastAt) {
		d.standbyDuration += now.Sub(d.lastAt)
	}
	if reason == "" {
		reason = "operator action"
	}
	d.events = append(d.events, WakeEvent{
		At:        now.UTC().Format(time.RFC3339),
		FromState: fromState,
		ToState:   toState,
		Reason:    reason,
	})
	if len(d.events) > maxRecentEvents {
		d.events = d.events[len(d.events)-maxRecentEvents:]
	}
	if standbyLike(fromState) && !standbyLike(toState) {
		d.lastWakeAt = now
		d.lastWakeReason = reason
	}
	d.lastState = toState
	d.lastAt = now
	return d.summary(now)
}

func (d *observedDisk) summary(now time.Time) PowerSummary {
	standby := d.standbyDuration
	if standbyLike(d.lastState) && now.After(d.lastAt) {
		standby += now.Sub(d.lastAt)
	}
	total := now.Sub(d.since)
	var pct float64
	if total > 0 {
		pct = float64(standby) * 100 / float64(total)
	}
	out := PowerSummary{
		ObservedSince:          d.since.UTC().Format(time.RFC3339),
		LastObservedAt:         d.lastAt.UTC().Format(time.RFC3339),
		TimeInStandbyPct:       pct,
		ObservedStandbySeconds: int64(standby.Seconds()),
		RecentWakeEvents:       append([]WakeEvent(nil), d.events...),
	}
	if !d.lastWakeAt.IsZero() {
		out.LastWakeAt = d.lastWakeAt.UTC().Format(time.RFC3339)
		out.LastWakeReason = d.lastWakeReason
	}
	return out
}

func standbyLike(state string) bool {
	return state == "standby" || state == "sleeping"
}
