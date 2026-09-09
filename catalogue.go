package main

import (
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strings"
	"unicode"
)

// Catalogue = keep a chat on purpose, durably, with a note and tags.
// The store is agent-recall's sidecar metadata on the rig; this endpoint
// only validates and forwards to the syzygy-recall bridges:
//
//	GET  /api/catalogue?sessionId=ID          read the current state
//	POST /api/catalogue {sessionId,note,tags} save (or edit)
//	POST /api/catalogue {sessionId,uncatalogue:true} drop the flag
//
// Every reply is the bridge's own JSON: sessionId, catalogued (ISO
// timestamp or ""), note, tags, allTags.  A session the index has never
// seen is a bare nil from the daemon, which becomes 404.

const (
	catalogueNoteLimit = 500
	catalogueTagLimit  = 40
	catalogueTagsLimit = 20
)

// validSessionID admits the UUIDs claude-agent-acp hands out and the
// looser ids other agents use, and nothing that could break out of an
// elisp string or a URL.
var validSessionID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

type catalogueRequest struct {
	SessionID   string   `json:"sessionId"`
	Note        string   `json:"note"`
	Tags        []string `json:"tags"`
	Uncatalogue bool     `json:"uncatalogue"`
}

// normalizeCatalogueTags mirrors agent-recall--catalogue-normalize-tags
// so the phone's echo of the saved tags matches what the rig stores:
// trimmed, without a leading #, lowercased, deduplicated, empties gone.
func normalizeCatalogueTags(tags []string) []string {
	seen := map[string]bool{}
	clean := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(tag), "#"))
		tag = strings.ToLower(tag)
		if tag == "" || seen[tag] {
			continue
		}
		seen[tag] = true
		clean = append(clean, tag)
	}
	return clean
}

// validateCatalogueRequest returns a client-facing reason when REQ is
// unusable, or "" when it is fine.  Tags are normalized in place.
func validateCatalogueRequest(req *catalogueRequest) string {
	if !validSessionID.MatchString(req.SessionID) {
		return "invalid session id"
	}
	if req.Uncatalogue {
		return ""
	}
	if len([]rune(req.Note)) > catalogueNoteLimit {
		return "note too long"
	}
	if strings.IndexFunc(req.Note, unicode.IsControl) >= 0 {
		return "note must be printable"
	}
	req.Tags = normalizeCatalogueTags(req.Tags)
	if len(req.Tags) > catalogueTagsLimit {
		return "too many tags"
	}
	for _, tag := range req.Tags {
		if len([]rune(tag)) > catalogueTagLimit {
			return "tag too long"
		}
		if strings.IndexFunc(tag, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0 {
			return "tags must be single words"
		}
	}
	return ""
}

func handleCatalogue(w http.ResponseWriter, r *http.Request) {
	var req catalogueRequest
	switch r.Method {
	case http.MethodGet:
		req.SessionID = r.URL.Query().Get("sessionId")
	case http.MethodPost:
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if reason := validateCatalogueRequest(&req); reason != "" {
		http.Error(w, reason, http.StatusBadRequest)
		return
	}

	var body []byte
	var err error
	switch {
	case r.Method == http.MethodGet:
		body, err = callElispJSON(r.Context(), "syzygy-recall-catalogue-get-json",
			elispB64(req.SessionID))
	case req.Uncatalogue:
		body, err = callElispJSON(r.Context(), "syzygy-recall-uncatalogue-json",
			elispB64(req.SessionID))
	default:
		tags, _ := json.Marshal(req.Tags)
		body, err = callElispJSON(r.Context(), "syzygy-recall-catalogue-json",
			elispB64(req.SessionID), elispB64(req.Note), elispB64(string(tags)))
	}
	if writeElispError(w, "catalogue", err, "no such session in the transcript index") {
		return
	}
	if !json.Valid(body) {
		log.Printf("catalogue: unreadable reply %q", body)
		http.Error(w, "unreadable reply from the daemon", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}
