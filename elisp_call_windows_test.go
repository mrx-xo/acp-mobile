//go:build windows

package main

import (
	"strings"
	"testing"
)

func TestWrapElispForFileUsesForwardSlashesOnWindows(t *testing.T) {
	got := wrapElispForFile("(fn)", `C:\Users\x\AppData\Local\Temp\acp-elisp-1`)
	if strings.Contains(got, `\`) {
		t.Fatalf("wrapped expression has a backslash: %s", got)
	}
	if !strings.Contains(got, `"C:/Users/x/AppData/Local/Temp/acp-elisp-1"`) {
		t.Fatalf("wrapped expression = %s", got)
	}
}
