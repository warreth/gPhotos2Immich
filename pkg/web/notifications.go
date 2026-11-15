package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"warreth.dev/gphotos2immich/pkg/config"
	"warreth.dev/gphotos2immich/pkg/immich"
	"warreth.dev/gphotos2immich/pkg/notify"
)

// handleNotificationTest sends one test notification through the selected
// channel. Immich also checks whether the API key can use the admin
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

	var req struct {
		Channel string `json:"channel"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}
	channel := strings.ToLower(strings.TrimSpace(req.Channel))
	if channel == "" {
		channel = "immich"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	w.Header().Set("Content-Type", "application/json")
	switch channel {
	case "immich":
		target, userID, err := notify.TargetFromConfig(ctx, cfg)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		err = target.CreateNotification(ctx, immich.Notification{
			UserID:      userID,
			Level:       "info",
			Title:       "gPhotos2Immich test",
			Description: "Notifications are working.",
		})
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	case "ntfy":
		if err := notify.TestNtfy(ctx, cfg.Notifications.Ntfy); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	default:
		http.Error(w, "channel must be immich or ntfy", http.StatusBadRequest)
		return
	}

	json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": channel})
}
