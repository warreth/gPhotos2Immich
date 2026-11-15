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

func TestSaveConfigMirrorsAccountAPIKey(t *testing.T) {
	server, _, path := newAccountsTestServer(t, config.Config{
		ApiKey: "stored-secret",
		ApiURL: "http://immich.invalid/api",
		Accounts: []config.AccountMapping{{
			ID: "acc-1", ImmichUserID: "u1", Name: "Lotte", ImmichAPIKey: "stored-secret",
		}},
		GooglePhotos: []config.GooglePhotosConfig{{URL: "https://photos.app.goo.gl/x"}},
	})

	// An omitted key keeps the default import account's key in the mirror.
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
		t.Fatalf("apiKey = %q, want the account key", cfg.ApiKey)
	}
	if cfg.DefaultImportAccount != "acc-1" {
		t.Fatalf("defaultImportAccount = %q", cfg.DefaultImportAccount)
	}
	if len(cfg.Accounts) != 1 || cfg.Accounts[0].ID != "acc-1" {
		t.Fatalf("accounts were not preserved: %#v", cfg.Accounts)
	}

	// A legacy key in the body never replaces the account key.
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
	if cfg := readSavedConfig(t, path); cfg.ApiKey != "stored-secret" {
		t.Fatalf("apiKey = %q, want the account key to win", cfg.ApiKey)
	}

	// Changing the account key updates the mirror.
	body = []byte(`{
		"apiURL": "http://immich.invalid/api",
		"accounts": [{"id":"acc-1","immichUserId":"u1","name":"Lotte","immichApiKey":"rotated-secret"}],
		"googlePhotos": [{"url":"https://photos.app.goo.gl/x"}]
	}`)
	req = httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	server.handleConfig(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if cfg := readSavedConfig(t, path); cfg.ApiKey != "rotated-secret" {
		t.Fatalf("apiKey = %q, want the updated account key", cfg.ApiKey)
	}
}

func TestSaveConfigKeepsLegacyKeyWithoutAccounts(t *testing.T) {
	server, _, path := newAccountsTestServer(t, config.Config{
		ApiKey: "legacy-secret",
		ApiURL: "http://immich.invalid/api",
	})

	body := []byte(`{
		"apiURL": "http://immich.invalid/api",
		"googlePhotos": []
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	server.handleConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	cfg := readSavedConfig(t, path)
	if cfg.ApiKey != "legacy-secret" {
		t.Fatalf("apiKey = %q, want the legacy key", cfg.ApiKey)
	}
	if cfg.DefaultImportAccount != "" || cfg.DefaultGoogleAccount != "" {
		t.Fatalf("defaults = %q / %q, want empty", cfg.DefaultImportAccount, cfg.DefaultGoogleAccount)
	}
}

func TestSaveConfigDerivesAccountDefaults(t *testing.T) {
	server, service, path := newAccountsTestServer(t, config.Config{
		ApiKey: "key-1",
		ApiURL: "http://immich.invalid/api",
		Accounts: []config.AccountMapping{
			{ID: "acc-1", ImmichUserID: "u1", Name: "One", ImmichAPIKey: "key-1", GoogleEmail: "one@example.com"},
			{ID: "acc-2", ImmichUserID: "u2", Name: "Two", ImmichAPIKey: "key-2", GoogleEmail: "two@example.com"},
		},
	})

	body := []byte(`{
		"apiURL": "http://immich.invalid/api",
		"defaultImportAccount": "acc-2",
		"defaultGoogleAccount": "acc-2",
		"googlePhotos": []
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	server.handleConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	cfg := readSavedConfig(t, path)
	if cfg.DefaultImportAccount != "acc-2" || cfg.DefaultGoogleAccount != "acc-2" {
		t.Fatalf("defaults = %q / %q", cfg.DefaultImportAccount, cfg.DefaultGoogleAccount)
	}
	if cfg.ApiKey != "key-2" {
		t.Fatalf("apiKey = %q, want the default import account key", cfg.ApiKey)
	}
	for _, acc := range cfg.Accounts {
		if acc.IsDefault != (acc.ID == "acc-2") {
			t.Fatalf("account %q IsDefault = %v", acc.ID, acc.IsDefault)
		}
	}
	for _, acc := range service.Accounts() {
		if acc.IsDefault != (acc.ID == "acc-2") {
			t.Fatalf("live account %q IsDefault = %v", acc.ID, acc.IsDefault)
		}
	}
}

func TestSaveConfigKeepsLegacyMirrorWithoutAccountKey(t *testing.T) {
	server, _, path := newAccountsTestServer(t, config.Config{
		ApiKey: "legacy-secret",
		ApiURL: "http://immich.invalid/api",
		Accounts: []config.AccountMapping{{
			ID: "acc-1", ImmichUserID: "u1", Name: "Lotte", GoogleEmail: "lotte@example.com",
		}},
	})

	body := []byte(`{
		"apiURL": "http://immich.invalid/api",
		"googlePhotos": []
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	server.handleConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	cfg := readSavedConfig(t, path)
	if cfg.ApiKey != "legacy-secret" {
		t.Fatalf("apiKey = %q, want the legacy key kept for migration", cfg.ApiKey)
	}
	if cfg.DefaultImportAccount != "" {
		t.Fatalf("defaultImportAccount = %q", cfg.DefaultImportAccount)
	}
}

func TestSaveConfigAllowsResetDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	server := NewServer(path)

	body := []byte(`{"googlePhotos":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	server.handleConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	cfg := readSavedConfig(t, path)
	if err := cfg.ValidateSettings(); err != nil {
		t.Fatalf("a reset document must pass ValidateSettings: %v", err)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("a reset document must still fail Validate")
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

func TestAccountsResponseDefaultsAndShape(t *testing.T) {
	server, _, _ := newAccountsTestServer(t, config.Config{
		ApiURL:               "http://immich.invalid/api",
		ApiKey:               "key-1",
		DefaultImportAccount: "acc-1",
		DefaultGoogleAccount: "acc-2",
		Accounts: []config.AccountMapping{
			{ID: "acc-1", ImmichUserID: "u1", ImmichAPIKey: "key-1", GoogleEmail: "one@example.com"},
			{ID: "acc-2", ImmichUserID: "u2", ImmichAPIKey: "key-2", GoogleEmail: "two@example.com", IsDefault: true},
		},
	})
	// Without a running service the stored accounts are reported instead of a
	// 503, so the first-run page can render.
	server.SetSyncBack(nil)

	req := httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
	rec := httptest.NewRecorder()
	server.handleAccounts(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Accounts []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"accounts"`
		Defaults struct {
			ImportAccount string `json:"importAccount"`
			GoogleAccount string `json:"googleAccount"`
		} `json:"defaults"`
		ImmichURL string          `json:"immichURL"`
		Primary   json.RawMessage `json:"primary"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Defaults.ImportAccount != "acc-1" || resp.Defaults.GoogleAccount != "acc-2" {
		t.Fatalf("defaults = %#v", resp.Defaults)
	}
	if resp.ImmichURL != "http://immich.invalid/api" {
		t.Fatalf("immichURL = %q", resp.ImmichURL)
	}
	if len(resp.Accounts) != 2 || resp.Accounts[0].ID != "acc-1" {
		t.Fatalf("accounts = %#v", resp.Accounts)
	}
	if len(resp.Primary) != 0 {
		t.Fatalf("primary must be omitted without a service: %s", resp.Primary)
	}
}

func TestHandleSetupAccount(t *testing.T) {
	immichServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/me" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Header.Get("x-api-key") {
		case "setup-key":
			w.Write([]byte(`{"id":"user-1","name":"Warre"}`))
		case "other-key":
			w.Write([]byte(`{"id":"user-2","name":"Lotte"}`))
		default:
			http.Error(w, `{"message":"Invalid API key"}`, http.StatusUnauthorized)
		}
	}))
	defer immichServer.Close()

	server, _, path := newAccountsTestServer(t, config.Config{
		ApiURL: "http://old.invalid/api",
		UIMode: config.UIModeAdvanced,
		GooglePhotos: []config.GooglePhotosConfig{{
			URL: "https://photos.app.goo.gl/x", AlbumName: "Holiday", SyncInterval: "12h",
		}},
	})
	server.OnConfigChange = func() {}

	body := []byte(`{"apiURL":"` + immichServer.URL + `","name":"","immichApiKey":"setup-key"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/setup/account", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	server.handleSetupAccount(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK      bool                  `json:"ok"`
		Account config.AccountMapping `json:"account"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Account.ID != "user-1" || resp.Account.Name != "Warre" {
		t.Fatalf("response = %#v", resp)
	}

	cfg := readSavedConfig(t, path)
	if cfg.ApiURL != immichServer.URL || cfg.ApiKey != "setup-key" {
		t.Fatalf("connection = %q / %q", cfg.ApiURL, cfg.ApiKey)
	}
	if len(cfg.Accounts) != 1 {
		t.Fatalf("accounts = %#v", cfg.Accounts)
	}
	acc := cfg.Accounts[0]
	if acc.ID != "user-1" || acc.ImmichUserID != "user-1" || acc.ImmichAPIKey != "setup-key" || acc.Name != "Warre" {
		t.Fatalf("account = %#v", acc)
	}
	if cfg.DefaultImportAccount != "user-1" {
		t.Fatalf("defaultImportAccount = %q", cfg.DefaultImportAccount)
	}
	if cfg.UIMode != config.UIModeAdvanced {
		t.Fatalf("uiMode was not preserved: %q", cfg.UIMode)
	}
	if len(cfg.GooglePhotos) != 1 || cfg.GooglePhotos[0].AlbumName != "Holiday" || cfg.GooglePhotos[0].SyncInterval != "12h" {
		t.Fatalf("albums were not preserved: %#v", cfg.GooglePhotos)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the setup config must be valid: %v", err)
	}

	// A retry for the same Immich user is idempotent and does not duplicate it.
	req = httptest.NewRequest(http.MethodPost, "/api/setup/account", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	server.handleSetupAccount(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("same-user setup status = %d: %s", rec.Code, rec.Body.String())
	}
	if cfg := readSavedConfig(t, path); len(cfg.Accounts) != 1 {
		t.Fatalf("same-user setup duplicated the account: %#v", cfg.Accounts)
	}

	// A second Immich user belongs on the accounts API.
	otherBody := []byte(`{"apiURL":"` + immichServer.URL + `","name":"Lotte","immichApiKey":"other-key"}`)
	req = httptest.NewRequest(http.MethodPost, "/api/setup/account", bytes.NewReader(otherBody))
	rec = httptest.NewRecorder()
	server.handleSetupAccount(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second user setup status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleSetupAccountReusesSameUser(t *testing.T) {
	immichServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"user-1","name":"Warre"}`))
	}))
	defer immichServer.Close()

	server, _, path := newAccountsTestServer(t, config.Config{
		ApiURL: "http://old.invalid/api",
		Accounts: []config.AccountMapping{{
			ID: "user-1", ImmichUserID: "user-1", Name: "Old name", ImmichAPIKey: "old-key",
			GoogleEmail: "warre@example.com",
		}},
	})
	server.OnConfigChange = func() {}

	body := []byte(`{"apiURL":"` + immichServer.URL + `","name":"","immichApiKey":"new-key"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/setup/account", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	server.handleSetupAccount(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	cfg := readSavedConfig(t, path)
	if len(cfg.Accounts) != 1 {
		t.Fatalf("accounts must not be duplicated: %#v", cfg.Accounts)
	}
	acc := cfg.Accounts[0]
	if acc.ImmichAPIKey != "new-key" || acc.Name != "Warre" || acc.GoogleEmail != "warre@example.com" {
		t.Fatalf("account = %#v", acc)
	}
	if cfg.ApiKey != "new-key" {
		t.Fatalf("apiKey = %q", cfg.ApiKey)
	}
}

func TestHandleResetAccounts(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	gotohpPath := filepath.Join(dir, "gotohp.config")
	cookiesPath := filepath.Join(dir, "web-cookies.txt")
	accountCookiesPath := filepath.Join(dir, "account-cookies.txt")
	statePath := filepath.Join(dir, "sync-state.json")
	for _, path := range []string{gotohpPath, cookiesPath, accountCookiesPath, statePath} {
		if err := os.WriteFile(path, []byte("secret"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	cfg := config.Config{
		ApiURL:               "http://immich.invalid/api",
		ApiKey:               "legacy-key",
		DefaultImportAccount: "acc-1",
		DefaultGoogleAccount: "acc-1",
		Accounts: []config.AccountMapping{{
			ID: "acc-1", ImmichUserID: "u1", Name: "One", ImmichAPIKey: "key-1",
			GoogleEmail: "one@example.com", WebCookiesFile: accountCookiesPath, IsDefault: true,
		}},
		GoogleUpload: config.GoogleUploadConfig{
			Enabled: true, GotohpConfig: gotohpPath, WebCookiesFile: cookiesPath,
			StorageMode: config.StorageModeQuota, Quality: config.QualityOriginal, Threads: 1,
		},
		Notifications: config.NotificationsConfig{
			Immich:          true,
			ImmichAccountID: "acc-1",
			OnSyncFailed:    true,
			Ntfy: config.NtfyConfig{
				Enabled: true, URL: "https://ntfy.sh", Topic: "photos", Token: "tk_secret",
			},
		},
		GooglePhotos: []config.GooglePhotosConfig{{
			URL: "https://photos.app.goo.gl/x", AlbumName: "Holiday", SyncInterval: "12h",
			ImmichAlbumID: "album-1", ImmichAccountID: "acc-1", UploadToGoogle: true,
		}},
	}
	content, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, content, 0644); err != nil {
		t.Fatal(err)
	}

	server := NewServer(configPath)
	server.OnConfigChange = func() {}

	req := httptest.NewRequest(http.MethodPost, "/api/accounts/reset", nil)
	rec := httptest.NewRecorder()
	server.handleResetAccounts(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}

	reset := readSavedConfig(t, configPath)
	if len(reset.Accounts) != 0 {
		t.Fatalf("accounts = %#v", reset.Accounts)
	}
	if reset.ApiKey != "" || reset.DefaultImportAccount != "" || reset.DefaultGoogleAccount != "" {
		t.Fatalf("account defaults were not cleared: %#v", reset)
	}
	if reset.Notifications.Immich || reset.Notifications.ImmichAccountID != "" || reset.Notifications.Ntfy.Token != "" {
		t.Fatalf("notifications were not cleared: %#v", reset.Notifications)
	}
	if reset.Notifications.Ntfy.URL != "https://ntfy.sh" || reset.Notifications.Ntfy.Topic != "photos" {
		t.Fatalf("unrelated ntfy settings were not preserved: %#v", reset.Notifications.Ntfy)
	}
	if reset.GoogleUpload.Enabled {
		t.Fatal("googleUpload.enabled must be cleared")
	}
	if reset.GoogleUpload.GotohpConfig != gotohpPath || reset.GoogleUpload.WebCookiesFile != cookiesPath {
		t.Fatalf("credential paths were not preserved: %#v", reset.GoogleUpload)
	}
	if len(reset.GooglePhotos) != 1 {
		t.Fatalf("albums = %#v", reset.GooglePhotos)
	}
	album := reset.GooglePhotos[0]
	if album.URL != cfg.GooglePhotos[0].URL || album.AlbumName != "Holiday" || album.SyncInterval != "12h" {
		t.Fatalf("album fields were not preserved: %#v", album)
	}
	if album.ImmichAlbumID != "" || album.ImmichAccountID != "" || album.UploadToGoogle {
		t.Fatalf("album targets were not cleared: %#v", album)
	}
	if err := reset.ValidateSettings(); err != nil {
		t.Fatalf("reset config must pass ValidateSettings: %v", err)
	}
	if err := reset.Validate(); err == nil {
		t.Fatal("reset config must still fail Validate")
	}

	for _, path := range []string{gotohpPath, cookiesPath, accountCookiesPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("credential file %s was not removed: %v", path, err)
		}
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("sync state must be preserved: %v", err)
	}
}
