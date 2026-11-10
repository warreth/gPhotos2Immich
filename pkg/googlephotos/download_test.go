package googlephotos

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ftypBox builds a minimal MP4 ftyp box so video payloads look real to sniffers.
func ftypBox(brand string) []byte {
	box := make([]byte, 16)
	binary.BigEndian.PutUint32(box[0:4], 16) // box size
	copy(box[4:8], "ftyp")
	copy(box[8:12], brand)
	return box
}

// fakeJPEG builds a minimal JPEG (SOI + APP0 marker) so image payloads pass sniffing.
func fakeJPEG() []byte {
	return []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01}
}

// videoPayload returns video bytes large enough to pass all size guards.
func videoPayload(brand string) []byte {
	return append(ftypBox(brand), bytes.Repeat([]byte{0xAA}, 2048)...)
}

// roundTripperFunc adapts a function into an http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// newTestClient wires an http.Handler into a Client without a real network,
// so every test exercises the full download-and-classify logic locally.
// The handler receives the real request paths ("/item=d", "/item=dv").
func newTestClient(h http.Handler) *Client {
	c := NewClient(slog.New(slog.NewTextHandler(io.Discard, nil)), 10)
	c.client.Timeout = 2 * time.Second
	c.client.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Result(), nil
	})
	return c
}

// Issue #8 regression: shared-album videos where "=d" serves a JPEG poster
// frame and "=dv" the real video must be classified and saved as videos.
func TestDownloadMediaVideoFromPosterFrame(t *testing.T) {
	data, ext, isVideo, isLive, err := DownloadMedia(context.Background(),
		newTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/item=d":
				w.Header().Set("Content-Type", "image/jpeg")
				w.Write(fakeJPEG())
			case "/item=dv":
				w.Header().Set("Content-Type", "video/mp4")
				w.Write(videoPayload("isom"))
			}
		})), "http://test/item")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isVideo {
		t.Fatalf("expected item to be classified as video, got image (ext=%s, size=%d)", ext, len(data))
	}
	if isLive {
		t.Fatal("standalone video must not be flagged as live photo")
	}
	if ext != ".mp4" {
		t.Fatalf("expected .mp4 extension, got %s", ext)
	}
	if !isVideoMagicBytes(data) {
		t.Fatal("downloaded data is not video bytes")
	}
}

// Live photo JPEG component: "=d" returns the photo and "=dv" its Apple
// sidecar. The image must stay the primary asset and be flagged as live.
func TestDownloadMediaLivePhotoSidecar(t *testing.T) {
	data, ext, isVideo, isLive, err := DownloadMedia(context.Background(),
		newTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/item=d":
				w.Header().Set("Content-Type", "image/jpeg")
				w.Write(fakeJPEG())
			case "/item=dv":
				w.Header().Set("Content-Type", "video/quicktime")
				w.Write(videoPayload("qt  "))
				w.Write([]byte("com.apple.quicktime.content.identifier"))
			}
		})), "http://test/item")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if isVideo {
		t.Fatal("live photo image must not be replaced by its video sidecar")
	}
	if !isLive {
		t.Fatal("expected live photo sidecar to be detected")
	}
	if ext != ".jpg" {
		t.Fatalf("expected .jpg extension for live photo image, got %s", ext)
	}
	if !isImageMagicBytes(data) {
		t.Fatal("primary asset must still be the image bytes")
	}
}

// Issue #21: an iPhone HEIC Live Photo serves the HEIC still on "=d" and the
// QuickTime clip on "=dv". The HEIC ftyp box must not be mistaken for a video.
func TestDownloadMediaHeicLivePhoto(t *testing.T) {
	heic := append(ftypBox("heic"), bytes.Repeat([]byte{0x11}, 2048)...)
	data, ext, isVideo, isLive, err := DownloadMedia(context.Background(),
		newTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/item=d":
				w.Header().Set("Content-Type", "image/heif")
				w.Write(heic)
			case "/item=dv":
				w.Header().Set("Content-Type", "video/quicktime")
				w.Write(videoPayload("qt  "))
				w.Write([]byte("com.apple.quicktime.content.identifier"))
			}
		})), "http://test/item")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if isVideo {
		t.Fatal("HEIC still must not be replaced by its video sidecar")
	}
	if !isLive {
		t.Fatal("expected live photo sidecar to be detected")
	}
	if ext != ".heic" {
		t.Fatalf("expected .heic extension, got %s", ext)
	}
	if !bytes.Equal(data, heic) {
		t.Fatal("primary asset must be the HEIC bytes")
	}
}

// A plain HEIC photo with no "=dv" stream must be stored as an image.
func TestDownloadMediaPlainHeic(t *testing.T) {
	heic := append(ftypBox("heic"), bytes.Repeat([]byte{0x11}, 2048)...)
	data, ext, isVideo, isLive, err := DownloadMedia(context.Background(),
		newTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/item=d":
				w.Header().Set("Content-Type", "image/heif")
				w.Write(heic)
			case "/item=dv":
				http.Error(w, "not found", http.StatusNotFound)
			}
		})), "http://test/item")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if isVideo || isLive {
		t.Fatalf("plain HEIC misclassified (isVideo=%v isLive=%v)", isVideo, isLive)
	}
	if ext != ".heic" {
		t.Fatalf("expected .heic extension, got %s", ext)
	}
	if !bytes.Equal(data, heic) {
		t.Fatal("expected HEIC bytes")
	}
}

// AVIF is also an ISO BMFF image and must not be treated as a video.
func TestDownloadMediaPlainAVIF(t *testing.T) {
	avif := append(ftypBox("avif"), bytes.Repeat([]byte{0x22}, 2048)...)
	data, ext, isVideo, isLive, err := DownloadMedia(context.Background(),
		newTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/item=d":
				w.Header().Set("Content-Type", "image/avif")
				w.Write(avif)
			case "/item=dv":
				http.Error(w, "not found", http.StatusNotFound)
			}
		})), "http://test/item")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if isVideo || isLive {
		t.Fatalf("AVIF misclassified (isVideo=%v isLive=%v)", isVideo, isLive)
	}
	if ext != ".avif" {
		t.Fatalf("expected .avif extension, got %s", ext)
	}
	if !bytes.Equal(data, avif) {
		t.Fatal("expected AVIF bytes")
	}
}

// A plain image whose "=dv" probe fails must be returned untouched as an image.
func TestDownloadMediaPlainImage(t *testing.T) {
	data, ext, isVideo, isLive, err := DownloadMedia(context.Background(),
		newTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/item=d":
				w.Header().Set("Content-Type", "image/jpeg")
				w.Write(fakeJPEG())
			case "/item=dv":
				http.Error(w, "not found", http.StatusNotFound)
			}
		})), "http://test/item")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if isVideo || isLive {
		t.Fatalf("plain image misclassified (isVideo=%v isLive=%v)", isVideo, isLive)
	}
	if ext != ".jpg" {
		t.Fatalf("expected .jpg extension, got %s", ext)
	}
	if !isImageMagicBytes(data) {
		t.Fatal("expected image bytes")
	}
}

// A video served directly on "=d" (no poster frame) must be re-fetched via
// "=dv" so the playable stream is stored instead of a container variant.
func TestDownloadMediaDirectVideo(t *testing.T) {
	data, ext, isVideo, isLive, err := DownloadMedia(context.Background(),
		newTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/item=d":
				w.Header().Set("Content-Type", "video/mp4")
				w.Write(videoPayload("isom"))
			case "/item=dv":
				w.Header().Set("Content-Type", "video/mp4")
				w.Write(videoPayload("mp42"))
			}
		})), "http://test/item")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isVideo {
		t.Fatal("expected video classification")
	}
	if isLive {
		t.Fatal("direct video must not be flagged as live photo")
	}
	if ext != ".mp4" {
		t.Fatalf("expected .mp4 extension, got %s", ext)
	}
	if !bytes.Contains(data, ftypBox("mp42")) {
		t.Fatal("expected the =dv stream bytes")
	}
}

// Sidecar validation: a non-video "=dv" response must be rejected instead of
// being saved as a bogus video asset.
func TestDownloadMediaRejectsFakeSidecar(t *testing.T) {
	data, isVideo, isLive, err := func() ([]byte, bool, bool, error) {
		d, _, v, l, e := DownloadMedia(context.Background(),
			newTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/item=d":
					w.Header().Set("Content-Type", "image/jpeg")
					w.Write(fakeJPEG())
				case "/item=dv":
					// HTML error page served with 200 and a video content type.
					w.Header().Set("Content-Type", "video/mp4")
					w.Write([]byte("<html>error page</html>"))
				}
			})), "http://test/item")
		return d, v, l, e
	}()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if isVideo || isLive {
		t.Fatalf("fake sidecar misclassified (isVideo=%v isLive=%v)", isVideo, isLive)
	}
	if !isImageMagicBytes(data) {
		t.Fatal("expected original image bytes when sidecar is invalid")
	}
}

// Download errors on "=d" must propagate to the caller.
func TestDownloadMediaDownloadError(t *testing.T) {
	// 404 is terminal (no retry), unlike 5xx which retries with real backoff.
	_, _, _, _, err := DownloadMedia(context.Background(),
		newTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "not found", http.StatusNotFound)
		})), "http://test/item")
	if err == nil {
		t.Fatal("expected error from failed download")
	}
}
