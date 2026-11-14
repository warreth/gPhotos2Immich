// Package upload owns the Immich -> Google Photos direction. It depends on
// small interfaces for Immich, the Google uploader, and the import state, so
// repairs in the public-album scraper or the web UI cannot leak into it.
package upload

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"warreth.dev/gphotos2immich/pkg/gphotos"
	"warreth.dev/gphotos2immich/pkg/immich"
)

// ImmichSource is the part of the Immich client the uploader needs.
type ImmichSource interface {
	GetAlbum(ctx context.Context, albumID string) (*immich.Album, error)
	GetAsset(ctx context.Context, assetID string) (*immich.Asset, error)
	DownloadAssetToFile(ctx context.Context, assetID, path string) error
}

// GoogleUploader is the part of the gotohp adapter the uploader needs.
type GoogleUploader interface {
	Upload(ctx context.Context, req gphotos.UploadRequest) ([]gphotos.UploadResult, error)
}

// SharedAlbumWriter adds media that is already in the Google library to an
// album that is shared with the account. The web client implements it; tests
// use a fake.
type SharedAlbumWriter interface {
	Available() bool
	AddItems(ctx context.Context, albumKey string, shared bool, mediaKeys []string) error
}

// AlbumInspector lists the media keys still present in a Google album.
type AlbumInspector interface {
	AlbumMediaKeys(ctx context.Context, albumKey string) (map[string]bool, error)
}

// ProgressReporter receives live upload progress for the Web UI.
type ProgressReporter interface {
	UploadStarted(album string, total int)
	UploadItem(uploaded, failed bool)
	UploadFinished(album string, uploaded, failed int)
}

// ImportedAssetSource tells the uploader which Immich assets came from Google
// Photos, so they are never uploaded back.
type ImportedAssetSource interface {
	AssetIDs() map[string]bool
}

// AccountRoute tells the uploader which Google account and web session to use
// for a given Immich asset ownerId.
type AccountRoute struct {
	GoogleEmail  string
	SharedWriter SharedAlbumWriter
	Inspector    AlbumInspector
}

// AccountResolver maps an Immich asset owner to the Google account that should
// receive it. The zero route means the primary gotohp account and the service
// level writer and inspector.
type AccountResolver interface {
	// ResolveOwner returns the route for ownerID. If ownerID is not mapped, it
	// returns the fallback route when strict is false, or ok=false when strict
	// is true (or no fallback exists).
	ResolveOwner(ownerID string, strict bool) (route AccountRoute, ok bool, fallbackUsed bool)
}

// AccountRouteByEmail lets a pending shared-album add find the browser session
// of the account that uploaded the files. Resolvers that keep per-account
// sessions implement it.
type AccountRouteByEmail interface {
	RouteForEmail(email string) (AccountRoute, bool)
}

// AlbumMapping is one enabled per-album backward upload.
type AlbumMapping struct {
	ImmichAlbumID  string
	GoogleAlbumURL string
	GoogleAlbumKey string
	UseQuota       bool
	Saver          bool
	SkipLivePhotos bool
	// Shared means the album belongs to someone else and the account can only
	// add to it through the web API.
	Shared bool
	// UseWebAdd means files are uploaded to the library first and then added
	// through the web API. Shared albums always need this; owned albums use it
	// when the re-upload check needs album inspection.
	UseWebAdd bool
	// ReuploadDeleted checks the target album and uploads items that are no
	// longer in it. It requires UseWebAdd and an AlbumInspector.
	ReuploadDeleted bool
	// StrictAccountMatching skips assets whose Immich owner has no Google
	// account mapping instead of uploading them with the fallback account.
	StrictAccountMatching bool
}

// Summary reports one album run.
type Summary struct {
	AlbumID  string
	Total    int
	Uploaded int
	Skipped  int
	Failed   int
}

// Service runs the backward upload for one album at a time.
type Service struct {
	Immich           ImmichSource
	Google           GoogleUploader
	State            *State
	Imported         ImportedAssetSource
	SharedWriter     SharedAlbumWriter
	Inspector        AlbumInspector
	Resolver         AccountResolver
	Progress         ProgressReporter
	Logger           *slog.Logger
	StagingRoot      string
	GotohpConfigPath string
	Threads          int
}

// uploadGroup is the staging area and session for one Google account.
type uploadGroup struct {
	key    string // Google email, or "" for the primary account
	dir    string
	route  AccountRoute
	assets map[string]bool
	paths  map[string]string // staged path -> immich asset id
	staged int
}

// SyncAlbum uploads new Immich assets to the mapped Google album.
func (s *Service) SyncAlbum(ctx context.Context, mapping AlbumMapping) (Summary, error) {
	summary := Summary{AlbumID: mapping.ImmichAlbumID}
	if mapping.ImmichAlbumID == "" {
		return summary, fmt.Errorf("immich album id is required")
	}
	if mapping.GoogleAlbumKey == "" {
		return summary, fmt.Errorf("google album key is required; verify the album mapping first")
	}
	useWebAdd := mapping.UseWebAdd || mapping.Shared
	// The single-account path keeps its early, explicit check. With an account
	// resolver the sessions are per owner, so availability is checked when the
	// album add actually runs.
	if s.Resolver == nil {
		writerReady := s.SharedWriter != nil && s.SharedWriter.Available()
		if useWebAdd && !writerReady {
			return summary, fmt.Errorf("this album needs a Google Photos browser session; connect one first")
		}
	}
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if s.State == nil {
		return summary, fmt.Errorf("upload state is required")
	}
	if s.Immich == nil || s.Google == nil {
		return summary, fmt.Errorf("immich and google uploader are required")
	}

	album, err := s.Immich.GetAlbum(ctx, mapping.ImmichAlbumID)
	if err != nil {
		return summary, fmt.Errorf("fetch immich album: %w", err)
	}
	summary.Total = len(album.Assets)
	if s.Progress != nil {
		s.Progress.UploadStarted(mapping.GoogleAlbumURL, summary.Total)
		defer func() {
			s.Progress.UploadFinished(mapping.GoogleAlbumURL, summary.Uploaded, summary.Failed)
		}()
	}

	imported := map[string]bool{}
	if s.Imported != nil {
		imported = s.Imported.AssetIDs()
	}

	albumDir := filepath.Join(s.StagingRoot, sanitizeID(mapping.ImmichAlbumID))
	if err := os.MkdirAll(albumDir, 0755); err != nil {
		return summary, fmt.Errorf("create staging directory: %w", err)
	}
	defer os.RemoveAll(albumDir)

	// If a previous run uploaded files but failed to add them to a shared
	// album, finish that add before uploading anything new. The media keys are
	// already known, so no file is downloaded twice.
	handled := make(map[string]bool)
	if useWebAdd {
		if err := s.finishPendingAdds(ctx, mapping, &summary, handled, logger); err != nil {
			return summary, err
		}
	}

	// Plan which assets are uploaded by which account. Assets that the state
	// already marks as done are only re-checked when the re-upload check is
	// enabled, and that decision needs the album contents, so they are
	// collected separately.
	type plannedAsset struct {
		asset immich.AlbumAsset
		route AccountRoute
	}
	var plan []plannedAsset
	var recheck []plannedAsset
	routes := make(map[string]AccountRoute)

	for _, asset := range album.Assets {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		if asset.Id == "" {
			continue
		}
		if imported[asset.Id] {
			summary.Skipped++
			continue
		}
		if state, ok := s.State.Get(mapping.ImmichAlbumID, asset.Id); ok {
			done := state.Status == "uploaded" && (state.AddedToAlbum || !useWebAdd)
			if done {
				if mapping.ReuploadDeleted && state.GoogleMediaKey != "" {
					recheck = append(recheck, plannedAsset{asset: asset})
					continue
				}
				if !handled[asset.Id] {
					summary.Skipped++
				}
				continue
			}
		}
		if asset.Type != "" && asset.Type != "IMAGE" && asset.Type != "VIDEO" {
			summary.Skipped++
			continue
		}
		if asset.LivePhotoVideoID != "" && mapping.SkipLivePhotos {
			summary.Skipped++
			continue
		}
		route, ok := s.resolveRoute(ctx, asset, mapping.StrictAccountMatching, logger)
		if !ok {
			summary.Skipped++
			continue
		}
		routes[route.GoogleEmail] = route
		plan = append(plan, plannedAsset{asset: asset, route: route})
	}

	for i := range recheck {
		route, ok := s.resolveRoute(ctx, recheck[i].asset, mapping.StrictAccountMatching, logger)
		if !ok {
			summary.Skipped++
			continue
		}
		routes[route.GoogleEmail] = route
		recheck[i].route = route
	}

	// Optional safety check: if a photo was deleted from Google Photos, decide
	// whether to upload it again. Off by default, because keeping deletions is
	// the safer behavior. A missing inspector means the check cannot run, so
	// the state is trusted.
	present := s.albumPresent(ctx, mapping, routes, logger)
	for _, entry := range recheck {
		state, _ := s.State.Get(mapping.ImmichAlbumID, entry.asset.Id)
		if present != nil && state.GoogleMediaKey != "" && !present[state.GoogleMediaKey] {
			logger.Info("Photo is missing from Google Photos; scheduling it again",
				"asset_id", entry.asset.Id, "media_key", state.GoogleMediaKey)
			s.State.Set(mapping.ImmichAlbumID, entry.asset.Id, AssetState{
				GoogleMediaKey: state.GoogleMediaKey,
				Filename:       state.Filename,
				IsLivePhoto:    state.IsLivePhoto,
				UploadedBy:     state.UploadedBy,
				Status:         "failed",
				Error:          "missing from Google Photos album",
			})
			plan = append(plan, entry)
			continue
		}
		summary.Skipped++
	}

	// Stage every planned asset in its account's sub-directory, so a gotohp
	// run for one account never sees another account's files.
	groups := make(map[string]*uploadGroup)
	var groupOrder []string
	staged := 0

	for _, entry := range plan {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		asset := entry.asset
		group := stagingGroup(groups, &groupOrder, albumDir, entry.route)
		if err := os.MkdirAll(group.dir, 0755); err != nil {
			return summary, fmt.Errorf("create staging directory: %w", err)
		}

		stillName := asset.OriginalFileName
		if stillName == "" {
			stillName = asset.Id + ".jpg"
		}
		stagedName := asset.Id + "__" + stillName
		stillPath := filepath.Join(group.dir, stagedName)
		if err := s.Immich.DownloadAssetToFile(ctx, asset.Id, stillPath); err != nil {
			logger.Warn("Could not download Immich asset for Google upload", "asset_id", asset.Id, "error", err)
			s.State.Set(mapping.ImmichAlbumID, asset.Id, AssetState{
				Filename:   stillName,
				UploadedBy: group.key,
				Status:     "failed",
				Error:      err.Error(),
			})
			summary.Failed++
			continue
		}
		setModTime(stillPath, asset.FileCreatedAt)
		group.paths[stillPath] = asset.Id
		group.assets[asset.Id] = true
		group.staged++
		staged++

		if asset.LivePhotoVideoID != "" && !mapping.SkipLivePhotos {
			videoAsset, err := s.Immich.GetAsset(ctx, asset.LivePhotoVideoID)
			if err != nil {
				logger.Warn("Could not read Live Photo video metadata; uploading the still only",
					"asset_id", asset.Id, "video_id", asset.LivePhotoVideoID, "error", err)
			} else {
				videoExt := filepath.Ext(videoAsset.OriginalFileName)
				if videoExt == "" {
					videoExt = ".MOV"
				}
				videoName := asset.Id + "__" + strings.TrimSuffix(stillName, filepath.Ext(stillName)) + videoExt
				videoPath := filepath.Join(group.dir, videoName)
				if err := s.Immich.DownloadAssetToFile(ctx, asset.LivePhotoVideoID, videoPath); err != nil {
					logger.Warn("Could not download Live Photo video; uploading the still only",
						"asset_id", asset.Id, "video_id", asset.LivePhotoVideoID, "error", err)
				} else {
					setModTime(videoPath, asset.FileCreatedAt)
					group.paths[videoPath] = asset.Id
					group.assets[asset.Id] = true
					group.staged++
					staged++
				}
			}
		}
	}

	if staged == 0 {
		s.State.SetAlbumMeta(mapping.ImmichAlbumID, mapping.GoogleAlbumKey, mapping.GoogleAlbumURL, time.Now())
		if err := s.State.Save(); err != nil {
			return summary, err
		}
		return summary, nil
	}

	var errs []error
	for _, key := range groupOrder {
		group := groups[key]
		if group.staged == 0 {
			continue
		}
		if err := s.uploadGroup(ctx, mapping, group, useWebAdd, &summary, logger); err != nil {
			errs = append(errs, err)
		}
	}
	return summary, errors.Join(errs...)
}

// resolveRoute asks the resolver which account receives this asset. Without a
// resolver every asset keeps the single-account behavior.
func (s *Service) resolveRoute(ctx context.Context, asset immich.AlbumAsset, strict bool, logger *slog.Logger) (AccountRoute, bool) {
	if s.Resolver == nil {
		return AccountRoute{}, true
	}
	ownerID := asset.OwnerID
	if ownerID == "" && s.Immich != nil {
		// Older album responses and some endpoints omit ownerId. Reading the
		// asset fills it in, so the owner is still routed correctly.
		if full, err := s.Immich.GetAsset(ctx, asset.Id); err == nil && full != nil {
			ownerID = full.OwnerID
		}
	}
	route, ok, fallbackUsed := s.Resolver.ResolveOwner(ownerID, strict)
	if !ok {
		logger.Info("Skipping asset: its Immich owner has no mapped Google account and strict account matching is enabled",
			"asset_id", asset.Id, "owner_id", ownerID)
		return AccountRoute{}, false
	}
	if fallbackUsed {
		logger.Debug("Uploading asset with the fallback Google account",
			"asset_id", asset.Id, "owner_id", ownerID, "account", route.GoogleEmail)
	}
	return route, true
}

// albumPresent loads the media keys of the target album for the re-upload
// check. It returns nil when the check cannot run, which keeps deletions.
func (s *Service) albumPresent(ctx context.Context, mapping AlbumMapping, routes map[string]AccountRoute, logger *slog.Logger) map[string]bool {
	if !mapping.ReuploadDeleted {
		return nil
	}
	inspectors := s.inspectorsFor(routes)
	if len(inspectors) == 0 {
		logger.Warn("Re-upload check is enabled but album inspection is not available")
		return nil
	}
	present := make(map[string]bool)
	for _, inspector := range inspectors {
		keys, err := inspector.AlbumMediaKeys(ctx, mapping.GoogleAlbumKey)
		if err != nil {
			logger.Warn("Could not inspect the Google album; skipping the re-upload check", "error", err)
			return nil
		}
		for key := range keys {
			present[key] = true
		}
	}
	logger.Debug("Loaded album media keys for the re-upload check", "count", len(present))
	return present
}

// inspectorsFor returns one inspector per account that has assets in this
// album, falling back to the service level inspector.
func (s *Service) inspectorsFor(routes map[string]AccountRoute) []AlbumInspector {
	if s.Resolver == nil {
		if s.Inspector == nil {
			return nil
		}
		return []AlbumInspector{s.Inspector}
	}
	keys := make([]string, 0, len(routes))
	for key := range routes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var inspectors []AlbumInspector
	needFallback := len(routes) == 0
	for _, key := range keys {
		if inspector := routes[key].Inspector; inspector != nil {
			inspectors = append(inspectors, inspector)
		} else {
			needFallback = true
		}
	}
	if needFallback && s.Inspector != nil {
		inspectors = append(inspectors, s.Inspector)
	}
	return inspectors
}

// finishPendingAdds retries album adds for files that are already in the
// library. Each account's session is used for the items it uploaded.
func (s *Service) finishPendingAdds(ctx context.Context, mapping AlbumMapping, summary *Summary, handled map[string]bool, logger *slog.Logger) error {
	pending := s.State.PendingAlbumAdds(mapping.ImmichAlbumID)
	if len(pending) == 0 {
		return nil
	}
	var order []string
	byAccount := make(map[string][]PendingAlbumAdd)
	for _, item := range pending {
		if _, ok := byAccount[item.UploadedBy]; !ok {
			order = append(order, item.UploadedBy)
		}
		byAccount[item.UploadedBy] = append(byAccount[item.UploadedBy], item)
	}
	for _, account := range order {
		items := byAccount[account]
		keys := make([]string, 0, len(items))
		for _, item := range items {
			keys = append(keys, item.MediaKey)
		}
		if err := s.addItems(ctx, s.writerForEmail(account), mapping.GoogleAlbumKey, mapping.Shared, keys); err != nil {
			return fmt.Errorf("add previously uploaded items to the shared album: %w", err)
		}
		for _, item := range items {
			s.State.MarkAdded(mapping.ImmichAlbumID, item.AssetID)
			handled[item.AssetID] = true
			summary.Uploaded++
		}
		logger.Info("Added previously uploaded items to the shared album",
			"album", mapping.GoogleAlbumURL, "items", len(items), "account", account)
	}
	if err := s.State.Save(); err != nil {
		return err
	}
	return nil
}

// uploadGroup uploads one account's staged files and adds them to the album.
func (s *Service) uploadGroup(ctx context.Context, mapping AlbumMapping, group *uploadGroup, useWebAdd bool, summary *Summary, logger *slog.Logger) error {
	threads := s.Threads
	if threads < 1 {
		threads = 1
	}
	uploadAlbumKey := mapping.GoogleAlbumKey
	if useWebAdd {
		// Upload to the library only; the album add is done below.
		uploadAlbumKey = ""
	}
	results, uploadErr := s.Google.Upload(ctx, gphotos.UploadRequest{
		StagingDir:     group.dir,
		AlbumKey:       uploadAlbumKey,
		Account:        group.route.GoogleEmail,
		UseQuota:       mapping.UseQuota,
		Saver:          mapping.Saver,
		PairLivePhotos: !mapping.SkipLivePhotos,
		Threads:        threads,
		ConfigPath:     s.GotohpConfigPath,
	})

	// gotohp can upload the files and still fail the album add. When that
	// happens on an album we thought we owned, it is almost always an album
	// shared with us. The web writer can finish the add with the media keys
	// gotohp already returned.
	writer := s.writerFor(group.route)
	writerReady := writer != nil && writer.Available()
	var albumErr *gphotos.AlbumError
	fallbackShared := false
	groupUseWebAdd := useWebAdd
	if !groupUseWebAdd && errors.As(uploadErr, &albumErr) && writerReady {
		groupUseWebAdd = true
		fallbackShared = !mapping.Shared
	}
	albumAddFailed := !groupUseWebAdd && errors.As(uploadErr, &albumErr)

	toAdd := make(map[string]string) // immich asset id -> google media key
	seen := make(map[string]bool)
	for _, result := range results {
		assetID := assetIDFromPath(result.Path, group)
		if assetID == "" {
			for _, p := range result.Paths {
				if id := assetIDFromPath(p, group); id != "" {
					assetID = id
					break
				}
			}
		}
		if assetID == "" {
			continue
		}
		seen[assetID] = true
		filename := strings.TrimPrefix(filepath.Base(result.Path), assetID+"__")
		if result.Error != "" {
			s.State.Set(mapping.ImmichAlbumID, assetID, AssetState{
				Filename:   filename,
				UploadedBy: group.key,
				Status:     "failed",
				Error:      result.Error,
			})
			summary.Failed++
			s.reportUpload(false, true)
			continue
		}
		if albumAddFailed && result.MediaKey != "" {
			s.State.Set(mapping.ImmichAlbumID, assetID, AssetState{
				GoogleMediaKey: result.MediaKey,
				Filename:       filename,
				IsLivePhoto:    result.IsLivePhoto,
				UploadedBy:     group.key,
				Status:         "failed",
				Error:          albumErr.Error(),
			})
			summary.Failed++
			s.reportUpload(false, true)
			continue
		}
		if result.Skipped {
			if groupUseWebAdd && result.MediaKey != "" {
				// The file is already in the library, but its album add still
				// has to happen. This is the crash-recovery path: the state
				// may not have recorded the media key.
				s.State.Set(mapping.ImmichAlbumID, assetID, AssetState{
					GoogleMediaKey: result.MediaKey,
					Filename:       filename,
					IsLivePhoto:    result.IsLivePhoto,
					UploadedBy:     group.key,
					Status:         "uploaded",
					UploadedAt:     time.Now(),
				})
				toAdd[assetID] = result.MediaKey
				s.reportUpload(true, false)
				continue
			}
			s.State.Set(mapping.ImmichAlbumID, assetID, AssetState{
				Filename:   filename,
				UploadedBy: group.key,
				Status:     "skipped",
				Error:      result.SkipReason,
			})
			summary.Skipped++
			s.reportUpload(false, false)
			continue
		}
		s.State.Set(mapping.ImmichAlbumID, assetID, AssetState{
			GoogleMediaKey: result.MediaKey,
			Filename:       filename,
			IsLivePhoto:    result.IsLivePhoto,
			UploadedBy:     group.key,
			Status:         "uploaded",
			AddedToAlbum:   !groupUseWebAdd,
			UploadedAt:     time.Now(),
		})
		if groupUseWebAdd {
			toAdd[assetID] = result.MediaKey
		} else {
			summary.Uploaded++
		}
		s.reportUpload(true, false)
	}

	// Staged assets that got no result at all are retried next run.
	for assetID := range group.assets {
		if seen[assetID] {
			continue
		}
		s.State.Set(mapping.ImmichAlbumID, assetID, AssetState{
			UploadedBy: group.key,
			Status:     "failed",
			Error:      "gotohp returned no result for this asset",
		})
		summary.Failed++
		s.reportUpload(false, true)
	}

	s.State.SetAlbumMeta(mapping.ImmichAlbumID, mapping.GoogleAlbumKey, mapping.GoogleAlbumURL, time.Now())
	// Persist the media keys before the album add, so a crash or a stale
	// browser session can retry the add without uploading the files again.
	if err := s.State.Save(); err != nil {
		return err
	}

	if groupUseWebAdd && len(toAdd) > 0 {
		keys := make([]string, 0, len(toAdd))
		for _, mediaKey := range toAdd {
			keys = append(keys, mediaKey)
		}
		if err := s.addItems(ctx, writer, mapping.GoogleAlbumKey, mapping.Shared || fallbackShared, keys); err != nil {
			for assetID := range toAdd {
				if state, ok := s.State.Get(mapping.ImmichAlbumID, assetID); ok {
					state.Error = err.Error()
					s.State.Set(mapping.ImmichAlbumID, assetID, state)
				}
			}
			summary.Failed += len(toAdd)
			_ = s.State.Save()
			return fmt.Errorf("add items to the shared album: %w", err)
		}
		for assetID := range toAdd {
			s.State.MarkAdded(mapping.ImmichAlbumID, assetID)
			summary.Uploaded++
		}
		if err := s.State.Save(); err != nil {
			return err
		}
		// The web add succeeded, so a previous gotohp album error is resolved.
		if albumErr != nil {
			uploadErr = nil
		}
	}

	if uploadErr != nil {
		if albumAddFailed {
			return fmt.Errorf("%w; this album is probably shared with you, so connect a Google Photos browser session in the Google upload settings", uploadErr)
		}
		return uploadErr
	}
	return nil
}

// stagingGroup returns the staging group for a route, creating its directory
// name on first use.
func stagingGroup(groups map[string]*uploadGroup, order *[]string, albumDir string, route AccountRoute) *uploadGroup {
	key := route.GoogleEmail
	if group, ok := groups[key]; ok {
		return group
	}
	dir := albumDir
	if key != "" {
		dir = filepath.Join(albumDir, sanitizeID(key))
	}
	group := &uploadGroup{
		key:    key,
		dir:    dir,
		route:  route,
		assets: make(map[string]bool),
		paths:  make(map[string]string),
	}
	groups[key] = group
	*order = append(*order, key)
	return group
}

// writerFor returns the route's web session, falling back to the service one.
func (s *Service) writerFor(route AccountRoute) SharedAlbumWriter {
	if route.SharedWriter != nil {
		return route.SharedWriter
	}
	return s.SharedWriter
}

// writerForEmail finds the session of the account that uploaded an asset.
func (s *Service) writerForEmail(email string) SharedAlbumWriter {
	if email == "" {
		return s.SharedWriter
	}
	if resolver, ok := s.Resolver.(AccountRouteByEmail); ok {
		if route, found := resolver.RouteForEmail(email); found {
			return s.writerFor(route)
		}
	}
	return s.SharedWriter
}

// addItems adds media keys to an album through the account's web session.
func (s *Service) addItems(ctx context.Context, writer SharedAlbumWriter, albumKey string, shared bool, mediaKeys []string) error {
	if writer == nil || !writer.Available() {
		return fmt.Errorf("this album needs a Google Photos browser session; connect one first")
	}
	return writer.AddItems(ctx, albumKey, shared, mediaKeys)
}

func (s *Service) reportUpload(uploaded, failed bool) {
	if s.Progress != nil {
		s.Progress.UploadItem(uploaded, failed)
	}
}

// assetIDFromPath maps a gotohp result path back to its Immich asset. It
// prefers the staged file table so two accounts can stage files with the same
// base name.
func assetIDFromPath(path string, group *uploadGroup) string {
	if id, ok := group.paths[path]; ok {
		return id
	}
	base := filepath.Base(path)
	if idx := strings.Index(base, "__"); idx > 0 {
		return base[:idx]
	}
	return ""
}

func setModTime(path string, at time.Time) {
	if at.IsZero() {
		return
	}
	_ = os.Chtimes(path, at, at)
}

func sanitizeID(id string) string {
	return strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(id)
}
