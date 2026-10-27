package nfs

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestWaitForServer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := waitForServer(ctx, address); err != nil {
		t.Fatal(err)
	}
	listener.Close()
	ctx, cancel = context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := waitForServer(ctx, address); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("closed server: %v", err)
	}
}
