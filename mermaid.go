package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
)

// handleAsset serves the vendored Mermaid bundle.  Immutable and a year
// long because the file only ever changes when the bundle is replaced, and
// the phone should never re-download 2.7 MB it already has.
func handleAsset(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.FileServerFS(assetsFS).ServeHTTP(w, r)
}

// handleMermaidConfig serves the rig's Mermaid configuration so the phone's
// diagrams look exactly like the Emacs markdown-xwidget preview.  Emacs
// writes the file from `mr-x/markdown-mermaid-config'
// (syzygy-export-mermaid-config); this is a read of that sidecar, nothing
// more.
//
// A missing file is not an error.  404 tells the page to stay on the
// MERMAID_CONFIG defaults built into index.html — the same hand-mirrored
// pattern AGENT_CUES uses — so a fresh machine, or a machine with no Emacs,
// still renders diagrams.
func handleMermaidConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		http.Error(w, "no mermaid config", http.StatusNotFound)
		return
	}
	data, err := os.ReadFile(filepath.Join(home, ".acp-mobile", "mermaid.json"))
	if err != nil {
		http.Error(w, "no mermaid config", http.StatusNotFound)
		return
	}
	// A truncated write (Emacs exporting while we read) must not poison the
	// page's config; fall back rather than serve half an object.
	if !json.Valid(data) {
		http.Error(w, "no mermaid config", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(data)
}
