package web

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"warreth.dev/gphotos2immich/pkg/config"
	"warreth.dev/gphotos2immich/pkg/immich"
)

// handleNotificationTest sends one test notification through Immich. It is
// also the quickest way to check whether the API key can use the admin
// notification endpoint.
func (s *Server) handleNotificationTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg, err := config.ReadConfig(s.configPath)
	if err != nil {
		http.Error(w, "Could not read configuration: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if cfg.ApiURL == "" || cfg.ApiKey == "" {
		http.Error(w, "Configure the Immich URL and API key first", http.StatusBadRequest)
		return
	}

	client := immich.NewClient(cfg.ApiURL, cfg.ApiKey, 1)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	userID, _, err := client.GetUser(ctx)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{"error": "Could not read the Immich user: " + err.Error()})
		return
	}
	err = client.CreateNotification(ctx, immich.Notification{
		UserID:      userID,
		Level:       "info",
		Title:       "gPhotos2Immich test",
		Description: "Notifications are working.",
	})
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}
