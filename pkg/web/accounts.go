package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	if strings.TrimSpace(cfg.ApiURL) == "" || strings.TrimSpace(cfg.EffectiveApiKey()) == "" {
		http.Error(w, "Immich URL and API key are required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	users, err := immich.NewClient(cfg.ApiURL, cfg.EffectiveApiKey(), 1).ListUsers(ctx)
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
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		s.migrateLegacyPrimary(ctx)
		json.NewEncoder(w).Encode(accountsResponse(ctx, sb, s.configPath))

	case http.MethodPost:
		if sb == nil {
			http.Error(w, "Accounts are not available until the connection is configured", http.StatusServiceUnavailable)
			return
		}
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
		if sb == nil {
			http.Error(w, "Accounts are not available until the connection is configured", http.StatusServiceUnavailable)
			return
		}
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

// migrateLegacyPrimary turns a pre-account config into the unified model. The
// legacy apiKey belongs to the Immich user that originally powered the
// connection; create or update that user's mapping and make it the default
// import account. It runs at most once per legacy key.
func (s *Server) migrateLegacyPrimary(ctx context.Context) {
	cfg, err := config.ReadConfig(s.configPath)
	if err != nil || cfg == nil {
		return
	}
	key := strings.TrimSpace(cfg.ApiKey)
	if key == "" || strings.TrimSpace(cfg.ApiURL) == "" {
		return
	}
	for _, acc := range cfg.Accounts {
		if strings.TrimSpace(acc.ImmichAPIKey) == key {
			return
		}
	}
	id, name, err := immich.NewClient(cfg.ApiURL, key, 1).GetUserOnce(ctx)
	if err != nil || strings.TrimSpace(id) == "" {
		return
	}
	accounts := append([]config.AccountMapping(nil), cfg.Accounts...)
	found := false
	for i := range accounts {
		if strings.EqualFold(accounts[i].ImmichUserID, id) || strings.EqualFold(accounts[i].ID, id) {
			accounts[i].ImmichAPIKey = key
			if strings.TrimSpace(accounts[i].Name) == "" {
				accounts[i].Name = name
			}
			found = true
			break
		}
	}
	if !found {
		accounts = append(accounts, config.NormalizeAccount(config.AccountMapping{
			ID:           id,
			Name:         name,
			ImmichUserID: id,
			ImmichAPIKey: key,
		}))
	}
	if s.SyncBack != nil {
		status := s.SyncBack.Account()
		if status.Connected && strings.TrimSpace(status.Email) != "" {
			for i := range accounts {
				if strings.EqualFold(accounts[i].ImmichUserID, id) && strings.TrimSpace(accounts[i].GoogleEmail) == "" {
					accounts[i].GoogleEmail = strings.TrimSpace(status.Email)
				}
			}
		}
	}
	if err := s.persistAccounts(accounts); err != nil {
		return
	}
	if s.SyncBack != nil {
		s.SyncBack.SetAccounts(accounts, cfg.GoogleUpload.StrictAccountMatching)
	}
	s.notifyConfigChange()
}

// accountsResponse collects the live account status, the two independent
// defaults and the Immich URL. Without a running service it reports the stored
// accounts so a first-run page can still render.
func accountsResponse(ctx context.Context, sb *syncback.Service, configPath string) map[string]interface{} {
	cfg, _ := config.ReadConfig(configPath)
	defaults := map[string]string{"importAccount": "", "googleAccount": ""}
	immichURL := ""
	resp := map[string]interface{}{
		"accounts":  []syncback.AccountMappingStatus{},
		"defaults":  defaults,
		"immichURL": immichURL,
	}
	if cfg != nil {
		immichURL = strings.TrimSpace(cfg.ApiURL)
		defaults = map[string]string{
			"importAccount": cfg.DefaultImportAccount,
			"googleAccount": cfg.DefaultGoogleAccount,
		}
		resp["defaults"] = defaults
		resp["immichURL"] = immichURL
	}

	if sb == nil {
		if cfg != nil {
			resp["accounts"] = offlineAccountStatuses(cfg.Accounts)
		}
		return resp
	}
	resp["primary"] = sb.PrimaryStatus(ctx, cfg)
	resp["accounts"] = sb.AccountMappings(ctx)
	resp["routing"] = sb.RoutingPreview(ctx, cfg)
	return resp
}

// offlineAccountStatuses exposes the stored mappings when no service is
// running, without testing any remote account.
func offlineAccountStatuses(accounts []config.AccountMapping) []syncback.AccountMappingStatus {
	out := make([]syncback.AccountMappingStatus, 0, len(accounts))
	for _, acc := range accounts {
		path := config.EffectiveAccountCookiesFile(acc)
		_, err := os.Stat(path)
		out = append(out, syncback.AccountMappingStatus{
			AccountMapping: acc,
			CookiesFile:    path,
			CookiesSaved:   err == nil,
		})
	}
	return out
}

// handlePrimaryProfile edits the primary Immich URL and API key without
// exposing the rest of the connection settings on a separate page.
func (s *Server) handlePrimaryProfile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cfg, err := config.ReadConfig(s.configPath)
	if err != nil {
		http.Error(w, "Could not read configuration: "+err.Error(), http.StatusBadRequest)
		return
	}

	var req struct {
		ApiURL string `json:"apiURL"`
		ApiKey string `json:"apiKey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	apiURL := strings.TrimSpace(req.ApiURL)
	if apiURL == "" {
		apiURL = strings.TrimSpace(cfg.ApiURL)
	}
	apiKey := strings.TrimSpace(req.ApiKey)
	if apiKey == "" {
		apiKey = strings.TrimSpace(cfg.EffectiveApiKey())
	}
	if apiURL == "" || apiKey == "" {
		http.Error(w, "apiURL and apiKey are required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	userID, userName, err := immich.NewClient(apiURL, apiKey, 1).GetUserOnce(ctx)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Could not connect to Immich: " + err.Error()})
		return
	}

	if err := s.writePrimaryProfile(apiURL, apiKey); err != nil {
		http.Error(w, "Could not save the primary profile: "+err.Error(), http.StatusInternalServerError)
		return
	}

	cachedImmichUserMu.Lock()
	cachedImmichUser = ""
	cachedImmichUserMu.Unlock()
	s.notifyConfigChange()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":     true,
		"user":   userName,
		"userId": userID,
	})
}

// writePrimaryProfile changes the connection fields and preserves every other
// value in config.json. With accounts configured, the key belongs to the
// default import account, so the legacy endpoint updates that mapping instead
// of leaving a mirror that ReadConfig would immediately clear.
func (s *Server) writePrimaryProfile(apiURL, apiKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	existingContent, err := os.ReadFile(s.configPath)
	if err != nil {
		return err
	}
	var existingMap map[string]interface{}
	if err := json.Unmarshal(existingContent, &existingMap); err != nil {
		return err
	}
	if existingMap == nil {
		existingMap = make(map[string]interface{})
	}
	var cfg config.Config
	if err := json.Unmarshal(existingContent, &cfg); err != nil {
		return err
	}

	existingMap["apiURL"] = apiURL
	if len(cfg.Accounts) > 0 {
		cfg.ApplyDefaults()
		index := 0
		for i, acc := range cfg.Accounts {
			if config.NormalizeAccount(acc).ID == cfg.DefaultImportAccount {
				index = i
				break
			}
		}
		cfg.Accounts[index] = config.NormalizeAccount(cfg.Accounts[index])
		cfg.Accounts[index].ImmichAPIKey = strings.TrimSpace(apiKey)
		cfg.ApiKey = apiKey
		cfg.ApplyDefaults()
		writeAccountDefaults(existingMap, &cfg)
	} else {
		existingMap["apiKey"] = apiKey
	}

	outBytes, err := json.MarshalIndent(existingMap, "", "    ")
	if err != nil {
		return err
	}
	return writeConfigFile(s.configPath, outBytes)
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

// persistAccounts writes the updated mappings into config.json and derives
// both default ids, the isDefault sync and the connection-key mirror with the
// same rules ReadConfig uses.
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

	normalized := make([]config.AccountMapping, 0, len(accounts))
	for _, acc := range accounts {
		normalized = append(normalized, config.NormalizeAccount(acc))
	}

	var cfg config.Config
	if len(existingContent) > 0 {
		_ = json.Unmarshal(existingContent, &cfg)
	}
	cfg.Accounts = normalized
	cfg.ApplyDefaults()
	writeAccountDefaults(existingMap, &cfg)

	outBytes, err := json.MarshalIndent(existingMap, "", "    ")
	if err != nil {
		return err
	}
	return writeConfigFile(s.configPath, outBytes)
}

// writeAccountDefaults stores the account list with its derived defaults and
// the internal connection-key mirror in a raw config document.
func writeAccountDefaults(doc map[string]interface{}, cfg *config.Config) {
	accounts := cfg.Accounts
	if accounts == nil {
		accounts = []config.AccountMapping{}
	}
	doc["accounts"] = accounts
	setOptionalConfigString(doc, "defaultImportAccount", cfg.DefaultImportAccount)
	setOptionalConfigString(doc, "defaultGoogleAccount", cfg.DefaultGoogleAccount)
	doc["apiKey"] = cfg.EffectiveApiKey()
}

// setOptionalConfigString writes a value, removing the key when it is empty so
// the JSON omitempty tags stay meaningful.
func setOptionalConfigString(doc map[string]interface{}, key, value string) {
	if strings.TrimSpace(value) == "" {
		delete(doc, key)
		return
	}
	doc[key] = value
}

// handleSetupAccount creates the first Immich account when the application is
// not running yet, for example right after a reset. Existing accounts are only
// accepted when they belong to the same Immich user.
func (s *Server) handleSetupAccount(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		APIURL       string `json:"apiURL"`
		Name         string `json:"name"`
		ImmichAPIKey string `json:"immichApiKey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}
	apiURL := strings.TrimSpace(req.APIURL)
	apiKey := strings.TrimSpace(req.ImmichAPIKey)
	if apiURL == "" || apiKey == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "apiURL and immichApiKey are required"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	userID, userName, err := immich.NewClient(apiURL, apiKey, 1).GetUserOnce(ctx)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Could not connect to Immich: " + err.Error()})
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	existingContent, err := os.ReadFile(s.configPath)
	if err != nil && !os.IsNotExist(err) {
		http.Error(w, "Could not read configuration: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var existingCfg config.Config
	if len(existingContent) > 0 {
		if err := json.Unmarshal(existingContent, &existingCfg); err != nil {
			http.Error(w, "Could not read configuration: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	var existingMap map[string]interface{}
	if len(existingContent) > 0 {
		_ = json.Unmarshal(existingContent, &existingMap)
	}
	if existingMap == nil {
		existingMap = make(map[string]interface{})
	}

	// Only the first account (or the same Immich user) may be created here;
	// additional mappings belong on /api/accounts.
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = strings.TrimSpace(userName)
	}
	accounts := make([]config.AccountMapping, 0, len(existingCfg.Accounts))
	setupIndex := -1
	for _, acc := range existingCfg.Accounts {
		acc = config.NormalizeAccount(acc)
		if strings.EqualFold(strings.TrimSpace(acc.ImmichUserID), userID) ||
			strings.EqualFold(strings.TrimSpace(acc.ID), userID) {
			if setupIndex >= 0 {
				continue // never duplicate the same Immich user
			}
			acc.ImmichUserID = userID
			acc.ImmichAPIKey = apiKey
			if acc.ID == "" {
				acc.ID = userID
			}
			if name != "" {
				acc.Name = name
			}
			setupIndex = len(accounts)
			accounts = append(accounts, config.NormalizeAccount(acc))
			continue
		}
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "an account already exists; use /api/accounts to add or change accounts",
		})
		return
	}
	if setupIndex < 0 {
		setupIndex = len(accounts)
		accounts = append(accounts, config.NormalizeAccount(config.AccountMapping{
			ID:           userID,
			Name:         name,
			ImmichUserID: userID,
			ImmichAPIKey: apiKey,
		}))
	}

	existingCfg.Accounts = accounts
	existingCfg.ApiKey = apiKey
	existingCfg.ApiURL = apiURL
	existingCfg.DefaultImportAccount = userID
	existingCfg.ApplyDefaults()

	existingMap["apiURL"] = apiURL
	writeAccountDefaults(existingMap, &existingCfg)

	outBytes, err := json.MarshalIndent(existingMap, "", "    ")
	if err != nil {
		http.Error(w, "Failed to serialize config", http.StatusInternalServerError)
		return
	}
	if err := writeConfigFile(s.configPath, outBytes); err != nil {
		http.Error(w, "Failed to write config file: "+err.Error(), http.StatusInternalServerError)
		return
	}

	cachedImmichUserMu.Lock()
	cachedImmichUser = ""
	cachedImmichUserMu.Unlock()
	s.notifyConfigChange()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":      true,
		"account": existingCfg.Accounts[setupIndex],
	})
}

// handleResetAccounts clears every account and credential so a user can start
// over. It works without a running application, because it only edits the
// configuration and the stored credential files.
func (s *Server) handleResetAccounts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	existingContent, err := os.ReadFile(s.configPath)
	if err != nil && !os.IsNotExist(err) {
		http.Error(w, "Could not read configuration: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var cfg config.Config
	if len(existingContent) > 0 {
		if err := json.Unmarshal(existingContent, &cfg); err != nil {
			http.Error(w, "Could not read configuration: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	var existingMap map[string]interface{}
	if len(existingContent) > 0 {
		_ = json.Unmarshal(existingContent, &existingMap)
	}
	if existingMap == nil {
		existingMap = make(map[string]interface{})
	}

	// Collect the credential paths before the account list is cleared.
	credentialFiles := []string{cfg.EffectiveGotohpConfig()}
	if path := strings.TrimSpace(cfg.GoogleUpload.WebCookiesFile); path != "" {
		credentialFiles = append(credentialFiles, path)
	}
	for _, acc := range cfg.Accounts {
		credentialFiles = append(credentialFiles, config.EffectiveAccountCookiesFile(acc))
	}

	cfg.Accounts = nil
	cfg.ApiKey = ""
	cfg.DefaultImportAccount = ""
	cfg.DefaultGoogleAccount = ""
	cfg.Notifications.Immich = false
	cfg.Notifications.ImmichAccountID = ""
	cfg.Notifications.Ntfy.Token = ""
	cfg.GoogleUpload.Enabled = false
	for i := range cfg.GooglePhotos {
		cfg.GooglePhotos[i].ImmichAlbumID = ""
		cfg.GooglePhotos[i].ImmichAccountID = ""
		cfg.GooglePhotos[i].UploadToGoogle = false
	}
	if cfg.GooglePhotos == nil {
		cfg.GooglePhotos = []config.GooglePhotosConfig{}
	}

	existingMap["accounts"] = []config.AccountMapping{}
	existingMap["apiKey"] = ""
	delete(existingMap, "defaultImportAccount")
	delete(existingMap, "defaultGoogleAccount")
	existingMap["notifications"] = cfg.Notifications
	existingMap["googleUpload"] = cfg.GoogleUpload
	existingMap["googlePhotos"] = cfg.GooglePhotos

	outBytes, err := json.MarshalIndent(existingMap, "", "    ")
	if err != nil {
		http.Error(w, "Failed to serialize config", http.StatusInternalServerError)
		return
	}
	if err := writeConfigFile(s.configPath, outBytes); err != nil {
		http.Error(w, "Failed to write config file: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Credentials go last, so a failed config write never strands a config that
	// still points at a deleted session.
	for _, path := range credentialFiles {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			fmt.Printf("Could not remove credential file %s: %v\n", path, err)
		}
	}

	s.notifyConfigChange()

	// The reset config is not valid yet, so OnConfigChange will not rebuild
	// the service. Clear the live mappings directly so the page reflects it.
	if sb := s.SyncBack; sb != nil {
		sb.SetAccounts(nil, false)
	}

	w.Write([]byte(`{"status":"ok"}`))
}

// notifyConfigChange rebuilds the runtime services with the new configuration.
func (s *Server) notifyConfigChange() {
	if s.OnConfigChange != nil {
		go s.OnConfigChange()
	}
}
