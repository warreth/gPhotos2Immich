package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"warreth.dev/gphotos2immich/pkg/progress"
)

// handleActivity returns the current structured activity snapshot. It is the
// polling fallback for browsers without EventSource support.
func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cachedImmichUserMu.Lock()
	userName := cachedImmichUser
	cachedImmichUserMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{
		"snapshot":   progress.CurrentSnapshot(),
		"immichUser": userName,
	})
}

// handleLive streams activity snapshots over Server-Sent Events. One
// connection replaces the dashboard and log polling loops.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming is not supported by this server", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	cachedImmichUserMu.Lock()
	userName := cachedImmichUser
	cachedImmichUserMu.Unlock()

	send := func() bool {
		cachedImmichUserMu.Lock()
		userName = cachedImmichUser
		cachedImmichUserMu.Unlock()
		payload := map[string]any{
			"snapshot":   progress.CurrentSnapshot(),
			"immichUser": userName,
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	updates, cancel := progress.Subscribe()
	defer cancel()
	if !send() {
		return
	}

	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-updates:
			// Coalesce bursts: one item per file should not become one HTTP
			// write per item.
			select {
			case <-time.After(200 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
			if !send() {
				return
			}
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
