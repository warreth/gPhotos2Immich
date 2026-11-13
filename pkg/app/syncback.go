package app

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"warreth.dev/gphotos2immich/pkg/config"
	"warreth.dev/gphotos2immich/pkg/googlephotos"
	"warreth.dev/gphotos2immich/pkg/gphotos"
	"warreth.dev/gphotos2immich/pkg/gphotosweb"
	"warreth.dev/gphotos2immich/pkg/immich"
	"warreth.dev/gphotos2immich/pkg/syncback"
	"warreth.dev/gphotos2immich/pkg/upload"
)

// resolveGotohpBinary finds the pinned CLI in the image or on PATH.
func resolveGotohpBinary(configured string) string {
	if configured != "" && configured != "gotohp-cli" {
		return configured
	}
	if _, err := exec.LookPath("gotohp-cli"); err == nil {
		return "gotohp-cli"
	}
	if _, err := os.Stat("/app/gotohp-cli"); err == nil {
		return "/app/gotohp-cli"
	}
	return configured
}

// NewSyncBack wires the optional Immich -> Google Photos direction from the
// configuration. It is always created so the web UI can connect an account and
// resolve tight album mappings; the per-album uploadToGoogle switch is the real
// opt-in, and GoogleUpload.Enabled only controls the automatic schedule.
func NewSyncBack(cfg *config.Config, imported upload.ImportedAssetSource, logger *slog.Logger) *syncback.Service {
	if logger == nil {
		logger = slog.Default()
	}

	dataDir := cfg.DataDir
	if dataDir == "" {
		dataDir = "./data"
	}

	google := gphotos.NewCLIClient(resolveGotohpBinary(cfg.EffectiveGotohpBinary()), cfg.EffectiveGotohpConfig())
	web := gphotosweb.NewClient(cfg.EffectiveWebCookiesFile())
	gpClient := googlephotos.NewClient(logger, cfg.EffectiveThreads())
	immichClient := immich.NewClient(cfg.ApiURL, cfg.ApiKey, cfg.EffectiveThreads())

	uploadState := upload.NewState(filepath.Join(dataDir, "upload-state.json"))
	if err := uploadState.Load(); err != nil {
		logger.Warn("Could not load upload state", "error", err)
	}

	uploader := &upload.Service{
		Immich:           immichClient,
		Google:           google,
		State:            uploadState,
		Imported:         imported,
		SharedWriter:     web,
		Logger:           logger,
		StagingRoot:      filepath.Join(dataDir, "upload-staging"),
		GotohpConfigPath: cfg.EffectiveGotohpConfig(),
		Threads:          cfg.EffectiveThreads(),
	}

	return syncback.New(syncback.Options{
		Google:         google,
		Resolver:       scraperResolver{client: gpClient},
		Albums:         immichAlbumLister{client: immichClient},
		Uploader:       uploader,
		Web:            web,
		WebCookiesPath: cfg.EffectiveWebCookiesFile(),
		Logger:         logger,
	})
}
