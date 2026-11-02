package api

import (
	"context"
	"log"
	"time"

	"github.com/JBailes/SmoothNAS/tierd/internal/backup/schedule"
)

// StartScheduler is owned by the daemon, rather than constructors used in tests.
func (h *BackupHandler) StartScheduler(ctx context.Context) {
	go func() {
		h.scheduleTick(time.Now())
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				if h.isDraining() {
					return
				}
				h.scheduleTick(now)
			}
		}
	}()
}
func (h *BackupHandler) scheduleTick(now time.Time) {
	h.configMu.Lock()
	defer h.configMu.Unlock()
	if h.isDraining() {
		return
	}
	configs, err := h.store.ListBackupConfigs()
	if err != nil {
		log.Printf("backup schedule: %v", err)
		return
	}
	for _, cfg := range configs {
		if !cfg.ScheduleEnabled {
			continue
		}
		slot, due := schedule.Due(now, cfg.ScheduleTime, cfg.ScheduleDays, cfg.LastScheduledAt)
		if !due {
			continue
		}
		claimed, err := h.store.ClaimBackupSlot(cfg.ID, cfg.LastScheduledAt, slot.UTC().Format(time.RFC3339))
		if err != nil {
			log.Printf("backup schedule claim %d: %v", cfg.ID, err)
			continue
		}
		if !claimed {
			continue
		}
		active, err := h.store.ListBackupRunsByConfig(cfg.ID, true)
		if err != nil {
			log.Printf("backup schedule %d: %v", cfg.ID, err)
			continue
		}
		if len(active) > 0 {
			log.Printf("backup schedule %d: skipped slot; already running", cfg.ID)
			continue
		}
		full, err := h.store.GetBackupConfig(cfg.ID)
		if err != nil {
			continue
		}
		id, err := h.store.CreateBackupRun(cfg.ID)
		if err != nil {
			log.Printf("backup schedule %d: %v", cfg.ID, err)
			continue
		}
		if err = h.guardDeleteMode(full); err != nil {
			_ = h.store.FailBackupRun(id, err.Error())
			continue
		}
		if !h.startRun(id, full, false, false) {
			_ = h.store.FailBackupRun(id, "backup service is shutting down")
		}
	}
}
