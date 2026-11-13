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
	Progress         ProgressReporter
	Logger           *slog.Logger
	StagingRoot      string
	GotohpConfigPath string
	Threads          int
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
	writerReady := s.SharedWriter != nil && s.SharedWriter.Available()
	useWebAdd := mapping.UseWebAdd || mapping.Shared
	if useWebAdd && !writerReady {
		return summary, fmt.Errorf("this album needs a Google Photos browser session; connect one first")
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

	// If a previous run uploaded files but failed to add them to a shared
	// album, finish that add before uploading anything new. The media keys are
	// already known, so no file is downloaded twice.
	handled := make(map[string]bool)
	if useWebAdd {
		pending := s.State.PendingAlbumAdds(mapping.ImmichAlbumID)
		if len(pending) > 0 {
			keys := make([]string, 0, len(pending))
			for _, item := range pending {
				keys = append(keys, item.MediaKey)
			}
			if err := s.SharedWriter.AddItems(ctx, mapping.GoogleAlbumKey, mapping.Shared, keys); err != nil {
				return summary, fmt.Errorf("add previously uploaded items to the shared album: %w", err)
			}
			for _, item := range pending {
				s.State.MarkAdded(mapping.ImmichAlbumID, item.AssetID)
				handled[item.AssetID] = true
				summary.Uploaded++
			}
			if err := s.State.Save(); err != nil {
				return summary, err
			}
			logger.Info("Added previously uploaded items to the shared album",
				"album", mapping.GoogleAlbumURL, "items", len(pending))
		}
	}

	// Optional safety check: if a photo was deleted from Google Photos, decide
	// whether to upload it again. Off by default, because keeping deletions is
	// the safer behavior. A missing inspector means the check cannot run, so
	// the state is trusted.
	var present map[string]bool
	if mapping.ReuploadDeleted {
		if s.Inspector == nil {
			logger.Warn("Re-upload check is enabled but album inspection is not available")
		} else {
			present, err = s.Inspector.AlbumMediaKeys(ctx, mapping.GoogleAlbumKey)
			if err != nil {
				logger.Warn("Could not inspect the Google album; skipping the re-upload check", "error", err)
				present = nil
			} else {
				logger.Debug("Loaded album media keys for the re-upload check", "count", len(present))
			}
		}
	}

	albumDir := filepath.Join(s.StagingRoot, sanitizeID(mapping.ImmichAlbumID))
	if err := os.MkdirAll(albumDir, 0755); err != nil {
		return summary, fmt.Errorf("create staging directory: %w", err)
	}
	defer os.RemoveAll(albumDir)

	manifest := make(map[string]string) // staged path -> immich asset id
	staged := 0

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
				missing := mapping.ReuploadDeleted && present != nil && state.GoogleMediaKey != "" && !present[state.GoogleMediaKey]
				if missing {
					logger.Info("Photo is missing from Google Photos; scheduling it again",
						"asset_id", asset.Id, "media_key", state.GoogleMediaKey)
					s.State.Set(mapping.ImmichAlbumID, asset.Id, AssetState{
						GoogleMediaKey: state.GoogleMediaKey,
						Filename:       state.Filename,
						IsLivePhoto:    state.IsLivePhoto,
						Status:         "failed",
						Error:          "missing from Google Photos album",
					})
				} else {
					if !handled[asset.Id] {
						summary.Skipped++
					}
					continue
				}
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

		stillName := asset.OriginalFileName
		if stillName == "" {
			stillName = asset.Id + ".jpg"
		}
		stagedName := asset.Id + "__" + stillName
		stillPath := filepath.Join(albumDir, stagedName)
		if err := s.Immich.DownloadAssetToFile(ctx, asset.Id, stillPath); err != nil {
			logger.Warn("Could not download Immich asset for Google upload", "asset_id", asset.Id, "error", err)
			s.State.Set(mapping.ImmichAlbumID, asset.Id, AssetState{
				Filename: stillName,
				Status:   "failed",
				Error:    err.Error(),
			})
			summary.Failed++
			continue
		}
		setModTime(stillPath, asset.FileCreatedAt)
		manifest[stillPath] = asset.Id
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
				videoPath := filepath.Join(albumDir, videoName)
				if err := s.Immich.DownloadAssetToFile(ctx, asset.LivePhotoVideoID, videoPath); err != nil {
					logger.Warn("Could not download Live Photo video; uploading the still only",
						"asset_id", asset.Id, "video_id", asset.LivePhotoVideoID, "error", err)
				} else {
					setModTime(videoPath, asset.FileCreatedAt)
					manifest[videoPath] = asset.Id
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
		StagingDir:     albumDir,
		AlbumKey:       uploadAlbumKey,
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
	var albumErr *gphotos.AlbumError
	fallbackShared := false
	if !useWebAdd && errors.As(uploadErr, &albumErr) && writerReady {
		useWebAdd = true
		fallbackShared = !mapping.Shared
	}
	albumAddFailed := !useWebAdd && errors.As(uploadErr, &albumErr)

	toAdd := make(map[string]string) // immich asset id -> google media key
	seen := make(map[string]bool)
	for _, result := range results {
		assetID := assetIDFromPath(result.Path, manifest)
		if assetID == "" {
			for _, p := range result.Paths {
				if id := assetIDFromPath(p, manifest); id != "" {
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
				Filename: filename,
				Status:   "failed",
				Error:    result.Error,
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
				Status:         "failed",
				Error:          albumErr.Error(),
			})
			summary.Failed++
			s.reportUpload(false, true)
			continue
		}
		if result.Skipped {
			if useWebAdd && result.MediaKey != "" {
				// The file is already in the library, but its album add still
				// has to happen. This is the crash-recovery path: the state
				// may not have recorded the media key.
				s.State.Set(mapping.ImmichAlbumID, assetID, AssetState{
					GoogleMediaKey: result.MediaKey,
					Filename:       filename,
					IsLivePhoto:    result.IsLivePhoto,
					Status:         "uploaded",
					UploadedAt:     time.Now(),
				})
				toAdd[assetID] = result.MediaKey
				s.reportUpload(true, false)
				continue
			}
			s.State.Set(mapping.ImmichAlbumID, assetID, AssetState{
				Filename: filename,
				Status:   "skipped",
				Error:    result.SkipReason,
			})
			summary.Skipped++
			s.reportUpload(false, false)
			continue
		}
		s.State.Set(mapping.ImmichAlbumID, assetID, AssetState{
			GoogleMediaKey: result.MediaKey,
			Filename:       filename,
			IsLivePhoto:    result.IsLivePhoto,
			Status:         "uploaded",
			AddedToAlbum:   !useWebAdd,
			UploadedAt:     time.Now(),
		})
		if useWebAdd {
			toAdd[assetID] = result.MediaKey
		} else {
			summary.Uploaded++
		}
		s.reportUpload(true, false)
	}

	// Staged assets that got no result at all are retried next run.
	manifestIDs := make(map[string]bool)
	for _, assetID := range manifest {
		manifestIDs[assetID] = true
	}
	for assetID := range manifestIDs {
		if seen[assetID] {
			continue
		}
		s.State.Set(mapping.ImmichAlbumID, assetID, AssetState{
			Status: "failed",
			Error:  "gotohp returned no result for this asset",
		})
		summary.Failed++
		s.reportUpload(false, true)
	}

	s.State.SetAlbumMeta(mapping.ImmichAlbumID, mapping.GoogleAlbumKey, mapping.GoogleAlbumURL, time.Now())
	// Persist the media keys before the album add, so a crash or a stale
	// browser session can retry the add without uploading the files again.
	if err := s.State.Save(); err != nil {
		return summary, err
	}

	if useWebAdd && len(toAdd) > 0 {
		keys := make([]string, 0, len(toAdd))
		for _, mediaKey := range toAdd {
			keys = append(keys, mediaKey)
		}
		if err := s.SharedWriter.AddItems(ctx, mapping.GoogleAlbumKey, mapping.Shared || fallbackShared, keys); err != nil {
			for assetID := range toAdd {
				if state, ok := s.State.Get(mapping.ImmichAlbumID, assetID); ok {
					state.Error = err.Error()
					s.State.Set(mapping.ImmichAlbumID, assetID, state)
				}
			}
			summary.Failed += len(toAdd)
			_ = s.State.Save()
			return summary, fmt.Errorf("add items to the shared album: %w", err)
		}
		for assetID := range toAdd {
			s.State.MarkAdded(mapping.ImmichAlbumID, assetID)
			summary.Uploaded++
		}
		if err := s.State.Save(); err != nil {
			return summary, err
		}
		// The web add succeeded, so a previous gotohp album error is resolved.
		if albumErr != nil {
			uploadErr = nil
		}
	}

	if uploadErr != nil {
		if albumAddFailed {
			return summary, fmt.Errorf("%w; this album is probably shared with you, so connect a Google Photos browser session in the Google upload settings", uploadErr)
		}
		return summary, uploadErr
	}
	return summary, nil
}

func (s *Service) reportUpload(uploaded, failed bool) {
	if s.Progress != nil {
		s.Progress.UploadItem(uploaded, failed)
	}
}

func assetIDFromPath(path string, manifest map[string]string) string {
	if id, ok := manifest[path]; ok {
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
