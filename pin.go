package main

import (
	"encoding/json"
	"log"
	"net/http"
)

// Phone pin = an ordering hint that dies with the chat.  The Orrery sorts
// pinned chats to the top.  The list lives in the daemon
// (syzygy-orrery-pin-json) as buffer names, so a page reload sees the
// same set and a killed chat drops out on its own.  Nothing is persisted
// and nothing is shared with the Mac's major-pane anchoring.
//
//	GET  /api/pin                          {pins:[...]}
//	POST /api/pin {bufferName, action?}    toggle; action "pin" or "unpin" forces
//
// The POST reply is {bufferName, pinned, pins}.  A buffer that is not
// live is a bare nil from the daemon, which becomes 404.

type pinRequest struct {
	BufferName string `json:"bufferName"`
	Action     string `json:"action"`
}

func handlePin(w http.ResponseWriter, r *http.Request) {
	var body []byte
	var err error
	switch r.Method {
	case http.MethodGet:
		body, err = callElispJSON(r.Context(), "syzygy-orrery-pin-json")
	case http.MethodPost:
		var req pinRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.BufferName == "" || !validBufferName.MatchString(req.BufferName) {
			http.Error(w, "invalid buffer name", http.StatusBadRequest)
			return
		}
		action := elispRaw("nil")
		switch req.Action {
		case "":
		case "pin", "unpin":
			action = elispStr(req.Action)
		default:
			http.Error(w, "invalid action", http.StatusBadRequest)
			return
		}
		body, err = callElispJSON(r.Context(), "syzygy-orrery-pin-json",
			elispB64(req.BufferName), action)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if writeElispError(w, "pin", err, "no such chat") {
		return
	}
	if !json.Valid(body) {
		log.Printf("pin: unreadable reply %q", body)
		http.Error(w, "unreadable reply from the daemon", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}
