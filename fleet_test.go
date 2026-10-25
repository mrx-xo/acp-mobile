package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRegistryParsesFixture(t *testing.T) {
	reg, err := loadRegistry("testdata/syzygy.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Machines) != 3 {
		t.Fatalf("machines = %d, want 3", len(reg.Machines))
	}
	m, ok := reg.byID("vengeance")
	if !ok || m.Name != "NEMESIS" || m.OS != "windows" || m.Role != "node" {
		t.Fatalf("vengeance = %+v", m)
	}
	if m.Wake == nil || m.Wake.Relay != "homelab" {
		t.Fatalf("wake = %+v", m.Wake)
	}
	if got := reg.peers("mrx"); len(got) != 2 {
		t.Fatalf("peers = %d, want 2", len(got))
	}
}

func TestLoadRegistryMissingFileIsNode(t *testing.T) {
	reg, err := loadRegistry(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.peers("mrx")) != 0 {
		t.Fatal("missing registry must yield no peers")
	}
}

func TestLoadRegistryRejectsMalformed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(p, []byte("{nope"), 0600)
	if _, err := loadRegistry(p); err == nil {
		t.Fatal("want parse error")
	}
}
