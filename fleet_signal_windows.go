//go:build windows

package main

// installFleetReload is a no-op: Windows has no SIGHUP.
func installFleetReload() {}
