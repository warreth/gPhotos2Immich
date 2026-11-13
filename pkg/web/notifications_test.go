package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"warreth.dev/gphotos2immich/pkg/config"
)

func writeNotificationConfig(t *testing.T, apiURL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Config{ApiURL: apiURL, ApiKey: "secret"}
	content, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNotificationTestEndpoint(t *testing.T) {
	var notification map[string]any
	immichServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/users/me":
			w.Write([]byte(`{"id":"user-1","name":"Test"}`))
		case "/api/admin/notifications":
			if err := json.NewDecoder(r.Body).Decode(&notification); err != nil {
				t.Errorf("decode notification: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"n1"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer immichServer.Close()

	server := NewServer(writeNotificationConfig(t, immichServer.URL+"/api"))
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/test", nil)
	w := httptest.NewRecorder()
	server.handleNotificationTest(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if notification["userId"] != "user-1" || notification["type"] != "Custom" || notification["level"] != "info" {
		t.Fatalf("notification = %#v", notification)
	}
}

func TestNotificationTestPermissionFailure(t *testing.T) {
	immichServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/users/me" {
			w.Write([]byte(`{"id":"user-1","name":"Test"}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"Missing notification.create permission"}`))
	}))
	defer immichServer.Close()

	server := NewServer(writeNotificationConfig(t, immichServer.URL+"/api"))
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/test", nil)
	w := httptest.NewRecorder()
	server.handleNotificationTest(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestNotificationTestMissingConfig(t *testing.T) {
	server := NewServer(writeNotificationConfig(t, ""))
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/test", nil)
	w := httptest.NewRecorder()
	server.handleNotificationTest(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", w.Code)
	}
}
