package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"warreth.dev/gphotos2immich/pkg/config"
	"warreth.dev/gphotos2immich/pkg/syncback"
)

func newAccountsTestServer(t *testing.T, cfg config.Config) (*Server, *syncback.Service, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	content, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
	service := syncback.New(syncback.Options{
		Accounts:       cfg.Accounts,
		StrictMatching: cfg.GoogleUpload.StrictAccountMatching,
		ImmichURL:      cfg.ApiURL,
	})
	server := NewServer(path)
	server.SetSyncBack(service)
	return server, service, path
}

func readSavedConfig(t *testing.T, path string) config.Config {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	if err := json.Unmarshal(content, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestHandleImmichUsers(t *testing.T) {
	immichServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"id":"u1","name":"Lotte","email":"lotte@example.com"},{"id":"u2","name":"Sam","email":"sam@example.com"}]`))
	}))
	defer immichServer.Close()

	server, _, _ := newAccountsTestServer(t, config.Config{ApiURL: immichServer.URL, ApiKey: "k"})
	req := httptest.NewRequest(http.MethodGet, "/api/immich/users", nil)
	rec := httptest.NewRecorder()
	server.handleImmichUsers(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Users []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Users) != 2 || resp.Users[0].Name != "Lotte" {
		t.Fatalf("users = %#v", resp.Users)
	}
}

func TestHandleAccountsSaveAndDelete(t *testing.T) {
	server, service, path := newAccountsTestServer(t, config.Config{
		UIMode:   config.UIModeStandard,
		Accounts: []config.AccountMapping{},
	})

	body := []byte(`{"immichUserId":"u1","name":"Lotte","googleEmail":"lotte@example.com"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/accounts", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	server.handleAccounts(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("save status = %d: %s", rec.Code, rec.Body.String())
	}
	var saved struct {
		Account  syncback.AccountMappingStatus   `json:"account"`
		Accounts []syncback.AccountMappingStatus `json:"accounts"`
		Routing  syncback.RoutingPreview         `json:"routing"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Account.ImmichUserID != "u1" || saved.Account.Name != "Lotte" {
		t.Fatalf("account = %#v", saved.Account)
	}
	if len(saved.Accounts) != 1 {
		t.Fatalf("accounts = %#v", saved.Accounts)
	}
	if len(service.Accounts()) != 1 {
		t.Fatalf("service accounts = %#v", service.Accounts())
	}

	cfg := readSavedConfig(t, path)
	if len(cfg.Accounts) != 1 || cfg.Accounts[0].ID != "u1" {
		t.Fatalf("saved accounts = %#v", cfg.Accounts)
	}
	if cfg.UIMode != config.UIModeStandard {
		t.Fatalf("uiMode = %q", cfg.UIMode)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
	rec = httptest.NewRecorder()
	server.handleAccounts(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d: %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/accounts?id=u1", nil)
	rec = httptest.NewRecorder()
	server.handleAccounts(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d: %s", rec.Code, rec.Body.String())
	}
	cfg = readSavedConfig(t, path)
	if len(cfg.Accounts) != 0 {
		t.Fatalf("saved accounts after delete = %#v", cfg.Accounts)
	}
	if len(service.Accounts()) != 0 {
		t.Fatalf("service accounts after delete = %#v", service.Accounts())
	}
}

func TestHandleAccountsDeleteAcceptsJSONBody(t *testing.T) {
	server, service, _ := newAccountsTestServer(t, config.Config{
		Accounts: []config.AccountMapping{{
			ID: "acc-1", ImmichUserID: "u1", Name: "Lotte", GoogleEmail: "lotte@example.com",
		}},
	})

	req := httptest.NewRequest(http.MethodDelete, "/api/accounts", strings.NewReader(`{"id":"acc-1"}`))
	rec := httptest.NewRecorder()
	server.handleAccounts(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(service.Accounts()) != 0 {
		t.Fatalf("service accounts = %#v", service.Accounts())
	}
}

func TestHandleAccountsDeleteUnknownReturns404(t *testing.T) {
	server, _, _ := newAccountsTestServer(t, config.Config{})
	req := httptest.NewRequest(http.MethodDelete, "/api/accounts?id=missing", nil)
	rec := httptest.NewRecorder()
	server.handleAccounts(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSaveConfigKeepsAutomationFields(t *testing.T) {
	server, service, path := newAccountsTestServer(t, config.Config{
		ApiKey:   "k",
		ApiURL:   "http://immich.invalid/api",
		UIMode:   config.UIModeStandard,
		Accounts: []config.AccountMapping{{ID: "acc-1", ImmichUserID: "u1", Name: "Lotte"}},
		GooglePhotos: []config.GooglePhotosConfig{{
			URL: "https://photos.app.goo.gl/x", ImmichAccountID: "acc-1",
		}},
	})

	body := []byte(`{
		"apiURL": "http://immich.invalid/api",
		"apiKey": "k",
		"uiMode": "advanced",
		"googleUpload": {"strictAccountMatching": true, "storageMode": "quota", "quality": "original", "threads": 1},
		"googlePhotos": [{"url":"https://photos.app.goo.gl/x","immichAccountId":"acc-1","uploadToGoogle":false}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	server.handleConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	cfg := readSavedConfig(t, path)
	if cfg.UIMode != config.UIModeAdvanced {
		t.Fatalf("uiMode = %q", cfg.UIMode)
	}
	if !cfg.GoogleUpload.StrictAccountMatching {
		t.Fatal("strictAccountMatching was not saved")
	}
	if len(cfg.Accounts) != 1 || cfg.Accounts[0].ID != "acc-1" {
		t.Fatalf("accounts were not preserved: %#v", cfg.Accounts)
	}
	if cfg.GooglePhotos[0].ImmichAccountID != "acc-1" {
		t.Fatalf("immichAccountId = %q", cfg.GooglePhotos[0].ImmichAccountID)
	}
	if len(service.Accounts()) != 1 {
		t.Fatalf("service accounts = %#v", service.Accounts())
	}

	// When the UI sends accounts, the live service takes the new list.
	body = []byte(`{
		"apiURL": "http://immich.invalid/api",
		"apiKey": "k",
		"accounts": [{"id":"acc-2","immichUserId":"u2","name":"Sam"}],
		"googleUpload": {"storageMode": "quota", "quality": "original", "threads": 1}
	}`)
	req = httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	server.handleConfig(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(service.Accounts()) != 1 || service.Accounts()[0].ID != "acc-2" {
		t.Fatalf("service accounts = %#v", service.Accounts())
	}
}

func TestSaveConfigPreservesOmittedAPIKey(t *testing.T) {
	server, _, path := newAccountsTestServer(t, config.Config{
		ApiKey: "stored-secret",
		ApiURL: "http://immich.invalid/api",
		Accounts: []config.AccountMapping{{
			ID: "acc-1", ImmichUserID: "u1", Name: "Lotte",
		}},
		GooglePhotos: []config.GooglePhotosConfig{{URL: "https://photos.app.goo.gl/x"}},
	})

	body := []byte(`{
		"apiURL": "http://immich.invalid/api",
		"googlePhotos": [{"url":"https://photos.app.goo.gl/x"}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	server.handleConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	cfg := readSavedConfig(t, path)
	if cfg.ApiKey != "stored-secret" {
		t.Fatalf("apiKey = %q, want the stored key", cfg.ApiKey)
	}
	if len(cfg.Accounts) != 1 || cfg.Accounts[0].ID != "acc-1" {
		t.Fatalf("accounts were not preserved: %#v", cfg.Accounts)
	}

	// An explicit new key still replaces the stored one.
	body = []byte(`{
		"apiURL": "http://immich.invalid/api",
		"apiKey": "rotated-secret",
		"googlePhotos": [{"url":"https://photos.app.goo.gl/x"}]
	}`)
	req = httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	server.handleConfig(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if cfg := readSavedConfig(t, path); cfg.ApiKey != "rotated-secret" {
		t.Fatalf("apiKey = %q", cfg.ApiKey)
	}
}

func TestHandlePrimaryProfile(t *testing.T) {
	var gotKey string
	immichServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/me" {
			http.NotFound(w, r)
			return
		}
		gotKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"primary-user","name":"Warre"}`))
	}))
	defer immichServer.Close()

	server, _, path := newAccountsTestServer(t, config.Config{
		ApiKey:   "old-key",
		ApiURL:   "http://old-immich.invalid/api",
		UIMode:   config.UIModeAdvanced,
		Accounts: []config.AccountMapping{{ID: "acc-1", ImmichUserID: "u1", Name: "Lotte"}},
		GooglePhotos: []config.GooglePhotosConfig{{
			URL: "https://photos.app.goo.gl/x", ImmichAccountID: "acc-1",
		}},
	})

	body := []byte(`{"apiURL":"` + immichServer.URL + `","apiKey":"new-key"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/primary-profile", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	server.handlePrimaryProfile(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if gotKey != "new-key" {
		t.Fatalf("tested key = %q", gotKey)
	}
	var resp struct {
		OK     bool   `json:"ok"`
		User   string `json:"user"`
		UserID string `json:"userId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.User != "Warre" || resp.UserID != "primary-user" {
		t.Fatalf("response = %#v", resp)
	}

	cfg := readSavedConfig(t, path)
	if cfg.ApiURL != immichServer.URL || cfg.ApiKey != "new-key" {
		t.Fatalf("connection = %q %q", cfg.ApiURL, cfg.ApiKey)
	}
	if cfg.UIMode != config.UIModeAdvanced {
		t.Fatalf("uiMode = %q", cfg.UIMode)
	}
	if len(cfg.Accounts) != 1 || cfg.Accounts[0].ID != "acc-1" {
		t.Fatalf("accounts were not preserved: %#v", cfg.Accounts)
	}
	if len(cfg.GooglePhotos) != 1 || cfg.GooglePhotos[0].ImmichAccountID != "acc-1" {
		t.Fatalf("albums were not preserved: %#v", cfg.GooglePhotos)
	}

	// Omitting the key keeps the stored one and tests with it.
	gotKey = ""
	body = []byte(`{"apiURL":"` + immichServer.URL + `"}`)
	req = httptest.NewRequest(http.MethodPost, "/api/primary-profile", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	server.handlePrimaryProfile(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second status = %d: %s", rec.Code, rec.Body.String())
	}
	if gotKey != "new-key" {
		t.Fatalf("second tested key = %q", gotKey)
	}
}

func TestHandlePrimaryProfileRejectsBadConnection(t *testing.T) {
	immichServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Invalid API key"}`, http.StatusUnauthorized)
	}))
	defer immichServer.Close()

	server, _, path := newAccountsTestServer(t, config.Config{
		ApiKey: "old-key",
		ApiURL: "http://old-immich.invalid/api",
	})

	body := []byte(`{"apiURL":"` + immichServer.URL + `","apiKey":"bad-key"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/primary-profile", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	server.handlePrimaryProfile(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	cfg := readSavedConfig(t, path)
	if cfg.ApiKey != "old-key" || cfg.ApiURL != "http://old-immich.invalid/api" {
		t.Fatalf("failed test must not rewrite the config: %q %q", cfg.ApiURL, cfg.ApiKey)
	}
}

func TestAccountsResponseIncludesPrimary(t *testing.T) {
	server, _, _ := newAccountsTestServer(t, config.Config{
		ApiKey: "k",
		// No API URL: PrimaryStatus reports an error without any network call.
	})
	req := httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
	rec := httptest.NewRecorder()
	server.handleAccounts(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Primary struct {
			APIKeyConfigured bool   `json:"apiKeyConfigured"`
			Error            string `json:"error"`
		} `json:"primary"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Primary.APIKeyConfigured {
		t.Fatal("expected apiKeyConfigured")
	}
	if resp.Primary.Error == "" {
		t.Fatal("expected an error for the missing Immich URL")
	}
}
