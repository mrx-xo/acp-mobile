package main

import (
	"encoding/json"
	"log"
	"net/http"
)

// --- Launch presets ---
//
// The spawn sheet's preset chips mirror the rig's mr-x/agent-shell-presets
// (emacs.org) through syzygy-presets-json, so the phone never carries a
// copy that drifts.  Only the one-char key travels back on /api/spawn;
// Emacs resolves it.

type spawnPreset struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Model  string `json:"model"`
	Mode   string `json:"mode"`
	Agent  string `json:"agent"`
	Effort string `json:"effort"`
}

func loadPresets(r *http.Request) ([]spawnPreset, error) {
	raw, err := callElispJSON(r.Context(), "syzygy-presets-json")
	if err != nil {
		return nil, err
	}
	var presets []spawnPreset
	if err := json.Unmarshal(raw, &presets); err != nil {
		return nil, &elispError{err: err, output: string(raw)}
	}
	if presets == nil {
		presets = []spawnPreset{}
	}
	return presets, nil
}

// handlePresets replies with the rig's launch presets in rig order.
// 404 when the rig has no preset list at all (the bridge returned nil).
func handlePresets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	presets, err := loadPresets(r)
	if writeElispError(w, "presets", err, "no presets on the rig") {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]interface{}{"presets": presets}); err != nil {
		log.Printf("presets: %v", err)
	}
}
