// Package syncback owns the optional Immich -> Google Photos direction.
//
// It is deliberately self-contained: account handling, album resolution,
// mapping verification and upload orchestration live here. The rest of the app
// only talks to Service. Public album scraping stays in pkg/googlephotos and is
// reached through the small AlbumResolver seam below, so a repair to the
// scraper never touches this module.
package syncback

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"warreth.dev/gphotos2immich/pkg/config"
	"warreth.dev/gphotos2immich/pkg/gphotos"
	"warreth.dev/gphotos2immich/pkg/gphotosweb"
	"warreth.dev/gphotos2immich/pkg/upload"
)

// AlbumResolver resolves a public Google Photos share URL to album metadata.
type AlbumResolver interface {
	Resolve(ctx context.Context, shareURL string) (AlbumInfo, error)
}

// AlbumInfo is the resolved Google album.
type AlbumInfo struct {
	MediaKey    string
	Title       string
	Items       int
	Shared      bool
	Owned       bool
	WebVerified bool
}

// ImmichAlbums lists albums for the mapping dropdown.
type ImmichAlbums interface {
	Albums(ctx context.Context) ([]Club, error)
}

// Club is one Immich album offered in the UI.
type Club struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Items int    `json:"items"`
}

// AccountStatus is what the UI shows for the Google account.
type AccountStatus struct {
	Connected bool
	Email     string
	Version   string
	Validated time.Time
	LastError string
}

// WebStatus is what the UI shows for the optional browser session.
type WebStatus struct {
	Connected bool      `json:"connected"`
	Validated time.Time `json:"validated,omitempty"`
	LastError string    `json:"lastError,omitempty"`
}

// AlbumUploader performs one album upload run.
type AlbumUploader interface {
	SyncAlbum(ctx context.Context, mapping upload.AlbumMapping) (upload.Summary, error)
}

// WebClient is the part of the browser-session client syncback needs. It is an
// interface so a repair to the private web API does not touch this module.
type WebClient interface {
	Available() bool
	Validate(ctx context.Context) (gphotosweb.Session, error)
	ValidateCookies(ctx context.Context, cookies []byte) (gphotosweb.Session, error)
	ListAlbums(ctx context.Context) ([]gphotosweb.Album, error)
	Reload()
}

// Options wires the service. Only Google is essential for the account flow;
// every other field may be nil when the caller does not need that part.
type Options struct {
	Google         gphotos.Client
	Resolver       AlbumResolver
	Albums         ImmichAlbums
	Uploader       AlbumUploader
	Web            WebClient
	WebCookiesPath string
	Logger         *slog.Logger
}

// Service coordinates the sync-back feature.
type Service struct {
	Google   gphotos.Client
	Resolver AlbumResolver
	Albums   ImmichAlbums
	Uploader AlbumUploader
	Web      WebClient

	webCookiesPath string

	mu          sync.Mutex
	status      AccountStatus
	webStatus   WebStatus
	webStatusAt time.Time
	webAlbums   []gphotosweb.Album
	webAlbumsAt time.Time
	running     map[string]bool
	logger      *slog.Logger
}

// New creates a service.
func New(opts Options) *Service {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		Google:         opts.Google,
		Resolver:       opts.Resolver,
		Albums:         opts.Albums,
		Uploader:       opts.Uploader,
		Web:            opts.Web,
		webCookiesPath: strings.TrimSpace(opts.WebCookiesPath),
		running:        make(map[string]bool),
		logger:         logger,
	}
}

// ErrDisabled is returned when the feature is not configured.
var ErrDisabled = errors.New("Google Photos upload is not configured")

func (s *Service) available() error {
	if s == nil || s.Google == nil {
		return ErrDisabled
	}
	return nil
}

// Account returns the cached account status.
func (s *Service) Account() AccountStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// Connect exchanges an Embedded Setup oauth_token for a credential.
func (s *Service) Connect(ctx context.Context, oauthToken string) (AccountStatus, error) {
	if err := s.available(); err != nil {
		return AccountStatus{}, err
	}
	oauthToken = strings.TrimSpace(oauthToken)
	if oauthToken == "" {
		return AccountStatus{}, fmt.Errorf("paste the oauth_token cookie value")
	}

	email, err := s.Google.AddAccount(ctx, oauthToken)
	if err != nil {
		s.setStatus(AccountStatus{LastError: err.Error()})
		return AccountStatus{}, err
	}
	version, verr := s.Google.Version(ctx)
	if verr != nil {
		s.logger.Warn("Could not read gotohp version", "error", verr)
	}
	status := AccountStatus{
		Connected: true,
		Email:     email,
		Version:   version,
		Validated: time.Now(),
	}
	s.setStatus(status)
	return status, nil
}

// Refresh validates the stored credential and refreshes the status.
func (s *Service) Refresh(ctx context.Context) (AccountStatus, error) {
	if err := s.available(); err != nil {
		return AccountStatus{}, err
	}
	accounts, err := s.Google.ListAccounts(ctx)
	if err != nil {
		s.setStatus(AccountStatus{LastError: err.Error()})
		return AccountStatus{}, err
	}
	if len(accounts) == 0 {
		status := AccountStatus{LastError: "no Google account connected"}
		s.setStatus(status)
		return status, nil
	}
	version, verr := s.Google.Version(ctx)
	if verr != nil {
		version = s.Account().Version
	}
	status := AccountStatus{
		Connected: true,
		Email:     accounts[0],
		Version:   version,
		Validated: time.Now(),
	}
	s.setStatus(status)
	return status, nil
}

// Disconnect removes the stored credential.
func (s *Service) Disconnect(ctx context.Context) error {
	if err := s.available(); err != nil {
		return err
	}
	status := s.Account()
	if status.Email == "" {
		accounts, err := s.Google.ListAccounts(ctx)
		if err != nil {
			return err
		}
		if len(accounts) == 0 {
			return nil
		}
		status.Email = accounts[0]
	}
	if err := s.Google.RemoveAccount(ctx, status.Email); err != nil {
		return err
	}
	s.setStatus(AccountStatus{})
	return nil
}

func (s *Service) setStatus(status AccountStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

// WebSessionStatus reports whether the optional browser session still works.
// The result is cached so opening the page does not hit Google every time.
func (s *Service) WebSessionStatus(ctx context.Context) WebStatus {
	if s == nil || s.Web == nil || !s.Web.Available() {
		return WebStatus{}
	}
	s.mu.Lock()
	if time.Since(s.webStatusAt) < 5*time.Minute {
		cached := s.webStatus
		s.mu.Unlock()
		return cached
	}
	s.mu.Unlock()

	_, err := s.Web.Validate(ctx)
	status := WebStatus{}
	if err != nil {
		status.LastError = err.Error()
	} else {
		status.Connected = true
		status.Validated = time.Now()
	}

	s.mu.Lock()
	s.webStatus = status
	s.webStatusAt = time.Now()
	s.mu.Unlock()
	return status
}

// RefreshWebSession clears the cached status and validates the session again.
func (s *Service) RefreshWebSession(ctx context.Context) WebStatus {
	if s == nil {
		return WebStatus{}
	}
	s.mu.Lock()
	s.webStatusAt = time.Time{}
	s.mu.Unlock()
	return s.WebSessionStatus(ctx)
}

// ConnectWebSession validates and stores a Netscape cookies.txt exported from
// a signed-in photos.google.com session.
func (s *Service) ConnectWebSession(ctx context.Context, cookies string) (WebStatus, error) {
	if s == nil || s.Web == nil {
		return WebStatus{}, fmt.Errorf("browser session support is not configured")
	}
	if s.webCookiesPath == "" {
		return WebStatus{}, fmt.Errorf("browser session file path is not configured")
	}
	cookies = strings.TrimSpace(cookies)
	if cookies == "" {
		return WebStatus{}, fmt.Errorf("paste the contents of cookies.txt")
	}
	if _, err := s.Web.ValidateCookies(ctx, []byte(cookies)); err != nil {
		status := WebStatus{LastError: err.Error()}
		s.setWebStatus(status)
		return status, err
	}

	if err := os.MkdirAll(filepath.Dir(s.webCookiesPath), 0755); err != nil {
		return WebStatus{}, fmt.Errorf("create browser session directory: %w", err)
	}
	tmp := s.webCookiesPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(cookies+"\n"), 0600); err != nil {
		return WebStatus{}, fmt.Errorf("write browser session: %w", err)
	}
	if err := os.Rename(tmp, s.webCookiesPath); err != nil {
		_ = os.Remove(tmp)
		return WebStatus{}, fmt.Errorf("replace browser session: %w", err)
	}
	s.Web.Reload()
	status := WebStatus{Connected: true, Validated: time.Now()}
	s.setWebStatus(status)
	return status, nil
}

// DisconnectWebSession deletes the stored cookies.
func (s *Service) DisconnectWebSession() error {
	if s == nil {
		return nil
	}
	if s.webCookiesPath != "" {
		if err := os.Remove(s.webCookiesPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove browser session: %w", err)
		}
	}
	if s.Web != nil {
		s.Web.Reload()
	}
	s.setWebStatus(WebStatus{})
	return nil
}

func (s *Service) setWebStatus(status WebStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.webStatus = status
	s.webStatusAt = time.Now()
	s.webAlbums = nil
	s.webAlbumsAt = time.Time{}
}

// listWebAlbums returns the account's albums, cached briefly so verifying and
// uploading several albums does not repeat the same web call.
func (s *Service) listWebAlbums(ctx context.Context) ([]gphotosweb.Album, error) {
	if s.Web == nil || !s.Web.Available() {
		return nil, nil
	}
	s.mu.Lock()
	if time.Since(s.webAlbumsAt) < 2*time.Minute {
		albums := s.webAlbums
		s.mu.Unlock()
		return albums, nil
	}
	s.mu.Unlock()

	albums, err := s.Web.ListAlbums(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.webAlbums = albums
	s.webAlbumsAt = time.Now()
	s.mu.Unlock()
	return albums, nil
}

func findWebAlbum(albums []gphotosweb.Album, key string) (gphotosweb.Album, bool) {
	for _, album := range albums {
		if album.Key == key {
			return album, true
		}
	}
	return gphotosweb.Album{}, false
}

// ResolveAlbum resolves a share URL to the album media key.
func (s *Service) ResolveAlbum(ctx context.Context, shareURL string) (AlbumInfo, error) {
	if s.Resolver == nil {
		return AlbumInfo{}, fmt.Errorf("album resolver is not configured")
	}
	shareURL = strings.TrimSpace(shareURL)
	if shareURL == "" {
		return AlbumInfo{}, fmt.Errorf("album URL is required")
	}
	info, err := s.Resolver.Resolve(ctx, shareURL)
	if err != nil {
		return AlbumInfo{}, err
	}
	if info.MediaKey == "" {
		return AlbumInfo{}, fmt.Errorf("could not find the album key for this share link")
	}
	return info, nil
}

// ImmichAlbums returns albums for the dropdown.
func (s *Service) ImmichAlbums(ctx context.Context) ([]Club, error) {
	if s.Albums == nil {
		return nil, fmt.Errorf("immich album list is not configured")
	}
	return s.Albums.Albums(ctx)
}

// Verify checks that both sides exist and classifies the album as owned or
// shared when a browser session is available.
func (s *Service) Verify(ctx context.Context, cfg *config.Config, albumIndex int) (AlbumInfo, error) {
	if cfg == nil || albumIndex < 0 || albumIndex >= len(cfg.GooglePhotos) {
		return AlbumInfo{}, fmt.Errorf("album not found in configuration")
	}
	gp := cfg.GooglePhotos[albumIndex]
	return s.VerifyURL(ctx, gp.URL)
}

// VerifyURL resolves and classifies one share URL.
func (s *Service) VerifyURL(ctx context.Context, shareURL string) (AlbumInfo, error) {
	info, err := s.ResolveAlbum(ctx, shareURL)
	if err != nil {
		return AlbumInfo{}, err
	}
	albums, err := s.listWebAlbums(ctx)
	if err != nil {
		return info, fmt.Errorf("read the Google Photos album list: %w", err)
	}
	if albums == nil {
		return info, nil
	}
	webAlbum, ok := findWebAlbum(albums, info.MediaKey)
	if !ok {
		return info, fmt.Errorf("this album is not in the connected Google account; open the share link and join it first")
	}
	info.Shared = webAlbum.Shared
	info.Owned = !webAlbum.Shared
	info.Items = webAlbum.Count
	info.WebVerified = true
	return info, nil
}

// Running reports whether an upload for this album is in progress.
func (s *Service) Running(albumID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running[albumID]
}

// StartSchedule runs the backward upload for every album with uploadToGoogle
// enabled, every interval, until ctx is cancelled. It is the automatic half of
// the feature; the manual per-album button works without it.
func (s *Service) StartSchedule(ctx context.Context, cfg *config.Config, interval time.Duration) {
	if s == nil || cfg == nil {
		return
	}
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for i, gp := range cfg.GooglePhotos {
					if !gp.UploadToGoogle || gp.ImmichAlbumID == "" {
						continue
					}
					runCtx, cancel := context.WithTimeout(ctx, time.Hour)
					summary, err := s.SyncAlbum(runCtx, cfg, i, "")
					cancel()
					if err != nil {
						s.logger.Warn("Backward upload failed", "album", gp.URL, "error", err)
						continue
					}
					if summary.Uploaded > 0 || summary.Failed > 0 {
						s.logger.Info("Backward upload finished",
							"album", gp.URL,
							"uploaded", summary.Uploaded,
							"skipped", summary.Skipped,
							"failed", summary.Failed,
						)
					}
				}
			}
		}
	}()
}

// SyncAlbum uploads new assets for one configured album.
func (s *Service) SyncAlbum(ctx context.Context, cfg *config.Config, albumIndex int, googleKey string) (upload.Summary, error) {
	if cfg == nil || albumIndex < 0 || albumIndex >= len(cfg.GooglePhotos) {
		return upload.Summary{}, fmt.Errorf("album not found in configuration")
	}
	gp := cfg.GooglePhotos[albumIndex]
	if !gp.UploadToGoogle {
		return upload.Summary{}, fmt.Errorf("backward upload is disabled for this album")
	}
	if googleKey == "" {
		info, err := s.ResolveAlbum(ctx, gp.URL)
		if err != nil {
			return upload.Summary{}, err
		}
		googleKey = info.MediaKey
	}
	if s.Uploader == nil {
		return upload.Summary{}, ErrDisabled
	}

	shared := false
	albums, err := s.listWebAlbums(ctx)
	if err != nil {
		return upload.Summary{}, fmt.Errorf("read the Google Photos album list: %w", err)
	}
	if albums != nil {
		webAlbum, ok := findWebAlbum(albums, googleKey)
		if !ok {
			return upload.Summary{}, fmt.Errorf("this album is not in the connected Google account; open the share link and join it first")
		}
		shared = webAlbum.Shared
	}

	albumID := gp.ImmichAlbumID
	s.mu.Lock()
	if s.running[albumID] {
		s.mu.Unlock()
		return upload.Summary{}, fmt.Errorf("an upload for this album is already running")
	}
	s.running[albumID] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.running, albumID)
		s.mu.Unlock()
	}()

	mapping := upload.AlbumMapping{
		ImmichAlbumID:  albumID,
		GoogleAlbumURL: gp.URL,
		GoogleAlbumKey: googleKey,
		UseQuota:       cfg.AlbumStorageMode(gp) == config.StorageModeQuota,
		Saver:          cfg.AlbumQuality(gp) == config.QualitySaver,
		SkipLivePhotos: cfg.AlbumSkipLivePhotos(gp),
		Shared:         shared,
	}
	return s.Uploader.SyncAlbum(ctx, mapping)
}
