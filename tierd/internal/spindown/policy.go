package spindown

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/db"
)

const (
	scopePool = "pool"
	scopeZFS  = "zfs"
)

type ActiveWindow struct {
	Days  []string `json:"days"`
	Start string   `json:"start"`
	End   string   `json:"end"`
}

type Decision struct {
	Allowed      bool
	ActiveNow    bool
	NextActiveAt string
	Reason       string
}

type TargetBalanceStatus struct {
	Active             bool   `json:"active"`
	StartedAt          string `json:"started_at,omitempty"`
	FinishedAt         string `json:"finished_at,omitempty"`
	CheckedAt          string `json:"checked_at,omitempty"`
	CandidateCount     int    `json:"candidate_count"`
	PlannedMoves       int    `json:"planned_moves"`
	PendingMoves       int    `json:"pending_moves"`
	Moved              int    `json:"moved"`
	Skipped            int    `json:"skipped"`
	CandidateExhausted bool   `json:"candidate_exhausted"`
	Reason             string `json:"reason,omitempty"`
}

func PoolEnabledKey(poolName string) string {
	return scopedKey(scopePool, poolName, "enabled")
}

func PoolWindowsKey(poolName string) string {
	return scopedKey(scopePool, poolName, "active_windows")
}

func PoolTargetBalanceStatusKey(poolName string) string {
	return scopedKey(scopePool, poolName, "target_balance")
}

func ZFSEnabledKey(poolName string) string {
	return scopedKey(scopeZFS, poolName, "enabled")
}

func ZFSWindowsKey(poolName string) string {
	return scopedKey(scopeZFS, poolName, "active_windows")
}

func scopedKey(scope, name, suffix string) string {
	return scope + ".spindown." + name + "." + suffix
}

func Enabled(store *db.Store, key string) (bool, error) {
	return store.GetBoolConfig(key, false)
}

func LoadWindows(store *db.Store, key string) ([]ActiveWindow, error) {
	val, err := store.GetConfig(key)
	if err == db.ErrNotFound || strings.TrimSpace(val) == "" {
		return []ActiveWindow{}, nil
	}
	if err != nil {
		return nil, err
	}
	var windows []ActiveWindow
	if err := json.Unmarshal([]byte(val), &windows); err != nil {
		return nil, fmt.Errorf("parse active windows: %w", err)
	}
	return NormalizeWindows(windows)
}

func StoreWindows(store *db.Store, key string, windows []ActiveWindow) ([]ActiveWindow, error) {
	normalized, err := NormalizeWindows(windows)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(normalized)
	if err != nil {
		return nil, err
	}
	if err := store.SetConfig(key, string(data)); err != nil {
		return nil, err
	}
	return normalized, nil
}

func LoadTargetBalanceStatus(store *db.Store, poolName string) (TargetBalanceStatus, error) {
	val, err := store.GetConfig(PoolTargetBalanceStatusKey(poolName))
	if err == db.ErrNotFound || strings.TrimSpace(val) == "" {
		return TargetBalanceStatus{}, nil
	}
	if err != nil {
		return TargetBalanceStatus{}, err
	}
	var status TargetBalanceStatus
	if err := json.Unmarshal([]byte(val), &status); err != nil {
		return TargetBalanceStatus{}, fmt.Errorf("parse target balance status: %w", err)
	}
	return status, nil
}

func StoreTargetBalanceStatus(store *db.Store, poolName string, status TargetBalanceStatus) error {
	data, err := json.Marshal(status)
	if err != nil {
		return err
	}
	return store.SetConfig(PoolTargetBalanceStatusKey(poolName), string(data))
}

func DecisionFor(store *db.Store, enabledKey, windowsKey string, now time.Time) (Decision, []ActiveWindow, error) {
	enabled, err := Enabled(store, enabledKey)
	if err != nil {
		return Decision{}, nil, err
	}
	windows, err := LoadWindows(store, windowsKey)
	if err != nil {
		return Decision{}, nil, err
	}
	if !enabled || len(windows) == 0 {
		return Decision{Allowed: true, ActiveNow: enabled && len(windows) == 0}, windows, nil
	}
	active := IsActive(windows, now)
	decision := Decision{
		Allowed:   active,
		ActiveNow: active,
		Reason:    "outside configured spindown active window",
	}
	if !active {
		if next, ok := NextActive(windows, now); ok {
			decision.NextActiveAt = next.UTC().Format(time.RFC3339)
		}
	}
	return decision, windows, nil
}

func NormalizeWindows(windows []ActiveWindow) ([]ActiveWindow, error) {
	out := make([]ActiveWindow, 0, len(windows))
	for _, w := range windows {
		days, err := normalizeDays(w.Days)
		if err != nil {
			return nil, err
		}
		if _, err := parseClock(w.Start); err != nil {
			return nil, fmt.Errorf("invalid active window start %q: %w", w.Start, err)
		}
		if _, err := parseClock(w.End); err != nil {
			return nil, fmt.Errorf("invalid active window end %q: %w", w.End, err)
		}
		if w.Start == w.End {
			return nil, fmt.Errorf("active window start and end must differ")
		}
		out = append(out, ActiveWindow{Days: days, Start: w.Start, End: w.End})
	}
	return out, nil
}

func IsActive(windows []ActiveWindow, now time.Time) bool {
	for _, w := range windows {
		if windowContains(w, now) {
			return true
		}
	}
	return false
}

func NextActive(windows []ActiveWindow, now time.Time) (time.Time, bool) {
	if IsActive(windows, now) {
		return now, true
	}
	base := now.Truncate(time.Minute).Add(time.Minute)
	var best time.Time
	found := false
	for i := 0; i < 8*24*60; i++ {
		candidate := base.Add(time.Duration(i) * time.Minute)
		if !IsActive(windows, candidate) {
			continue
		}
		if !found || candidate.Before(best) {
			best = candidate
			found = true
		}
		break
	}
	return best, found
}

func windowContains(w ActiveWindow, now time.Time) bool {
	start, _ := parseClock(w.Start)
	end, _ := parseClock(w.End)
	nowMinute := now.Hour()*60 + now.Minute()
	today := strings.ToLower(now.Weekday().String()[:3])
	yesterday := strings.ToLower(now.AddDate(0, 0, -1).Weekday().String()[:3])
	overnight := end <= start
	if !overnight {
		return hasDay(w.Days, today) && nowMinute >= start && nowMinute < end
	}
	return (hasDay(w.Days, today) && nowMinute >= start) ||
		(hasDay(w.Days, yesterday) && nowMinute < end)
}

func normalizeDays(days []string) ([]string, error) {
	if len(days) == 0 {
		return []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}, nil
	}
	expanded := make(map[string]bool)
	for _, day := range days {
		switch strings.ToLower(strings.TrimSpace(day)) {
		case "daily", "all":
			for _, d := range []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"} {
				expanded[d] = true
			}
		case "weekdays":
			for _, d := range []string{"mon", "tue", "wed", "thu", "fri"} {
				expanded[d] = true
			}
		case "weekends":
			expanded["sat"] = true
			expanded["sun"] = true
		case "mon", "monday":
			expanded["mon"] = true
		case "tue", "tuesday":
			expanded["tue"] = true
		case "wed", "wednesday":
			expanded["wed"] = true
		case "thu", "thursday":
			expanded["thu"] = true
		case "fri", "friday":
			expanded["fri"] = true
		case "sat", "saturday":
			expanded["sat"] = true
		case "sun", "sunday":
			expanded["sun"] = true
		default:
			return nil, fmt.Errorf("invalid active window day %q", day)
		}
	}
	out := make([]string, 0, len(expanded))
	for _, d := range []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"} {
		if expanded[d] {
			out = append(out, d)
		}
	}
	return out, nil
}

func hasDay(days []string, day string) bool {
	for _, d := range days {
		if d == day {
			return true
		}
	}
	return false
}

func parseClock(s string) (int, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("expected HH:MM")
	}
	hour, err := strconv.Atoi(parts[0])
	if err != nil || hour < 0 || hour > 23 {
		return 0, fmt.Errorf("hour must be 00-23")
	}
	minute, err := strconv.Atoi(parts[1])
	if err != nil || minute < 0 || minute > 59 {
		return 0, fmt.Errorf("minute must be 00-59")
	}
	return hour*60 + minute, nil
}
