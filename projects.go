package main

import (
	"encoding/json"
	"log"
	"net/http"
	"path/filepath"
	"time"
)

type project struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Live bool   `json:"live"`
	// Machine is the id of the machine that owns the project.
	Machine string `json:"machine,omitempty"`
}

func liveCwds() []string {
	seen := map[string]bool{}
	var cwds []string
	for _, sock := range discoverSockets() {
		cwd := probeSocket(sock.path, sock.pid).Cwd
		if cwd != "" && !seen[cwd] {
			seen[cwd] = true
			cwds = append(cwds, cwd)
		}
	}
	return cwds
}

var projectLiveCwds = liveCwds

func mergeProjects(rig []project, live []string) []project {
	result := make([]project, 0, len(rig)+len(live))
	seen := map[string]bool{}
	liveSet := map[string]bool{}
	for _, path := range live {
		liveSet[path] = true
	}
	for _, p := range rig {
		if seen[p.Path] {
			continue
		}
		p.Live = liveSet[p.Path]
		result = append(result, p)
		seen[p.Path] = true
	}
	for _, path := range live {
		if seen[path] {
			continue
		}
		result = append(result, project{Name: filepath.Base(path), Path: path, Live: true})
		seen[path] = true
	}
	return result
}

func loadProjects(r *http.Request) ([]project, error) {
	raw, err := callElispJSON(r.Context(), "syzygy-projects-json")
	if err != nil {
		return nil, err
	}
	var projects []project
	if err := json.Unmarshal(raw, &projects); err != nil {
		return nil, &elispError{err: err, output: string(raw)}
	}
	if projects == nil {
		projects = []project{}
	}
	return projects, nil
}

// projectsFanOutBudget bounds each peer's /api/projects answer.
const projectsFanOutBudget = 3 * time.Second

// mergedProjects is this machine's projects followed by every awake peer's,
// in registry order, each tagged with its machine.  A peer that fails is
// left out.  A project on two machines appears twice.
func mergedProjects(r *http.Request) ([]project, error) {
	local, err := localProjects(r)
	if err != nil {
		return nil, err
	}
	fleet.mu.RLock()
	self := fleet.self
	peers := fleet.reg.peers(self)
	fleet.mu.RUnlock()
	out := append(make([]project, 0, len(local)), local...)
	replies := fanOutJSON(r.Context(), "/api/projects", []byte(`{}`), projectsFanOutBudget, nil)
	for _, m := range peers {
		var body struct {
			Projects []project `json:"projects"`
		}
		if json.Unmarshal(replies[m.ID], &body) != nil {
			continue
		}
		for _, p := range body.Projects {
			p.Machine = m.ID
			out = append(out, p)
		}
	}
	return out, nil
}

// localProjects is this machine's rig projects plus live ones, tagged with
// this machine's id.  It fails only when there is nothing to list.
func localProjects(r *http.Request) ([]project, error) {
	live := projectLiveCwds()
	rig, err := loadProjects(r)
	if err != nil {
		if len(live) == 0 {
			return nil, err
		}
		log.Printf("projects: %v; using live projects", err)
		rig = []project{}
	}
	projects := mergeProjects(rig, live)
	fleet.mu.RLock()
	self := fleet.self
	fleet.mu.RUnlock()
	for i := range projects {
		projects[i].Machine = self
	}
	return projects, nil
}

func handleProjects(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	fleet.mu.RLock()
	hasPeers := len(fleet.reg.peers(fleet.self)) > 0
	fleet.mu.RUnlock()
	var projects []project
	var err error
	if isPeerRequest(r) || !hasPeers {
		projects, err = localProjects(r)
	} else {
		projects, err = mergedProjects(r)
	}
	if err != nil {
		writeElispError(w, "projects", err, "no projects on the rig")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]interface{}{"projects": projects}); err != nil {
		log.Printf("projects: %v", err)
	}
}
