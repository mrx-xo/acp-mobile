//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// installFleetReload re-reads the registry on SIGHUP.
func installFleetReload() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	go func() {
		for range ch {
			reloadFleet()
		}
	}()
}
