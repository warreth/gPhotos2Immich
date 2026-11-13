package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"warreth.dev/gphotos2immich/pkg/config"
	"warreth.dev/gphotos2immich/pkg/gphotos"
	"warreth.dev/gphotos2immich/pkg/gphotosweb"
	"warreth.dev/gphotos2immich/pkg/syncback"
	"warreth.dev/gphotos2immich/pkg/upload"
)

type stubGoogle struct {
	accounts []string
	added    string
	addErr   error
	removed  string
}

func (f *stubGoogle) AddAccount(_ context.Context, token string) (string, error) {
	f.added = token
	if f.addErr != nil {
		return "", f.addErr
	}
	return "user@example.com", nil
}
func (f *stubGoogle) ListAccounts(context.Context) ([]string, error) { return f.accounts, nil }
func (f *stubGoogle) RemoveAccount(_ context.Context, email string) error {
	f.removed = email
	return nil
}
func (f *stubGoogle) Upload(context.Context, gphotos.UploadRequest) ([]gphotos.UploadResult, error) {
	return nil, nil
}
func (f *stubGoogle) Version(context.Context) (string, error) { return "v0.11.0", nil }

var _ gphotos.Client = (*stubGoogle)(nil)

type stubResolver struct{ info syncback.AlbumInfo }

func (r stubResolver) Resolve(context.Context, string) (syncback.AlbumInfo, error) {
	return r.info, nil
}

type stubAlbums struct{ albums []syncback.Club }

func (a stubAlbums) Albums(context.Context) ([]syncback.Club, error) { return a.albums, nil }

type stubUploader struct {
	summary upload.Summary
	err     error
	album   upload.AlbumMapping
}

func (u *stubUploader) SyncAlbum(_ context.Context, mapping upload.AlbumMapping) (upload.Summary, error) {
	u.album = mapping
	return u.summary, u.err
}

type stubWeb struct {
	available    bool
	albums       []gphotosweb.Album
	validateErr  error
	validateCErr error
	reloads      int
}

func (w *stubWeb) Available() bool { return w.available }
func (w *stubWeb) Validate(context.Context) (gphotosweb.Session, error) {
	return gphotosweb.Session{}, w.validateErr
}
func (w *stubWeb) ValidateCookies(context.Context, []byte) (gphotosweb.Session, error) {
	return gphotosweb.Session{}, w.validateCErr
}
func (w *stubWeb) ListAlbums(context.Context) ([]gphotosweb.Album, error) {
	return w.albums, nil
}
func (w *stubWeb) Reload() { w.reloads++ }

func newSyncbackServer(t *testing.T) (*Server, *stubGoogle, *stubUploader, *stubWeb) {
	t.Helper()
	google := &stubGoogle{accounts: []string{"user@example.com"}}
	uploader := &stubUploader{summary: upload.Summary{Uploaded: 3}}
	resolver := stubResolver{info: syncback.AlbumInfo{MediaKey: "AF1QipKey", Title: "Album", Items: 7}}
	web := &stubWeb{}
	dir := t.TempDir()
	service := syncback.New(syncback.Options{
		Google:         google,
		Resolver:       resolver,
		Albums:         stubAlbums{albums: []syncback.Club{{ID: "a1", Name: "Album", Items: 7}}},
		Uploader:       uploader,
		Web:            web,
		WebCookiesPath: dir + "/cookies.txt",
	})

	configPath := dir + "/config.json"
	cfg := config.Config{
		ApiKey: "k", ApiURL: "http://x/api",
		GooglePhotos: []config.GooglePhotosConfig{{
			URL: "https://photos.app.goo.gl/x", ImmichAlbumID: "a1", UploadToGoogle: true,
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
	server.SetSyncBack(service)
	return server, google, uploader, web
}

func TestGoogleAccountGet(t *testing.T) {
	server, _, _, _ := newSyncbackServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/google/account", nil)
	w := httptest.NewRecorder()
	server.handleGoogleAccount(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "connected") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestGoogleAccountConnect(t *testing.T) {
	server, google, _, _ := newSyncbackServer(t)
	body := bytes.NewBufferString(`{"oauthToken":"token-123"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/google/account", body)
	w := httptest.NewRecorder()
	server.handleGoogleAccount(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if google.added != "token-123" {
		t.Fatalf("token = %q", google.added)
	}
	if !strings.Contains(w.Body.String(), "user@example.com") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestGoogleAccountConnectFailure(t *testing.T) {
	server, google, _, _ := newSyncbackServer(t)
	google.addErr = context.DeadlineExceeded
	body := bytes.NewBufferString(`{"oauthToken":"bad"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/google/account", body)
	w := httptest.NewRecorder()
	server.handleGoogleAccount(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestGoogleAccountDelete(t *testing.T) {
	server, google, _, _ := newSyncbackServer(t)
	req := httptest.NewRequest(http.MethodDelete, "/api/google/account", nil)
	w := httptest.NewRecorder()
	server.handleGoogleAccount(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if google.removed != "user@example.com" {
		t.Fatalf("removed = %q", google.removed)
	}
}

func TestGoogleAlbums(t *testing.T) {
	server, _, _, _ := newSyncbackServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/google/albums", nil)
	w := httptest.NewRecorder()
	server.handleGoogleAlbums(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"id":"a1"`) {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestGoogleVerify(t *testing.T) {
	server, _, _, _ := newSyncbackServer(t)
	body := bytes.NewBufferString(`{"url":"https://photos.app.goo.gl/x"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/google/verify", body)
	w := httptest.NewRecorder()
	server.handleGoogleVerify(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "AF1QipKey") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestGoogleUpload(t *testing.T) {
	server, _, uploader, _ := newSyncbackServer(t)
	body := bytes.NewBufferString(`{"albumIndex":0,"mediaKey":"AF1QipKey"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/google/upload", body)
	w := httptest.NewRecorder()
	server.handleGoogleUpload(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if uploader.album.GoogleAlbumKey != "AF1QipKey" {
		t.Fatalf("mapping = %#v", uploader.album)
	}
	if uploader.album.ImmichAlbumID != "a1" {
		t.Fatalf("immich album = %q", uploader.album.ImmichAlbumID)
	}
}

func TestGoogleVerifyShared(t *testing.T) {
	server, _, _, web := newSyncbackServer(t)
	web.available = true
	web.albums = []gphotosweb.Album{{Key: "AF1QipKey", Title: "Album", Count: 672, Shared: true}}
	body := bytes.NewBufferString(`{"url":"https://photos.app.goo.gl/x"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/google/verify", body)
	w := httptest.NewRecorder()
	server.handleGoogleVerify(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"shared":true`) {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestGoogleWebSessionGet(t *testing.T) {
	server, _, _, _ := newSyncbackServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/google/web-session", nil)
	w := httptest.NewRecorder()
	server.handleGoogleWebSession(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"connected":false`) {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestGoogleWebSessionConnect(t *testing.T) {
	server, _, _, web := newSyncbackServer(t)
	body := bytes.NewBufferString(`{"cookies":"cookie-line"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/google/web-session", body)
	w := httptest.NewRecorder()
	server.handleGoogleWebSession(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"connected":true`) {
		t.Fatalf("body = %s", w.Body.String())
	}
	if web.reloads != 1 {
		t.Fatalf("reloads = %d", web.reloads)
	}
}

func TestGoogleWebSessionConnectFailure(t *testing.T) {
	server, _, _, web := newSyncbackServer(t)
	web.validateCErr = gphotosweb.ErrNoSession
	body := bytes.NewBufferString(`{"cookies":"bad"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/google/web-session", body)
	w := httptest.NewRecorder()
	server.handleGoogleWebSession(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestGoogleWebSessionDelete(t *testing.T) {
	server, _, _, _ := newSyncbackServer(t)
	req := httptest.NewRequest(http.MethodDelete, "/api/google/web-session", nil)
	w := httptest.NewRecorder()
	server.handleGoogleWebSession(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestGoogleEndpointsWithoutService(t *testing.T) {
	server := NewServer("dummy.json")
	for _, target := range []string{"/api/google/account", "/api/google/albums", "/api/google/verify", "/api/google/upload", "/api/google/web-session"} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		w := httptest.NewRecorder()
		switch target {
		case "/api/google/account":
			server.handleGoogleAccount(w, req)
		case "/api/google/albums":
			server.handleGoogleAlbums(w, req)
		case "/api/google/verify":
			server.handleGoogleVerify(w, req)
		case "/api/google/upload":
			server.handleGoogleUpload(w, req)
		case "/api/google/web-session":
			server.handleGoogleWebSession(w, req)
		}
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s status = %d", target, w.Code)
		}
	}
}
