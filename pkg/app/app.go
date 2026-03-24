package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"warreth.dev/gphotos2immich/pkg/config"
	"warreth.dev/gphotos2immich/pkg/googlephotos"
	"warreth.dev/gphotos2immich/pkg/immich"
	"warreth.dev/gphotos2immich/pkg/progress"
	"warreth.dev/gphotos2immich/pkg/state"
	"warreth.dev/gphotos2immich/pkg/util"
)

type App struct {
	Cfg      *config.Config
	Client   *immich.Client
	GPClient *googlephotos.Client
	Logger   *slog.Logger
	State    *state.SyncState
}

func New(cfg *config.Config) (*App, error) {
	level := slog.LevelInfo
	if cfg.Debug {
		level = slog.LevelDebug
	}
	opts := &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.LevelKey {
				return slog.Attr{}
			}
			if a.Key == slog.TimeKey {
				t := a.Value.Time()
				return slog.String(slog.TimeKey, t.Format("15:04:05"))
			}
			return a
		},
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, opts))
	workers := cfg.Workers
	if workers < 1 {
		workers = 1
	}
	client := immich.NewClient(cfg.ApiURL, cfg.ApiKey, workers)
	gpClient := googlephotos.NewClient(logger, workers)

	// Initialize persistent dedup state
	dataDir := cfg.DataDir
	if dataDir == "" {
		dataDir = "./data"
	}
	syncState := state.New(filepath.Join(dataDir, "sync-state.json"))
	if err := syncState.Load(); err != nil {
		logger.Warn("Failed to load sync state, starting fresh", "error", err)
	}
	logger.Info("Loaded persistent dedup state", "entries", syncState.Count())

	return &App{
		Cfg:      cfg,
		Client:   client,
		GPClient: gpClient,
		Logger:   logger,
		State:    syncState,
	}, nil
}

func (a *App) Run(ctx context.Context) error {
	a.Logger.Info("Starting Immich Sync")

	id, name, err := a.Client.GetUser(ctx)
	if err != nil {
		return fmt.Errorf("failed to connect to Immich: %w", err)
	}
	a.Logger.Info("Connected to Immich", "user_id", id, "name", name)

	if len(a.Cfg.GooglePhotos) == 0 {
		a.Logger.Warn("No albums configured")
		return nil
	}

	// Initialize schedule - all albums due immediately
	nextRun := make(map[string]time.Time)
	for _, ac := range a.Cfg.GooglePhotos {
		nextRun[ac.URL] = time.Now()
	}

	albumWorkers := a.Cfg.AlbumWorkers
	if albumWorkers < 1 {
		albumWorkers = 1
	}

	for {
		// Check for shutdown between cycles
		select {
		case <-ctx.Done():
			a.Logger.Info("Shutdown requested, stopping sync loop")
			return nil
		default:
		}

		// Collect albums due for sync and find earliest next run
		var due []config.GooglePhotosConfig
		earliest := time.Now().Add(24 * time.Hour)

		for _, ac := range a.Cfg.GooglePhotos {
			if !time.Now().Before(nextRun[ac.URL]) {
				due = append(due, ac)
			} else if nextRun[ac.URL].Before(earliest) {
				earliest = nextRun[ac.URL]
			}
		}

		if len(due) > 0 {
			// Fetch album list and global assets from Immich once per sync cycle
			albumCache, err := a.Client.GetAlbums(ctx)
			if err != nil {
				a.Logger.Warn("Failed to fetch Immich album list", "error", err)
			}

			globalAssets, err := a.Client.SearchAssetsByDevice(ctx, "gphotos2immich-go")
			if err != nil {
				a.Logger.Warn("Failed to fetch global assets, will fall back to re-upload for duplicates", "error", err)
				globalAssets = make(map[string]string)
			}
			a.Logger.Debug("Pre-fetched global assets from Immich", "count", len(globalAssets))

			a.Logger.Info("Processing due albums", "count", len(due), "album_workers", albumWorkers)

			// Process due albums concurrently with bounded concurrency
			sem := make(chan struct{}, albumWorkers)
			var wg sync.WaitGroup
			for _, ac := range due {
				wg.Add(1)
				go func(ac config.GooglePhotosConfig) {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					a.processAlbum(ctx, ac, albumCache, globalAssets)
				}(ac)
			}
			wg.Wait()

			// Schedule next runs
			for _, ac := range due {
				interval, err := time.ParseDuration(ac.SyncInterval)
				if err != nil || interval == 0 {
					interval = 24 * time.Hour
				}
				nextRun[ac.URL] = time.Now().Add(interval)
				a.Logger.Info("Scheduled next sync", "album", ac.URL, "next_run", nextRun[ac.URL].Format("15:04:05"))
			}
			continue
		}

		// Wait until the next album is due or context cancellation
		waitDuration := time.Until(earliest)
		if waitDuration < time.Second {
			waitDuration = time.Second
		}
		a.Logger.Debug("Waiting for next sync cycle", "wait", waitDuration.Round(time.Second))

		select {
		case <-ctx.Done():
			a.Logger.Info("Shutdown requested, stopping sync loop")
			return nil
		case <-time.After(waitDuration):
		}
	}
}

type processResult struct {
	ID              string
	WasUploaded     bool
	Error           error
	BytesDownloaded int64
	BytesUploaded   int64
}

// resolveAlbumID finds or creates the Immich album for a given config entry
func (a *App) resolveAlbumID(ctx context.Context, ac config.GooglePhotosConfig, albumTitle string, albumCache []immich.Album, logger *slog.Logger) string {
	if ac.ImmichAlbumID != "" {
		return ac.ImmichAlbumID
	}
	for _, album := range albumCache {
		if album.AlbumName == albumTitle {
			return album.Id
		}
	}
	logger.Info("Creating Immich album", "title", albumTitle)
	newAlbum, err := a.Client.CreateAlbum(ctx, albumTitle)
	if err != nil {
		logger.Error("Error creating album", "error", err)
		return ""
	}
	return newAlbum.Id
}

// prefetchAlbumAssets fetches existing asset names from an Immich album for deduplication
func (a *App) prefetchAlbumAssets(ctx context.Context, albumId string, logger *slog.Logger) map[string]string {
	existingFiles := make(map[string]string)
	if albumId == "" {
		return existingFiles
	}
	albumDetails, err := a.Client.GetAlbum(ctx, albumId)
	if err != nil {
		logger.Warn("Failed to fetch album details", "error", err)
		return existingFiles
	}
	for _, asset := range albumDetails.Assets {
		name := util.StripExtension(asset.OriginalFileName)
		existingFiles[name] = asset.Id
	}
	logger.Debug("Pre-fetched album assets", "count", len(existingFiles))
	return existingFiles
}

func (a *App) processAlbum(ctx context.Context, ac config.GooglePhotosConfig, albumCache []immich.Album, globalAssets map[string]string) {
	logger := a.Logger.With("album_url", ac.URL)
	logger.Info("Syncing Google Photos Album")

	album, err := googlephotos.ScrapeAlbum(ctx, a.GPClient, ac.URL)
	if err != nil {
		logger.Error("Error scraping album", "error", err)
		return
	}

	albumTitle := album.Title
	if ac.AlbumName != "" {
		albumTitle = ac.AlbumName
	}
	logger.Info("Found photos in album", "count", len(album.Photos), "title", albumTitle)

	if len(album.Photos) == 0 {
		logger.Info("No photos found, skipping")
		return
	}

	albumId := a.resolveAlbumID(ctx, ac, albumTitle, albumCache, logger)
	existingFiles := a.prefetchAlbumAssets(ctx, albumId, logger)

	logger.Debug("Dedup cache loaded", "album_assets", len(existingFiles), "global_assets", len(globalAssets))

	var newAssetIds []string

	total := len(album.Photos)
	processed := 0
	added := 0
	skipped := 0
	failed := 0

	numWorkers := a.Cfg.Workers
	if numWorkers < 1 {
		numWorkers = 1
	}
	if numWorkers > total {
		numWorkers = total
	}

	logger.Debug("Processing items", "total_items", total, "workers", numWorkers)

	// Create and start progress tracker
	tracker := progress.New(albumTitle, total, a.Cfg.Debug)
	tracker.Start()

	jobs := make(chan googlephotos.Photo, numWorkers*2)
	results := make(chan processResult, numWorkers*2)
	var wg sync.WaitGroup

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				id, uploaded, bytesDown, bytesUp, err := a.processItem(ctx, p, albumTitle, ac.URL, existingFiles, globalAssets)
				results <- processResult{ID: id, WasUploaded: uploaded, Error: err, BytesDownloaded: bytesDown, BytesUploaded: bytesUp}
			}
		}()
	}

	// Feed jobs with context cancellation support
	go func() {
		defer close(jobs)
		for _, p := range album.Photos {
			select {
			case <-ctx.Done():
				return
			case jobs <- p:
			}
		}
	}()

	// Close results after all workers finish
	go func() {
		wg.Wait()
		close(results)
	}()

	// Stream results, flushing new assets to album every 10%
	flushInterval := total / 10
	if flushInterval < 1 {
		flushInterval = 1
	}
	lastFlushCount := 0

	for res := range results {
		processed++
		wasFailed := false
		wasSkipped := false
		wasAdded := false

		if res.Error != nil {
			logger.Error("Failed to process item", "error", res.Error)
			failed++
			wasFailed = true
		} else {
			if res.WasUploaded {
				added++
				wasAdded = true
			} else {
				skipped++
				wasSkipped = true
			}
			if res.ID != "" {
				newAssetIds = append(newAssetIds, res.ID)
			}
		}

		// Update progress tracker
		tracker.RecordItem(res.BytesDownloaded, res.BytesUploaded, wasAdded, wasSkipped, wasFailed)

		// Flush new assets to album periodically for incremental progress
		if albumId != "" && len(newAssetIds) > lastFlushCount && processed%flushInterval == 0 {
			batch := newAssetIds[lastFlushCount:]
			lastFlushCount = len(newAssetIds)
			logger.Debug("Adding assets to album (incremental)", "count", len(batch), "progress", fmt.Sprintf("%d/%d", processed, total))
			if err := a.Client.AddAssetsToAlbum(ctx, albumId, batch); err != nil {
				logger.Error("Error adding assets to album", "error", err)
			}
		}

		// Log progress every 100 items in debug mode
		if a.Cfg.Debug && processed%100 == 0 {
			logger.Debug("Progress", "processed", processed, "total", total, "added", added, "skipped", skipped, "failed", failed)
		}
	}

	// Stop tracker and print final summary
	tracker.Stop()

	// Final flush: add remaining unflushed assets to album
	if albumId != "" && len(newAssetIds) > lastFlushCount {
		remaining := newAssetIds[lastFlushCount:]
		logger.Debug("Finalizing album assets", "count", len(remaining), "album", albumTitle)
		if err := a.Client.AddAssetsToAlbum(ctx, albumId, remaining); err != nil {
			logger.Error("Error adding assets to album", "error", err)
		}
	}
	if a.Cfg.Debug {
		logger.Info("Sync finished", "added", added, "skipped", skipped, "failed", failed, "total", processed)
	}

	// Persist dedup state to disk after each album
	if err := a.State.Save(); err != nil {
		logger.Warn("Failed to save dedup state", "error", err)
	}
}

func (a *App) processItem(ctx context.Context, p googlephotos.Photo, albumTitle, albumURL string, existingFiles map[string]string, globalAssets map[string]string) (string, bool, int64, int64, error) {
	safeId := strings.ReplaceAll(p.ID, "/", "_")
	safeId = strings.ReplaceAll(safeId, ":", "_")
	baseName := fmt.Sprintf("gp_%s", safeId)

	// Dedup checks against Immich (album-level, global device search, and persistent state)
	if assetId, exists := existingFiles[baseName]; exists {
		a.Logger.Debug("Asset already in album (Immich album lookup)", "id", assetId, "filename", baseName)
		return "", false, 0, 0, nil
	}

	if assetId, exists := globalAssets[baseName]; exists {
		a.Logger.Debug("Asset exists in Immich (device search), adding to album", "id", assetId, "filename", baseName)
		return assetId, false, 0, 0, nil
	}

	// Persistent local state catches assets uploaded under a different deviceId/filename
	if assetId, exists := a.State.Get(baseName); exists {
		a.Logger.Debug("Asset found in persistent dedup cache", "id", assetId, "filename", baseName)
		return assetId, false, 0, 0, nil
	}

	a.Logger.Debug("Asset not found in dedup, will download",
		"baseName", baseName,
		"gp_item_id", p.ID,
	)

	if a.Cfg.StrictMetadata && p.TakenAt.IsZero() {
		a.Logger.Warn("Skipping item with missing metadata date",
			"id", p.ID, "url", p.URL)
		return "", false, 0, 0, nil
	}

	a.Logger.Debug("Downloading item", "id", safeId)
	data, ext, isVideo, err := googlephotos.DownloadMedia(ctx, a.GPClient, p.URL)
	if err != nil {
		return "", false, 0, 0, fmt.Errorf("error downloading item: %w", err)
	}

	bytesDownloaded := int64(len(data))

	if isVideo && a.Cfg.SkipVideos {
		a.Logger.Debug("Skipping video item", "id", p.ID)
		return "", false, bytesDownloaded, 0, nil
	}

	filename := baseName + ext

	description := p.Description
	sep := "\n"
	if description != "" {
		sep = "\n\n"
	}
	description += fmt.Sprintf("%sSource Album: %s (%s)", sep, albumTitle, albumURL)

	if p.TakenAt.IsZero() {
		a.Logger.Warn("Uploading item with missing metadata date (using current time)",
			"id", safeId, "url", p.URL, "is_video", isVideo)
	}

	// Handle motion photos for images
	if !isVideo {
		imageData, videoData, isMotion := googlephotos.ExtractMotionPhoto(data, a.Logger)

		if isMotion {
			a.Logger.Debug("Detected motion photo",
				"id", safeId,
				"image_size", len(imageData),
				"video_size", len(videoData),
			)

			// Upload the video part first
			videoFilename := baseName + ".mp4"
			videoId, videoDup, videoErr := a.Client.UploadAsset(ctx,
				videoData, videoFilename, p.TakenAt, "")
			if videoErr != nil {
				a.Logger.Warn("Failed to upload motion video, uploading image as static photo", "error", videoErr)
			} else if videoDup {
				a.Logger.Debug("Motion video deduplicated by Immich", "filename", videoFilename, "id", videoId)
			}

			// Upload the image linked to the video
			var uploadedId string
			var isDup bool
			if videoId != "" {
				uploadedId, isDup, err = a.Client.UploadAssetWithLive(ctx,
					imageData, filename, p.TakenAt, description, videoId)
			} else {
				uploadedId, isDup, err = a.Client.UploadAsset(ctx,
					imageData, filename, p.TakenAt, description)
			}
			if err != nil {
				return "", false, bytesDownloaded, 0, fmt.Errorf("error uploading %s: %w", filename, err)
			}
			if uploadedId == "" {
				return "", false, bytesDownloaded, 0, fmt.Errorf("upload returned empty ID for %s", filename)
			}

			bytesUploaded := int64(len(imageData) + len(videoData))
			a.State.Set(baseName, uploadedId)
			if isDup {
				a.Logger.Debug("Motion photo deduplicated by Immich", "filename", filename, "id", uploadedId)
				return uploadedId, false, bytesDownloaded, bytesUploaded, nil
			}
			a.Logger.Debug("Uploaded motion photo", "filename", filename, "id", uploadedId)
			return uploadedId, true, bytesDownloaded, bytesUploaded, nil
		}

		// Not a motion photo
		data = imageData
	}

	uploadedId, isDup, err := a.Client.UploadAsset(ctx, data, filename, p.TakenAt, description)
	if err != nil {
		return "", false, bytesDownloaded, 0, fmt.Errorf("error uploading %s: %w", filename, err)
	}
	if uploadedId == "" {
		return "", false, bytesDownloaded, 0, fmt.Errorf("upload returned empty ID for %s", filename)
	}

	bytesUploaded := int64(len(data))
	a.State.Set(baseName, uploadedId)

	if isDup {
		a.Logger.Debug("Asset deduplicated by Immich", "filename", filename, "id", uploadedId)
		return uploadedId, false, bytesDownloaded, bytesUploaded, nil
	}

	a.Logger.Debug("Uploaded item", "filename", filename, "id", uploadedId)
	return uploadedId, true, bytesDownloaded, bytesUploaded, nil
}

