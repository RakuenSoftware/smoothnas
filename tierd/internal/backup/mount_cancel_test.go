package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMountCancellationStopsFilesystemHelper(t *testing.T) {
	bin := t.TempDir()
	// Keep the helper's output pipes open in a child process: killing only
	// the mount wrapper would leave CombinedOutput waiting for this child.
	if err := os.WriteFile(filepath.Join(bin, "mount"), []byte("#!/bin/sh\nsleep 30 &\nwait\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	original := nfsDialFn
	nfsDialFn = func(context.Context, string) error { return nil }
	t.Cleanup(func() { nfsDialFn = original })
	for _, protocol := range []string{"nfs", "smb"} {
		t.Run(protocol, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			start := time.Now()
			err := mount(ctx, Config{TargetType: protocol, Host: "192.0.2.1", Share: "/share"}, t.TempDir())
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("cancelled mount: %v", err)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("helper survived cancellation: %v", elapsed)
			}
		})
	}
}

func TestMountPortProbeUsesRunContext(t *testing.T) {
	original := nfsDialFn
	nfsDialFn = func(ctx context.Context, _ string) error { <-ctx.Done(); return ctx.Err() }
	t.Cleanup(func() { nfsDialFn = original })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := mount(ctx, Config{TargetType: "nfs", Host: "192.0.2.1"}, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("probe ignored cancellation: %v", err)
	}
}
