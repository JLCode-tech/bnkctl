package test

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestRunConnectivity_LocalListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	addr := ln.Addr().String()
	res, err := RunConnectivity(context.Background(), ConnectivityConfig{
		Target:  addr,
		Proto:   "tcp",
		Count:   2,
		Timeout: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("RunConnectivity failed: %v", err)
	}

	if res.Successful != 2 {
		t.Fatalf("expected 2 successful probes, got %d", res.Successful)
	}
	if res.LossPercent != 0 {
		t.Fatalf("expected 0 loss, got %f", res.LossPercent)
	}
}
