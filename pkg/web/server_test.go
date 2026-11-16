package web

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"warreth.dev/gphotos2immich/pkg/redact"
)

func TestSaveConfig(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	s := NewServer(configPath)

	body := []byte(`{
		"apiURL": "http://localhost:2283/api",
		"apiKey": "test-key",
		"workers": 2,
		"albumWorkers": 3,
		"debug": true,
		"strictMetadata": true,
		"skipVideos": true,
		"dataDir": "/tmp/g2i-data",
		"dedupMode": "repair",
		"updateExistingMetadata": true,
		"googlePhotos": []
	}`)

	req := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body))
	w := httptest.NewRecorder()

	s.handleConfig(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("failed to read written config: %v", err)
	}

	saved := string(content)
	for _, want := range []string{
		`"apiURL": "http://localhost:2283/api"`,
		`"albumWorkers": 3`,
		`"strictMetadata": true`,
		`"skipVideos": true`,
		`"dataDir": "/tmp/g2i-data"`,
		`"dedupMode": "repair"`,
		`"updateExistingMetadata": true`,
	} {
		if !strings.Contains(saved, want) {
			t.Fatalf("saved content missing %s: %s", want, saved)
		}
	}
}

func TestSaveConfigRejectsInvalidConfig(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	s := NewServer(configPath)

	body := []byte(`{
		"apiURL": "http://localhost:2283/api",
		"apiKey": "test-key",
		"notifications": {
			"ntfy": {"enabled": true, "url": "https://ntfy.sh", "topic": ""}
		},
		"googlePhotos": []
	}`)

	req := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.handleConfig(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid ntfy config, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "notifications.ntfy.topic") {
		t.Fatalf("expected the validation error, got %s", w.Body.String())
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("invalid config was written to disk: %v", err)
	}
}

func TestWriteConfigFileFallsBackWhenRenameIsBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}

	originalRename := renameFile
	renameFile = func(_, _ string) error { return syscall.EBUSY }
	t.Cleanup(func() { renameFile = originalRename })

	if err := writeConfigFile(path, []byte("new")); err != nil {
		t.Fatalf("writeConfigFile() = %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "new" {
		t.Fatalf("content = %q, want new", content)
	}
}

func TestTestConnection(t *testing.T) {
	immichServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/users/me" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"user-1","name":"Test User"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer immichServer.Close()

	s := NewServer("dummy.json")
	body := []byte(fmt.Sprintf(`{"apiURL":%q,"apiKey":"test-key"}`, immichServer.URL))
	req := httptest.NewRequest(http.MethodPost, "/api/test-connection", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.handleTestConnection(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Test User") {
		t.Fatalf("response missing user name: %s", w.Body.String())
	}
}

func TestSaveConfigPermissionError(t *testing.T) {
	// Create read-only directory
	tmpDir := t.TempDir()
	roDir := filepath.Join(tmpDir, "ro")
	if err := os.Mkdir(roDir, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.Chmod(roDir, 0755)
	})

	configPath := filepath.Join(roDir, "config.json")
	s := NewServer(configPath)

	body := []byte(`{
		"apiURL": "http://localhost:2283/api",
		"apiKey": "test-key"
	}`)

	req := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body))
	w := httptest.NewRecorder()

	s.handleConfig(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected status 500 when file cannot be written, got %d", resp.StatusCode)
	}

	bodyStr := w.Body.String()
	if !strings.Contains(bodyStr, "Failed to write config file") {
		t.Fatalf("expected error message to contain 'Failed to write config file', got %s", bodyStr)
	}
}

func TestHandleSync(t *testing.T) {
	s := NewServer("dummy.json")
	triggered := make(chan struct{}, 2)
	s.OnSyncTrigger = func() {
		triggered <- struct{}{}
	}

	// Test POST
	req := httptest.NewRequest(http.MethodPost, "/api/sync", nil)
	w := httptest.NewRecorder()
	s.handleSync(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on POST /api/sync, got %d", resp.StatusCode)
	}

	// Wait for the goroutine
	select {
	case <-triggered:
	case <-time.After(time.Second):
		t.Fatal("expected OnSyncTrigger to be called")
	}

	// Test GET (for easy browser/Home Assistant webhook usage)
	reqGet := httptest.NewRequest(http.MethodGet, "/sync_now", nil)
	wGet := httptest.NewRecorder()
	s.handleSync(wGet, reqGet)

	respGet := wGet.Result()
	if respGet.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on GET /sync_now, got %d", respGet.StatusCode)
	}

	select {
	case <-triggered:
	case <-time.After(time.Second):
		t.Fatal("expected OnSyncTrigger to be called on GET")
	}

	// Test Method Not Allowed
	reqDelete := httptest.NewRequest(http.MethodDelete, "/api/sync", nil)
	wDelete := httptest.NewRecorder()
	s.handleSync(wDelete, reqDelete)

	if wDelete.Result().StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 Method Not Allowed on DELETE, got %d", wDelete.Result().StatusCode)
	}
}
func TestHandleStatus(t *testing.T) {
	s := NewServer("dummy.json")

	req := httptest.NewRequest(http.MethodGet, "/sync/status", nil)
	w := httptest.NewRecorder()

	s.handleStatus(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on /sync/status, got %d", resp.StatusCode)
	}

	bodyStr := w.Body.String()
	if !strings.Contains(bodyStr, "immichUser") || !strings.Contains(bodyStr, "lastRun") || !strings.Contains(bodyStr, "nextRun") {
		t.Fatalf("response missing expected fields: %s", bodyStr)
	}
}

func TestLogBufferRedactsSecrets(t *testing.T) {
	t.Setenv(redact.EnvVar, "")
	redact.SetSecrets([]string{"super-secret-key"})
	t.Cleanup(func() { redact.SetSecrets(nil) })

	var stdout bytes.Buffer
	previous := originalStdout
	originalStdout = &stdout
	t.Cleanup(func() { originalStdout = previous })

	var buf LogBuffer
	if _, err := fmt.Fprintf(&buf, "api key super-secret-key and https://photos.google.com/share/AlbumSecret?key=ShareKey\n"); err != nil {
		t.Fatal(err)
	}

	for _, value := range []string{"super-secret-key", "AlbumSecret", "ShareKey"} {
		if strings.Contains(buf.String(), value) {
			t.Fatalf("sensitive value %q survived in the log buffer: %s", value, buf.String())
		}
		if strings.Contains(stdout.String(), value) {
			t.Fatalf("sensitive value %q survived in stdout: %s", value, stdout.String())
		}
	}
	if !strings.Contains(buf.String(), redact.Placeholder) {
		t.Fatalf("log buffer missing redaction placeholder: %s", buf.String())
	}
}

func TestLogBufferRedactsSecretSplitAcrossWrites(t *testing.T) {
	t.Setenv(redact.EnvVar, "")
	redact.SetSecrets([]string{"super-secret-key"})
	t.Cleanup(func() { redact.SetSecrets(nil) })

	var stdout bytes.Buffer
	previous := originalStdout
	originalStdout = &stdout
	t.Cleanup(func() { originalStdout = previous })

	var buf LogBuffer
	buf.Write([]byte("api key super-"))
	if strings.Contains(buf.String(), "super-secret") {
		t.Fatalf("partial secret leaked before the line ended: %s", buf.String())
	}
	buf.Write([]byte("secret-key\n"))

	if strings.Contains(buf.String(), "super-secret-key") || strings.Contains(stdout.String(), "super-secret-key") {
		t.Fatalf("split secret survived redaction: buffer=%s stdout=%s", buf.String(), stdout.String())
	}
	if !strings.Contains(buf.String(), redact.Placeholder) {
		t.Fatalf("log buffer missing redaction placeholder: %s", buf.String())
	}
}
