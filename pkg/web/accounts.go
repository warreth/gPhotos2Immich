package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"warreth.dev/gphotos2immich/pkg/config"
	"warreth.dev/gphotos2immich/pkg/immich"
	"warreth.dev/gphotos2immich/pkg/syncback"
)

// handleImmichUsers lists the Immich users so the account wizard can offer a
// dropdown instead of asking for a UUID by hand.
func (s *Server) handleImmichUsers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cfg, err := config.ReadConfig(s.configPath)
	if err != nil {
		http.Error(w, "Could not read configuration: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(cfg.ApiURL) == "" || strings.TrimSpace(cfg.ApiKey) == "" {
		http.Error(w, "Immich URL and API key are required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	users, err := immich.NewClient(cfg.ApiURL, cfg.ApiKey, 1).ListUsers(ctx)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if users == nil {
		users = []immich.User{}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"users": users})
}

// handleAccounts manages the Immich user to Google account mappings.
func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	sb := s.syncBack()
	if sb == nil {
		http.Error(w, "Accounts are not configured", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		json.NewEncoder(w).Encode(accountsResponse(ctx, sb, s.configPath))

	case http.MethodPost:
		var in syncback.AccountInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "Invalid JSON body", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
		defer cancel()

		saved, err := sb.SaveAccount(ctx, in)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		if err := s.persistAccounts(sb.Accounts()); err != nil {
			http.Error(w, "Could not save accounts: "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.notifyConfigChange()

		resp := accountsResponse(ctx, sb, s.configPath)
		resp["account"] = saved
		json.NewEncoder(w).Encode(resp)

	case http.MethodDelete:
		id, err := accountIDFromRequest(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()

		if err := sb.DeleteAccount(ctx, id); err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, syncback.ErrAccountNotFound) {
				status = http.StatusNotFound
			}
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		if err := s.persistAccounts(sb.Accounts()); err != nil {
			http.Error(w, "Could not save accounts: "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.notifyConfigChange()

		json.NewEncoder(w).Encode(accountsResponse(ctx, sb, s.configPath))

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// accountsResponse collects the live account status and the routing preview.
func accountsResponse(ctx context.Context, sb *syncback.Service, configPath string) map[string]interface{} {
	cfg, _ := config.ReadConfig(configPath)
	return map[string]interface{}{
		"accounts": sb.AccountMappings(ctx),
		"routing":  sb.RoutingPreview(ctx, cfg),
	}
}

// accountIDFromRequest accepts ?id=<id> or a JSON body with {"id": "..."}.
func accountIDFromRequest(r *http.Request) (string, error) {
	if id := strings.TrimSpace(r.URL.Query().Get("id")); id != "" {
		return id, nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return "", errors.New("could not read request body")
	}
	var payload struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", errors.New("invalid JSON body")
	}
	id := strings.TrimSpace(payload.ID)
	if id == "" {
		return "", errors.New("id is required")
	}
	return id, nil
}

// persistAccounts writes the updated mappings into config.json while keeping
// every field the web UI does not own.
func (s *Server) persistAccounts(accounts []config.AccountMapping) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	existingContent, _ := os.ReadFile(s.configPath)
	var existingMap map[string]interface{}
	if len(existingContent) > 0 {
		_ = json.Unmarshal(existingContent, &existingMap)
	}
	if existingMap == nil {
		existingMap = make(map[string]interface{})
	}
	if accounts == nil {
		accounts = []config.AccountMapping{}
	}
	existingMap["accounts"] = accounts

	outBytes, err := json.MarshalIndent(existingMap, "", "    ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.configPath, outBytes, 0644)
}

// notifyConfigChange rebuilds the runtime services with the new configuration.
func (s *Server) notifyConfigChange() {
	if s.OnConfigChange != nil {
		go s.OnConfigChange()
	}
}
