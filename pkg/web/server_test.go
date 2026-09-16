package web

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveConfig(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	s := NewServer(configPath)

	body := []byte(`{
		"apiURL": "http://localhost:2283/api",
		"apiKey": "test-key",
		"workers": 2,
		"debug": true,
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

	if !strings.Contains(string(content), "http://localhost:2283/api") {
		t.Fatalf("saved content missing expected apiURL: %s", string(content))
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
