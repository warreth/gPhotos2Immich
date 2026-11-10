package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
)

type App struct {
	Cfg           *config.Config
	Client        *immich.Client
	GPClient      *googlephotos.Client
	Logger        *slog.Logger
	State         *state.SyncState
	syncCh        chan struct{}
	descriptionCh chan descriptionUpdate
}

type descriptionUpdate struct {
	assetID         string
	description     string
	waitForMetadata bool
	force           bool
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
		Cfg:           cfg,
		Client:        client,
		GPClient:      gpClient,
		Logger:        logger,
		State:         syncState,
		syncCh:        make(chan struct{}, 1),
		descriptionCh: make(chan descriptionUpdate, 10000),
	}, nil
}

// TriggerSync requests an immediate sync of all albums outside the schedule
func (a *App) TriggerSync() {
	select {
	case a.syncCh <- struct{}{}:
	default:
	}
}

// queueDescription schedules an asset description update without blocking the
// upload workers. Immich v3 ignores the description field on upload and its
// metadata-extraction job can overwrite a description written too early, so
// updates are applied asynchronously after extraction has finished.
func (a *App) queueDescription(update descriptionUpdate) {
	if update.assetID == "" || update.description == "" {
		return
	}
	select {
	case a.descriptionCh <- update:
	default:
		a.Logger.Warn("Description update queue is full, skipping", "asset_id", update.assetID)
	}
}

func (a *App) descriptionWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case update := <-a.descriptionCh:
			a.applyDescriptionUpdate(ctx, update)
		}
	}
}

func (a *App) applyDescriptionUpdate(ctx context.Context, update descriptionUpdate) {
	if update.waitForMetadata {
		deadline := time.Now().Add(30 * time.Second)
		for {
			asset, err := a.Client.GetAsset(ctx, update.assetID)
			if err == nil && asset.HasMetadata {
				break
			}
			var httpErr *immich.HTTPError
			if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
				return
			}
			if time.Now().After(deadline) {
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
	}

	if !update.force {
		asset, err := a.Client.GetAsset(ctx, update.assetID)
		if err == nil {
			current := asset.ExifInfo.Description
			if current != "" && current != update.description && !strings.Contains(current, "Source Album:") {
				a.Logger.Debug("Leaving user-edited description untouched", "asset_id", update.assetID)
				return
			}
		}
	}

	if err := a.Client.UpdateAssetDescription(ctx, update.assetID, update.description); err != nil {
		a.Logger.Warn("Failed to update asset description", "asset_id", update.assetID, "error", err)
	}
}

func (a *App) queueUploadDescription(assetID, description string, isDup bool) {
	if description == "" || assetID == "" {
		return
	}
	if isDup && !a.Cfg.UpdateExistingMetadata {
		return
	}
	a.queueDescription(descriptionUpdate{
		assetID:         assetID,
		description:     description,
		waitForMetadata: !isDup,
		force:           !isDup,
	})
}

func (a *App) maybeQueueDescription(assetID string, p googlephotos.Photo, albumTitle, albumURL string, globalAssets *immich.AssetIndex) {
	if !a.Cfg.UpdateExistingMetadata || assetID == "" {
		return
	}
	description := buildDescription(p, albumTitle, albumURL)
	if description == "" {
		return
	}
	if globalAssets != nil {
		if current, ok := globalAssets.Descriptions[assetID]; ok && descriptionAlreadySet(current, p.Photographer) {
			return
		}
	}
	a.queueDescription(descriptionUpdate{
		assetID:     assetID,
		description: description,
	})
}

func descriptionAlreadySet(current, photographer string) bool {
	if current == "" {
		return false
	}
	if !strings.Contains(current, "Source Album:") {
		// A custom caption is left untouched by updateExistingMetadata.
		return true
	}
	if photographer == "" {
		return true
	}
	return strings.Contains(current, "Photographer: "+photographer)
}

func buildDescription(p googlephotos.Photo, albumTitle, albumURL string) string {
	parts := make([]string, 0, 3)
	if p.Description != "" {
		parts = append(parts, p.Description)
	}
	if p.Photographer != "" {
		parts = append(parts, "Photographer: "+p.Photographer)
	}
	parts = append(parts, fmt.Sprintf("Source Album: %s (%s)", albumTitle, albumURL))
	return strings.Join(parts, "\n\n")
}

const (
	immichConnectInitialDelay = 2 * time.Second
	immichConnectMaxDelay     = 30 * time.Second
)

// waitForImmich waits for Immich to accept requests. The sync container is
// often started at the same time as Immich, which then still needs to boot, so
// connection failures are retried with backoff instead of aborting startup.
// Client errors (bad API key, wrong URL) are returned immediately because
// retrying them cannot succeed.
func (a *App) waitForImmich(ctx context.Context) (string, string, error) {
	delay := immichConnectInitialDelay
	for attempt := 1; ; attempt++ {
		id, name, err := a.Client.GetUser(ctx)
		if err == nil {
			return id, name, nil
		}
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		if isPermanentImmichError(err) {
			return "", "", err
		}

		a.Logger.Warn("Immich is not reachable yet, retrying",
			"attempt", attempt,
			"retry_in", delay.Round(time.Second),
			"error", err,
		)

		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(delay):
		}

		delay *= 2
		if delay > immichConnectMaxDelay {
			delay = immichConnectMaxDelay
		}
	}
}

// isPermanentImmichError reports whether retrying the request cannot help,
// for example when the API key is rejected or the API URL does not exist.
func isPermanentImmichError(err error) bool {
	var httpErr *immich.HTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	return httpErr.StatusCode >= 400 &&
		httpErr.StatusCode < 500 &&
		httpErr.StatusCode != http.StatusTooManyRequests
}

func (a *App) Run(ctx context.Context) error {
	a.Logger.Info("Starting Immich Sync")

	id, name, err := a.waitForImmich(ctx)
	if err != nil {
		if ctx.Err() != nil {
			a.Logger.Info("Shutdown requested, stopping sync loop")
			return nil
		}
		return fmt.Errorf("failed to connect to Immich: %w", err)
	}
	a.Logger.Info("Connected to Immich", "user_id", id, "name", name)

	if len(a.Cfg.GooglePhotos) == 0 {
		a.Logger.Warn("No albums configured")
		return nil
	}

	// Description updates are applied asynchronously so a slow metadata
	// extraction job never blocks the upload workers.
	for i := 0; i < 4; i++ {
		go a.descriptionWorker(ctx)
	}

	// Initialize schedule - all albums due immediately
	nextRun := make(map[string]time.Time)
	nowStart := time.Now()
	for _, ac := range a.Cfg.GooglePhotos {
		nextRun[ac.URL] = nowStart
	}
	progress.SetScheduleTimes(time.Time{}, nowStart)

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

			globalAssets, err := a.Client.SearchAssets(ctx, "gphotos2immich-go")
			if err != nil {
				a.Logger.Warn("Failed to fetch global assets, dedup state will be trusted without verification", "error", err)
				globalAssets = nil
			} else {
				a.Logger.Debug("Pre-fetched global assets from Immich", "count", globalAssets.Count)
			}

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

			// Schedule next runs and record completion
			nowCompleted := time.Now()
			nextEarliest := nowCompleted.Add(24 * time.Hour)
			for _, ac := range due {
				interval, err := time.ParseDuration(ac.SyncInterval)
				if err != nil || interval == 0 {
					interval = 24 * time.Hour
				}
				nextRun[ac.URL] = nowCompleted.Add(interval)
				a.Logger.Info("Scheduled next sync", "album", ac.URL, "next_run", nextRun[ac.URL].Format("15:04:05"))
			}
			for _, nr := range nextRun {
				if nr.Before(nextEarliest) {
					nextEarliest = nr
				}
			}
			progress.SetScheduleTimes(nowCompleted, nextEarliest)
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
		case <-a.syncCh:
			a.Logger.Info("Sync triggered manually, running all albums immediately")
			now := time.Now()
			for _, ac := range a.Cfg.GooglePhotos {
				nextRun[ac.URL] = now
			}
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

// prefetchAlbumAssets fetches existing assets from an Immich album for deduplication.
func (a *App) prefetchAlbumAssets(ctx context.Context, albumId string, logger *slog.Logger) *immich.AssetIndex {
	index := immich.NewAssetIndex()
	if albumId == "" {
		return index
	}
	albumDetails, err := a.Client.GetAlbum(ctx, albumId)
	if err != nil {
		logger.Warn("Failed to fetch album details", "error", err)
		return index
	}
	for _, asset := range albumDetails.Assets {
		index.Add(asset.Id, asset.OriginalFileName, "", "")
	}
	logger.Debug("Pre-fetched album assets", "count", index.Count)
	return index
}

func (a *App) processAlbum(ctx context.Context, ac config.GooglePhotosConfig, albumCache []immich.Album, globalAssets *immich.AssetIndex) {
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

	globalCount := 0
	if globalAssets != nil {
		globalCount = globalAssets.Count
	}
	logger.Debug("Dedup cache loaded", "album_assets", existingFiles.Count, "global_assets", globalCount)

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

func (a *App) processItem(ctx context.Context, p googlephotos.Photo, albumTitle, albumURL string, albumAssets, globalAssets *immich.AssetIndex) (string, bool, int64, int64, error) {
	safeId := strings.ReplaceAll(p.ID, "/", "_")
	safeId = strings.ReplaceAll(safeId, ":", "_")
	baseName := fmt.Sprintf("gp_%s", safeId)
	isVideoItem := p.IsVideo

	// Album-level dedup: the album is fetched fresh from Immich, so a hit here
	// is authoritative even when the persistent state is stale.
	if assetId, exists := findIndexedAsset(albumAssets, baseName, isVideoItem); exists {
		a.Logger.Debug("Asset already in album (Immich album lookup)", "id", assetId, "filename", baseName)
		a.State.Set(baseName, assetId)
		a.maybeQueueDescription(assetId, p, albumTitle, albumURL, globalAssets)
		return "", false, 0, 0, nil
	}

	// Global dedup: assets uploaded by this tool, including compressed
	// replacements whose filename carries a ".cmp" marker.
	if assetId, exists := findIndexedAsset(globalAssets, baseName, isVideoItem); exists {
		a.Logger.Debug("Asset exists in Immich (global search), adding to album", "id", assetId, "filename", baseName)
		a.State.Set(baseName, assetId)
		a.maybeQueueDescription(assetId, p, albumTitle, albumURL, globalAssets)
		return assetId, false, 0, 0, nil
	}

	// Persistent local state catches assets uploaded under a different
	// deviceId/filename. Verify it when the global index is available so
	// deletions and compressed replacements are handled correctly.
	stateAssetID, stateAction := a.resolveStateAsset(baseName, isVideoItem, globalAssets)
	switch stateAction {
	case stateUse:
		a.Logger.Debug("Asset found in persistent dedup cache", "id", stateAssetID, "filename", baseName)
		a.maybeQueueDescription(stateAssetID, p, albumTitle, albumURL, globalAssets)
		return stateAssetID, false, 0, 0, nil
	case stateSkip:
		a.Logger.Debug("State asset no longer exists in Immich, skipping (dedupMode=safe)",
			"id", stateAssetID, "filename", baseName)
		return "", false, 0, 0, nil
	case stateReupload:
		a.Logger.Info("State asset no longer exists in Immich, re-uploading",
			"old_id", stateAssetID, "filename", baseName)
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
	media, err := googlephotos.DownloadItemMedia(ctx, a.GPClient, p)
	if err != nil {
		return "", false, 0, 0, fmt.Errorf("error downloading item: %w", err)
	}
	data, ext, isVideo := media.Data, media.Ext, media.Kind == "video"

	bytesDownloaded := int64(len(data))
	if len(media.VideoData) > 0 {
		bytesDownloaded += int64(len(media.VideoData))
	}

	// If a type-mismatched state entry turns out to match the media we just
	// downloaded (for example a video whose album metadata said image), keep
	// the existing asset instead of re-uploading it on every sync.
	if stateAssetID != "" && globalAssets != nil && globalAssets.IDs[stateAssetID] {
		downloadedIsVideo := media.Kind == "video"
		stateType := globalAssets.Types[stateAssetID]
		if (downloadedIsVideo && stateType == "VIDEO") || (!downloadedIsVideo && stateType == "IMAGE") {
			a.Logger.Debug("Downloaded media matches existing state asset, skipping upload",
				"id", stateAssetID, "filename", baseName)
			a.maybeQueueDescription(stateAssetID, p, albumTitle, albumURL, globalAssets)
			return stateAssetID, false, bytesDownloaded, 0, nil
		}
	}

	if isVideo && a.Cfg.SkipVideos {
		a.Logger.Debug("Skipping video item", "id", p.ID)
		return "", false, bytesDownloaded, 0, nil
	}

	filename := baseName + ext
	if isVideo {
		a.Logger.Debug("Classified item as video",
			"id", safeId,
			"filename", filename,
			"ext", ext,
		)
	}

	description := buildDescription(p, albumTitle, albumURL)

	if p.TakenAt.IsZero() {
		a.Logger.Warn("Uploading item with missing metadata date (using current time)",
			"id", safeId, "url", p.URL, "is_video", isVideo)
	}

	// Handle motion photos for images
	if !isVideo {
		imageData, videoData, isMotion, hadMotionXMP := googlephotos.ExtractMotionPhoto(data, a.Logger)
		motionVideoExt := ".mp4"
		// Some Motion Photos contain identifying XMP metadata while Google
		// serves the video component separately through the =dv endpoint.
		// Preserve upstream's sidecar behavior for those files.
		if !isMotion && hadMotionXMP {
			sidecarData, sidecarExt, sidecarErr := googlephotos.DownloadMotionVideoSidecar(ctx, a.GPClient, p.URL)
			if sidecarErr == nil {
				videoData = sidecarData
				isMotion = true
				if sidecarExt != "" {
					motionVideoExt = sidecarExt
				}
				a.Logger.Debug("Motion photo sidecar downloaded",
					"id", safeId,
					"video_size", len(videoData),
				)
			} else {
				a.Logger.Debug("Motion XMP found but sidecar unavailable, stripping XMP flags",
					"id", safeId,
					"error", sidecarErr,
				)
				googlephotos.StripMotionPhotoXMP(imageData)
			}
		}

		// Live photos detected via Apple QuickTime sidecar metadata: the
		// video component was already fetched by DownloadItemMedia.
		if !isMotion && !hadMotionXMP && media.Kind == "live" {
			videoData = media.VideoData
			isMotion = true
			if media.VideoExt != "" {
				motionVideoExt = media.VideoExt
			}
			a.Logger.Debug("Live photo sidecar downloaded",
				"id", safeId,
				"video_size", len(videoData),
			)
		}

		if isMotion {
			a.Logger.Debug("Detected motion photo",
				"id", safeId,
				"image_size", len(imageData),
				"video_size", len(videoData),
			)

			// Upload the video part first
			videoFilename := baseName + motionVideoExt
			videoId, videoDup, videoErr := a.Client.UploadAssetWithVisibility(ctx,
				videoData, videoFilename, p.TakenAt, "", "hidden")
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
			// Only apply the live photo link for deduplicated assets, since UploadAssetWithLive
			// already applied it during the initial upload
			if videoId != "" && isDup {
				if linkErr := a.Client.LinkLivePhotoToAsset(ctx, uploadedId, videoId); linkErr != nil {
					a.Logger.Warn("Failed to link motion video to deduplicated photo", "photo_id", uploadedId, "video_id", videoId, "error", linkErr)
				}
			}

			bytesUploaded := int64(len(imageData))
			if videoId != "" {
				bytesUploaded += int64(len(videoData))
			}
			a.State.Set(baseName, uploadedId)
			a.queueUploadDescription(uploadedId, description, isDup)
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
	a.queueUploadDescription(uploadedId, description, isDup)

	if isDup {
		a.Logger.Debug("Asset deduplicated by Immich", "filename", filename, "id", uploadedId)
		return uploadedId, false, bytesDownloaded, bytesUploaded, nil
	}

	a.Logger.Debug("Uploaded item", "filename", filename, "id", uploadedId)
	return uploadedId, true, bytesDownloaded, bytesUploaded, nil
}

func findIndexedAsset(index *immich.AssetIndex, baseName string, isVideo bool) (string, bool) {
	if index == nil {
		return "", false
	}
	if isVideo {
		return index.FindVideo(baseName, "")
	}
	return index.FindImage(baseName)
}

type stateAction int

const (
	stateNone stateAction = iota
	stateUse
	stateSkip
	stateReupload
)

// resolveStateAsset decides what to do with a persistent state entry for an
// item. It returns the asset ID and one of:
//   - stateNone: no state entry, continue with a normal download.
//   - stateUse: the entry is still valid (or a replacement was adopted).
//   - stateSkip: the asset was deleted and safe mode says to respect that.
//   - stateReupload: the asset is missing and repair mode wants it back.
func (a *App) resolveStateAsset(baseName string, isVideo bool, globalAssets *immich.AssetIndex) (string, stateAction) {
	assetID, exists := a.State.Get(baseName)
	if !exists {
		return "", stateNone
	}

	if !a.Cfg.VerifyDedupState() || globalAssets == nil {
		return assetID, stateUse
	}

	if globalAssets.IDs[assetID] {
		stateType := globalAssets.Types[assetID]
		if (isVideo && stateType == "VIDEO") || (!isVideo && stateType == "IMAGE") {
			return assetID, stateUse
		}
		// The state points at a different media type. This is the old HEIC
		// Live Photo bug (an image item recorded as its .mov clip), so
		// re-process the item instead of skipping it. Keep the ID so the
		// post-download check can still avoid a needless re-upload when the
		// downloaded media actually matches the existing asset.
		return assetID, stateReupload
	}

	if a.Cfg.AdoptReplacements() {
		if replacement, ok := globalAssets.FindReplacement(baseName, isVideo); ok {
			a.State.Set(baseName, replacement)
			return replacement, stateUse
		}
	}

	if a.Cfg.ReuploadMissing() {
		return "", stateReupload
	}
	return "", stateSkip
}
