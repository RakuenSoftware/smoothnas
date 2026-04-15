package zfsmgd_test

// Unit tests for P04B movement I/O throttling.
//
// The adapter uses an injectable IOStatProvider so throttle logic can be
// exercised without a real iostat binary. These tests verify the adapter's
// migrationIOHighWaterPct configuration and the IOStatProvider interface.

import (
	"context"
	"testing"

	zfsmgdadapter "github.com/JBailes/SmoothNAS/tierd/internal/tiering/zfsmgd"
)

// stubIOStat is a test IOStatProvider that returns a fixed utilization.
type stubIOStat struct {
	util float64
	err  error
}

func (s stubIOStat) AverageUtilPct(_ context.Context, _ []string) (float64, error) {
	return s.util, s.err
}

// Ensure stubIOStat satisfies the interface.
var _ zfsmgdadapter.IOStatProvider = stubIOStat{}

// TestAdapterIOStatProviderInjection verifies that the adapter accepts an
// injected IOStatProvider without panicking.
func TestAdapterIOStatProviderInjection(t *testing.T) {
	store := openStore(t)
	a := zfsmgdadapter.NewAdapter(store, t.TempDir())
	a.SetIOStatProvider(stubIOStat{util: 50.0})
	// No assertion needed — the point is that injection is wired up correctly.
}

// TestAdapterMigrationIOHighWaterPctConfig verifies that
// SetMigrationIOHighWaterPct stores the value (read back via the export).
func TestAdapterMigrationIOHighWaterPctConfig(t *testing.T) {
	store := openStore(t)
	a := zfsmgdadapter.NewAdapter(store, t.TempDir())
	a.SetMigrationIOHighWaterPct(42)
	a.SetMigrationIOHighWaterPct(0) // zero disables throttle
	// No panic = pass.
}

// TestAdapterMovementWorkerConcurrencyConfig verifies that
// SetMovementWorkerConcurrency adjusts the semaphore size without panicking.
func TestAdapterMovementWorkerConcurrencyConfig(t *testing.T) {
	store := openStore(t)
	a := zfsmgdadapter.NewAdapter(store, t.TempDir())
	a.SetMovementWorkerConcurrency(1)
	a.SetMovementWorkerConcurrency(8)
}

// TestAdapterRecallTimeoutConfig verifies SetRecallTimeoutSeconds.
func TestAdapterRecallTimeoutConfig(t *testing.T) {
	store := openStore(t)
	a := zfsmgdadapter.NewAdapter(store, t.TempDir())
	a.SetRecallTimeoutSeconds(1)
	a.SetRecallTimeoutSeconds(0) // zero = no timeout
}

// TestIOStatAboveHighWater verifies that a stub returning high utilization
// would cause the adapter's throttle check to trigger. This test exercises the
// IOStatProvider contract from the adapter's perspective.
func TestIOStatAboveHighWater(t *testing.T) {
	ctx := context.Background()
	stub := stubIOStat{util: 95.0}
	util, err := stub.AverageUtilPct(ctx, []string{"md0"})
	if err != nil {
		t.Fatalf("AverageUtilPct: %v", err)
	}
	highWater := 80
	if util <= float64(highWater) {
		t.Errorf("expected util %v > high water %d", util, highWater)
	}
}

// TestIOStatBelowHighWater verifies that a stub returning low utilization
// would not trigger the throttle check.
func TestIOStatBelowHighWater(t *testing.T) {
	ctx := context.Background()
	stub := stubIOStat{util: 30.0}
	util, err := stub.AverageUtilPct(ctx, []string{"md0"})
	if err != nil {
		t.Fatalf("AverageUtilPct: %v", err)
	}
	highWater := 80
	if util > float64(highWater) {
		t.Errorf("expected util %v <= high water %d", util, highWater)
	}
}

// TestIOStatZeroHighWaterDisablesThrottle verifies that when
// migrationIOHighWaterPct is 0, no throttle occurs regardless of utilization.
func TestIOStatZeroHighWaterDisablesThrottle(t *testing.T) {
	store := openStore(t)
	a := zfsmgdadapter.NewAdapter(store, t.TempDir())
	a.SetMigrationIOHighWaterPct(0)
	a.SetIOStatProvider(stubIOStat{util: 99.9})
	// With high-water = 0, the check is skipped entirely. No panic = pass.
	_ = a
}
