package notify

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"warreth.dev/gphotos2immich/pkg/config"
	"warreth.dev/gphotos2immich/pkg/immich"
)

type fakeSender struct {
	calls []immich.Notification
	err   error
}

func (f *fakeSender) CreateNotification(_ context.Context, notification immich.Notification) error {
	f.calls = append(f.calls, notification)
	return f.err
}

func enabledConfig() config.NotificationsConfig {
	return config.NotificationsConfig{
		Immich:           true,
		OnSyncFailed:     true,
		OnImportComplete: false,
		OnUploadComplete: true,
	}
}

func TestSendEnabledEvent(t *testing.T) {
	sender := &fakeSender{}
	service := New(sender, enabledConfig(), nil)
	service.SetUserID("user-1")

	service.Send(context.Background(), EventUploadComplete, "success", "Upload complete", "3 photos added", map[string]any{"album": "Leiding"})
	if len(sender.calls) != 1 {
		t.Fatalf("calls = %#v", sender.calls)
	}
	call := sender.calls[0]
	if call.UserID != "user-1" || call.Level != "success" || call.Title != "Upload complete" {
		t.Fatalf("call = %#v", call)
	}
	if call.Data["album"] != "Leiding" {
		t.Fatalf("data = %#v", call.Data)
	}
}

func TestSendSkipsDisabledEvents(t *testing.T) {
	sender := &fakeSender{}
	service := New(sender, enabledConfig(), nil)
	service.SetUserID("user-1")

	service.Send(context.Background(), EventImportComplete, "success", "Import complete", "", nil)
	if len(sender.calls) != 0 {
		t.Fatalf("disabled event was sent: %#v", sender.calls)
	}
}

func TestSendRequiresUserID(t *testing.T) {
	sender := &fakeSender{}
	service := New(sender, enabledConfig(), nil)

	service.Send(context.Background(), EventSyncFailed, "error", "Sync failed", "", nil)
	if len(sender.calls) != 0 {
		t.Fatalf("notification without a user id was sent: %#v", sender.calls)
	}
}

func TestFailedSendDoesNotPanic(t *testing.T) {
	sender := &fakeSender{err: errors.New("403 missing permission")}
	service := New(sender, enabledConfig(), nil)
	service.SetUserID("user-1")

	service.Send(context.Background(), EventSyncFailed, "error", "Sync failed", "album failed", nil)
	if len(sender.calls) != 1 {
		t.Fatalf("calls = %#v", sender.calls)
	}
}

func TestCancelledContextStillSends(t *testing.T) {
	sender := &fakeSender{}
	service := New(sender, enabledConfig(), nil)
	service.SetUserID("user-1")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	service.Send(ctx, EventSyncFailed, "error", "Sync failed", "", nil)
	if len(sender.calls) != 1 {
		t.Fatalf("calls = %#v", sender.calls)
	}
}

func ntfyConfig(serverURL string) config.NotificationsConfig {
	return config.NotificationsConfig{
		Ntfy: config.NtfyConfig{
			Enabled:      true,
			URL:          serverURL,
			Topic:        "family photos",
			Token:        "tk_test",
			Priority:     "high",
			OnSyncFailed: true,
		},
	}
}

func TestNtfySendPostsTitlePriorityAndBody(t *testing.T) {
	var gotPath, gotBody, gotTitle, gotPriority, gotAuth, gotContentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotTitle = r.Header.Get("Title")
		gotPriority = r.Header.Get("Priority")
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"n1"}`))
	}))
	defer server.Close()

	cfg := config.NtfyConfig{URL: server.URL + "/", Topic: "family photos", Token: "tk_test", Priority: "high"}
	if err := NewNtfyClient().Send(context.Background(), cfg, "Sync failed", "album broke", cfg.Priority); err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if gotPath != "/family%20photos" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotBody != "album broke" {
		t.Fatalf("body = %q", gotBody)
	}
	if gotTitle != "Sync failed" || gotPriority != "high" {
		t.Fatalf("title = %q priority = %q", gotTitle, gotPriority)
	}
	if gotAuth != "Bearer tk_test" {
		t.Fatalf("authorization = %q", gotAuth)
	}
	if gotContentType != "text/plain; charset=utf-8" {
		t.Fatalf("content type = %q", gotContentType)
	}
}

func TestNtfySendOmitsOptionalHeaders(t *testing.T) {
	var gotAuth, gotPriority string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPriority = r.Header.Get("Priority")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := config.NtfyConfig{URL: server.URL, Topic: "topic"}
	if err := NewNtfyClient().Send(context.Background(), cfg, "title", "message", ""); err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if gotAuth != "" {
		t.Fatalf("unexpected authorization header %q", gotAuth)
	}
	if gotPriority != "" {
		t.Fatalf("unexpected priority header %q", gotPriority)
	}
}

func TestNtfySendReportsHTTPFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("topic is reserved"))
	}))
	defer server.Close()

	err := NewNtfyClient().Send(context.Background(), config.NtfyConfig{URL: server.URL, Topic: "topic"}, "t", "m", "")
	if err == nil {
		t.Fatal("expected an error")
	}
	text := err.Error()
	if !strings.Contains(text, "403") || !strings.Contains(text, "topic is reserved") {
		t.Fatalf("error = %q", text)
	}
}

func TestTestNtfySendsTestMessage(t *testing.T) {
	var title, body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		title = r.Header.Get("Title")
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := TestNtfy(context.Background(), config.NtfyConfig{URL: server.URL, Topic: "topic"}); err != nil {
		t.Fatalf("TestNtfy failed: %v", err)
	}
	if title != "gPhotos2Immich test" || body != "Notifications are working." {
		t.Fatalf("title = %q body = %q", title, body)
	}
}

func TestTargetFromConfigPrimaryProfile(t *testing.T) {
	var gotKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/me" {
			http.NotFound(w, r)
			return
		}
		gotKey = r.Header.Get("x-api-key")
		w.Write([]byte(`{"id":"primary-user","name":"Primary"}`))
	}))
	defer server.Close()

	cfg := &config.Config{ApiURL: server.URL, ApiKey: "primary-key"}
	target, userID, err := TargetFromConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("TargetFromConfig failed: %v", err)
	}
	if target == nil || userID != "primary-user" {
		t.Fatalf("target = %#v user = %q", target, userID)
	}
	if gotKey != "primary-key" {
		t.Fatalf("api key = %q", gotKey)
	}
}

func TestTargetFromConfigMappedAccount(t *testing.T) {
	cfg := &config.Config{
		ApiURL: "http://immich.invalid/api",
		ApiKey: "primary-key",
		Notifications: config.NotificationsConfig{
			ImmichAccountID: " ACC-LOTTE ",
		},
		Accounts: []config.AccountMapping{{
			ID: "acc-lotte", ImmichUserID: "lotte-user", ImmichAPIKey: "lotte-key",
		}},
	}
	target, userID, err := TargetFromConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("TargetFromConfig failed: %v", err)
	}
	if target == nil || userID != "lotte-user" {
		t.Fatalf("target = %#v user = %q", target, userID)
	}
	client, ok := target.(*immich.Client)
	if !ok {
		t.Fatalf("target type = %T", target)
	}
	if client.APIKey != "lotte-key" || client.APIURL != cfg.ApiURL {
		t.Fatalf("client = %q %q", client.APIURL, client.APIKey)
	}
}

func TestTargetFromConfigMappedAccountCanResolveUser(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "lotte-key" {
			http.Error(w, "bad key", http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"id":"resolved-user","name":"Lotte"}`))
	}))
	defer server.Close()

	cfg := &config.Config{
		ApiURL: server.URL,
		ApiKey: "primary-key",
		Notifications: config.NotificationsConfig{
			ImmichAccountID: "acc-lotte",
		},
		Accounts: []config.AccountMapping{{
			ID: "acc-lotte", ImmichAPIKey: "lotte-key",
		}},
	}
	_, userID, err := TargetFromConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("TargetFromConfig failed: %v", err)
	}
	if userID != "resolved-user" {
		t.Fatalf("user = %q", userID)
	}
}

func TestTargetFromConfigErrors(t *testing.T) {
	missing := &config.Config{
		ApiURL: "http://immich/api", ApiKey: "k",
		Notifications: config.NotificationsConfig{ImmichAccountID: "missing"},
	}
	_, _, err := TargetFromConfig(context.Background(), missing)
	if err == nil || !strings.Contains(err.Error(), "notification target account not found") {
		t.Fatalf("error = %v", err)
	}

	keyless := &config.Config{
		ApiURL: "http://immich/api", ApiKey: "k",
		Notifications: config.NotificationsConfig{ImmichAccountID: "acc-1"},
		Accounts:      []config.AccountMapping{{ID: "acc-1", ImmichUserID: "u1"}},
	}
	_, _, err = TargetFromConfig(context.Background(), keyless)
	if err == nil || !strings.Contains(err.Error(), "notification target account has no Immich API key") {
		t.Fatalf("error = %v", err)
	}
}

func TestSendIsIndependentPerChannel(t *testing.T) {
	var ntfyHits int
	ntfyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ntfyHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer ntfyServer.Close()

	sender := &fakeSender{err: errors.New("immich is down")}
	cfg := ntfyConfig(ntfyServer.URL)
	cfg.Immich = true
	cfg.OnSyncFailed = true
	service := New(sender, cfg, nil)
	service.SetImmichTarget(sender, "user-1")

	service.Send(context.Background(), EventSyncFailed, "error", "Sync failed", "album broke", nil)
	if len(sender.calls) != 1 || ntfyHits != 1 {
		t.Fatalf("immich calls = %d, ntfy hits = %d", len(sender.calls), ntfyHits)
	}
}

func TestNtfyFailureDoesNotBlockImmich(t *testing.T) {
	ntfyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer ntfyServer.Close()

	sender := &fakeSender{}
	cfg := ntfyConfig(ntfyServer.URL)
	cfg.Immich = true
	cfg.OnSyncFailed = true
	service := New(sender, cfg, nil)
	service.SetImmichTarget(sender, "user-1")

	service.Send(context.Background(), EventSyncFailed, "error", "Sync failed", "album broke", nil)
	if len(sender.calls) != 1 {
		t.Fatalf("immich calls = %d", len(sender.calls))
	}
}
