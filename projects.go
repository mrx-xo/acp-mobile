package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"path/filepath"
)

type project struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Live bool   `json:"live"`
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

func handleProjects(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	live := projectLiveCwds()
	rig, err := loadProjects(r)
	if err != nil {
		if errors.Is(err, errElispNotFound) {
			if len(live) == 0 {
				writeElispError(w, "projects", err, "no projects on the rig")
				return
			}
			log.Printf("projects: %v; using live projects", err)
		} else if len(live) == 0 {
			writeElispError(w, "projects", err, "no projects on the rig")
			return
		} else {
			log.Printf("projects: %v; using live projects", err)
		}
		rig = []project{}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]interface{}{"projects": mergeProjects(rig, live)}); err != nil {
		log.Printf("projects: %v", err)
	}
}
