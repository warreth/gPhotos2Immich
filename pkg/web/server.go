package web

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"warreth.dev/gphotos2immich/pkg/config"
	"warreth.dev/gphotos2immich/pkg/immich"
	"warreth.dev/gphotos2immich/pkg/progress"
	"warreth.dev/gphotos2immich/pkg/syncback"
)

const maxLogSize = 100 * 1024 // 100KB max logs stored

type LogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

var originalStdout = os.Stdout

func (l *LogBuffer) Write(p []byte) (n int, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// write to original os.Stdout
	originalStdout.Write(p)

	l.buf.Write(p)
	if l.buf.Len() > maxLogSize {
		// simple truncation: drop first half
		truncated := l.buf.Bytes()[l.buf.Len()/2:]
		l.buf.Reset()
		l.buf.Write(truncated)
	}
	return len(p), nil
}

func (l *LogBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

var GlobalLogBuffer = &LogBuffer{}

//go:embed index.html
var indexHTML []byte

type Server struct {
	configPath     string
	mu             sync.Mutex
	OnConfigChange func()
	OnSyncTrigger  func()
	SyncBack       *syncback.Service
	PrivateImport  PrivateImportManager
}

func NewServer(configPath string) *Server {
	return &Server{configPath: configPath}
}

func (s *Server) Start(port int) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/immich/users", s.handleImmichUsers)
	mux.HandleFunc("/api/accounts", s.handleAccounts)
	mux.HandleFunc("/api/primary-profile", s.handlePrimaryProfile)
	mux.HandleFunc("/api/test-connection", s.handleTestConnection)
	mux.HandleFunc("/api/logs", s.handleLogs)
	mux.HandleFunc("/api/activity", s.handleActivity)
	mux.HandleFunc("/api/live", s.handleLive)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/sync/status", s.handleStatus)
	mux.HandleFunc("/api/sync", s.handleSync)
	mux.HandleFunc("/sync_now", s.handleSync)
	mux.HandleFunc("/api/google/account", s.handleGoogleAccount)
	mux.HandleFunc("/api/google/albums", s.handleGoogleAlbums)
	mux.HandleFunc("/api/google/verify", s.handleGoogleVerify)
	mux.HandleFunc("/api/google/upload", s.handleGoogleUpload)
	mux.HandleFunc("/api/google/web-session", s.handleGoogleWebSession)
	mux.HandleFunc("/api/google/private/albums", s.handlePrivateAlbums)
	mux.HandleFunc("/api/google/private/import", s.handlePrivateImport)
	mux.HandleFunc("/api/google/private/status", s.handlePrivateStatus)
	mux.HandleFunc("/api/google/private/cancel", s.handlePrivateCancel)
	mux.HandleFunc("/api/notifications/test", s.handleNotificationTest)

	addr := fmt.Sprintf(":%d", port)
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	fmt.Printf("Config Web UI running on http://localhost%s\n", addr)
	return srv.ListenAndServe()
}

// SetSyncBack replaces the sync-back service, used after a config reload.
func (s *Server) SetSyncBack(service *syncback.Service) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.SyncBack = service
}

// syncBack returns the current service. Handlers use it instead of reading the
// field directly, so a config reload cannot race with an in-flight request.
func (s *Server) syncBack() *syncback.Service {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.SyncBack
}

// SetPrivateImport replaces the private import manager after a config reload.
func (s *Server) SetPrivateImport(manager PrivateImportManager) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.PrivateImport = manager
}

func (s *Server) privateImport() PrivateImportManager {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.PrivateImport
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	io.WriteString(w, GlobalLogBuffer.String())
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.OnSyncTrigger != nil {
		go s.OnSyncTrigger()
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok","message":"Sync triggered"}`))
}

var cachedImmichUser string
var cachedImmichUserMu sync.Mutex

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	// Get current target album progress
	album, processed, total := progress.ActiveStatus()

	// Optionally resolve user if not known (don't block long though)
	userName := ""
	cachedImmichUserMu.Lock()
	userName = cachedImmichUser
	cachedImmichUserMu.Unlock()

	if userName == "" {
		// Read config to initialize client
		cfgBytes, err := os.ReadFile(s.configPath)
		if err == nil {
			var configData struct {
				ApiURL string `json:"apiURL"`
				ApiKey string `json:"apiKey"`
			}
			if err := json.Unmarshal(cfgBytes, &configData); err == nil && configData.ApiURL != "" && configData.ApiKey != "" {
				go func() {
					client := immich.NewClient(configData.ApiURL, configData.ApiKey, 1)
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if _, name, err := client.GetUser(ctx); err == nil && name != "" {
						cachedImmichUserMu.Lock()
						cachedImmichUser = name
						cachedImmichUserMu.Unlock()
					}
				}()
			}
		}
	}

	lastRun, nextRun := progress.ScheduleTimes()
	var lastRunStr, nextRunStr string
	if !lastRun.IsZero() {
		lastRunStr = lastRun.UTC().Format(time.RFC3339)
	}
	if !nextRun.IsZero() {
		nextRunStr = nextRun.UTC().Format(time.RFC3339)
	}

	resp := map[string]interface{}{
		"immichUser": userName,
		"album":      album,
		"processed":  processed,
		"total":      total,
		"lastRun":    lastRunStr,
		"nextRun":    nextRunStr,
		"activity":   progress.CurrentSnapshot(),
	}

	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch r.Method {
	case http.MethodGet:
		s.getConfig(w, r)
	case http.MethodPost:
		s.saveConfig(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	content, err := os.ReadFile(s.configPath)
	if err != nil {
		if os.IsNotExist(err) {
			content = []byte(`{"googlePhotos": []}`) // return empty structure if missing
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(content)
}

func (s *Server) saveConfig(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Could not read request body", http.StatusBadRequest)
		return
	}
	var newCfg config.Config
	if err := json.Unmarshal(body, &newCfg); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(body, &raw)

	// Read existing to keep any extra fields intact
	existingContent, _ := os.ReadFile(s.configPath)
	var existingMap map[string]interface{}
	if len(existingContent) > 0 {
		_ = json.Unmarshal(existingContent, &existingMap)
	}
	if existingMap == nil {
		existingMap = make(map[string]interface{})
	}
	var existingCfg config.Config
	if len(existingContent) > 0 {
		_ = json.Unmarshal(existingContent, &existingCfg)
	}

	// The settings form may omit the API key so a saved page never overwrites
	// the stored secret with an empty value.
	apiKey := newCfg.ApiKey
	if strings.TrimSpace(apiKey) == "" {
		apiKey = existingCfg.ApiKey
	}
	connectionChanged := newCfg.ApiURL != existingCfg.ApiURL || apiKey != existingCfg.ApiKey

	// Update every field exposed in the UI, preserving any extra fields that
	// were added to config.json by hand.
	existingMap["apiURL"] = newCfg.ApiURL
	existingMap["apiKey"] = apiKey
	existingMap["debug"] = newCfg.Debug
	existingMap["workers"] = newCfg.Workers
	existingMap["albumWorkers"] = newCfg.AlbumWorkers
	existingMap["strictMetadata"] = newCfg.StrictMetadata
	existingMap["skipVideos"] = newCfg.SkipVideos
	existingMap["dataDir"] = newCfg.DataDir
	existingMap["dedupMode"] = newCfg.DedupMode
	existingMap["updateExistingMetadata"] = newCfg.UpdateExistingMetadata
	existingMap["notifications"] = mergeNotifications(existingMap["notifications"], newCfg.Notifications, raw["notifications"])
	if _, ok := raw["uiMode"]; ok {
		existingMap["uiMode"] = newCfg.UIMode
	}
	// Merge so fields the UI does not expose (binary and credential paths,
	// retries, auto-update) are preserved.
	existingMap["googleUpload"] = mergeGoogleUpload(existingMap["googleUpload"], newCfg.GoogleUpload)
	existingMap["googlePhotos"] = newCfg.GooglePhotos

	// Accounts are edited on their own API, so an omitted list keeps the one on
	// disk instead of erasing every mapping.
	accounts := newCfg.Accounts
	if _, ok := raw["accounts"]; ok {
		existingMap["accounts"] = accounts
	} else {
		accounts = existingCfg.Accounts
	}

	// Prettify output
	outBytes, err := json.MarshalIndent(existingMap, "", "    ")
	if err != nil {
		http.Error(w, "Failed to serialize config", http.StatusInternalServerError)
		return
	}

	// Validate the merged document before writing it. The live app refuses an
	// invalid config, so writing one would leave the file and the running
	// service out of sync while the UI reported a successful save.
	mergedBytes, err := json.Marshal(existingMap)
	if err != nil {
		http.Error(w, "Failed to serialize config", http.StatusInternalServerError)
		return
	}
	var mergedCfg config.Config
	if err := json.Unmarshal(mergedBytes, &mergedCfg); err != nil {
		http.Error(w, "Failed to read merged config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := mergedCfg.Validate(); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	if err := writeConfigFile(s.configPath, outBytes); err != nil {
		fmt.Printf("Failed to write config file %s: %v\n", s.configPath, err)
		http.Error(w, fmt.Sprintf("Failed to write config file: %v", err), http.StatusInternalServerError)
		return
	}

	// Keep the live service in step with the file. The handler already holds
	// s.mu, so read the field directly instead of calling syncBack().
	if sb := s.SyncBack; sb != nil {
		sb.SetAccounts(accounts, newCfg.GoogleUpload.StrictAccountMatching)
	}

	// The cached Immich user may belong to the previous credentials.
	if connectionChanged {
		cachedImmichUserMu.Lock()
		cachedImmichUser = ""
		cachedImmichUserMu.Unlock()
	}

	if s.OnConfigChange != nil {
		go s.OnConfigChange()
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status": "ok"}`))
}

// writeConfigFile replaces the config atomically so a concurrent reader never
// sees a half-written JSON document. The existing file mode is preserved.
func writeConfigFile(path string, data []byte) error {
	mode := os.FileMode(0644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// mergeNotifications keeps nested notification settings the current form does
// not expose. The Immich toggles are owned by the form; an account selection
// or ntfy block that was not submitted stays as stored.
func mergeNotifications(existing interface{}, incoming config.NotificationsConfig, raw json.RawMessage) map[string]interface{} {
	merged := map[string]interface{}{}
	if stored, ok := existing.(map[string]interface{}); ok {
		for key, value := range stored {
			merged[key] = value
		}
	}
	if len(raw) == 0 {
		return merged
	}
	var sent map[string]json.RawMessage
	if err := json.Unmarshal(raw, &sent); err != nil {
		return merged
	}
	if _, ok := sent["immich"]; ok {
		merged["immich"] = incoming.Immich
	}
	if _, ok := sent["immichAccountId"]; ok {
		merged["immichAccountId"] = incoming.ImmichAccountID
	}
	if _, ok := sent["onSyncFailed"]; ok {
		merged["onSyncFailed"] = incoming.OnSyncFailed
	}
	if _, ok := sent["onImportComplete"]; ok {
		merged["onImportComplete"] = incoming.OnImportComplete
	}
	if _, ok := sent["onUploadComplete"]; ok {
		merged["onUploadComplete"] = incoming.OnUploadComplete
	}
	if _, ok := sent["ntfy"]; ok {
		merged["ntfy"] = incoming.Ntfy
	}
	return merged
}

// mergeGoogleUpload keeps non-UI fields from the existing config while taking
// the values the UI owns from the request.
func mergeGoogleUpload(existing interface{}, incoming config.GoogleUploadConfig) map[string]interface{} {
	merged := map[string]interface{}{}
	if raw, ok := existing.(map[string]interface{}); ok {
		for key, value := range raw {
			merged[key] = value
		}
	}
	merged["enabled"] = incoming.Enabled
	merged["storageMode"] = incoming.StorageMode
	merged["quality"] = incoming.Quality
	merged["skipLivePhotos"] = incoming.SkipLivePhotos
	merged["reuploadDeleted"] = incoming.ReuploadDeleted
	merged["threads"] = incoming.Threads
	merged["strictAccountMatching"] = incoming.StrictAccountMatching
	return merged
}

func (s *Server) handleTestConnection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ApiURL string `json:"apiURL"`
		ApiKey string `json:"apiKey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.ApiURL == "" || req.ApiKey == "" {
		http.Error(w, "apiURL and apiKey are required", http.StatusBadRequest)
		return
	}

	client := immich.NewClient(req.ApiURL, req.ApiKey, 1)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	id, name, err := client.GetUserOnce(ctx)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{
		"status": "ok",
		"userId": id,
		"user":   name,
	})
}
