package syncback

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"warreth.dev/gphotos2immich/pkg/config"
	"warreth.dev/gphotos2immich/pkg/gphotos"
	"warreth.dev/gphotos2immich/pkg/gphotosweb"
	"warreth.dev/gphotos2immich/pkg/upload"
)

type fakeGoogle struct {
	accounts  []string
	added     string
	addErr    error
	removeErr error
	version   string
	removed   string
}

func (f *fakeGoogle) AddAccount(_ context.Context, token string) (string, error) {
	f.added = token
	if f.addErr != nil {
		return "", f.addErr
	}
	return "user@example.com", nil
}

func (f *fakeGoogle) ListAccounts(context.Context) ([]string, error) {
	return f.accounts, nil
}

func (f *fakeGoogle) RemoveAccount(_ context.Context, email string) error {
	f.removed = email
	return f.removeErr
}

func (f *fakeGoogle) Upload(context.Context, gphotos.UploadRequest) ([]gphotos.UploadResult, error) {
	return nil, nil
}

func (f *fakeGoogle) Version(context.Context) (string, error) {
	return f.version, nil
}

type fakeResolver struct {
	info AlbumInfo
	err  error
	url  string
}

func (f *fakeResolver) Resolve(_ context.Context, shareURL string) (AlbumInfo, error) {
	f.url = shareURL
	return f.info, f.err
}

type fakeUploader struct {
	mapping upload.AlbumMapping
	summary upload.Summary
	err     error
}

func (f *fakeUploader) SyncAlbum(_ context.Context, mapping upload.AlbumMapping) (upload.Summary, error) {
	f.mapping = mapping
	return f.summary, f.err
}

type fakeWeb struct {
	available          bool
	albums             []gphotosweb.Album
	validateErr        error
	validateCookiesErr error
	listErr            error
	reloads            int
	cookies            []byte
}

func (f *fakeWeb) Available() bool { return f.available }
func (f *fakeWeb) Validate(context.Context) (gphotosweb.Session, error) {
	return gphotosweb.Session{}, f.validateErr
}
func (f *fakeWeb) ValidateCookies(_ context.Context, cookies []byte) (gphotosweb.Session, error) {
	f.cookies = append([]byte(nil), cookies...)
	return gphotosweb.Session{}, f.validateCookiesErr
}
func (f *fakeWeb) ListAlbums(context.Context) ([]gphotosweb.Album, error) {
	return f.albums, f.listErr
}
func (f *fakeWeb) Reload() { f.reloads++ }

func testConfig() *config.Config {
	return &config.Config{
		ApiKey: "k", ApiURL: "http://x/api",
		GoogleUpload: config.GoogleUploadConfig{StorageMode: config.StorageModeQuota, Quality: config.QualityOriginal},
		GooglePhotos: []config.GooglePhotosConfig{{
			URL:            "https://photos.app.goo.gl/x",
			ImmichAlbumID:  "album-1",
			UploadToGoogle: true,
		}},
	}
}

func newService(google gphotos.Client, resolver AlbumResolver, albums ImmichAlbums, uploader AlbumUploader) *Service {
	return New(Options{Google: google, Resolver: resolver, Albums: albums, Uploader: uploader})
}

func TestConnect(t *testing.T) {
	google := &fakeGoogle{version: "v0.11.0"}
	service := newService(google, &fakeResolver{}, nil, nil)

	status, err := service.Connect(context.Background(), "  oauth-token  ")
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	if google.added != "oauth-token" {
		t.Fatalf("token passed = %q", google.added)
	}
	if !status.Connected || status.Email != "user@example.com" || status.Version != "v0.11.0" {
		t.Fatalf("status = %#v", status)
	}
	if !service.Account().Connected {
		t.Fatal("status was not cached")
	}
}

func TestConnectRejectsEmptyToken(t *testing.T) {
	service := newService(&fakeGoogle{}, &fakeResolver{}, nil, nil)
	if _, err := service.Connect(context.Background(), "   "); err == nil {
		t.Fatal("expected an error for an empty token")
	}
}

func TestConnectRecordsFailure(t *testing.T) {
	google := &fakeGoogle{addErr: errors.New("Google rejected the oauth_token")}
	service := newService(google, &fakeResolver{}, nil, nil)

	if _, err := service.Connect(context.Background(), "token"); err == nil {
		t.Fatal("expected an error")
	}
	if service.Account().LastError == "" {
		t.Fatal("expected the error to be recorded in the status")
	}
	if service.Account().Connected {
		t.Fatal("a failed connect must not report connected")
	}
}

func TestRefresh(t *testing.T) {
	google := &fakeGoogle{accounts: []string{"a@example.com"}, version: "v0.11.0"}
	service := newService(google, &fakeResolver{}, nil, nil)

	status, err := service.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh failed: %v", err)
	}
	if !status.Connected || status.Email != "a@example.com" {
		t.Fatalf("status = %#v", status)
	}
}

func TestRefreshWithoutAccount(t *testing.T) {
	service := newService(&fakeGoogle{}, &fakeResolver{}, nil, nil)
	status, err := service.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh failed: %v", err)
	}
	if status.Connected {
		t.Fatal("no account must not report connected")
	}
	if status.LastError == "" {
		t.Fatal("expected a reason")
	}
}

func TestDisconnect(t *testing.T) {
	google := &fakeGoogle{accounts: []string{"a@example.com"}}
	service := newService(google, &fakeResolver{}, nil, nil)

	if err := service.Disconnect(context.Background()); err != nil {
		t.Fatalf("Disconnect failed: %v", err)
	}
	if google.removed != "a@example.com" {
		t.Fatalf("removed = %q", google.removed)
	}
	if service.Account().Connected {
		t.Fatal("status must be cleared after disconnect")
	}
}

func TestResolveAlbum(t *testing.T) {
	resolver := &fakeResolver{info: AlbumInfo{MediaKey: "AF1QipKey", Title: "Album", Items: 12}}
	service := newService(&fakeGoogle{}, resolver, nil, nil)

	info, err := service.ResolveAlbum(context.Background(), " https://photos.app.goo.gl/x ")
	if err != nil {
		t.Fatalf("ResolveAlbum failed: %v", err)
	}
	if info.MediaKey != "AF1QipKey" {
		t.Fatalf("info = %#v", info)
	}
	if resolver.url != "https://photos.app.goo.gl/x" {
		t.Fatalf("url = %q", resolver.url)
	}
}

func TestResolveAlbumRequiresKey(t *testing.T) {
	service := newService(&fakeGoogle{}, &fakeResolver{info: AlbumInfo{Title: "Album"}}, nil, nil)
	if _, err := service.ResolveAlbum(context.Background(), "https://photos.app.goo.gl/x"); err == nil {
		t.Fatal("expected an error when the album key is missing")
	}
}

func TestSyncAlbumRejectsDisabled(t *testing.T) {
	cfg := testConfig()
	cfg.GooglePhotos[0].UploadToGoogle = false
	service := newService(&fakeGoogle{}, &fakeResolver{}, nil, nil)

	if _, err := service.SyncAlbum(context.Background(), cfg, 0, "key"); err == nil {
		t.Fatal("expected an error when backward upload is disabled")
	}
}

func TestSyncAlbumResolvesKeyWhenMissing(t *testing.T) {
	resolver := &fakeResolver{info: AlbumInfo{MediaKey: "resolved-key"}}
	service := newService(&fakeGoogle{}, resolver, nil, nil)

	cfg := testConfig()
	_, err := service.SyncAlbum(context.Background(), cfg, 0, "")
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("expected ErrDisabled after resolution, got %v", err)
	}
	if resolver.url == "" {
		t.Fatal("expected the resolver to be called")
	}
}

func TestVerifyClassifiesSharedAlbum(t *testing.T) {
	resolver := &fakeResolver{info: AlbumInfo{MediaKey: "shared-key", Title: "Leiding", Items: 1}}
	web := &fakeWeb{available: true, albums: []gphotosweb.Album{{
		Key: "shared-key", Title: "Leiding", Count: 672, Shared: true,
	}}}
	service := New(Options{Google: &fakeGoogle{}, Resolver: resolver, Web: web})

	info, err := service.Verify(context.Background(), testConfig(), 0)
	if err != nil {
		t.Fatalf("Verify failed: %v", err)
	}
	if !info.Shared || info.Owned || !info.WebVerified {
		t.Fatalf("info = %#v", info)
	}
	if info.Items != 672 {
		t.Fatalf("items = %d", info.Items)
	}
}

func TestVerifyClassifiesOwnedAlbum(t *testing.T) {
	resolver := &fakeResolver{info: AlbumInfo{MediaKey: "owned-key"}}
	web := &fakeWeb{available: true, albums: []gphotosweb.Album{{Key: "owned-key", Shared: false}}}
	service := New(Options{Google: &fakeGoogle{}, Resolver: resolver, Web: web})

	info, err := service.Verify(context.Background(), testConfig(), 0)
	if err != nil {
		t.Fatalf("Verify failed: %v", err)
	}
	if info.Shared || !info.Owned || !info.WebVerified {
		t.Fatalf("info = %#v", info)
	}
}

func TestVerifyRejectsUnjoinedAlbum(t *testing.T) {
	resolver := &fakeResolver{info: AlbumInfo{MediaKey: "missing-key"}}
	web := &fakeWeb{available: true, albums: []gphotosweb.Album{{Key: "other-key"}}}
	service := New(Options{Google: &fakeGoogle{}, Resolver: resolver, Web: web})

	_, err := service.Verify(context.Background(), testConfig(), 0)
	if err == nil {
		t.Fatal("expected an error for an album the account cannot see")
	}
}

func TestVerifyWithoutWebSession(t *testing.T) {
	resolver := &fakeResolver{info: AlbumInfo{MediaKey: "key"}}
	service := New(Options{Google: &fakeGoogle{}, Resolver: resolver, Web: &fakeWeb{}})

	info, err := service.Verify(context.Background(), testConfig(), 0)
	if err != nil {
		t.Fatalf("Verify failed: %v", err)
	}
	if info.WebVerified || info.Shared {
		t.Fatalf("info = %#v", info)
	}
}

func TestSyncAlbumPassesSharedMapping(t *testing.T) {
	uploader := &fakeUploader{}
	web := &fakeWeb{available: true, albums: []gphotosweb.Album{{Key: "shared-key", Shared: true}}}
	service := New(Options{
		Google: &fakeGoogle{}, Resolver: &fakeResolver{}, Uploader: uploader, Web: web,
	})

	if _, err := service.SyncAlbum(context.Background(), testConfig(), 0, "shared-key"); err != nil {
		t.Fatalf("SyncAlbum failed: %v", err)
	}
	if !uploader.mapping.Shared {
		t.Fatalf("mapping = %#v", uploader.mapping)
	}
	if !uploader.mapping.UseWebAdd {
		t.Fatalf("shared album must use the web add path: %#v", uploader.mapping)
	}
}

func TestSyncAlbumReuploadEnablesWebAddForOwnedAlbum(t *testing.T) {
	cfg := testConfig()
	cfg.GoogleUpload.ReuploadDeleted = true
	uploader := &fakeUploader{}
	web := &fakeWeb{available: true, albums: []gphotosweb.Album{{Key: "owned-key", Shared: false}}}
	service := New(Options{
		Google: &fakeGoogle{}, Resolver: &fakeResolver{}, Uploader: uploader, Web: web,
	})

	if _, err := service.SyncAlbum(context.Background(), cfg, 0, "owned-key"); err != nil {
		t.Fatalf("SyncAlbum failed: %v", err)
	}
	if uploader.mapping.Shared || !uploader.mapping.UseWebAdd || !uploader.mapping.ReuploadDeleted {
		t.Fatalf("mapping = %#v", uploader.mapping)
	}
}

func TestSyncAlbumReuploadWithoutWebSessionIsSkipped(t *testing.T) {
	cfg := testConfig()
	cfg.GoogleUpload.ReuploadDeleted = true
	uploader := &fakeUploader{}
	web := &fakeWeb{available: false}
	service := New(Options{
		Google: &fakeGoogle{}, Resolver: &fakeResolver{}, Uploader: uploader, Web: web,
	})

	if _, err := service.SyncAlbum(context.Background(), cfg, 0, "owned-key"); err != nil {
		t.Fatalf("SyncAlbum failed: %v", err)
	}
	if uploader.mapping.UseWebAdd || uploader.mapping.ReuploadDeleted {
		t.Fatalf("without a session the check must be skipped: %#v", uploader.mapping)
	}
}

func TestSyncAlbumRejectsUnjoinedAlbum(t *testing.T) {
	service := New(Options{
		Google:   &fakeGoogle{},
		Resolver: &fakeResolver{},
		Uploader: &fakeUploader{},
		Web:      &fakeWeb{available: true, albums: []gphotosweb.Album{{Key: "other-key"}}},
	})
	if _, err := service.SyncAlbum(context.Background(), testConfig(), 0, "shared-key"); err == nil {
		t.Fatal("expected an error for an unjoined album")
	}
}

func TestWebSessionStatusNotConfigured(t *testing.T) {
	service := New(Options{Google: &fakeGoogle{}, Web: &fakeWeb{}})
	if service.WebSessionStatus(context.Background()).Connected {
		t.Fatal("no cookies file must not report connected")
	}
}

func TestWebSessionStatusConnected(t *testing.T) {
	service := New(Options{Google: &fakeGoogle{}, Web: &fakeWeb{available: true}})
	status := service.WebSessionStatus(context.Background())
	if !status.Connected {
		t.Fatalf("status = %#v", status)
	}
}

func TestWebSessionStatusExpired(t *testing.T) {
	service := New(Options{Google: &fakeGoogle{}, Web: &fakeWeb{
		available: true, validateErr: gphotosweb.ErrNoSession,
	}})
	status := service.WebSessionStatus(context.Background())
	if status.Connected || status.LastError == "" {
		t.Fatalf("status = %#v", status)
	}
}

func TestConnectWebSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	web := &fakeWeb{available: true}
	service := New(Options{Google: &fakeGoogle{}, Web: web, WebCookiesPath: path})

	status, err := service.ConnectWebSession(context.Background(), "cookie-line")
	if err != nil {
		t.Fatalf("ConnectWebSession failed: %v", err)
	}
	if !status.Connected {
		t.Fatalf("status = %#v", status)
	}
	if web.reloads != 1 {
		t.Fatalf("reloads = %d", web.reloads)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "cookie-line\n" {
		t.Fatalf("file = %q", content)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
}

func TestConnectWebSessionRejectsBadCookies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	web := &fakeWeb{available: true, validateCookiesErr: gphotosweb.ErrNoSession}
	service := New(Options{Google: &fakeGoogle{}, Web: web, WebCookiesPath: path})

	if _, err := service.ConnectWebSession(context.Background(), "cookie-line"); err == nil {
		t.Fatal("expected invalid cookies to fail")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("invalid cookies must not be saved")
	}
}

func TestDisconnectWebSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(path, []byte("cookie"), 0600); err != nil {
		t.Fatal(err)
	}
	web := &fakeWeb{available: true}
	service := New(Options{Google: &fakeGoogle{}, Web: web, WebCookiesPath: path})

	if err := service.DisconnectWebSession(); err != nil {
		t.Fatalf("DisconnectWebSession failed: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("cookies file was not removed")
	}
	if web.reloads != 1 {
		t.Fatalf("reloads = %d", web.reloads)
	}
}

func TestAccountBeforeConnect(t *testing.T) {
	service := newService(&fakeGoogle{}, &fakeResolver{}, nil, nil)
	status := service.Account()
	if status.Connected {
		t.Fatal("a fresh service must not report connected")
	}
	if status.Validated.IsZero() {
		// Zero time is expected before the first refresh.
		_ = time.Time{}
	}
}

var _ gphotos.Client = (*fakeGoogle)(nil)
var _ AlbumResolver = (*fakeResolver)(nil)
var _ AlbumUploader = (*fakeUploader)(nil)
var _ WebClient = (*fakeWeb)(nil)
