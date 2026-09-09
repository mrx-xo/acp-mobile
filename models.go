package main

import (
	"encoding/json"
	"log"
	"net/http"
)

func registerModelHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/api/models", handleModels)
	mux.HandleFunc("/api/model", handleModel)
}

func validModelID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x20 || id[i] > 0x7e || id[i] == '"' || id[i] == '\\' {
			return false
		}
	}
	return true
}

func handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		BufferName string `json:"bufferName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.BufferName == "" || !validBufferName.MatchString(req.BufferName) {
		http.Error(w, "invalid buffer name", http.StatusBadRequest)
		return
	}
	raw, err := callElispJSON(r.Context(), "syzygy-models-json", elispStr(req.BufferName))
	if writeElispError(w, "models", err, "no such session") {
		return
	}
	var resp struct {
		Current string `json:"current"`
		Models  []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		writeElispError(w, "models", &elispError{err: err, output: string(raw)}, "no such session")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(json.RawMessage(raw)); err != nil {
		log.Printf("models: %v", err)
	}
}

func handleModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		BufferName string `json:"bufferName"`
		ModelID    string `json:"modelId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.BufferName == "" || !validBufferName.MatchString(req.BufferName) {
		http.Error(w, "invalid buffer name", http.StatusBadRequest)
		return
	}
	if !validModelID(req.ModelID) {
		http.Error(w, "invalid model id", http.StatusBadRequest)
		return
	}
	raw, err := callElispJSON(r.Context(), "syzygy-model-set-json",
		elispStr(req.BufferName), elispStr(req.ModelID))
	if writeElispError(w, "model", err, "no such session") {
		return
	}
	var resp struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		writeElispError(w, "model", &elispError{err: err, output: string(raw)}, "no such session")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if !resp.OK {
		w.WriteHeader(http.StatusConflict)
	}
	if err := json.NewEncoder(w).Encode(json.RawMessage(raw)); err != nil {
		log.Printf("model: %v", err)
	}
}
