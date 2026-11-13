package immich

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreateNotification(t *testing.T) {
	var body map[string]any
	var apiKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/admin/notifications" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q", r.Method)
		}
		apiKey = r.Header.Get("x-api-key")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"n1","title":"Test"}`))
	}))
	defer server.Close()

	client := NewClient(server.URL+"/api", "secret-key", 1)
	err := client.CreateNotification(context.Background(), Notification{
		UserID:      "user-1",
		Level:       "success",
		Title:       "Upload complete",
		Description: "3 photos added",
		Data:        map[string]any{"album": "Leiding"},
	})
	if err != nil {
		t.Fatalf("CreateNotification failed: %v", err)
	}
	if apiKey != "secret-key" {
		t.Fatalf("api key = %q", apiKey)
	}
	if body["userId"] != "user-1" || body["type"] != "Custom" || body["level"] != "success" {
		t.Fatalf("body = %#v", body)
	}
	if body["title"] != "Upload complete" || body["description"] != "3 photos added" {
		t.Fatalf("body = %#v", body)
	}
	if data, ok := body["data"].(map[string]any); !ok || data["album"] != "Leiding" {
		t.Fatalf("data = %#v", body["data"])
	}
}

func TestCreateNotificationDefaultsToInfo(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	client := NewClient(server.URL+"/api", "key", 1)
	if err := client.CreateNotification(context.Background(), Notification{
		UserID: "user-1", Title: "Test",
	}); err != nil {
		t.Fatalf("CreateNotification failed: %v", err)
	}
	if body["level"] != "info" {
		t.Fatalf("level = %#v", body["level"])
	}
}

func TestCreateNotificationPermissionError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"Missing notification.create permission"}`))
	}))
	defer server.Close()

	client := NewClient(server.URL+"/api", "key", 1)
	err := client.CreateNotification(context.Background(), Notification{
		UserID: "user-1", Title: "Test",
	})
	if err == nil {
		t.Fatal("expected a permission error")
	}
}

func TestCreateNotificationValidation(t *testing.T) {
	client := NewClient("http://example.invalid/api", "key", 1)
	if err := client.CreateNotification(context.Background(), Notification{Title: "Test"}); err == nil {
		t.Fatal("expected a missing user id error")
	}
	if err := client.CreateNotification(context.Background(), Notification{UserID: "u"}); err == nil {
		t.Fatal("expected a missing title error")
	}
	if err := client.CreateNotification(context.Background(), Notification{UserID: "u", Title: "t", Level: "nope"}); err == nil {
		t.Fatal("expected an invalid level error")
	}
}
