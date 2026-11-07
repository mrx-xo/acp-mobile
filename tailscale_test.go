package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Concurrent runs of the macOS tailscale CLI race on the system VPN
// profile (2026-11-06: three simultaneous runs replaced the profile and
// left Tailscale stopped for twenty minutes), so every invocation must go
// through one lock.
func TestTailscaleOutputRunsOneCLIAtATime(t *testing.T) {
	old := tailscaleExec
	defer func() { tailscaleExec = old }()
	var inflight, peak int32
	tailscaleExec = func(ctx context.Context, args ...string) ([]byte, error) {
		n := atomic.AddInt32(&inflight, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if n <= p || atomic.CompareAndSwapInt32(&peak, p, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		atomic.AddInt32(&inflight, -1)
		return []byte("{}"), nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := tailscaleOutput(context.Background(), "status", "--json"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if peak != 1 {
		t.Fatalf("peak concurrent tailscale CLI runs = %d, want 1", peak)
	}
}
