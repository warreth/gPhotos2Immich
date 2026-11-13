// Package privateimport owns the experimental import of private Google Photos
// albums into Immich. It is separate from the public shared-album scraper and
// from the Immich -> Google upload direction.
package privateimport

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"warreth.dev/gphotos2immich/pkg/gphotosweb"
	"warreth.dev/gphotos2immich/pkg/immich"
)

// WebSource is the part of the Google Photos web client this package needs.
type WebSource interface {
	ListAlbums(ctx context.Context) ([]gphotosweb.Album, error)
	ListAlbumItems(ctx context.Context, albumKey string) ([]gphotosweb.MediaItem, error)
	DownloadMediaToFile(ctx context.Context, mediaKey, destPath string) (gphotosweb.MediaInfo, error)
}

// ImmichTarget is the part of the Immich client this package needs.
type ImmichTarget interface {
	CreateAlbum(ctx context.Context, name string) (*immich.Album, error)
	AddAssetsToAlbum(ctx context.Context, albumID string, assetIDs []string) error
	UploadAssetFromFile(ctx context.Context, path, filename string, createdAt time.Time, description string) (string, bool, error)
}

// State is the persistent dedup store shared with the import direction.
type State interface {
	Get(baseName string) (string, bool)
	Set(baseName, assetID string)
	Save() error
}

// Service imports one private Google Photos album at a time.
type Service struct {
	Web         WebSource
	Immich      ImmichTarget
	State       State
	Logger      *slog.Logger
	StagingRoot string
}

// Request describes one private import.
type Request struct {
	AlbumKey        string
	AlbumTitle      string
	ImmichAlbumID   string
	ImmichAlbumName string
}

// Summary reports one private import run.
type Summary struct {
	Album    string
	Total    int
	Imported int
	Skipped  int
	Failed   int
}

// Import downloads every item in the album and adds it to the target Immich
// album. Items already imported by this tool are skipped. The state is written
// only after the Immich album add succeeds, so a failed add is retried on the
// next run without losing the item.
func (s *Service) Import(ctx context.Context, req Request, progress func(Summary)) (Summary, error) {
	summary := Summary{Album: req.AlbumTitle}
	if s.Web == nil || s.Immich == nil || s.State == nil {
		return summary, fmt.Errorf("private import is not configured")
	}
	albumKey := strings.TrimSpace(req.AlbumKey)
	if albumKey == "" {
		return summary, fmt.Errorf("album key is required")
	}
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}

	albumID := strings.TrimSpace(req.ImmichAlbumID)
	if albumID == "" {
		name := strings.TrimSpace(req.ImmichAlbumName)
		if name == "" {
			name = strings.TrimSpace(req.AlbumTitle)
		}
		if name == "" {
			name = "Google Photos import"
		}
		album, err := s.Immich.CreateAlbum(ctx, name)
		if err != nil {
			return summary, fmt.Errorf("create Immich album: %w", err)
		}
		albumID = album.Id
	}

	items, err := s.Web.ListAlbumItems(ctx, albumKey)
	if err != nil {
		return summary, fmt.Errorf("list Google Photos album: %w", err)
	}
	summary.Total = len(items)
	if progress != nil {
		progress(summary)
	}

	albumDir := filepath.Join(s.StagingRoot, "private-"+sanitize(albumKey))
	if err := os.MkdirAll(albumDir, 0755); err != nil {
		return summary, fmt.Errorf("create staging directory: %w", err)
	}
	defer os.RemoveAll(albumDir)

	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		stateKey := "gp_private_" + item.Key
		if _, ok := s.State.Get(stateKey); ok {
			summary.Skipped++
			if progress != nil {
				progress(summary)
			}
			continue
		}

		downloadPath := filepath.Join(albumDir, sanitize(item.Key)+".download")
		info, err := s.Web.DownloadMediaToFile(ctx, item.Key, downloadPath)
		if err != nil {
			logger.Warn("Could not download private Google Photos item", "media_key", item.Key, "error", err)
			summary.Failed++
			if progress != nil {
				progress(summary)
			}
			continue
		}
		filename := filepath.Base(info.Filename)
		if filename == "." || filename == "" || filename == string(filepath.Separator) {
			filename = item.Key + ".jpg"
		}
		if item.IsVideo && filename == item.Key+".jpg" {
			filename = item.Key + ".mp4"
		}
		createdAt := info.TakenAt
		if createdAt.IsZero() {
			createdAt = item.TakenAt
		}
		if createdAt.IsZero() {
			createdAt = time.Now()
		}
		description := info.Description
		if description == "" {
			title := strings.TrimSpace(req.AlbumTitle)
			if title == "" {
				title = "Google Photos"
			}
			description = "Imported from Google Photos album: " + title
		}

		assetID, _, err := s.Immich.UploadAssetFromFile(ctx, downloadPath, filename, createdAt, description)
		if err != nil {
			logger.Warn("Could not upload private Google Photos item to Immich", "media_key", item.Key, "error", err)
			_ = os.Remove(downloadPath)
			summary.Failed++
			if progress != nil {
				progress(summary)
			}
			continue
		}
		if err := s.Immich.AddAssetsToAlbum(ctx, albumID, []string{assetID}); err != nil {
			logger.Warn("Could not add private import to Immich album", "media_key", item.Key, "error", err)
			_ = os.Remove(downloadPath)
			summary.Failed++
			if progress != nil {
				progress(summary)
			}
			continue
		}
		s.State.Set(stateKey, assetID)
		_ = os.Remove(downloadPath)
		summary.Imported++
		if progress != nil {
			progress(summary)
		}
	}

	if err := s.State.Save(); err != nil {
		return summary, fmt.Errorf("save import state: %w", err)
	}
	return summary, nil
}

// ListAlbums returns private albums only. Shared albums belong to the public
// scraper and the normal import flow.
func (s *Service) ListAlbums(ctx context.Context) ([]gphotosweb.Album, error) {
	if s.Web == nil {
		return nil, fmt.Errorf("private import is not configured")
	}
	albums, err := s.Web.ListAlbums(ctx)
	if err != nil {
		return nil, err
	}
	private := make([]gphotosweb.Album, 0, len(albums))
	for _, album := range albums {
		if !album.Shared {
			private = append(private, album)
		}
	}
	return private, nil
}

func sanitize(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "item"
	}
	return strings.NewReplacer("/", "_", "\\", "_", ":", "_", "..", "_").Replace(value)
}
