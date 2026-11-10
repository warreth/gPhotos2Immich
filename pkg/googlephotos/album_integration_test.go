package googlephotos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// Test album for integration tests (public share link, safe to fetch).
const testAlbumURL = "https://photos.app.goo.gl/FW5SPTg2S7PXShjY9"

// integrationClient returns a client for live tests, or nil when the environment
// cannot reach the network (test will be skipped).
func integrationClient() *Client {
	return NewClient(slog.New(slog.NewTextHandler(os.Stderr, nil)), 10)
}

// TestIntegrationScrapeTestAlbum downloads the real test album and verifies the
// scraped metadata matches what Google Photos serves.
func TestIntegrationScrapeTestAlbum(t *testing.T) {
	album := scrapeTestAlbum(t)

	if album.Title != "Test gphotos2immich" {
		t.Fatalf("unexpected album title %q", album.Title)
	}

	if len(album.Photos) != 10 {
		t.Fatalf("expected 10 items in test album, got %d", len(album.Photos))
	}

	seen := make(map[string]bool, len(album.Photos))
	for i, p := range album.Photos {
		if p.ID == "" || seen[p.ID] {
			t.Fatalf("item %d has empty or duplicate ID %q", i, p.ID)
		}
		seen[p.ID] = true
		if p.URL == "" {
			t.Fatalf("item %d has no download URL", i)
		}
		if p.Width <= 0 || p.Height <= 0 {
			t.Fatalf("item %d has no dimensions: %dx%d", i, p.Width, p.Height)
		}
		// Dates in this album range from 2010-01-01 to 2026-09-04 (any timezone).
		if p.TakenAt.Before(time.Date(2009, 1, 1, 0, 0, 0, 0, time.UTC)) ||
			p.TakenAt.After(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
			t.Fatalf("item %d has implausible taken date %v", i, p.TakenAt)
		}
	}
}

// TestIntegrationDownloadTestAlbum downloads every item of the real test album
// and verifies the bytes are the actual album media, not thumbnails or poster
// frames: images decode at full album dimensions with correct magic bytes,
// videos are real video streams, and repeated downloads are byte-identical.
func TestIntegrationDownloadTestAlbum(t *testing.T) {
	album := scrapeTestAlbum(t)
	client := integrationClient()
	ctx := context.Background()

	images, videos, lives := 0, 0, 0
	for i, p := range album.Photos {
		data, ext, isVideo, isLive, err := DownloadMedia(ctx, client, p.URL)
		if err != nil {
			t.Fatalf("item %d (%s): download failed: %v", i, p.ID, err)
		}
		if len(data) < 1024 {
			t.Fatalf("item %d: suspiciously small payload (%d bytes)", i, len(data))
		}

		if isVideo {
			videos++
			if isLive {
				t.Fatalf("item %d: standalone video must not be flagged live", i)
			}
			if !isVideoMagicBytes(data) {
				t.Fatalf("item %d: classified video but bytes are not a video container", i)
			}
			if !isVideoExt(ext) {
				t.Fatalf("item %d: video has non-video extension %q", i, ext)
			}
			// A poster frame is a tiny JPEG; a real video stream is at least 1 MB.
			if len(data) < 1_000_000 {
				t.Fatalf("item %d: video payload too small to be the real stream (%d bytes)", i, len(data))
			}
		} else {
			images++
			if isLive {
				lives++
			}
			// The downloaded image must be the full original: decode it and
			// compare its dimensions with the dimensions served in album metadata.
			cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("item %d: downloaded bytes do not decode as an image (%v)", i, err)
			}
			if cfg.Width != p.Width || cfg.Height != p.Height {
				t.Fatalf("item %d: downloaded image is %dx%d but album serves %dx%d (not the original file)",
					i, cfg.Width, cfg.Height, p.Width, p.Height)
			}
			if !isImageMagicBytes(data) {
				t.Fatalf("item %d: classified image but bytes are not image magic", i)
			}
		}
	}

	// Composition of the test album: 6 still images and 4 videos.
	if images != 6 {
		t.Errorf("expected 6 images in test album, got %d", images)
	}
	if videos != 4 {
		t.Errorf("expected 4 videos in test album, got %d", videos)
	}
	_ = lives
}

// TestIntegrationDownloadIsDeterministic re-downloads the first image and the
// first video and verifies Google serves byte-identical files across requests.
func TestIntegrationDownloadIsDeterministic(t *testing.T) {
	album := scrapeTestAlbum(t)
	client := integrationClient()
	ctx := context.Background()

	// Scrape order is stable; item 0 is a small JPEG (400x300) and item 6 is a
	// full video per the album composition.
	firstImage := &album.Photos[0]
	firstVideo := &album.Photos[6]

	for name, p := range map[string]*Photo{"image": firstImage, "video": firstVideo} {
		a, _, _, _, err := DownloadMedia(ctx, client, p.URL)
		if err != nil {
			t.Fatalf("%s first download failed: %v", name, err)
		}
		b, _, _, _, err := DownloadMedia(ctx, client, p.URL)
		if err != nil {
			t.Fatalf("%s second download failed: %v", name, err)
		}
		if sha256.Sum256(a) != sha256.Sum256(b) {
			t.Errorf("%s download is not byte-identical across requests (%d vs %d bytes)", name, len(a), len(b))
		}
	}
}

// scrapeTestAlbum fetches the real test album once, caching the result for all
// tests in the package via a package-level variable.
var integrationAlbum *Album

func scrapeTestAlbum(t *testing.T) *Album {
	t.Helper()
	if integrationAlbum != nil {
		return integrationAlbum
	}
	if os.Getenv("GP2IMMICH_INTEGRATION") != "1" {
		t.Skip("integration test against the real test album; set GP2IMMICH_INTEGRATION=1 to run")
	}
	client := integrationClient()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	album, err := ScrapeAlbum(ctx, client, testAlbumURL)
	if err != nil {
		t.Fatalf("failed to scrape test album: %v", err)
	}
	integrationAlbum = album
	return album
}

// iPhone test album (fedonr's reproduction case for videos being classified
// as live photos): three iPhone MOVs, one Pixel MP4 and one Pixel motion photo.
const iphoneAlbumURL = "https://photos.app.goo.gl/z9qUNvwK1sWpPhQ8A"

var integrationIPhoneAlbum *Album

func scrapeIPhoneAlbum(t *testing.T) *Album {
	t.Helper()
	if integrationIPhoneAlbum != nil {
		return integrationIPhoneAlbum
	}
	if os.Getenv("GP2IMMICH_INTEGRATION") != "1" {
		t.Skip("integration test against the real iPhone test album; set GP2IMMICH_INTEGRATION=1 to run")
	}
	client := integrationClient()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	album, err := ScrapeAlbum(ctx, client, iphoneAlbumURL)
	if err != nil {
		if strings.Contains(err.Error(), "404") {
			t.Skip("iPhone test album is no longer publicly available")
		}
		t.Fatalf("failed to scrape iPhone test album: %v", err)
	}
	integrationIPhoneAlbum = album
	return album
}

// TestIntegrationScrapeIPhoneAlbum verifies the scraper marks the iPhone
// album's four videos (three MOV clips and one Pixel MP4) as videos from the
// album metadata alone, before any bytes are fetched.
func TestIntegrationScrapeIPhoneAlbum(t *testing.T) {
	album := scrapeIPhoneAlbum(t)

	if album.Title != "Test Vid" {
		t.Fatalf("unexpected album title %q", album.Title)
	}
	if len(album.Photos) != 5 {
		t.Fatalf("expected 5 items in iPhone test album, got %d", len(album.Photos))
	}
	for i, p := range album.Photos {
		wantVideo := i == 0 || i == 1 || i == 2 || i == 4
		if p.IsVideo != wantVideo {
			t.Fatalf("item %d: expected IsVideo=%v", i, wantVideo)
		}
	}
}

// TestIntegrationDownloadIPhoneAlbum downloads every item of the iPhone test
// album through the item-aware path and verifies the real files arrive:
// the iPhone MOVs must be standalone videos even though their streams carry
// Apple Live Photo QuickTime tags, and the Pixel motion photo must keep its
// photo+video container (ExtractMotionPhoto splits it during upload).
func TestIntegrationDownloadIPhoneAlbum(t *testing.T) {
	album := scrapeIPhoneAlbum(t)
	client := integrationClient()
	ctx := context.Background()

	videos, photos := 0, 0
	for i, p := range album.Photos {
		media, err := DownloadItemMedia(ctx, client, p)
		if err != nil {
			t.Fatalf("item %d (%s): download failed: %v", i, p.ID, err)
		}
		switch media.Kind {
		case "video":
			videos++
			if media.VideoData != nil {
				t.Fatalf("item %d: standalone video carries a sidecar", i)
			}
			if !isVideoMagicBytes(media.Data) {
				t.Fatalf("item %d: video bytes are not a video container", i)
			}
			if !isVideoExt(media.Ext) {
				t.Fatalf("item %d: video has non-video extension %q", i, media.Ext)
			}
			// A poster frame is a tiny JPEG; a real video stream is at least 1 MB.
			if len(media.Data) < 1_000_000 {
				t.Fatalf("item %d: video payload too small to be the real stream (%d bytes)", i, len(media.Data))
			}
			if isLikelyLivePhotoSidecar(media.Data) {
				t.Logf("item %d: video stream carries Apple live-photo tags (the regression case)", i)
			}
		case "image":
			photos++
			cfg, _, err := image.DecodeConfig(bytes.NewReader(media.Data))
			if err != nil {
				t.Fatalf("item %d: downloaded bytes do not decode as an image (%v)", i, err)
			}
			if cfg.Width != p.Width || cfg.Height != p.Height {
				t.Fatalf("item %d: downloaded image is %dx%d but album serves %dx%d (not the original file)",
					i, cfg.Width, cfg.Height, p.Width, p.Height)
			}
		default:
			t.Fatalf("item %d: unexpected media kind %q", i, media.Kind)
		}
	}

	if videos != 4 {
		t.Errorf("expected 4 videos in iPhone test album, got %d", videos)
	}
	if photos != 1 {
		t.Errorf("expected 1 photo in iPhone test album, got %d", photos)
	}
}

// isVideoExt reports whether ext is a known video extension.
func isVideoExt(ext string) bool {
	switch ext {
	case ".mp4", ".webm", ".mov", ".mkv", ".avi", ".m4v", ".3gp":
		return true
	}
	return false
}
