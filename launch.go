package main

import (
	"encoding/json"
	"net/http"
)

// Explicit choices are identifiers from the rig's advertised catalogue.
// Constructors and credential handling stay entirely inside Emacs.
type launchSettings struct {
	Agent  string `json:"agent"`
	Model  string `json:"model"`
	Mode   string `json:"mode"`
	Effort string `json:"effort"`
}

func handleLaunchOptions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	raw, err := callElispJSON(r.Context(), "syzygy-launch-options-json")
	if writeElispError(w, "launch-options", err, "Launch settings are unavailable") {
		return
	}
	var reply struct {
		Agents       []json.RawMessage `json:"agents"`
		DefaultAgent string            `json:"defaultAgent"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil || reply.Agents == nil {
		http.Error(w, "Invalid launch settings response", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(raw)
}

func handleExplicitLaunch(w http.ResponseWriter, r *http.Request, req spawnRequest) {
	s := req.Settings
	if req.CloneOf != "" || req.Cwd == "" ||
		!validModelID(s.Agent) || !validModelID(s.Model) || !validModelID(s.Mode) ||
		(s.Effort != "" && !validModelID(s.Effort)) {
		http.Error(w, "Invalid launch settings", 400)
		return
	}
	encoded, err := json.Marshal(req)
	if err != nil {
		http.Error(w, "Invalid launch request", 400)
		return
	}
	raw, err := callElispJSON(r.Context(), "syzygy-launch-json", elispB64(string(encoded)))
	if writeElispError(w, "launch", err, "Agent launch is unavailable") {
		return
	}
	var reply struct {
		OK         *bool  `json:"ok"`
		BufferName string `json:"bufferName"`
		Error      string `json:"error"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil || reply.OK == nil ||
		(*reply.OK && reply.BufferName == "") || (!*reply.OK && reply.Error == "") {
		http.Error(w, "Invalid agent launch response", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if !*reply.OK {
		w.WriteHeader(http.StatusConflict)
	}
	_, _ = w.Write(raw)
}
