//go:build windows

package main

import "testing"

func TestAllowedTranscriptPathAcceptsALowerCaseDrive(t *testing.T) {
	if !allowedTranscriptPath(`c:/Users/x/proj/.agent-shell/transcripts/a.md`, `C:\Users\x`) {
		t.Fatal("a transcript under the home folder was refused because of drive-letter case")
	}
	if allowedTranscriptPath(`c:/Users/other/proj/.agent-shell/transcripts/a.md`, `C:\Users\x`) {
		t.Fatal("a transcript outside the home folder was allowed")
	}
}
