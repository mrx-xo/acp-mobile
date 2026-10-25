package main

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// wakeInfo says how to wake a sleeping machine (spec 6.2).
type wakeInfo struct {
	Mac   string `json:"mac"`
	Relay string `json:"relay"`
	From  string `json:"from"`
}

// machine is one registry entry.  addr overrides ip:port for tests.
type machine struct {
	ID   string    `json:"id"`
	Name string    `json:"name"`
	Host string    `json:"host"`
	IP   string    `json:"ip"`
	OS   string    `json:"os"`
	Role string    `json:"role"`
	Wake *wakeInfo `json:"wake"`
	Gaps []string  `json:"gaps"`

	addr string `json:"-"`
}

type registry struct {
	Machines []machine `json:"machines"`
}

// loadRegistry reads the registry file.  A missing file is the normal node
// case and yields an empty registry; malformed JSON is an error.
func loadRegistry(path string) (registry, error) {
	var reg registry
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return reg, nil
		}
		return reg, err
	}
	if err := json.Unmarshal(data, &reg); err != nil {
		return registry{}, err
	}
	return reg, nil
}

// peers returns every machine other than self.
func (r registry) peers(self string) []machine {
	var out []machine
	for _, m := range r.Machines {
		if m.ID != self {
			out = append(out, m)
		}
	}
	return out
}

func (r registry) byID(id string) (machine, bool) {
	for _, m := range r.Machines {
		if m.ID == id {
			return m, true
		}
	}
	return machine{}, false
}

// localMachineID is the first line of ~/.config/machine-id, else the first
// label of the tailscale hostname, else "local".
func localMachineID() string {
	if home, err := os.UserHomeDir(); err == nil {
		if data, err := os.ReadFile(filepath.Join(home, ".config", "machine-id")); err == nil {
			line, _, _ := strings.Cut(string(data), "\n")
			if id := strings.TrimSpace(line); id != "" {
				return id
			}
		}
	}
	if info, err := tailscaleSelf(); err == nil {
		if label, _, _ := strings.Cut(info.Hostname, "."); label != "" {
			return label
		}
	}
	return "local"
}

// registryPath is the configured registry, default ~/fleet/syzygy.json.
func registryPath() string {
	home, _ := os.UserHomeDir()
	p := appConfig.Registry
	if p == "" {
		return filepath.Join(home, "fleet", "syzygy.json")
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

var fleet struct {
	mu   sync.RWMutex
	reg  registry
	self string
}

// reloadFleet re-reads the registry.  A malformed file keeps the previous
// registry.
func reloadFleet() {
	path := registryPath()
	reg, err := loadRegistry(path)
	if err != nil {
		log.Printf("fleet: keeping previous registry, cannot load %s: %v", path, err)
		return
	}
	fleet.mu.Lock()
	fleet.reg = reg
	self := fleet.self
	fleet.mu.Unlock()
	log.Printf("fleet: %d peers (self=%s)", len(reg.peers(self)), self)
}
