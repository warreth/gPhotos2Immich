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
	"sync"
	"time"

	"warreth.dev/gphotos2immich/pkg/config"
	"warreth.dev/gphotos2immich/pkg/immich"
	"warreth.dev/gphotos2immich/pkg/progress"
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
}

func NewServer(configPath string) *Server {
	return &Server{configPath: configPath}
}

func (s *Server) Start(port int) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/logs", s.handleLogs)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/sync/status", s.handleStatus)
	mux.HandleFunc("/api/sync", s.handleSync)
	mux.HandleFunc("/sync_now", s.handleSync)

	addr := fmt.Sprintf(":%d", port)
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	fmt.Printf("Config Web UI running on http://localhost%s\n", addr)
	return srv.ListenAndServe()
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
	var newCfg config.Config
	if err := json.NewDecoder(r.Body).Decode(&newCfg); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}

	// Read existing to keep any extra fields intact
	existingContent, _ := os.ReadFile(s.configPath)
	var existingMap map[string]interface{}
	if len(existingContent) > 0 {
		_ = json.Unmarshal(existingContent, &existingMap)
	}
	if existingMap == nil {
		existingMap = make(map[string]interface{})
	}

	// Update only fields exposed in UI
	existingMap["apiURL"] = newCfg.ApiURL
	existingMap["apiKey"] = newCfg.ApiKey
	existingMap["workers"] = newCfg.Workers
	existingMap["debug"] = newCfg.Debug
	existingMap["googlePhotos"] = newCfg.GooglePhotos

	// Prettify output
	outBytes, err := json.MarshalIndent(existingMap, "", "    ")
	if err != nil {
		http.Error(w, "Failed to serialize config", http.StatusInternalServerError)
		return
	}

	if err := os.WriteFile(s.configPath, outBytes, 0644); err != nil {
		fmt.Printf("Failed to write config file %s: %v\n", s.configPath, err)
		http.Error(w, fmt.Sprintf("Failed to write config file: %v", err), http.StatusInternalServerError)
		return
	}

	if s.OnConfigChange != nil {
		go s.OnConfigChange()
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status": "ok"}`))
}
