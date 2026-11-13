package web

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"warreth.dev/gphotos2immich/pkg/config"
	"warreth.dev/gphotos2immich/pkg/syncback"
)

// handleGoogleAccount manages the Google account used for backward upload.
// GET returns the current status, POST connects with an oauth_token, DELETE
// disconnects.
func (s *Server) handleGoogleAccount(w http.ResponseWriter, r *http.Request) {
	sb := s.syncBack()
	if sb == nil {
		http.Error(w, "Google Photos upload is not configured", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		status := sb.Account()
		if status.Validated.IsZero() {
			// First request after a restart: read the stored credential.
			ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			defer cancel()
			if refreshed, err := sb.Refresh(ctx); err == nil {
				status = refreshed
			} else {
				status.LastError = err.Error()
			}
		}
		json.NewEncoder(w).Encode(googleStatusResponse(status))

	case http.MethodPost:
		var body struct {
			OAuthToken string `json:"oauthToken"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "Invalid JSON body", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		status, err := sb.Connect(ctx, body.OAuthToken)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(googleStatusResponse(status))

	case http.MethodDelete:
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if err := sb.Disconnect(ctx); err != nil {
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(googleStatusResponse(sb.Account()))

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleGoogleAlbums lists Immich albums for the mapping dropdown.
func (s *Server) handleGoogleAlbums(w http.ResponseWriter, r *http.Request) {
	sb := s.syncBack()
	if sb == nil {
		http.Error(w, "Google Photos upload is not configured", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	albums, err := sb.ImmichAlbums(ctx)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"albums": albums})
}

// handleGoogleVerify resolves a share URL to its album key.
func (s *Server) handleGoogleVerify(w http.ResponseWriter, r *http.Request) {
	sb := s.syncBack()
	if sb == nil {
		http.Error(w, "Google Photos upload is not configured", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	info, err := sb.VerifyURL(ctx, body.URL)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"mediaKey":    info.MediaKey,
		"title":       info.Title,
		"items":       info.Items,
		"shared":      info.Shared,
		"owned":       info.Owned,
		"webVerified": info.WebVerified,
	})
}

// handleGoogleWebSession manages the optional photos.google.com cookies used
// to add items to albums that someone else owns.
func (s *Server) handleGoogleWebSession(w http.ResponseWriter, r *http.Request) {
	sb := s.syncBack()
	if sb == nil {
		http.Error(w, "Google Photos upload is not configured", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		status := sb.WebSessionStatus(ctx)
		if r.URL.Query().Get("refresh") == "1" {
			status = sb.RefreshWebSession(ctx)
		}
		json.NewEncoder(w).Encode(webStatusResponse(status))

	case http.MethodPost:
		var body struct {
			Cookies string `json:"cookies"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "Invalid JSON body", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		status, err := sb.ConnectWebSession(ctx, body.Cookies)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error":  err.Error(),
				"status": webStatusResponse(status),
			})
			return
		}
		json.NewEncoder(w).Encode(webStatusResponse(status))

	case http.MethodDelete:
		if err := sb.DisconnectWebSession(); err != nil {
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(webStatusResponse(sb.WebSessionStatus(r.Context())))

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleGoogleUpload runs the backward upload for one configured album.
func (s *Server) handleGoogleUpload(w http.ResponseWriter, r *http.Request) {
	sb := s.syncBack()
	if sb == nil {
		http.Error(w, "Google Photos upload is not configured", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		AlbumIndex int    `json:"albumIndex"`
		MediaKey   string `json:"mediaKey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	cfg, err := config.ReadConfig(s.configPath)
	if err != nil {
		http.Error(w, "Could not read configuration: "+err.Error(), http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()

	summary, err := sb.SyncAlbum(ctx, cfg, body.AlbumIndex, body.MediaKey)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":   err.Error(),
			"summary": summary,
		})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"summary": summary})
}

func googleStatusResponse(status syncback.AccountStatus) map[string]interface{} {
	resp := map[string]interface{}{
		"connected": status.Connected,
		"email":     status.Email,
		"version":   status.Version,
		"lastError": status.LastError,
	}
	if !status.Validated.IsZero() {
		resp["validated"] = status.Validated.UTC().Format(time.RFC3339)
	}
	return resp
}

func webStatusResponse(status syncback.WebStatus) map[string]interface{} {
	resp := map[string]interface{}{
		"connected": status.Connected,
		"lastError": status.LastError,
	}
	if !status.Validated.IsZero() {
		resp["validated"] = status.Validated.UTC().Format(time.RFC3339)
	}
	return resp
}
