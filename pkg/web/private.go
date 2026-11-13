package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"warreth.dev/gphotos2immich/pkg/gphotosweb"
	"warreth.dev/gphotos2immich/pkg/privateimport"
)

// PrivateImportManager is the part of the private import manager the Web UI
// uses. The interface keeps the handlers testable.
type PrivateImportManager interface {
	Start(ctx context.Context, request privateimport.Request) (privateimport.Status, error)
	Status() privateimport.Status
	Cancel()
	Albums(ctx context.Context) ([]gphotosweb.Album, error)
}

// handlePrivateAlbums lists private albums in the signed-in Google account.
func (s *Server) handlePrivateAlbums(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	manager := s.privateImport()
	if manager == nil {
		http.Error(w, "Private album import is not configured", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	albums, err := manager.Albums(ctx)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"albums": albums})
}

// handlePrivateImport starts one private album import.
func (s *Server) handlePrivateImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	manager := s.privateImport()
	if manager == nil {
		http.Error(w, "Private album import is not configured", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		AlbumKey        string `json:"albumKey"`
		AlbumTitle      string `json:"albumTitle"`
		ImmichAlbumID   string `json:"immichAlbumId"`
		ImmichAlbumName string `json:"immichAlbumName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.AlbumKey) == "" {
		http.Error(w, "albumKey is required", http.StatusBadRequest)
		return
	}
	status, err := manager.Start(r.Context(), privateimport.Request{
		AlbumKey:        body.AlbumKey,
		AlbumTitle:      body.AlbumTitle,
		ImmichAlbumID:   body.ImmichAlbumID,
		ImmichAlbumName: body.ImmichAlbumName,
	})
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		code := http.StatusBadGateway
		if strings.Contains(err.Error(), "already running") {
			code = http.StatusConflict
		}
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "status": status})
		return
	}
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]any{"status": status})
}

// handlePrivateStatus returns the current or last private import state.
func (s *Server) handlePrivateStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	manager := s.privateImport()
	if manager == nil {
		http.Error(w, "Private album import is not configured", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(manager.Status())
}

// handlePrivateCancel stops the running private import.
func (s *Server) handlePrivateCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	manager := s.privateImport()
	if manager == nil {
		http.Error(w, "Private album import is not configured", http.StatusServiceUnavailable)
		return
	}
	manager.Cancel()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(manager.Status())
}
