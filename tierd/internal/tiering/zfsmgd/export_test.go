package zfsmgd

// This file exports internal symbols for use by the external _test package.
// These exports must only be used in tests.

// SetRecallTimeoutSeconds overrides the recall timeout for testing.
func (a *Adapter) SetRecallTimeoutSeconds(n int) { a.recallTimeoutSeconds = n }

// SetMigrationIOHighWaterPct overrides the I/O high-water threshold for testing.
func (a *Adapter) SetMigrationIOHighWaterPct(n int) { a.migrationIOHighWaterPct = n }

// SetIOStatProvider overrides the iostat implementation for testing.
func (a *Adapter) SetIOStatProvider(p IOStatProvider) { a.iostat = p }

// SetMovementWorkerConcurrency replaces the movement semaphore with a new one
// of the given size. Must be called before any movement workers are started.
func (a *Adapter) SetMovementWorkerConcurrency(n int) {
	a.movementWorkerConcurrency = n
	a.movementSem = make(chan struct{}, n)
}

// ExportedRecoverMovementLog exposes recoverMovementLog for direct testing.
func (a *Adapter) ExportedRecoverMovementLog() error { return a.recoverMovementLog() }
